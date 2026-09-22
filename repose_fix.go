package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func parseAuditFixID(value string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(value, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("fix ID must be a positive integer")
	}
	return id, nil
}

func runReposeFixCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		return errors.New("usage: repose fix <create|list|show|delete|run>")
	}
	command := args[0]
	switch command {
	case "create", "list", "show", "delete", "run":
	default:
		return fmt.Errorf("unknown fix command %q; run repose help fix", command)
	}
	flags := newFlagSet("fix "+command, environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	asJSON := false
	if command != "run" {
		flags.BoolVar(&asJSON, "json", false, "output JSON")
	}
	worktree := ""
	config := auditModelConfig{Harness: codexReviewerName, Effort: "high", Timeout: time.Hour}
	limit, retryFailed := 0, false
	if command == "run" {
		flags.StringVar(&worktree, "worktree", "", "development checkout to edit (required)")
		flags.StringVar(&config.Model, "model", "", "Codex model identifier (required)")
		flags.StringVar(&config.Effort, "effort", "high", "reasoning effort")
		flags.StringVar(&config.Binary, "binary", codexReviewerName, "Codex executable")
		flags.DurationVar(&config.Timeout, "timeout", time.Hour, "per-fix timeout")
		flags.IntVar(&limit, "limit", 0, "maximum queue entries to attempt; zero is unlimited")
		flags.BoolVar(&retryFailed, "retry-failed", false, "retry failed queue entries")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	switch command {
	case "create":
		if flags.NArg() == 0 {
			return errors.New("usage: repose fix create FINDING_ID [FINDING_ID ...] [--json] [--repo DIR]")
		}
	case "list":
		if flags.NArg() != 0 {
			return errors.New("usage: repose fix list [--json] [--repo DIR]")
		}
	case "show", "delete":
		if flags.NArg() != 1 {
			return fmt.Errorf("usage: repose fix %s FIX_ID [--json] [--repo DIR]", command)
		}
	case "run":
		if flags.NArg() != 0 {
			return errors.New("usage: repose fix run --worktree DIR --model MODEL [OPTIONS]")
		}
		if strings.TrimSpace(worktree) == "" || strings.TrimSpace(config.Model) == "" {
			return errors.New("fix run requires --worktree DIR and --model MODEL")
		}
		if strings.TrimSpace(config.Effort) == "" || config.Timeout <= 0 || limit < 0 {
			return errors.New("effort and a positive timeout are required; limit must not be negative")
		}
		binary, err := exec.LookPath(config.Binary)
		if err != nil {
			return err
		}
		config.Binary = reposeAbsolutePath(environment.Cwd, binary)
	}

	scanRepository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repoPath))
	if err != nil {
		return err
	}
	database, err := reposeDatabasePath(ctx, scanRepository)
	if err != nil {
		return err
	}
	store, err := openInventoryStore(ctx, database, false)
	if err != nil {
		return err
	}
	defer store.Close()

	switch command {
	case "create":
		ids := make([]int64, 0, flags.NArg())
		for _, value := range flags.Args() {
			id, err := parseFindingID(value)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		fix, err := store.createAuditFix(ctx, ids, "", environmentNow(environment))
		if err != nil {
			return err
		}
		if asJSON {
			return writeInventoryJSON(environment.Stdout, fix)
		}
		fmt.Fprintf(environment.Stdout, "Created fix #%d with %d findings: %s.\n", fix.ID, len(fix.Findings), formatAuditFixFindingIDs(fix.Findings))
		return nil
	case "list":
		fixes, err := store.auditFixes(ctx)
		if err != nil {
			return err
		}
		if asJSON {
			return writeInventoryJSON(environment.Stdout, fixes)
		}
		return writeAuditFixList(environment, fixes)
	case "show":
		id, err := parseAuditFixID(flags.Arg(0))
		if err != nil {
			return err
		}
		fix, err := store.auditFix(ctx, id)
		if err != nil {
			return err
		}
		if asJSON {
			return writeInventoryJSON(environment.Stdout, fix)
		}
		return writeAuditFixDetail(environment, fix)
	case "delete":
		id, err := parseAuditFixID(flags.Arg(0))
		if err != nil {
			return err
		}
		if err := store.deleteAuditFix(ctx, id); err != nil {
			return err
		}
		if asJSON {
			return writeInventoryJSON(environment.Stdout, struct {
				ID      int64 `json:"id"`
				Deleted bool  `json:"deleted"`
			}{id, true})
		}
		fmt.Fprintf(environment.Stdout, "Deleted pending fix #%d.\n", id)
		return nil
	case "run":
		workRepository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, worktree))
		if err != nil {
			return fmt.Errorf("open development worktree: %w", err)
		}
		now := func() time.Time { return environmentNow(environment) }
		return runAuditFixQueue(ctx, store, scanRepository, workRepository, config, limit, retryFailed,
			newFixRunner(workRepository, environment), environment.Stdout, now)
	}
	return nil
}

func formatAuditFixFindingIDs(findings []Finding) string {
	values := make([]string, len(findings))
	for index, finding := range findings {
		values[index] = fmt.Sprintf("#%d", finding.ID)
	}
	return strings.Join(values, ", ")
}

func writeAuditFixList(environment cliEnvironment, fixes []auditFix) error {
	if len(fixes) == 0 {
		fmt.Fprintln(environment.Stdout, "No fixes queued.")
		return nil
	}
	writer := tabwriter.NewWriter(environment.Stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "FIX\tSTATUS\tFINDINGS\tUPDATED"); err != nil {
		return err
	}
	for _, fix := range fixes {
		if _, err := fmt.Fprintf(writer, "#%d\t%s\t%s\t%s\n", fix.ID, fix.Status, formatAuditFixFindingIDs(fix.Findings), fix.UpdatedAt.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func writeAuditFixDetail(environment cliEnvironment, fix auditFix) error {
	fmt.Fprintf(environment.Stdout, "Fix #%d  %s\nCreated: %s\nUpdated: %s\nFindings: %s\n", fix.ID, fix.Status,
		fix.CreatedAt.Format(time.RFC3339), fix.UpdatedAt.Format(time.RFC3339), formatAuditFixFindingIDs(fix.Findings))
	for _, finding := range fix.Findings {
		fmt.Fprintf(environment.Stdout, "  #%d %s  %s  %s\n", finding.ID, strings.ToUpper(finding.Severity), findingLocation(finding), finding.Title)
	}
	if len(fix.Attempts) == 0 {
		fmt.Fprintln(environment.Stdout, "Attempts: none")
		return nil
	}
	fmt.Fprintln(environment.Stdout, "Attempts:")
	for _, attempt := range fix.Attempts {
		fmt.Fprintf(environment.Stdout, "  #%d %s  %s/%s  %s", attempt.Number, attempt.Status, attempt.Model.Model, attempt.Model.Effort, attempt.StartedAt.Format(time.RFC3339))
		if attempt.Output != nil {
			fmt.Fprintf(environment.Stdout, "  %s", singleLine(attempt.Output.Summary))
		}
		if attempt.Error != "" {
			fmt.Fprintf(environment.Stdout, "  %s", singleLine(attempt.Error))
		}
		fmt.Fprintln(environment.Stdout)
	}
	return nil
}
