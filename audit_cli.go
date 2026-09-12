package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func runAuditCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		return errors.New("usage: repose scan <create|run|resume|list|show|tasks|attempts|prompt|pause|interrupt>")
	}
	command := args[0]
	switch command {
	case "create", "run", "resume", "list", "show", "tasks", "attempts", "prompt", "pause", "interrupt":
	default:
		return fmt.Errorf("unknown scan command %q", command)
	}
	flags := newFlagSet("scan "+command, environment.Stderr)
	repo := flags.String("repo", ".", "dedicated scan checkout")
	asJSON := flags.Bool("json", false, "output JSON")
	selection := inventorySelection{Path: ".", Status: "included"}
	limits := inventoryPlanLimits{8, 65536}
	config := auditModelConfig{Harness: "codex", Effort: "high", Timeout: defaultAuditTimeout}
	goal, instructionsFile, inventoryID, indexID := "Find correctness issues.", "", "current", ""
	runOptions := auditRunOptions{Jobs: 2}
	if command == "create" {
		flags.StringVar(&inventoryID, "inventory", "current", "approved inventory ID")
		flags.StringVar(&indexID, "index", "", "saved semantic profile ID")
		flags.StringVar(&selection.Path, "path", ".", "target path prefix")
		flags.StringVar(&selection.Group, "group", "", "target group")
		flags.StringVar(&selection.Tag, "tag", "", "target tag")
		flags.StringVar(&goal, "goal", goal, "review question")
		flags.IntVar(&limits.MaxFiles, "max-files", 8, "files per assignment")
		flags.Int64Var(&limits.MaxBytes, "max-bytes", 65536, "target bytes per assignment")
		flags.StringVar(&config.Harness, "harness", "codex", "codex, claude, or gemini")
		flags.StringVar(&config.Model, "model", "", "model identifier (required)")
		flags.StringVar(&config.Effort, "effort", "high", "reasoning effort; Gemini requires default")
		flags.StringVar(&config.Binary, "binary", "", "runner executable")
		flags.DurationVar(&config.Timeout, "timeout", config.Timeout, "per-assignment timeout")
		flags.StringVar(&instructionsFile, "instructions", "", "file of project guidance to freeze in the scan")
	}
	if command == "run" || command == "resume" {
		flags.IntVar(&runOptions.Jobs, "jobs", 2, "parallel assignments (1..32)")
		flags.IntVar(&runOptions.Limit, "limit", 0, "total assignment attempts across all workers, including retries; 0 is unlimited")
		flags.DurationVar(&runOptions.Duration, "duration", 0, "stop dispatching after this duration; let active tasks finish")
		flags.DurationVar(&runOptions.Timeout, "timeout", 0, "override the saved per-assignment timeout for this invocation (positive duration)")
		flags.BoolVar(&runOptions.RetryFailed, "retry-failed", false, "retry failed assignments, retaining previous attempts")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if (command == "run" || command == "resume") && flags.Changed("timeout") && runOptions.Timeout <= 0 {
		return errors.New("--timeout must be a positive duration")
	}
	expected := 1
	if command == "create" || command == "list" {
		expected = 0
	}
	if command == "prompt" {
		expected = 2
	}
	if command == "resume" && flags.NArg() > 1 {
		return errors.New("scan resume expects zero or one scan ID")
	}
	if command != "resume" && flags.NArg() != expected {
		return fmt.Errorf("scan %s expects %d positional arguments", command, expected)
	}
	if command == "create" {
		if err := selection.validate(); err != nil {
			return err
		}
		if config.Harness != "codex" && config.Harness != "claude" && config.Harness != "gemini" {
			return errors.New("harness must be codex, claude, or gemini")
		}
		if strings.TrimSpace(config.Model) == "" || strings.TrimSpace(config.Effort) == "" || config.Timeout <= 0 {
			return errors.New("model, effort, and a positive timeout are required")
		}
		if config.Harness == "gemini" && config.Effort != "default" {
			return errors.New("Gemini requires --effort default")
		}
		if config.Binary == "" {
			config.Binary = config.Harness
		}
		binary, err := exec.LookPath(config.Binary)
		if err != nil {
			return err
		}
		config.Binary = reposeAbsolutePath(environment.Cwd, binary)
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repo))
	if err != nil {
		return err
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	writing := command == "create" || command == "run" || command == "resume" || command == "pause" || command == "interrupt"
	var store *inventoryStore
	if writing {
		store, err = openInventoryStore(ctx, database, false)
	} else {
		store, err = openInventoryReadOnly(ctx, database)
	}
	if err != nil {
		return err
	}
	defer store.Close()
	if command == "create" {
		record, err := store.Inventory(ctx, inventoryID)
		if err != nil {
			return err
		}
		if record.ReviewedAt == nil {
			return fmt.Errorf("inventory %s is awaiting review; approve that version with inventory approve %s", shortSHA(record.ID), record.ID[:12])
		}
		if err = checkInventory(ctx, repository, record.Inventory); err != nil {
			return err
		}
		snapshot, err := store.semanticSnapshot(ctx, record.Inventory, indexID)
		if err != nil {
			return err
		}
		plan, err := planInventory(record, selection, goal, limits)
		if err != nil {
			return err
		}
		if snapshot != nil {
			plan, err = planSemanticInventory(record, selection, goal, limits, snapshot)
			if err != nil {
				return err
			}
		}
		spec := auditSpec{Plan: plan, Model: config, PromptVersion: auditPromptVersion}
		if instructionsFile != "" {
			data, err := readLimitedFile(reposeAbsolutePath(environment.Cwd, instructionsFile), 65536)
			if err != nil {
				return err
			}
			spec.Instructions = string(data)
		}
		inputs, err := prepareAuditInputs(ctx, record, spec)
		if err != nil {
			return err
		}
		if err = checkInventory(ctx, repository, record.Inventory); err != nil {
			return err
		}
		scan, err := store.createAudit(ctx, record, spec, inputs, environmentNow(environment))
		if err != nil {
			return err
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, scan)
		}
		printAuditStatus(environment.Stdout, scan)
		fmt.Fprintf(environment.Stdout, "Created; no model calls yet. Start with: repose scan run %s --repo %s --jobs 2 --limit 4\n", scan.ID[:12], strconv.Quote(*repo))
		return nil
	}
	if command == "list" {
		if store.version < 4 {
			if *asJSON {
				return writeInventoryJSON(environment.Stdout, []auditScan{})
			}
			fmt.Fprintln(environment.Stdout, "No scans.")
			return nil
		}
		rows, err := store.db.QueryContext(ctx, "SELECT id FROM audit_scans ORDER BY rowid DESC")
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		scans := []auditScan{}
		for _, id := range ids {
			scan, err := store.audit(ctx, id)
			if err != nil {
				return err
			}
			scans = append(scans, scan)
			if !*asJSON {
				printAuditStatus(environment.Stdout, scan)
			}
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, scans)
		}
		return nil
	}
	selector := flags.Arg(0)
	if command == "resume" && flags.NArg() == 0 {
		ids, err := store.resumableAuditIDs(ctx, runOptions.RetryFailed)
		if err != nil {
			return err
		}
		switch len(ids) {
		case 0:
			message := "no scans have resumable work; use scan list to inspect scans"
			if !runOptions.RetryFailed {
				message += ", or --retry-failed to include failed assignments"
			}
			return errors.New(message)
		case 1:
			selector = ids[0]
			fmt.Fprintf(environment.Stderr, "Selected scan %s.\n", selector[:12])
		default:
			fmt.Fprintln(environment.Stderr, "Scans with resumable work:")
			for _, id := range ids {
				candidate, err := store.audit(ctx, id)
				if err != nil {
					return err
				}
				printAuditStatus(environment.Stderr, candidate)
			}
			return errors.New("multiple scans have resumable work; specify an ID with scan resume ID")
		}
	}
	scan, err := store.audit(ctx, selector)
	if err != nil {
		return err
	}
	switch command {
	case "show":
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, scan)
		}
		printAuditStatus(environment.Stdout, scan)
	case "run", "resume":
		runner := newAuditRunnerForScan(repository, environment, scan)
		runErr := runAudit(ctx, store, repository, scan, runOptions, runner, environment.Stderr)
		readContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		updated, err := store.audit(readContext, scan.ID)
		if err != nil {
			return err
		}
		if *asJSON {
			if err = writeInventoryJSON(environment.Stdout, updated); err != nil {
				return err
			}
		} else {
			printAuditStatus(environment.Stdout, updated)
		}
		return runErr
	case "pause", "interrupt":
		result, err := store.db.ExecContext(ctx, "UPDATE audit_scans SET control=? WHERE id=? AND status='running'", command, scan.ID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("scan has no running coordinator; use scan resume to recover abandoned tasks")
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, map[string]string{"scan_id": scan.ID, "requested": command})
		}
		fmt.Fprintf(environment.Stdout, "Requested %s for scan %s.\n", command, scan.ID[:12])
	case "tasks", "prompt":
		tasks, err := store.auditTasks(ctx, scan.ID)
		if err != nil {
			return err
		}
		if command == "prompt" {
			selector := flags.Arg(1)
			matches := []auditTask{}
			for _, task := range tasks {
				if strconv.Itoa(task.Ordinal) == selector || len(selector) >= 4 && strings.HasPrefix(task.ID, selector) {
					matches = append(matches, task)
				}
			}
			if len(matches) != 1 {
				return errors.New("task selector must identify one ordinal or assignment ID")
			}
			_, err = io.WriteString(environment.Stdout, matches[0].Input.Prompt)
			return err
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, tasks)
		}
		for _, task := range tasks {
			fmt.Fprintf(environment.Stdout, "%4d %s %-17s attempt=%d %d files / %d bytes\n", task.Ordinal, shortSHA(task.ID), task.Status, task.AttemptID, len(task.Input.Assignment.Files), task.Input.Assignment.Bytes)
		}
	case "attempts":
		attempts, err := store.auditAttempts(ctx, scan.ID)
		if err != nil {
			return err
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, attempts)
		}
		for _, attempt := range attempts {
			fmt.Fprintf(environment.Stdout, "%d %s #%d %s %dms", attempt.ID, shortSHA(attempt.TaskID), attempt.Number, attempt.Status, attempt.DurationMilliseconds)
			if attempt.Timeout > 0 {
				fmt.Fprintf(environment.Stdout, " timeout=%s", attempt.Timeout)
			}
			if attempt.Invocation.Usage != nil {
				fmt.Fprintf(environment.Stdout, " input=%d output=%d", attempt.Invocation.Usage.InputTokens, attempt.Invocation.Usage.OutputTokens)
			}
			fmt.Fprintln(environment.Stdout)
			if attempt.Error != "" {
				fmt.Fprintln(environment.Stdout, "  "+inventoryDisplay(attempt.Error))
			}
		}
	}
	return nil
}

func printAuditStatus(output io.Writer, scan auditScan) {
	units := "assignments"
	if scan.Spec.Recheck != nil {
		fmt.Fprintf(output, "Recheck of scan %s: %d confirmed, %d false positive, %d uncertain\n", shortSHA(scan.Spec.Recheck.SourceScanID), scan.Verdicts["confirmed"], scan.Verdicts["false_positive"], scan.Verdicts["uncertain"])
		if scan.Spec.Recheck.BatchMax > 0 {
			units = "batches"
			fmt.Fprintf(output, "%d findings; maximum %d per batch, grouped by original assignment\n", len(scan.Spec.Recheck.Findings), scan.Spec.Recheck.BatchMax)
		}
	}
	fmt.Fprintf(output, "Scan %s  %s  %s/%s/%s\nInventory %s  observed %s\n%d %s: %d pending, %d running, %d completed, %d unable to assess, %d failed\n", scan.ID[:12], scan.Status, scan.Spec.Model.Harness, scan.Spec.Model.Model, scan.Spec.Model.Effort,
		shortSHA(scan.Spec.Plan.InventoryID), shortSHA(scan.Spec.Plan.SnapshotSHA), len(scan.Spec.Plan.Assignments), units, scan.Counts["pending"], scan.Counts["running"], scan.Counts["completed"], scan.Counts["unable_to_assess"], scan.Counts["failed"])
	if scan.Control != "" {
		fmt.Fprintln(output, "Control requested:", scan.Control)
	}
	if scan.Blocked != nil {
		fmt.Fprintf(output, "Provider limit: %s: %s\n", scan.Blocked.Kind, inventoryDisplay(scan.Blocked.Message))
		if scan.Blocked.RetryAt != nil {
			fmt.Fprintln(output, "Provider retry time:", scan.Blocked.RetryAt.Format(time.RFC3339))
		}
		fmt.Fprintln(output, "Blocked assignments remain pending; use scan resume when capacity is available.")
	}
}
