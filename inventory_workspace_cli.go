package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

func runInventoryWorkspaceCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	command := args[0]
	flags := newFlagSet("inventory "+command, environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	var asJSON bool
	if command != "browse" {
		flags.BoolVar(&asJSON, "json", false, "output JSON")
	}
	selection := inventorySelection{Path: ".", Status: "included"}
	edit := inventoryAnnotationEdit{}
	editing := command == "group" || command == "annotate"
	if editing {
		if command == "group" {
			flags.StringVar(&edit.Group, "name", "", "group name")
		} else {
			flags.StringArrayVar(&edit.Tags, "tag", nil, "add a tag (repeatable)")
			flags.StringVar(&edit.Note, "note", "", "add a review note")
		}
	} else {
		flags.StringVar(&selection.Path, "path", ".", "repository-relative file or directory")
		flags.StringVar(&selection.Group, "group", "", "inventory group")
		flags.StringVar(&selection.Tag, "tag", "", "inventory tag")
		if command != "plan" {
			flags.StringVar(&selection.Status, "status", "included", "included, excluded, all, missing-command, header-unmapped")
		}
	}
	depth := 1
	if command == "tree" {
		flags.IntVar(&depth, "depth", 1, "levels of directories below --path")
	}
	goal := "Find correctness issues."
	limits := inventoryPlanLimits{MaxFiles: 8, MaxBytes: 65536}
	useSemantic, indexID := false, ""
	planTUI := false
	if command == "plan" {
		flags.BoolVar(&planTUI, "tui", false, "browse assignments, symbols, and context (uses saved index when available)")
		flags.BoolVar(&useSemantic, "semantic", false, "plan using saved symbols and include context")
		flags.StringVar(&indexID, "index", "", "semantic profile ID (implies --semantic)")
		flags.StringVar(&goal, "goal", goal, "question for every assignment")
		flags.IntVar(&limits.MaxFiles, "max-files", limits.MaxFiles, "maximum files per assignment")
		flags.Int64Var(&limits.MaxBytes, "max-bytes", limits.MaxBytes, "maximum target source bytes per assignment")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if planTUI && asJSON {
		return fmt.Errorf("--tui and --json cannot be used together")
	}
	if editing {
		if flags.NArg() == 0 {
			return fmt.Errorf("inventory %s requires at least one path prefix", command)
		}
		if command == "group" && strings.TrimSpace(edit.Group) == "" {
			return fmt.Errorf("group requires a nonblank --name")
		}
		if command == "annotate" && edit.Note == "" && len(edit.Tags) == 0 {
			return fmt.Errorf("annotate requires --tag or --note")
		}
	} else if flags.NArg() > 1 {
		return fmt.Errorf("inventory %s accepts at most one inventory ID", command)
	}
	if err := selection.validate(); err != nil {
		return err
	}
	if depth < 0 || depth > 100 {
		return fmt.Errorf("--depth must be between 0 and 100")
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repoPath))
	if err != nil {
		return err
	}
	databasePath, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	var store *inventoryStore
	if editing {
		store, err = openInventoryStore(ctx, databasePath, false)
	} else {
		store, err = openInventoryReadOnly(ctx, databasePath)
	}
	if err != nil {
		return err
	}
	defer store.Close()
	selector := flags.Arg(0)
	if editing {
		selector = "current"
	}
	record, err := store.Inventory(ctx, selector)
	if err != nil {
		return err
	}
	if editing {
		updated, err := editInventoryAnnotations(record.Inventory, flags.Args(), edit)
		if err != nil {
			return err
		}
		changes := inventoryAnnotationChanges(record.Inventory, updated)
		saved, err := store.saveCurrentInventory(ctx, updated, &record.ID, environmentNow(environment))
		if err != nil {
			return err
		}
		if asJSON {
			return writeInventoryJSON(environment.Stdout, struct {
				PreviousID string                      `json:"previous_id"`
				Record     InventoryRecord             `json:"record"`
				Changes    []inventoryAnnotationChange `json:"changes"`
			}{record.ID, saved, changes})
		}
		fmt.Fprintf(environment.Stdout, "Current inventory: %s (%s)\nAnnotations changed: %d files. Policy saved for future builds.\n",
			saved.ID[:12], inventoryReviewLabel(saved.ReviewedAt != nil), len(changes))
		for _, change := range changes[:min(20, len(changes))] {
			fmt.Fprintf(environment.Stdout, "%q  group=%q  tags=%q  notes=%q\n", change.Path, change.After.Group, change.After.Tags, change.After.Notes)
		}
		if len(changes) > 20 {
			fmt.Fprintln(environment.Stdout, "Showing the first 20 changed files; use --json for the complete list.")
		}
		return nil
	}
	files := selection.files(record.Inventory)
	switch command {
	case "tree":
		rows := inventoryTree(files, selection.Path, depth)
		if asJSON {
			return writeInventoryJSON(environment.Stdout, struct {
				InventoryID string             `json:"inventory_id"`
				Selection   inventorySelection `json:"selection"`
				Rows        []inventoryTreeRow `json:"rows"`
			}{record.ID, selection, rows})
		}
		fmt.Fprintf(environment.Stdout, "Inventory: %s  status: %s (counts include selected descendants)\n", record.ID[:12], selection.Status)
		writer := tabwriter.NewWriter(environment.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "PATH\tFILES\tSOURCES\tHEADERS\tEXCLUDED\tNO COMMAND\tBYTES")
		for _, row := range rows {
			fmt.Fprintf(writer, "%s%q\t%d\t%d\t%d\t%d\t%d\t%d\n", strings.Repeat("  ", row.Depth), row.Path,
				row.Files, row.Sources, row.Headers, row.Excluded, row.MissingCommand, row.Bytes)
		}
		return writer.Flush()
	case "groups":
		groups := inventoryGroups(files)
		if asJSON {
			return writeInventoryJSON(environment.Stdout, struct {
				InventoryID string                  `json:"inventory_id"`
				Selection   inventorySelection      `json:"selection"`
				Groups      []inventoryGroupSummary `json:"groups"`
			}{record.ID, selection, groups})
		}
		fmt.Fprintf(environment.Stdout, "Inventory: %s  status: %s\n", record.ID[:12], selection.Status)
		return printInventoryGroups(environment.Stdout, groups)
	case "inspect":
		inspection := inspectInventory(record, selection)
		if asJSON {
			return writeInventoryJSON(environment.Stdout, inspection)
		}
		fmt.Fprintf(environment.Stdout, "Inventory: %s\nSelection: path=%q group=%q tag=%q status=%s\n", record.ID[:12], selection.Path, selection.Group, selection.Tag, selection.Status)
		fmt.Fprintf(environment.Stdout, "%d files, %d bytes; %d sources, %d headers, %d excluded\n%d sources without commands; %d headers without direct commands (see index-status for semantic coverage)\n",
			inspection.Files, inspection.Bytes, inspection.Sources, inspection.Headers, inspection.Excluded, inspection.MissingCommand, inspection.UnmappedHeader)
		if err := printInventoryGroups(environment.Stdout, inspection.Groups); err != nil {
			return err
		}
		fmt.Fprintln(environment.Stdout, "Largest selected files (up to 10):")
		for _, file := range inspection.Largest {
			fmt.Fprintf(environment.Stdout, "  %9d  %q\n", file.Bytes, file.Path)
		}
		fmt.Fprintln(environment.Stdout, "Matching policy rules (in precedence order):")
		return writeInventoryJSON(environment.Stdout, inspection.Rules)
	case "plan":
		plan, err := planInventory(record, selection, goal, limits)
		if err != nil {
			return err
		}
		var snapshot *semanticSnapshot
		if useSemantic || indexID != "" || planTUI {
			snapshot, err = store.semanticSnapshot(ctx, record.Inventory, indexID)
			if err == nil && (snapshot != nil || useSemantic || indexID != "") {
				plan, err = planSemanticInventory(record, selection, goal, limits, snapshot)
			}
		}
		if err != nil {
			return err
		}
		if asJSON {
			return writeInventoryJSON(environment.Stdout, plan)
		}
		if planTUI {
			return runInventoryBrowser(ctx, environment, databasePath, record, selection, false, &inventoryPlanSession{Plan: plan, Semantic: snapshot})
		}
		return printInventoryPlan(environment.Stdout, plan)
	case "browse":
		return runInventoryBrowser(ctx, environment, databasePath, record, selection, selector == "" || selector == "current", nil)
	}
	return nil
}

func printInventoryGroups(output io.Writer, groups []inventoryGroupSummary) error {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "GROUP\tFILES\tSOURCES\tHEADERS\tEXCLUDED\tNO COMMAND\tBYTES\tTAGS\tNOTES")
	for _, group := range groups {
		fmt.Fprintf(writer, "%q\t%d\t%d\t%d\t%d\t%d\t%d\t%q\t%q\n", group.Name, group.Files, group.Sources,
			group.Headers, group.Excluded, group.MissingCommand, group.Bytes, group.Tags, group.Notes)
	}
	return writer.Flush()
}

func printInventoryPlan(output io.Writer, plan inventoryPlan) error {
	fmt.Fprintf(output, "Assignment preview: %s\nInventory: %s  snapshot: %s\nGoal: %q\n%d assignments, %d files, %d bytes\n",
		plan.ID[:12], plan.InventoryID[:12], shortSHA(plan.SnapshotSHA), plan.Goal, len(plan.Assignments), plan.Files, plan.Bytes)
	if plan.SemanticProfileID == "" {
		fmt.Fprintln(output, "File-based preview only; no scan is saved or started. Bytes measure target files, not model context. Headers await semantic association.")
	} else {
		fmt.Fprintf(output, "Semantic index: %s; byte ranges are zero-based, end-exclusive. Every selected byte remains covered. Context paths do not count against target limits. No scan is started.\n", plan.SemanticProfileID[:12])
	}
	for _, assignment := range plan.Assignments {
		fmt.Fprintf(output, "\n%s  group=%q  %d files  %d bytes\n", assignment.ID[:12], assignment.Group, len(assignment.Files), assignment.Bytes)
		if len(assignment.Ranges) > 0 {
			for _, part := range assignment.Ranges {
				fmt.Fprintf(output, "  %q bytes [%d,%d) %q\n", part.Path, part.StartByte, part.EndByte, part.Symbols)
			}
			for _, dependency := range assignment.Context {
				fmt.Fprintf(output, "  context %q (%s)\n", dependency.Path, dependency.Reason)
			}
		} else {
			for _, file := range assignment.Files {
				fmt.Fprintf(output, "  %q\n", file.Path)
			}
		}
		for _, warning := range assignment.Warnings {
			fmt.Fprintf(output, "  ! %q\n", warning)
		}
	}
	return nil
}
