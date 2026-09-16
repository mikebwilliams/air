package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

type reposeStatusInventory struct {
	ID              string     `json:"id"`
	SnapshotSHA     string     `json:"snapshot_sha"`
	CreatedAt       time.Time  `json:"created_at"`
	ReviewedAt      *time.Time `json:"reviewed_at,omitempty"`
	Approved        bool       `json:"approved"`
	TrackedFiles    int        `json:"tracked_files"`
	InScopeFiles    int        `json:"in_scope_files"`
	Sources         int        `json:"sources"`
	Headers         int        `json:"headers"`
	ExcludedFiles   int        `json:"excluded_files"`
	MissingCommands int        `json:"missing_commands"`
	UnmappedHeaders int        `json:"unmapped_headers"`
}

type reposeStatusTaskCounts struct {
	Total          int `json:"total"`
	Pending        int `json:"pending"`
	Running        int `json:"running"`
	Completed      int `json:"completed"`
	UnableToAssess int `json:"unable_to_assess"`
	Failed         int `json:"failed"`
}

func (c reposeStatusTaskCounts) processed() int {
	return c.Completed + c.UnableToAssess + c.Failed
}

func (c reposeStatusTaskCounts) remaining() int {
	return c.Pending + c.Running
}

func (c *reposeStatusTaskCounts) add(other reposeStatusTaskCounts) {
	c.Total += other.Total
	c.Pending += other.Pending
	c.Running += other.Running
	c.Completed += other.Completed
	c.UnableToAssess += other.UnableToAssess
	c.Failed += other.Failed
}

type reposeStatusUsage struct {
	Attempts                       int   `json:"attempts"`
	FinishedAttempts               int   `json:"finished_attempts"`
	ReportedAttempts               int   `json:"reported_attempts"`
	InputTokens                    int64 `json:"input_tokens"`
	CachedInputTokens              int64 `json:"cached_input_tokens"`
	CacheWriteTokens               int64 `json:"cache_write_tokens"`
	CacheWriteReportedAttempts     int   `json:"cache_write_reported_attempts"`
	OutputTokens                   int64 `json:"output_tokens"`
	ReasoningOutputTokens          int64 `json:"reasoning_output_tokens"`
	ReasoningOutputUnreportedCount int   `json:"reasoning_output_unreported_attempts"`
}

func (u *reposeStatusUsage) add(other reposeStatusUsage) {
	u.Attempts += other.Attempts
	u.FinishedAttempts += other.FinishedAttempts
	u.ReportedAttempts += other.ReportedAttempts
	u.InputTokens += other.InputTokens
	u.CachedInputTokens += other.CachedInputTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.CacheWriteReportedAttempts += other.CacheWriteReportedAttempts
	u.OutputTokens += other.OutputTokens
	u.ReasoningOutputTokens += other.ReasoningOutputTokens
	u.ReasoningOutputUnreportedCount += other.ReasoningOutputUnreportedCount
}

type reposeStatusTiming struct {
	WorkerMilliseconds                   int64  `json:"worker_ms"`
	SuccessfulSamples                    int    `json:"successful_samples"`
	SuccessfulMilliseconds               int64  `json:"successful_ms"`
	AverageSuccessfulMilliseconds        *int64 `json:"average_successful_ms,omitempty"`
	RemainingTasks                       int    `json:"remaining_tasks"`
	UnestimatedRemainingTasks            int    `json:"unestimated_remaining_tasks"`
	EstimatedRemainingWorkerMilliseconds *int64 `json:"estimated_remaining_worker_ms,omitempty"`
	EstimatedRemainingWallMilliseconds   *int64 `json:"estimated_remaining_wall_ms,omitempty"`
	RemainingEstimateComplete            bool   `json:"remaining_estimate_complete"`
}

type reposeStatusScan struct {
	ID               string                 `json:"id"`
	Kind             string                 `json:"kind"`
	SourceScanID     string                 `json:"source_scan_id,omitempty"`
	InventoryID      string                 `json:"inventory_id"`
	SnapshotSHA      string                 `json:"snapshot_sha"`
	SelectedFindings int                    `json:"selected_findings,omitempty"`
	CreatedAt        time.Time              `json:"created_at"`
	Status           string                 `json:"status"`
	Harness          string                 `json:"harness"`
	Model            string                 `json:"model"`
	Effort           string                 `json:"effort"`
	Goal             string                 `json:"goal"`
	Tasks            reposeStatusTaskCounts `json:"tasks"`
	Usage            reposeStatusUsage      `json:"usage"`
	Timing           reposeStatusTiming     `json:"timing"`
	Blocked          *auditProviderLimit    `json:"blocked,omitempty"`
	Verdicts         map[string]int         `json:"verdicts,omitempty"`
}

type reposeStatusFindings struct {
	ScopeScanID    string         `json:"scope_scan_id,omitempty"`
	Total          int            `json:"total"`
	Open           int            `json:"open"`
	Dismissed      int            `json:"dismissed"`
	OpenBySeverity map[string]int `json:"open_by_severity"`
	Verification   map[string]int `json:"verification"`
}

type reposeStatusReport struct {
	Inventory    reposeStatusInventory  `json:"inventory"`
	ScanScope    string                 `json:"scan_scope"`
	EstimateJobs int                    `json:"estimate_jobs,omitempty"`
	ScanCounts   map[string]int         `json:"scan_counts"`
	Tasks        reposeStatusTaskCounts `json:"tasks"`
	Scans        []reposeStatusScan     `json:"scans"`
	Findings     reposeStatusFindings   `json:"findings"`
	Usage        reposeStatusUsage      `json:"usage"`
	Timing       reposeStatusTiming     `json:"timing"`
}

func runReposeStatusCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("status", environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	scanSelector := flags.String("scan", "", "restrict status to one scan ID or latest")
	jobs := flags.Int("jobs", 0, "parallel jobs for an approximate remaining wall-clock estimate")
	asJSON := flags.Bool("json", false, "output JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: repose status [--scan ID|latest] [--jobs N] [--json] [--repo DIR]")
	}
	if flags.Changed("jobs") && (*jobs < 1 || *jobs > 32) {
		return errors.New("--jobs must be 1..32 when supplied")
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repoPath))
	if err != nil {
		return err
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	store, err := openInventoryReadOnly(ctx, database)
	if err != nil {
		return err
	}
	defer store.Close()
	report, err := buildReposeStatus(ctx, store, *scanSelector, *jobs, environmentNow(environment))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeInventoryJSON(environment.Stdout, report)
	}
	printReposeStatus(environment.Stdout, report)
	return nil
}

func buildReposeStatus(ctx context.Context, store *inventoryStore, selector string, jobs int, now time.Time) (reposeStatusReport, error) {
	record, err := store.Inventory(ctx, "current")
	if err != nil {
		return reposeStatusReport{}, err
	}
	report := reposeStatusReport{
		Inventory: statusInventory(record), ScanScope: "all", EstimateJobs: jobs,
		ScanCounts: map[string]int{}, Scans: []reposeStatusScan{},
	}
	var scans []auditScan
	if selector != "" {
		scan, err := store.audit(ctx, selector)
		if err != nil {
			return report, err
		}
		scans = []auditScan{scan}
		report.ScanScope = scan.ID
	} else {
		scans, err = store.audits(ctx)
		if err != nil {
			return report, err
		}
	}
	for _, scan := range scans {
		summary, err := buildReposeScanStatus(ctx, store, scan, jobs, now)
		if err != nil {
			return report, err
		}
		report.Scans = append(report.Scans, summary)
		report.ScanCounts[summary.Status]++
		report.Tasks.add(summary.Tasks)
		report.Usage.add(summary.Usage)
	}
	report.Timing = aggregateReposeStatusTiming(report.Scans, jobs)
	findingScope := ""
	if len(scans) == 1 && selector != "" {
		findingScope = scans[0].ID
		if scans[0].Spec.Recheck != nil {
			findingScope = scans[0].Spec.Recheck.SourceScanID
		}
	}
	report.Findings, err = loadReposeStatusFindings(ctx, store, findingScope)
	return report, err
}

func statusInventory(record InventoryRecord) reposeStatusInventory {
	result := reposeStatusInventory{ID: record.ID, SnapshotSHA: record.Inventory.SnapshotSHA, CreatedAt: record.CreatedAt,
		ReviewedAt: record.ReviewedAt, Approved: record.ReviewedAt != nil, TrackedFiles: len(record.Inventory.Files)}
	for _, file := range record.Inventory.Files {
		if file.Excluded {
			result.ExcludedFiles++
			continue
		}
		result.InScopeFiles++
		switch file.Kind {
		case "source":
			result.Sources++
			if len(file.CommandIDs) == 0 {
				result.MissingCommands++
			}
		case "header":
			result.Headers++
			if len(file.CommandIDs) == 0 {
				result.UnmappedHeaders++
			}
		}
	}
	return result
}

func statusTaskCounts(scan auditScan) reposeStatusTaskCounts {
	return reposeStatusTaskCounts{Total: len(scan.Spec.Plan.Assignments), Pending: scan.Counts["pending"], Running: scan.Counts["running"],
		Completed: scan.Counts["completed"], UnableToAssess: scan.Counts["unable_to_assess"], Failed: scan.Counts["failed"]}
}

func buildReposeScanStatus(ctx context.Context, store *inventoryStore, scan auditScan, jobs int, now time.Time) (reposeStatusScan, error) {
	usage, timing, err := loadReposeStatusAttempts(ctx, store, scan.ID, now)
	if err != nil {
		return reposeStatusScan{}, err
	}
	tasks := statusTaskCounts(scan)
	finishReposeStatusTiming(&timing, tasks.remaining(), jobs)
	result := reposeStatusScan{ID: scan.ID, Kind: "review", CreatedAt: scan.CreatedAt, Status: scan.Status,
		InventoryID: scan.Spec.Plan.InventoryID, SnapshotSHA: scan.Spec.Plan.SnapshotSHA,
		Harness: scan.Spec.Model.Harness, Model: scan.Spec.Model.Model, Effort: scan.Spec.Model.Effort, Goal: scan.Spec.Plan.Goal,
		Tasks: tasks, Usage: usage, Timing: timing, Blocked: scan.Blocked, Verdicts: scan.Verdicts}
	if scan.Spec.Recheck != nil {
		result.Kind, result.SourceScanID, result.SelectedFindings = "recheck", scan.Spec.Recheck.SourceScanID, len(scan.Spec.Recheck.Findings)
	}
	return result, nil
}

func loadReposeStatusAttempts(ctx context.Context, store *inventoryStore, scanID string, now time.Time) (reposeStatusUsage, reposeStatusTiming, error) {
	const query = `WITH attempt_values AS MATERIALIZED (
		SELECT status,finished_at,json_extract(document,'$.duration_ms','$.invocation.usage') AS values_json
		FROM audit_attempts WHERE scan_id=?
	) SELECT count(*),
		coalesce(sum(CASE WHEN finished_at IS NOT NULL THEN 1 ELSE 0 END),0),
		coalesce(sum(CASE WHEN json_type(values_json,'$[1]')='object' THEN 1 ELSE 0 END),0),
		coalesce(sum(json_extract(values_json,'$[1].input_tokens')),0),
		coalesce(sum(json_extract(values_json,'$[1].cached_input_tokens')),0),
		coalesce(sum(json_extract(values_json,'$[1].cache_write_tokens')),0),
		coalesce(sum(CASE WHEN json_type(values_json,'$[1].cache_write_tokens')='integer' THEN 1 ELSE 0 END),0),
		coalesce(sum(json_extract(values_json,'$[1].output_tokens')),0),
		coalesce(sum(json_extract(values_json,'$[1].reasoning_output_tokens')),0),
		coalesce(sum(CASE WHEN json_extract(values_json,'$[1].reasoning_output_tokens_unreported')=1 THEN 1 ELSE 0 END),0),
		coalesce(sum(CASE WHEN finished_at IS NOT NULL THEN json_extract(values_json,'$[0]') ELSE 0 END),0),
		coalesce(sum(CASE WHEN finished_at IS NOT NULL AND status IN ('completed','unable_to_assess') THEN 1 ELSE 0 END),0),
		coalesce(sum(CASE WHEN finished_at IS NOT NULL AND status IN ('completed','unable_to_assess') THEN json_extract(values_json,'$[0]') ELSE 0 END),0)
		FROM attempt_values`
	var usage reposeStatusUsage
	var timing reposeStatusTiming
	err := store.db.QueryRowContext(ctx, query, scanID).Scan(&usage.Attempts, &usage.FinishedAttempts, &usage.ReportedAttempts,
		&usage.InputTokens, &usage.CachedInputTokens, &usage.CacheWriteTokens, &usage.CacheWriteReportedAttempts,
		&usage.OutputTokens, &usage.ReasoningOutputTokens, &usage.ReasoningOutputUnreportedCount,
		&timing.WorkerMilliseconds, &timing.SuccessfulSamples, &timing.SuccessfulMilliseconds)
	if err != nil {
		return usage, timing, err
	}
	rows, err := store.db.QueryContext(ctx, "SELECT started_at FROM audit_attempts WHERE scan_id=? AND status='running'", scanID)
	if err != nil {
		return usage, timing, err
	}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			return usage, timing, err
		}
		started, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			rows.Close()
			return usage, timing, err
		}
		if now.After(started) {
			timing.WorkerMilliseconds += now.Sub(started).Milliseconds()
		}
	}
	return usage, timing, rows.Close()
}

func finishReposeStatusTiming(timing *reposeStatusTiming, remaining, jobs int) {
	timing.RemainingTasks = remaining
	if timing.SuccessfulSamples > 0 {
		average := timing.SuccessfulMilliseconds / int64(timing.SuccessfulSamples)
		timing.AverageSuccessfulMilliseconds = &average
	}
	if remaining == 0 {
		zero := int64(0)
		timing.EstimatedRemainingWorkerMilliseconds = &zero
		timing.RemainingEstimateComplete = true
		if jobs > 0 {
			timing.EstimatedRemainingWallMilliseconds = &zero
		}
		return
	}
	if timing.AverageSuccessfulMilliseconds == nil {
		timing.UnestimatedRemainingTasks = remaining
		return
	}
	worker := *timing.AverageSuccessfulMilliseconds * int64(remaining)
	timing.EstimatedRemainingWorkerMilliseconds = &worker
	timing.RemainingEstimateComplete = true
	if jobs > 0 {
		wall := *timing.AverageSuccessfulMilliseconds * int64((remaining+jobs-1)/jobs)
		timing.EstimatedRemainingWallMilliseconds = &wall
	}
}

func aggregateReposeStatusTiming(scans []reposeStatusScan, jobs int) reposeStatusTiming {
	result := reposeStatusTiming{RemainingEstimateComplete: true}
	workerEstimate, wallEstimate := int64(0), int64(0)
	for _, scan := range scans {
		result.WorkerMilliseconds += scan.Timing.WorkerMilliseconds
		result.SuccessfulSamples += scan.Timing.SuccessfulSamples
		result.SuccessfulMilliseconds += scan.Timing.SuccessfulMilliseconds
		result.RemainingTasks += scan.Timing.RemainingTasks
		result.UnestimatedRemainingTasks += scan.Timing.UnestimatedRemainingTasks
		if scan.Timing.EstimatedRemainingWorkerMilliseconds == nil {
			result.RemainingEstimateComplete = false
		} else {
			workerEstimate += *scan.Timing.EstimatedRemainingWorkerMilliseconds
		}
		if jobs > 0 {
			if scan.Timing.EstimatedRemainingWallMilliseconds == nil {
				result.RemainingEstimateComplete = false
			} else {
				wallEstimate += *scan.Timing.EstimatedRemainingWallMilliseconds
			}
		}
	}
	if result.SuccessfulSamples > 0 {
		average := result.SuccessfulMilliseconds / int64(result.SuccessfulSamples)
		result.AverageSuccessfulMilliseconds = &average
	}
	if result.RemainingEstimateComplete || result.RemainingTasks > result.UnestimatedRemainingTasks {
		result.EstimatedRemainingWorkerMilliseconds = &workerEstimate
		if jobs > 0 {
			result.EstimatedRemainingWallMilliseconds = &wallEstimate
		}
	}
	return result
}

func loadReposeStatusFindings(ctx context.Context, store *inventoryStore, scanID string) (reposeStatusFindings, error) {
	result := reposeStatusFindings{ScopeScanID: scanID, OpenBySeverity: map[string]int{}, Verification: map[string]int{}}
	if store.version < 4 {
		return result, nil
	}
	query := `SELECT dismissed_at IS NULL,coalesce(json_extract(document,'$.severity'),'unknown'),count(*) FROM audit_findings`
	args := []any{}
	if scanID != "" {
		query += " WHERE scan_id=?"
		args = append(args, scanID)
	}
	query += " GROUP BY dismissed_at IS NULL,json_extract(document,'$.severity')"
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var open bool
		var severity string
		var count int
		if err := rows.Scan(&open, &severity, &count); err != nil {
			rows.Close()
			return result, err
		}
		result.Total += count
		if open {
			result.Open += count
			result.OpenBySeverity[severity] += count
		} else {
			result.Dismissed += count
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	if store.version >= 5 && result.Total > 0 {
		verificationQuery := `SELECT r.outcome,count(*) FROM audit_recheck_results r
			JOIN (SELECT finding_id,max(attempt_id) AS attempt_id FROM audit_recheck_results GROUP BY finding_id) latest
			ON latest.finding_id=r.finding_id AND latest.attempt_id=r.attempt_id
			JOIN audit_findings f ON f.id=r.finding_id`
		verificationArgs := []any{}
		if scanID != "" {
			verificationQuery += " WHERE f.scan_id=?"
			verificationArgs = append(verificationArgs, scanID)
		}
		verificationQuery += " GROUP BY r.outcome"
		verifications, err := store.db.QueryContext(ctx, verificationQuery, verificationArgs...)
		if err != nil {
			return result, err
		}
		for verifications.Next() {
			var outcome string
			var count int
			if err := verifications.Scan(&outcome, &count); err != nil {
				verifications.Close()
				return result, err
			}
			result.Verification[outcome] = count
		}
		if err := verifications.Err(); err != nil {
			verifications.Close()
			return result, err
		}
		verifications.Close()
	}
	checked := 0
	for _, count := range result.Verification {
		checked += count
	}
	result.Verification["unchecked"] = result.Total - checked
	return result, nil
}

func printReposeStatus(output io.Writer, report reposeStatusReport) {
	inv := report.Inventory
	fmt.Fprintf(output, "Inventory %s  %s  observed %s\n", shortSHA(inv.ID), inventoryReviewLabel(inv.Approved), shortSHA(inv.SnapshotSHA))
	fmt.Fprintf(output, "%d tracked files; %d in scope (%d sources, %d headers); %d excluded\n", inv.TrackedFiles, inv.InScopeFiles, inv.Sources, inv.Headers, inv.ExcludedFiles)
	if inv.MissingCommands > 0 || inv.UnmappedHeaders > 0 {
		fmt.Fprintf(output, "Coverage: %d sources without commands; %d headers without direct commands\n", inv.MissingCommands, inv.UnmappedHeaders)
	}
	f := report.Findings
	fmt.Fprintf(output, "Findings: %d total; %d open (%d error, %d warning, %d info); %d dismissed\n",
		f.Total, f.Open, f.OpenBySeverity["error"], f.OpenBySeverity["warning"], f.OpenBySeverity["info"], f.Dismissed)
	if f.Total > 0 {
		fmt.Fprintf(output, "Verification: %d unchecked, %d confirmed, %d false positive, %d uncertain\n",
			f.Verification["unchecked"], f.Verification["confirmed"], f.Verification["false_positive"], f.Verification["uncertain"])
	}
	if len(report.Scans) == 0 {
		fmt.Fprintln(output, "Scans: none.")
	} else {
		fmt.Fprintf(output, "Scans: %d total%s\n", len(report.Scans), formatStatusCounts(report.ScanCounts))
		t := report.Tasks
		fmt.Fprintf(output, "Assignments: %d total; %d pending, %d running, %d completed, %d unable to assess, %d failed\n",
			t.Total, t.Pending, t.Running, t.Completed, t.UnableToAssess, t.Failed)
		visible := statusScansForText(report.Scans, report.ScanScope != "all")
		for _, scan := range visible {
			fmt.Fprintf(output, "  %s  %-7s %-10s %s/%s/%s  %d/%d processed",
				shortSHA(scan.ID), scan.Kind, scan.Status, scan.Harness, scan.Model, scan.Effort, scan.Tasks.processed(), scan.Tasks.Total)
			if scan.Tasks.Pending > 0 || scan.Tasks.Running > 0 || scan.Tasks.Failed > 0 {
				fmt.Fprintf(output, "; %d pending, %d running, %d failed", scan.Tasks.Pending, scan.Tasks.Running, scan.Tasks.Failed)
			}
			fmt.Fprintln(output)
			if scan.Kind == "recheck" {
				fmt.Fprintf(output, "    %d findings from scan %s: %d confirmed, %d false positive, %d uncertain\n",
					scan.SelectedFindings, shortSHA(scan.SourceScanID), scan.Verdicts["confirmed"], scan.Verdicts["false_positive"], scan.Verdicts["uncertain"])
			}
			if scan.Blocked != nil {
				fmt.Fprintf(output, "    provider limit: %s: %s\n", scan.Blocked.Kind, inventoryDisplay(scan.Blocked.Message))
			}
		}
	}
	u := report.Usage
	fmt.Fprintf(output, "Usage: %d input tokens (%d cached), %d output tokens across %d of %d %s reporting usage\n",
		u.InputTokens, u.CachedInputTokens, u.OutputTokens, u.ReportedAttempts, u.Attempts, statusPlural(u.Attempts, "attempt", "attempts"))
	timing := report.Timing
	if timing.SuccessfulSamples == 0 {
		fmt.Fprintf(output, "Worker time: %s; no successful timing samples\n", formatMilliseconds(timing.WorkerMilliseconds))
	} else {
		fmt.Fprintf(output, "Worker time: %s; successful average %s across %d %s\n", formatMilliseconds(timing.WorkerMilliseconds),
			formatMilliseconds(*timing.AverageSuccessfulMilliseconds), timing.SuccessfulSamples, statusPlural(timing.SuccessfulSamples, "attempt", "attempts"))
	}
	if timing.RemainingTasks > 0 {
		if timing.EstimatedRemainingWorkerMilliseconds == nil {
			fmt.Fprintf(output, "Remaining: %d %s; estimate unavailable until a successful timed attempt completes\n", timing.RemainingTasks, statusPlural(timing.RemainingTasks, "assignment", "assignments"))
		} else {
			fmt.Fprintf(output, "Remaining: %d %s; about %s of worker time", timing.RemainingTasks, statusPlural(timing.RemainingTasks, "assignment", "assignments"), formatMilliseconds(*timing.EstimatedRemainingWorkerMilliseconds))
			if timing.UnestimatedRemainingTasks > 0 {
				fmt.Fprintf(output, " for %d estimated; %d lack timing samples", timing.RemainingTasks-timing.UnestimatedRemainingTasks, timing.UnestimatedRemainingTasks)
			}
			if timing.EstimatedRemainingWallMilliseconds != nil {
				fmt.Fprintf(output, "; about %s at %d jobs", formatMilliseconds(*timing.EstimatedRemainingWallMilliseconds), report.EstimateJobs)
			} else {
				fmt.Fprint(output, "; use --jobs N for an approximate wall-clock estimate")
			}
			fmt.Fprintln(output)
		}
	}
}

func statusPlural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func formatStatusCounts(counts map[string]int) string {
	order := []string{"pending", "running", "paused", "completed", "incomplete", "invalid"}
	parts := []string{}
	seen := map[string]bool{}
	for _, status := range order {
		if count := counts[status]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, status))
			seen[status] = true
		}
	}
	other := []string{}
	for status, count := range counts {
		if count > 0 && !seen[status] {
			other = append(other, fmt.Sprintf("%d %s", count, status))
		}
	}
	sort.Strings(other)
	parts = append(parts, other...)
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, ", ")
}

func statusScansForText(scans []reposeStatusScan, selected bool) []reposeStatusScan {
	if selected || len(scans) <= 1 {
		return scans
	}
	visible := make([]reposeStatusScan, 0, len(scans))
	for _, scan := range scans {
		if scan.Status != "completed" {
			visible = append(visible, scan)
		}
	}
	if len(visible) == 0 {
		visible = append(visible, scans[0])
	}
	return visible
}
