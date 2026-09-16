package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

type reposeStatsFilters struct {
	ScanID string     `json:"scan_id,omitempty"`
	Model  string     `json:"model,omitempty"`
	Since  *time.Time `json:"since,omitempty"`
}

type reposeStatsScanCounts struct {
	Total    int `json:"total"`
	Reviews  int `json:"reviews"`
	Rechecks int `json:"rechecks"`
}

type reposeStatsAttemptCounts struct {
	Total    int            `json:"total"`
	Reviews  int            `json:"reviews"`
	Rechecks int            `json:"rechecks"`
	Finished int            `json:"finished"`
	Running  int            `json:"running"`
	Outcomes map[string]int `json:"outcomes"`
}

func (c *reposeStatsAttemptCounts) add(other reposeStatsAttemptCounts) {
	c.Total += other.Total
	c.Reviews += other.Reviews
	c.Rechecks += other.Rechecks
	c.Finished += other.Finished
	c.Running += other.Running
	if c.Outcomes == nil {
		c.Outcomes = map[string]int{}
	}
	for outcome, count := range other.Outcomes {
		c.Outcomes[outcome] += count
	}
}

type reposeStatsTiming struct {
	WorkerMilliseconds          int64  `json:"worker_ms"`
	FinishedMilliseconds        int64  `json:"finished_ms"`
	TimedAttempts               int    `json:"timed_attempts"`
	RunningAttempts             int    `json:"running_attempts"`
	AverageFinishedMilliseconds *int64 `json:"average_finished_ms,omitempty"`
}

func (t *reposeStatsTiming) add(other reposeStatsTiming) {
	t.WorkerMilliseconds += other.WorkerMilliseconds
	t.FinishedMilliseconds += other.FinishedMilliseconds
	t.TimedAttempts += other.TimedAttempts
	t.RunningAttempts += other.RunningAttempts
	if t.TimedAttempts > 0 {
		average := t.FinishedMilliseconds / int64(t.TimedAttempts)
		t.AverageFinishedMilliseconds = &average
	}
}

type reposeCostSummary struct {
	MinimumMicrousd          int64 `json:"minimum_microusd"`
	MaximumMicrousd          int64 `json:"maximum_microusd"`
	AccountedAttempts        int   `json:"accounted_attempts"`
	ReportedCostAttempts     int   `json:"reported_cost_attempts"`
	EstimatedCostAttempts    int   `json:"estimated_cost_attempts"`
	UnknownCostAttempts      int   `json:"unknown_cost_attempts"`
	UnpricedUsageAttempts    int   `json:"unpriced_usage_attempts"`
	NoAccountingDataAttempts int   `json:"no_accounting_data_attempts"`
	Complete                 bool  `json:"complete"`
}

func (c *reposeCostSummary) add(other reposeCostSummary) {
	c.MinimumMicrousd += other.MinimumMicrousd
	c.MaximumMicrousd += other.MaximumMicrousd
	c.AccountedAttempts += other.AccountedAttempts
	c.ReportedCostAttempts += other.ReportedCostAttempts
	c.EstimatedCostAttempts += other.EstimatedCostAttempts
	c.UnknownCostAttempts += other.UnknownCostAttempts
	c.UnpricedUsageAttempts += other.UnpricedUsageAttempts
	c.NoAccountingDataAttempts += other.NoAccountingDataAttempts
	c.Complete = c.UnknownCostAttempts == 0
}

type reposeStatsGroup struct {
	Harness  string                   `json:"harness"`
	Model    string                   `json:"model"`
	Effort   string                   `json:"effort"`
	Attempts reposeStatsAttemptCounts `json:"attempts"`
	Usage    reposeStatusUsage        `json:"usage"`
	Timing   reposeStatsTiming        `json:"timing"`
	Cost     reposeCostSummary        `json:"cost"`
}

type reposeStatsReport struct {
	Filters  reposeStatsFilters       `json:"filters"`
	Scans    reposeStatsScanCounts    `json:"scans"`
	Attempts reposeStatsAttemptCounts `json:"attempts"`
	Usage    reposeStatusUsage        `json:"usage"`
	Timing   reposeStatsTiming        `json:"timing"`
	Cost     reposeCostSummary        `json:"cost"`
	Findings reposeStatusFindings     `json:"findings"`
	Groups   []reposeStatsGroup       `json:"groups"`
}

type reposeAccountingAttempt struct {
	Status               string
	StartedAt            time.Time
	FinishedAt           *time.Time
	DurationMilliseconds int64
	Usage                *TokenUsage
	ReportedCostMicrousd *int64
}

type reposeStatsGroupKey struct {
	harness string
	model   string
	effort  string
}

func runReposeStatsCLI(ctx context.Context, command string, args []string, environment cliEnvironment) error {
	flags := newFlagSet(command, environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	scanSelector := flags.String("scan", "", "restrict accounting to one scan ID or latest")
	model := flags.String("model", "", "include only attempts performed by this model")
	sinceValue := flags.String("since", "", "include attempts started on or after YYYY-MM-DD or RFC3339")
	asJSON := flags.Bool("json", false, "output JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: repose %s [--scan ID|latest] [--model MODEL] [--since DATE] [--json] [--repo DIR]", command)
	}
	if strings.TrimSpace(*model) != *model {
		return errors.New("model must not have leading or trailing whitespace")
	}
	var since *time.Time
	if *sinceValue != "" {
		parsed, err := time.Parse(time.RFC3339, *sinceValue)
		if err != nil {
			parsed, err = time.Parse("2006-01-02", *sinceValue)
		}
		if err != nil {
			return fmt.Errorf("invalid --since %q: use YYYY-MM-DD or RFC3339", *sinceValue)
		}
		since = &parsed
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
	report, err := buildReposeStats(ctx, store, *scanSelector, *model, since, environmentNow(environment))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeInventoryJSON(environment.Stdout, report)
	}
	if command == "cost" {
		printReposeCostReport(environment.Stdout, report)
	} else {
		printReposeStatsReport(environment.Stdout, report)
	}
	return nil
}

func buildReposeStats(ctx context.Context, store *inventoryStore, selector, modelFilter string, since *time.Time, now time.Time) (reposeStatsReport, error) {
	report := reposeStatsReport{
		Filters: reposeStatsFilters{Model: modelFilter, Since: since}, Attempts: reposeStatsAttemptCounts{Outcomes: map[string]int{}},
		Groups: []reposeStatsGroup{},
	}
	var scans []auditScan
	var err error
	if selector != "" {
		scan, scanErr := store.audit(ctx, selector)
		if scanErr != nil {
			return report, scanErr
		}
		scans = []auditScan{scan}
		report.Filters.ScanID = scan.ID
	} else {
		scans, err = store.audits(ctx)
		if err != nil {
			return report, err
		}
	}
	groups := map[reposeStatsGroupKey]*reposeStatsGroup{}
	for _, scan := range scans {
		if modelFilter != "" && scan.Spec.Model.Model != modelFilter {
			continue
		}
		report.Scans.Total++
		kind := "review"
		if scan.Spec.Recheck != nil {
			kind = "recheck"
			report.Scans.Rechecks++
		} else {
			report.Scans.Reviews++
		}
		attempts, err := loadReposeAccountingAttempts(ctx, store, scan.ID, since)
		if err != nil {
			return report, err
		}
		key := reposeStatsGroupKey{scan.Spec.Model.Harness, scan.Spec.Model.Model, scan.Spec.Model.Effort}
		for _, attempt := range attempts {
			group := groups[key]
			if group == nil {
				group = &reposeStatsGroup{Harness: key.harness, Model: key.model, Effort: key.effort,
					Attempts: reposeStatsAttemptCounts{Outcomes: map[string]int{}}}
				groups[key] = group
			}
			counts, usage, timing, cost, err := summarizeReposeAccountingAttempt(scan.Spec.Model.Model, kind, attempt, now)
			if err != nil {
				return report, fmt.Errorf("account scan %s attempt: %w", shortSHA(scan.ID), err)
			}
			report.Attempts.add(counts)
			report.Usage.add(usage)
			report.Timing.add(timing)
			report.Cost.add(cost)
			group.Attempts.add(counts)
			group.Usage.add(usage)
			group.Timing.add(timing)
			group.Cost.add(cost)
		}
	}
	for _, group := range groups {
		report.Groups = append(report.Groups, *group)
	}
	report.Cost.Complete = report.Cost.UnknownCostAttempts == 0
	sort.Slice(report.Groups, func(i, j int) bool {
		if report.Groups[i].Harness != report.Groups[j].Harness {
			return report.Groups[i].Harness < report.Groups[j].Harness
		}
		if report.Groups[i].Model != report.Groups[j].Model {
			return report.Groups[i].Model < report.Groups[j].Model
		}
		return report.Groups[i].Effort < report.Groups[j].Effort
	})
	findingScope := ""
	if selector != "" && len(scans) == 1 {
		findingScope = scans[0].ID
		if scans[0].Spec.Recheck != nil {
			findingScope = scans[0].Spec.Recheck.SourceScanID
		}
	}
	report.Findings, err = loadReposeStatusFindings(ctx, store, findingScope)
	return report, err
}

func loadReposeAccountingAttempts(ctx context.Context, store *inventoryStore, scanID string, since *time.Time) ([]reposeAccountingAttempt, error) {
	query := `SELECT status,started_at,finished_at,
		json_extract(document,'$.duration_ms','$.invocation.usage','$.invocation.reported_cost_microusd')
		FROM audit_attempts WHERE scan_id=?`
	args := []any{scanID}
	if since != nil {
		query += " AND julianday(started_at)>=julianday(?)"
		args = append(args, formatTime(*since))
	}
	query += " ORDER BY id"
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := []reposeAccountingAttempt{}
	for rows.Next() {
		var attempt reposeAccountingAttempt
		var started string
		var finished *string
		var valuesJSON string
		if err := rows.Scan(&attempt.Status, &started, &finished, &valuesJSON); err != nil {
			return nil, err
		}
		attempt.StartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return nil, err
		}
		if finished != nil {
			parsed, err := time.Parse(time.RFC3339Nano, *finished)
			if err != nil {
				return nil, err
			}
			attempt.FinishedAt = &parsed
		}
		var values []json.RawMessage
		if err := json.Unmarshal([]byte(valuesJSON), &values); err != nil || len(values) != 3 {
			return nil, errors.New("invalid stored attempt accounting")
		}
		if string(values[0]) != "null" {
			if err := json.Unmarshal(values[0], &attempt.DurationMilliseconds); err != nil {
				return nil, err
			}
		}
		if string(values[1]) != "null" {
			var usage TokenUsage
			if err := json.Unmarshal(values[1], &usage); err != nil {
				return nil, err
			}
			attempt.Usage = &usage
		}
		if string(values[2]) != "null" {
			var cost int64
			if err := json.Unmarshal(values[2], &cost); err != nil {
				return nil, err
			}
			attempt.ReportedCostMicrousd = &cost
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func summarizeReposeAccountingAttempt(model, kind string, attempt reposeAccountingAttempt, now time.Time) (reposeStatsAttemptCounts, reposeStatusUsage, reposeStatsTiming, reposeCostSummary, error) {
	counts := reposeStatsAttemptCounts{Total: 1, Outcomes: map[string]int{attempt.Status: 1}}
	if kind == "recheck" {
		counts.Rechecks = 1
	} else {
		counts.Reviews = 1
	}
	usage := reposeStatusUsage{Attempts: 1}
	timing := reposeStatsTiming{}
	if attempt.FinishedAt != nil {
		counts.Finished = 1
		usage.FinishedAttempts = 1
		timing.TimedAttempts = 1
		timing.FinishedMilliseconds = attempt.DurationMilliseconds
		timing.WorkerMilliseconds = attempt.DurationMilliseconds
		average := attempt.DurationMilliseconds
		timing.AverageFinishedMilliseconds = &average
	} else {
		counts.Running = 1
		timing.RunningAttempts = 1
		if now.After(attempt.StartedAt) {
			timing.WorkerMilliseconds = now.Sub(attempt.StartedAt).Milliseconds()
		}
	}
	if attempt.Usage != nil {
		if err := validateTokenUsage(*attempt.Usage); err != nil {
			return counts, usage, timing, reposeCostSummary{}, err
		}
		usage.ReportedAttempts = 1
		usage.InputTokens = attempt.Usage.InputTokens
		usage.CachedInputTokens = attempt.Usage.CachedInputTokens
		usage.OutputTokens = attempt.Usage.OutputTokens
		usage.ReasoningOutputTokens = attempt.Usage.ReasoningOutputTokens
		if attempt.Usage.CacheWriteTokens != nil {
			usage.CacheWriteTokens = *attempt.Usage.CacheWriteTokens
			usage.CacheWriteReportedAttempts = 1
		}
		if attempt.Usage.ReasoningOutputTokensUnreported {
			usage.ReasoningOutputUnreportedCount = 1
		}
	}
	cost, err := reposeAttemptCost(model, attempt.Usage, attempt.ReportedCostMicrousd)
	return counts, usage, timing, cost, err
}

func reposeAttemptCost(model string, usage *TokenUsage, reported *int64) (reposeCostSummary, error) {
	result := reposeCostSummary{}
	if reported != nil {
		if *reported < 0 {
			return result, errors.New("reported cost must not be negative")
		}
		result.MinimumMicrousd, result.MaximumMicrousd = *reported, *reported
		result.AccountedAttempts, result.ReportedCostAttempts, result.Complete = 1, 1, true
		return result, nil
	}
	if usage == nil {
		result.UnknownCostAttempts, result.NoAccountingDataAttempts = 1, 1
		return result, nil
	}
	estimate, err := modelByName(model).EstimateCost(*usage)
	if err != nil {
		return result, err
	}
	if estimate == nil {
		result.UnknownCostAttempts, result.UnpricedUsageAttempts = 1, 1
		return result, nil
	}
	result.MinimumMicrousd, result.MaximumMicrousd = estimate.MinimumMicrousd, estimate.MaximumMicrousd
	result.AccountedAttempts, result.EstimatedCostAttempts, result.Complete = 1, 1, true
	return result, nil
}

func printReposeStatsReport(output io.Writer, report reposeStatsReport) {
	printReposeStatsFilters(output, report.Filters)
	fmt.Fprintf(output, "Scans: %d selected (%d review, %d recheck)\n", report.Scans.Total, report.Scans.Reviews, report.Scans.Rechecks)
	printReposeAttemptCounts(output, report.Attempts)
	printReposeTiming(output, report.Timing)
	printReposeTokenUsage(output, report.Usage)
	printReposeCostSummary(output, report.Cost)
	f := report.Findings
	fmt.Fprintf(output, "Findings: %d open, %d dismissed; verification: %d unchecked, %d confirmed, %d false positive, %d uncertain\n",
		f.Open, f.Dismissed, f.Verification["unchecked"], f.Verification["confirmed"], f.Verification["false_positive"], f.Verification["uncertain"])
	printReposeStatsGroups(output, report.Groups)
}

func printReposeCostReport(output io.Writer, report reposeStatsReport) {
	printReposeStatsFilters(output, report.Filters)
	printReposeCostSummary(output, report.Cost)
	printReposeAttemptCounts(output, report.Attempts)
	printReposeStatsGroups(output, report.Groups)
}

func printReposeStatsFilters(output io.Writer, filters reposeStatsFilters) {
	parts := []string{}
	if filters.ScanID != "" {
		parts = append(parts, "scan="+shortSHA(filters.ScanID))
	}
	if filters.Model != "" {
		parts = append(parts, "model="+filters.Model)
	}
	if filters.Since != nil {
		parts = append(parts, "since="+filters.Since.Format(time.RFC3339))
	}
	if len(parts) > 0 {
		fmt.Fprintln(output, "Filters:", strings.Join(parts, ", "))
	}
}

func printReposeAttemptCounts(output io.Writer, counts reposeStatsAttemptCounts) {
	fmt.Fprintf(output, "Attempts: %d total (%d review, %d recheck); %d finished, %d running%s\n",
		counts.Total, counts.Reviews, counts.Rechecks, counts.Finished, counts.Running, formatReposeAttemptOutcomes(counts.Outcomes))
}

func formatReposeAttemptOutcomes(counts map[string]int) string {
	order := []string{"completed", "unable_to_assess", "failed", "interrupted", auditQuotaExhausted, auditRateLimited, "running"}
	parts, seen := []string{}, map[string]bool{}
	for _, status := range order {
		if count := counts[status]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, strings.ReplaceAll(status, "_", " ")))
			seen[status] = true
		}
	}
	other := []string{}
	for status, count := range counts {
		if count > 0 && !seen[status] {
			other = append(other, fmt.Sprintf("%d %s", count, strings.ReplaceAll(status, "_", " ")))
		}
	}
	sort.Strings(other)
	parts = append(parts, other...)
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, ", ")
}

func printReposeTiming(output io.Writer, timing reposeStatsTiming) {
	if timing.TimedAttempts == 0 {
		fmt.Fprintf(output, "Worker time: %s recorded; no finished timing samples\n", formatMilliseconds(timing.WorkerMilliseconds))
		return
	}
	fmt.Fprintf(output, "Worker time: %s recorded; finished average %s across %d %s",
		formatMilliseconds(timing.WorkerMilliseconds), formatMilliseconds(*timing.AverageFinishedMilliseconds), timing.TimedAttempts,
		statusPlural(timing.TimedAttempts, "attempt", "attempts"))
	if timing.RunningAttempts > 0 {
		fmt.Fprintf(output, "; %d running", timing.RunningAttempts)
	}
	fmt.Fprintln(output)
}

func printReposeTokenUsage(output io.Writer, usage reposeStatusUsage) {
	fmt.Fprintf(output, "Tokens: %d input (%d cached), %d cache writes, %d output (%d reasoning); reported by %d of %d %s\n",
		usage.InputTokens, usage.CachedInputTokens, usage.CacheWriteTokens, usage.OutputTokens, usage.ReasoningOutputTokens,
		usage.ReportedAttempts, usage.Attempts, statusPlural(usage.Attempts, "attempt", "attempts"))
	if missing := usage.ReportedAttempts - usage.CacheWriteReportedAttempts; missing > 0 {
		fmt.Fprintf(output, "Cache writes: unreported by %d attempts with token usage\n", missing)
	}
	if usage.ReasoningOutputUnreportedCount > 0 {
		fmt.Fprintf(output, "Reasoning output: unreported by %d attempts\n", usage.ReasoningOutputUnreportedCount)
	}
}

func printReposeCostSummary(output io.Writer, cost reposeCostSummary) {
	if cost.AccountedAttempts == 0 {
		fmt.Fprint(output, "Estimated cost: unavailable")
	} else {
		fmt.Fprint(output, "Estimated cost: ", formatCostRange(cost.MinimumMicrousd, cost.MaximumMicrousd))
	}
	if cost.UnknownCostAttempts > 0 {
		fmt.Fprintf(output, " plus %d %s with unknown cost", cost.UnknownCostAttempts, statusPlural(cost.UnknownCostAttempts, "attempt", "attempts"))
	}
	fmt.Fprintf(output, " (%d accounted %s: %d harness-reported, %d token-price estimates)\n",
		cost.AccountedAttempts, statusPlural(cost.AccountedAttempts, "attempt", "attempts"), cost.ReportedCostAttempts, cost.EstimatedCostAttempts)
	if cost.UnpricedUsageAttempts > 0 {
		fmt.Fprintf(output, "Unknown pricing: %d %s %s token usage for models without prices\n", cost.UnpricedUsageAttempts,
			statusPlural(cost.UnpricedUsageAttempts, "attempt", "attempts"), statusPlural(cost.UnpricedUsageAttempts, "has", "have"))
	}
	if cost.NoAccountingDataAttempts > 0 {
		fmt.Fprintf(output, "Missing accounting: %d %s %s neither token usage nor reported cost\n", cost.NoAccountingDataAttempts,
			statusPlural(cost.NoAccountingDataAttempts, "attempt", "attempts"), statusPlural(cost.NoAccountingDataAttempts, "has", "have"))
	}
}

func printReposeStatsGroups(output io.Writer, groups []reposeStatsGroup) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintln(output, "By model and effort:")
	for _, group := range groups {
		name := group.Harness + "/" + group.Model + "/" + group.Effort
		cost := ""
		if group.Cost.AccountedAttempts > 0 {
			cost = formatCostRange(group.Cost.MinimumMicrousd, group.Cost.MaximumMicrousd)
		}
		if group.Cost.UnknownCostAttempts > 0 {
			if cost != "" {
				cost += " + "
			}
			cost += fmt.Sprintf("%d unknown", group.Cost.UnknownCostAttempts)
		}
		if cost == "" {
			cost = "unavailable"
		}
		fmt.Fprintf(output, "  %s: %d %s (%d review, %d recheck), %d input, %d output, %s\n",
			name, group.Attempts.Total, statusPlural(group.Attempts.Total, "attempt", "attempts"), group.Attempts.Reviews, group.Attempts.Rechecks,
			group.Usage.InputTokens, group.Usage.OutputTokens, cost)
	}
}
