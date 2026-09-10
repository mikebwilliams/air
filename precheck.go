package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const stagedPrecheckSHA = "STAGED"

type precheckPlan struct {
	Mode    string
	FromSHA string
	ToSHA   string
	Commits []string
	Staged  bool
}

type precheckResolution struct {
	ID        int64  `json:"id"`
	Reason    string `json:"reason"`
	TargetSHA string `json:"target_sha"`
	Existing  bool   `json:"existing_finding"`
}

type precheckAttempt struct {
	Commit                     CommitMetadata       `json:"commit"`
	Status                     string               `json:"status"`
	SkipReason                 string               `json:"skip_reason,omitempty"`
	Summary                    string               `json:"summary,omitempty"`
	NewFindings                []Finding            `json:"new_findings"`
	ResolvedFindings           []precheckResolution `json:"resolved_findings"`
	ResolutionCandidates       int                  `json:"resolution_candidates"`
	DeferredResolutionFindings int                  `json:"deferred_resolution_findings"`
	Usage                      *TokenUsage          `json:"usage,omitempty"`
	EstimatedCostMicrousd      *int64               `json:"estimated_cost_microusd,omitempty"`
	EstimatedCostMaxMicrousd   *int64               `json:"estimated_cost_max_microusd,omitempty"`
	ReportedCostMicrousd       *int64               `json:"reported_cost_microusd,omitempty"`
	DurationMilliseconds       *int64               `json:"duration_ms,omitempty"`
}

type precheckAccounting struct {
	Attempts                   int   `json:"attempts"`
	InputTokens                int64 `json:"input_tokens"`
	CachedInputTokens          int64 `json:"cached_input_tokens"`
	CacheWriteTokens           int64 `json:"cache_write_tokens"`
	CacheWritesUnreported      int   `json:"cache_writes_unreported"`
	OutputTokens               int64 `json:"output_tokens"`
	ReasoningOutputTokens      int64 `json:"reasoning_output_tokens"`
	ReasoningOutputsUnreported int   `json:"reasoning_outputs_unreported"`
	MinimumCostMicrousd        int64 `json:"minimum_cost_microusd"`
	MaximumCostMicrousd        int64 `json:"maximum_cost_microusd"`
	PricedAttempts             int   `json:"priced_attempts"`
	UnknownCostAttempts        int   `json:"unknown_cost_attempts"`
	DurationMilliseconds       int64 `json:"duration_ms"`
}

type precheckReport struct {
	Mode                 string               `json:"mode"`
	FromSHA              string               `json:"from_sha"`
	ToSHA                string               `json:"to_sha"`
	GeneratedAt          time.Time            `json:"generated_at"`
	Harness              string               `json:"harness"`
	Model                string               `json:"model"`
	ReasoningEffort      string               `json:"reasoning_effort,omitempty"`
	PromptVersion        string               `json:"prompt_version"`
	Hints                []ReviewHint         `json:"hints,omitempty"`
	Attempts             []precheckAttempt    `json:"attempts"`
	Findings             []Finding            `json:"findings"`
	PredictedResolutions []precheckResolution `json:"predicted_resolutions"`
	Accounting           precheckAccounting   `json:"accounting"`
}

type precheckOptions struct {
	Plan         precheckPlan
	OpenFindings []Finding
	Identity     ReviewIdentity
	NewReviewer  reviewerFactory
	Now          func() time.Time
	ElapsedNow   func() time.Time
}

func runPrecheck(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("precheck", environment.Stderr)
	staged := flags.Bool("staged", false, "review the staged index against HEAD")
	format := flags.String("format", "text", "output format: text, json, sarif, or html")
	outputPath := flags.StringP("output", "o", "", "write output to a file instead of standard output")
	failOn := flags.String("fail-on", "", "exit nonzero for findings at or above: info, warning, or error")
	reviewerFlags := addReviewerFlags(flags, "per-target")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: air precheck [--staged] [--format FORMAT] [--fail-on SEVERITY] [FROM..TO]")
	}
	if *staged && flags.NArg() != 0 {
		return errors.New("--staged and an explicit commit range cannot be used together")
	}
	*format = strings.ToLower(strings.TrimSpace(*format))
	if !validPrecheckFormat(*format) {
		return fmt.Errorf("invalid --format %q; expected text, json, sarif, or html", *format)
	}
	*failOn = strings.ToLower(strings.TrimSpace(*failOn))
	if *failOn != "" && *failOn != "info" && *failOn != "warning" && *failOn != "error" {
		return fmt.Errorf("invalid --fail-on %q; expected info, warning, or error", *failOn)
	}

	repository, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	plan, err := buildPrecheckPlan(ctx, repository, *staged, flags.Args(), environmentNow(environment))
	if err != nil {
		return err
	}
	openFindings, err := store.OpenFindings(ctx)
	if err != nil {
		return err
	}
	setFlags := visitedFlagNames(flags)
	configuration, err := resolveReviewerConfiguration(ctx, store, environment, reviewerFlags, setFlags)
	if err != nil {
		return err
	}
	reviewPrompt, err := resolveReviewerPrompt(ctx, store, "review", (*reviewerFlags.hints)...)
	if err != nil {
		return err
	}
	identity := ReviewIdentity{
		Harness: configuration.Harness, Model: modelByName(configuration.Model),
		ReasoningEffort: configuration.Effort, PromptVersion: reviewPrompt.PromptVersion,
		Hints: reviewPrompt.Hints,
	}
	factory := func() (Reviewer, ReviewIdentity, error) {
		return newConfiguredReviewer(ctx, repository, store, environment, configuration, reviewPrompt)
	}
	report, err := precheckRepository(ctx, repository, precheckOptions{
		Plan: plan, OpenFindings: openFindings, Identity: identity, NewReviewer: factory,
		Now: environment.Now, ElapsedNow: environment.ElapsedNow,
	})
	if err != nil {
		return err
	}
	if err := writeCommandOutput(environment.Stdout, *outputPath, func(output io.Writer) error {
		return writePrecheckReport(output, repository, report, *format)
	}); err != nil {
		return err
	}
	if count := precheckFailureCount(report.Findings, *failOn); count != 0 {
		noun := "findings"
		if count == 1 {
			noun = "finding"
		}
		return fmt.Errorf("precheck found %d open %s at or above %s", count, noun, *failOn)
	}
	return nil
}

func validPrecheckFormat(format string) bool {
	switch format {
	case "text", "json", "sarif", "html":
		return true
	default:
		return false
	}
}

func buildPrecheckPlan(
	ctx context.Context,
	repository *GitRepository,
	staged bool,
	args []string,
	now time.Time,
) (precheckPlan, error) {
	if staged {
		headSHA, err := repository.ResolveCommit(ctx, "HEAD")
		if err != nil {
			return precheckPlan{}, err
		}
		masterSHA, err := repository.MasterSHA(ctx)
		if err != nil {
			return precheckPlan{}, err
		}
		if headSHA != masterSHA {
			return precheckPlan{}, fmt.Errorf("HEAD %s is not the tip of master %s",
				shortSHA(headSHA), shortSHA(masterSHA))
		}
		return precheckPlan{
			Mode: "staged", FromSHA: headSHA, ToSHA: stagedPrecheckSHA, Staged: true,
			Commits: []string{stagedPrecheckSHA},
		}, nil
	}
	if len(args) == 1 {
		fromRevision, toRevision, err := splitTwoDotRange(args[0])
		if err != nil {
			return precheckPlan{}, err
		}
		fromSHA, err := repository.ResolveCommit(ctx, fromRevision)
		if err != nil {
			return precheckPlan{}, err
		}
		toSHA, err := repository.ResolveCommit(ctx, toRevision)
		if err != nil {
			return precheckPlan{}, err
		}
		commits, err := repository.EnumerateRange(ctx, fromSHA+".."+toSHA)
		if err != nil {
			return precheckPlan{}, err
		}
		return precheckPlan{Mode: "range", FromSHA: fromSHA, ToSHA: toSHA, Commits: commits}, nil
	}
	upstreamSHA, err := repository.MasterUpstreamSHA(ctx)
	if err != nil {
		return precheckPlan{}, err
	}
	masterSHA, err := repository.MasterSHA(ctx)
	if err != nil {
		return precheckPlan{}, err
	}
	commits, err := repository.EnumerateRange(ctx, upstreamSHA+".."+masterSHA)
	if err != nil {
		return precheckPlan{}, err
	}
	return precheckPlan{Mode: "unpushed", FromSHA: upstreamSHA, ToSHA: masterSHA, Commits: commits}, nil
}

func precheckRepository(
	ctx context.Context,
	repository *GitRepository,
	options precheckOptions,
) (precheckReport, error) {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	elapsedNow := options.ElapsedNow
	if elapsedNow == nil {
		elapsedNow = time.Now
	}
	report := precheckReport{
		Mode: options.Plan.Mode, FromSHA: options.Plan.FromSHA, ToSHA: options.Plan.ToSHA,
		GeneratedAt: now(), Harness: normalizedHarness(options.Identity.Harness),
		Model: options.Identity.Model.Name, ReasoningEffort: options.Identity.ReasoningEffort,
		PromptVersion: options.Identity.PromptVersion, Hints: options.Identity.Hints,
		Attempts: make([]precheckAttempt, 0, len(options.Plan.Commits)),
		Findings: []Finding{}, PredictedResolutions: []precheckResolution{},
	}
	active := make(map[int64]Finding, len(options.OpenFindings))
	var nextID int64 = 1
	for _, finding := range options.OpenFindings {
		active[finding.ID] = finding
		if finding.ID >= nextID {
			nextID = finding.ID + 1
		}
	}
	provisional := make(map[int64]bool)
	var reviewer Reviewer
	identity := options.Identity
	for _, sha := range options.Plan.Commits {
		if err := ctx.Err(); err != nil {
			return precheckReport{}, err
		}
		started := elapsedNow()
		target, diff, err := loadPrecheckTarget(ctx, repository, options.Plan.Staged, sha, report.GeneratedAt)
		if err != nil {
			return precheckReport{}, err
		}
		attempt := precheckAttempt{
			Commit: target, NewFindings: []Finding{}, ResolvedFindings: []precheckResolution{},
		}
		if reason := diffSkipReason(diff); reason != "" {
			attempt.Status = "skipped"
			attempt.SkipReason = reason
			report.Attempts = append(report.Attempts, attempt)
			continue
		}
		if reviewer == nil {
			if options.NewReviewer == nil {
				return precheckReport{}, errors.New("no model reviewer is configured")
			}
			reviewer, identity, err = options.NewReviewer()
			if err != nil {
				return precheckReport{}, err
			}
			report.Harness = normalizedHarness(identity.Harness)
			report.Model = identity.Model.Name
			report.ReasoningEffort = identity.ReasoningEffort
			report.PromptVersion = identity.PromptVersion
			report.Hints = identity.Hints
		}
		open := sortedActiveFindings(active)
		candidates, deferred := selectResolutionCandidates(open, diff.TextFiles)
		attempt.ResolutionCandidates = len(candidates)
		attempt.DeferredResolutionFindings = deferred
		allowed := make(map[int64]struct{}, len(candidates))
		for _, finding := range candidates {
			allowed[finding.ID] = struct{}{}
		}
		result, err := reviewer.Review(ctx, ReviewInput{
			Commit: target, OpenFindings: candidates, Staged: options.Plan.Staged,
		})
		if err != nil {
			return precheckReport{}, reviewFailure(target.SHA, len(candidates), deferred, err)
		}
		result.Duration = elapsedNow().Sub(started)
		if result.Duration < 0 {
			return precheckReport{}, fmt.Errorf("measure precheck duration for %s: clock moved backwards", shortSHA(target.SHA))
		}
		if result.Usage == nil {
			return precheckReport{}, fmt.Errorf("review target %s: reviewer did not report token usage", shortSHA(target.SHA))
		}
		if err := validateReviewOutput(result.Output, allowed); err != nil {
			return precheckReport{}, fmt.Errorf("validate precheck result for %s: %w", shortSHA(target.SHA), err)
		}
		attempt.Status = "reviewed"
		attempt.Summary = result.Output.Summary
		for _, resolution := range result.Output.ResolvedFindings {
			resolved := precheckResolution{
				ID: resolution.ID, Reason: resolution.Reason, TargetSHA: target.SHA,
				Existing: !provisional[resolution.ID],
			}
			delete(active, resolution.ID)
			delete(provisional, resolution.ID)
			attempt.ResolvedFindings = append(attempt.ResolvedFindings, resolved)
			report.PredictedResolutions = append(report.PredictedResolutions, resolved)
		}
		for _, candidate := range result.Output.NewFindings {
			finding := Finding{
				ID: nextID, IntroducedSHA: target.SHA, Severity: candidate.Severity,
				Title: candidate.Title, Description: candidate.Description,
				File: candidate.File, Line: candidate.Line, Symbol: candidate.Symbol,
			}
			nextID++
			active[finding.ID] = finding
			provisional[finding.ID] = true
			attempt.NewFindings = append(attempt.NewFindings, finding)
		}
		if err := addPrecheckAccounting(&report, &attempt, identity, result); err != nil {
			return precheckReport{}, err
		}
		report.Attempts = append(report.Attempts, attempt)
	}
	for id := range provisional {
		if finding, exists := active[id]; exists {
			report.Findings = append(report.Findings, finding)
		}
	}
	sort.Slice(report.Findings, func(i, j int) bool { return report.Findings[i].ID < report.Findings[j].ID })
	return report, nil
}

func loadPrecheckTarget(
	ctx context.Context,
	repository *GitRepository,
	staged bool,
	sha string,
	now time.Time,
) (CommitMetadata, DiffResult, error) {
	if staged {
		headSHA, err := repository.ResolveCommit(ctx, "HEAD")
		if err != nil {
			return CommitMetadata{}, DiffResult{}, err
		}
		diff, err := repository.StagedDiff(ctx, headSHA)
		if err != nil {
			return CommitMetadata{}, DiffResult{}, err
		}
		return CommitMetadata{
			SHA: stagedPrecheckSHA, ParentSHA: headSHA, Date: now.Format(time.RFC3339),
			Message: "Staged changes",
		}, diff, nil
	}
	metadata, err := repository.CommitMetadata(ctx, sha)
	if err != nil {
		return CommitMetadata{}, DiffResult{}, err
	}
	diff, err := repository.CommitDiff(ctx, metadata.ParentSHA, metadata.SHA)
	if err != nil {
		return CommitMetadata{}, DiffResult{}, err
	}
	return metadata, diff, nil
}

func sortedActiveFindings(active map[int64]Finding) []Finding {
	findings := make([]Finding, 0, len(active))
	for _, finding := range active {
		findings = append(findings, finding)
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	return findings
}

func addPrecheckAccounting(
	report *precheckReport,
	attempt *precheckAttempt,
	identity ReviewIdentity,
	result ReviewResult,
) error {
	usage := *result.Usage
	if err := validateTokenUsage(usage); err != nil {
		return fmt.Errorf("invalid precheck token usage: %w", err)
	}
	estimate, err := identity.Model.EstimateCost(usage)
	if err != nil {
		return fmt.Errorf("estimate precheck cost: %w", err)
	}
	attempt.Usage = result.Usage
	attempt.ReportedCostMicrousd = result.ReportedCostMicrousd
	duration := result.Duration.Milliseconds()
	attempt.DurationMilliseconds = &duration
	accounting := &report.Accounting
	accounting.Attempts++
	accounting.InputTokens += usage.InputTokens
	accounting.CachedInputTokens += usage.CachedInputTokens
	accounting.OutputTokens += usage.OutputTokens
	accounting.ReasoningOutputTokens += usage.ReasoningOutputTokens
	accounting.DurationMilliseconds += duration
	if usage.CacheWriteTokens == nil {
		accounting.CacheWritesUnreported++
	} else {
		accounting.CacheWriteTokens += *usage.CacheWriteTokens
	}
	if usage.ReasoningOutputTokensUnreported {
		accounting.ReasoningOutputsUnreported++
	}
	if estimate != nil {
		minimum, maximum := estimate.MinimumMicrousd, estimate.MaximumMicrousd
		attempt.EstimatedCostMicrousd = &minimum
		attempt.EstimatedCostMaxMicrousd = &maximum
	}
	if result.ReportedCostMicrousd != nil {
		accounting.MinimumCostMicrousd += *result.ReportedCostMicrousd
		accounting.MaximumCostMicrousd += *result.ReportedCostMicrousd
		accounting.PricedAttempts++
	} else if estimate != nil {
		accounting.MinimumCostMicrousd += estimate.MinimumMicrousd
		accounting.MaximumCostMicrousd += estimate.MaximumMicrousd
		accounting.PricedAttempts++
	} else {
		accounting.UnknownCostAttempts++
	}
	return nil
}

func writePrecheckReport(
	output io.Writer,
	repository *GitRepository,
	report precheckReport,
	format string,
) error {
	switch format {
	case "json":
		return writeJSON(output, report)
	case "sarif":
		return writeJSON(output, buildSARIF(report.Findings))
	case "html":
		return writePrecheckHTML(output, repository, report)
	default:
		writePrecheckText(output, report)
		return nil
	}
}

func writePrecheckHTML(
	output io.Writer,
	repository *GitRepository,
	report precheckReport,
) error {
	commits := make(map[string]CommitMetadata, len(report.Attempts))
	for _, attempt := range report.Attempts {
		commits[attempt.Commit.SHA] = attempt.Commit
	}
	htmlReport := htmlExportReport{
		Version: 1, Title: "AIR Precheck Report",
		Repository:  filepath.Base(repository.WorkTree),
		GeneratedAt: report.GeneratedAt.Format(time.RFC3339),
		Findings:    make([]htmlExportFinding, 0, len(report.Findings)),
	}
	for _, finding := range report.Findings {
		metadata := commits[finding.IntroducedSHA]
		display := findingDisplayMetadata{Blame: commitAuthorName(metadata.Author)}
		if parsed, err := time.Parse(time.RFC3339Nano, metadata.Date); err == nil {
			display.CommitDate = parsed
		}
		preview := findingDiffPreview{Message: "Diff excerpts are unavailable in precheck exports."}
		htmlReport.Findings = append(htmlReport.Findings,
			makeHTMLExportFinding(finding, display, FindingReview{}, nil, preview, nil))
	}
	return writeHTMLExportReport(output, htmlReport)
}

func commitAuthorName(author string) string {
	if index := strings.LastIndex(author, " <"); index > 0 && strings.HasSuffix(author, ">") {
		return author[:index]
	}
	return author
}

func writePrecheckText(output io.Writer, report precheckReport) {
	fmt.Fprintf(output, "Precheck: %s, %d targets\n", report.Mode, len(report.Attempts))
	if report.Mode != "staged" {
		fmt.Fprintf(output, "Range: %s..%s\n", shortSHA(report.FromSHA), shortSHA(report.ToSHA))
	}
	printReviewHints(output, report.Hints)
	for _, attempt := range report.Attempts {
		label := shortSHA(attempt.Commit.SHA)
		if attempt.Status == "skipped" {
			fmt.Fprintf(output, "%s  skipped: %s\n", label, attempt.SkipReason)
			continue
		}
		fmt.Fprintf(output, "%s  %d new, %d predicted resolved\n",
			label, len(attempt.NewFindings), len(attempt.ResolvedFindings))
	}
	fmt.Fprintln(output, "\nOutstanding findings:")
	printFindingList(output, report.Findings)
	if len(report.PredictedResolutions) != 0 {
		fmt.Fprintln(output, "\nPredicted resolutions:")
		for _, resolution := range report.PredictedResolutions {
			kind := "precheck"
			if resolution.Existing {
				kind = "existing"
			}
			fmt.Fprintf(output, "  #%d (%s) by %s: %s\n",
				resolution.ID, kind, shortSHA(resolution.TargetSHA), resolution.Reason)
		}
	}
	if report.Accounting.Attempts != 0 {
		fmt.Fprintln(output)
		printTokenTotals(output,
			report.Accounting.InputTokens, report.Accounting.CachedInputTokens,
			report.Accounting.CacheWriteTokens, report.Accounting.CacheWritesUnreported,
			report.Accounting.OutputTokens, report.Accounting.ReasoningOutputTokens,
			report.Accounting.ReasoningOutputsUnreported)
		printCostTotals(output,
			report.Accounting.MinimumCostMicrousd, report.Accounting.MaximumCostMicrousd,
			report.Accounting.PricedAttempts, report.Accounting.UnknownCostAttempts)
		fmt.Fprintf(output, "Review time: %s\n", formatMilliseconds(report.Accounting.DurationMilliseconds))
	}
}

func precheckFailureCount(findings []Finding, threshold string) int {
	if threshold == "" {
		return 0
	}
	thresholdRank := findingSeverityRank(threshold)
	count := 0
	for _, finding := range findings {
		if findingSeverityRank(finding.Severity) <= thresholdRank {
			count++
		}
	}
	return count
}
