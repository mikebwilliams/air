package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
)

const reposeVersion = "0.5-dev"

func runReposeCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		printReposeUsage(environment.Stdout)
		return nil
	}
	if args[0] == "help" {
		return runReposeHelpCommand(args[1:], environment)
	}
	if args[0] == "--help" || args[0] == "-h" {
		printReposeUsage(environment.Stdout)
		return nil
	}
	if args[0] == "--version" {
		return runReposeVersionCommand(ctx, args[1:], environment)
	}
	command, found := findReposeCLICommand(args[0])
	if !found {
		return fmt.Errorf("unknown command %q; run repose help", args[0])
	}
	if containsHelpFlag(args[1:]) {
		printReposeCommandHelp(environment.Stdout, commandHelpTarget(command, args[1:]))
		return nil
	}
	return command.Run(ctx, args[1:], environment)
}

func runReposeVersionCommand(_ context.Context, args []string, environment cliEnvironment) error {
	if len(args) != 0 {
		return errors.New("usage: repose version")
	}
	fmt.Fprintf(environment.Stdout, "repose %s\n", reposeVersion)
	return nil
}

func runReposeInventoryCommand(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		return errors.New("usage: repose inventory <build|list|show|files|tree|groups|inspect|group|annotate|plan|browse|index|index-status|symbols|includes|check|approve|exclude|include|exclusions|policy>")
	}
	return runInventoryCLI(ctx, args, environment)
}

func runReposeStatsCommand(ctx context.Context, args []string, environment cliEnvironment) error {
	return runReposeStatsCLI(ctx, "stats", args, environment)
}

func runReposeCostCommand(ctx context.Context, args []string, environment cliEnvironment) error {
	return runReposeStatsCLI(ctx, "cost", args, environment)
}

func runReposeFindingsCommand(ctx context.Context, args []string, environment cliEnvironment) error {
	return runAuditFindingsCLI(ctx, append([]string{"findings"}, args...), environment)
}

func runReposeFindingCommand(ctx context.Context, args []string, environment cliEnvironment) error {
	return runAuditFindingsCLI(ctx, append([]string{"finding"}, args...), environment)
}

func runInventoryCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	command := args[0]
	switch command {
	case "index", "index-status", "symbols", "includes":
		return runSemanticCLI(ctx, args, environment)
	case "tree", "groups", "inspect", "group", "annotate", "plan", "browse":
		return runInventoryWorkspaceCLI(ctx, args, environment)
	case "exclude", "include", "exclusions", "policy":
		return runInventoryPolicyCLI(ctx, args, environment)
	case "build", "list", "show", "files", "check", "approve":
	default:
		return fmt.Errorf("unknown inventory command %q; run repose help", command)
	}
	flags := newFlagSet("inventory "+command, environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	asJSON := flags.Bool("json", false, "output JSON")
	var compileCommands, policyPath, prefix, group, tag string
	status := "included"
	if command == "build" {
		flags.StringVar(&compileCommands, "compile-commands", "build/compile_commands.json", "compilation database relative to the checkout")
		flags.StringVar(&policyPath, "policy", "", "JSON inventory rules")
	}
	if command == "files" {
		flags.StringVar(&prefix, "path", ".", "repository-relative file or directory")
		flags.StringVar(&group, "group", "", "inventory group")
		flags.StringVar(&tag, "tag", "", "inventory tag")
		flags.StringVar(&status, "status", "included", "included, excluded, all, missing-command, header-unmapped")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() > 1 || (command == "build" || command == "list") && flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments for repose inventory %s", command)
	}
	selector := flags.Arg(0)
	if command == "approve" && (selector == "" || selector == "latest" || selector == "current") {
		return errors.New("approval requires an explicit inventory ID; inspect it with repose inventory show first")
	}
	if command == "files" {
		if err := validateInventoryPolicy(InventoryPolicy{Rules: []InventoryRule{{Prefix: prefix}}}); err != nil {
			return fmt.Errorf("invalid --path: %w", err)
		}
		switch status {
		case "included", "excluded", "all", "missing-command", "header-unmapped":
		default:
			return fmt.Errorf("unknown file status %q", status)
		}
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repoPath))
	if err != nil {
		return err
	}
	if command == "build" {
		return runInventoryBuildCLI(ctx, repository, compileCommands, flags.Changed("compile-commands"), policyPath, *asJSON, environment)
	}
	databasePath, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	var store *inventoryStore
	if command == "approve" {
		store, err = openInventoryStore(ctx, databasePath, false)
	} else {
		store, err = openInventoryReadOnly(ctx, databasePath)
	}
	if err != nil {
		return err
	}
	defer store.Close()
	if command == "list" {
		items, err := store.ListInventories(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, items)
		}
		for _, item := range items {
			current := ""
			if item.Current {
				current = " (current)"
			}
			fmt.Fprintf(environment.Stdout, "%s  %s  %s  %s%s\n", item.ID[:12], shortSHA(item.SnapshotSHA), item.CreatedAt.Format("2006-01-02 15:04:05Z07:00"), inventoryReviewLabel(item.ReviewedAt != nil), current)
		}
		if len(items) == 0 {
			fmt.Fprintln(environment.Stdout, "No saved inventories.")
		}
		return nil
	}
	record, err := store.Inventory(ctx, selector)
	if err != nil {
		return err
	}
	switch command {
	case "show":
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, record)
		}
		return printInventorySummary(environment.Stdout, record)
	case "files":
		files := []InventoryFile{}
		for _, file := range record.Inventory.Files {
			if !inventoryPrefixMatches(file.Path, prefix) || group != "" && file.Group != group ||
				tag != "" && !slices.Contains(file.Tags, tag) || !inventoryFileMatchesStatus(file, status) {
				continue
			}
			files = append(files, file)
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, files)
		}
		writer := tabwriter.NewWriter(environment.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "PATH\tGROUP\tKIND\tBYTES\tCOMMANDS\tEXCLUSION")
		for _, file := range files {
			fmt.Fprintf(writer, "%q\t%q\t%s\t%d\t%d\t%q\n", file.Path, file.Group, file.Kind, file.Bytes, len(file.CommandIDs), file.ExclusionReason)
		}
		return writer.Flush()
	case "check", "approve":
		if err := checkInventory(ctx, repository, record.Inventory); err != nil {
			return err
		}
		if command == "approve" {
			if err := store.MarkInventoryReviewed(ctx, record.ID, environmentNow(environment)); err != nil {
				return err
			}
			record, err = store.Inventory(ctx, record.ID)
			if err != nil {
				return err
			}
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, record)
		}
		if command == "approve" {
			fmt.Fprintf(environment.Stdout, "Inventory %s approved.\n", record.ID[:12])
		} else {
			fmt.Fprintf(environment.Stdout, "Inventory %s matches the checkout and recorded build inputs.\n", record.ID[:12])
		}
		return nil
	}
	return nil
}

func runInventoryBuildCLI(ctx context.Context, repository *GitRepository, compilePath string, explicitCompilePath bool, policyPath string, asJSON bool, environment cliEnvironment) error {
	options := inventoryBuildOptions{CompileCommands: compilePath, Policy: InventoryPolicy{Rules: []InventoryRule{}}}
	if policyPath != "" {
		data, err := os.ReadFile(reposeAbsolutePath(environment.Cwd, policyPath))
		if err != nil {
			return fmt.Errorf("read inventory policy: %w", err)
		}
		options.Policy, err = readInventoryPolicy(strings.NewReader(string(data)))
		if err != nil {
			return err
		}
	}
	databasePath, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	var store *inventoryStore
	defer func() {
		if store != nil {
			store.Close()
		}
	}()
	currentID := ""
	if _, err := os.Stat(databasePath); err == nil {
		store, err = openInventoryStore(ctx, databasePath, false)
		if err != nil {
			return err
		}
		currentID, err = store.CurrentInventoryID(ctx)
		if err != nil {
			return err
		}
		if currentID != "" {
			current, err := store.Inventory(ctx, currentID)
			if err != nil {
				return err
			}
			if policyPath == "" {
				options.Policy = current.Inventory.Policy
				options.AllowUnmatchedPolicy = true
			}
			if !explicitCompilePath {
				options.CompileCommands = current.Inventory.BuildInputs[0].Path
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	inventory, err := buildInventory(ctx, repository, options)
	if err != nil {
		return err
	}
	if store == nil {
		store, err = openInventoryStore(ctx, databasePath, true)
		if err != nil {
			return err
		}
	}
	record, err := store.saveCurrentInventory(ctx, inventory, &currentID, environmentNow(environment))
	if err != nil {
		return err
	}
	if asJSON {
		return writeInventoryJSON(environment.Stdout, record)
	}
	return printInventorySummary(environment.Stdout, record)
}

func reposeAbsolutePath(cwd, filename string) string {
	if filepath.IsAbs(filename) {
		return filepath.Clean(filename)
	}
	return filepath.Join(cwd, filename)
}

func inventoryFileMatchesStatus(file InventoryFile, status string) bool {
	switch status {
	case "all":
		return true
	case "included":
		return !file.Excluded
	case "excluded":
		return file.Excluded
	case "missing-command":
		return !file.Excluded && file.Kind == "source" && len(file.CommandIDs) == 0
	case "header-unmapped":
		return !file.Excluded && file.Kind == "header" && len(file.CommandIDs) == 0
	}
	return false
}

func inventoryReviewLabel(reviewed bool) string {
	if reviewed {
		return "approved"
	}
	return "awaiting review"
}

func writeInventoryJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printInventorySummary(output io.Writer, record InventoryRecord) error {
	inventory := record.Inventory
	fmt.Fprintf(output, "Inventory: %s (%s)\nSnapshot:  %s\nCheckout:  %s\n", record.ID[:12], inventoryReviewLabel(record.ReviewedAt != nil), inventory.SnapshotSHA, inventory.WorkTree)
	fmt.Fprintf(output, "Build:     %s\n", inventory.BuildInputs[0].Path)
	type counts struct{ included, sources, headers, excluded, missing int }
	groups := make(map[string]*counts)
	var total counts
	tracked := make(map[string]bool, len(inventory.Files))
	unmappedHeaders := 0
	for _, file := range inventory.Files {
		tracked[filepath.Join(inventory.WorkTree, filepath.FromSlash(file.Path))] = true
		if groups[file.Group] == nil {
			groups[file.Group] = &counts{}
		}
		for _, count := range []*counts{groups[file.Group], &total} {
			if file.Excluded {
				count.excluded++
			} else {
				count.included++
				if file.Kind == "source" {
					count.sources++
					if len(file.CommandIDs) == 0 {
						count.missing++
					}
				} else if file.Kind == "header" {
					count.headers++
				}
			}
		}
		if inventoryFileMatchesStatus(file, "header-unmapped") {
			unmappedHeaders++
		}
	}
	external, untracked := make(map[string]bool), make(map[string]bool)
	for _, command := range inventory.Commands {
		relative, err := filepath.Rel(inventory.WorkTree, command.File)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			external[command.File] = true
		} else if !tracked[command.File] {
			untracked[command.File] = true
		}
	}
	fmt.Fprintf(output, "Files:     %d tracked; %d in scope (%d sources, %d headers); %d excluded\n", len(inventory.Files), total.included, total.sources, total.headers, total.excluded)
	fmt.Fprintf(output, "Commands:  %d entries; %d external and %d untracked input files outside the inventory\n", len(inventory.Commands), len(external), len(untracked))
	fmt.Fprintf(output, "Coverage:  %d in-scope sources without commands; %d headers without direct compilation commands\n", total.missing, unmappedHeaders)
	fmt.Fprintln(output, "Use inventory index-status for saved semantic coverage. Inventory approval does not run a model.")
	if unmatched := inventoryUnmatchedPrefixes(inventory); len(unmatched) > 0 {
		fmt.Fprintf(output, "Retained policy prefixes without matches in this snapshot: %s\n", strings.Join(unmatched, ", "))
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "\nGROUP\tIN SCOPE\tSOURCES\tHEADERS\tEXCLUDED\tMISSING COMMAND")
	for _, key := range keys {
		count := groups[key]
		fmt.Fprintf(writer, "%q\t%d\t%d\t%d\t%d\t%d\n", key, count.included, count.sources, count.headers, count.excluded, count.missing)
	}
	return writer.Flush()
}
