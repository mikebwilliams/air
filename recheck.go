package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultRecheckBatchBy    = "file"
	defaultRecheckRetryLimit = 3
)

type recheckReviewerFactory func() (RecheckReviewer, ReviewIdentity, error)

type recheckOptions struct {
	FindingIDs      []int64
	Harness         string
	Model           string
	ReasoningEffort string
	PromptVersion   string
	Limit           int
	BatchBy         string
	BatchSize       int
	Jobs            int
	Force           bool
	DryRun          bool
	ContinueOnError bool
	RetryOnError    bool
	RetryLimit      int
	Output          io.Writer
	Now             func() time.Time
	ElapsedNow      func() time.Time
	NewReviewer     recheckReviewerFactory
}

func recheckRepository(
	ctx context.Context,
	repository *GitRepository,
	store *Store,
	options recheckOptions,
) error {
	if options.Limit < 0 {
		return errors.New("recheck limit must not be negative")
	}
	if options.RetryLimit < 0 {
		return errors.New("recheck retry limit must not be negative")
	}
	if options.Jobs < 0 {
		return errors.New("recheck jobs must be positive")
	}
	if options.Jobs == 0 {
		options.Jobs = 1
	}
	if options.BatchSize <= 0 || options.BatchSize > maxRecheckBatchSize {
		return fmt.Errorf("recheck batch size must be between 1 and %d", maxRecheckBatchSize)
	}
	if options.BatchBy == "" {
		options.BatchBy = defaultRecheckBatchBy
	}
	if err := validateRecheckBatchBy(options.BatchBy); err != nil {
		return err
	}
	if options.Model == "" {
		return errors.New("recheck model must not be empty")
	}
	headSHA, err := repository.ResolveCommit(ctx, "HEAD")
	if err != nil {
		return err
	}
	masterSHA, err := repository.MasterSHA(ctx)
	if err != nil {
		return err
	}
	if headSHA != masterSHA {
		return fmt.Errorf("HEAD %s is not the tip of master %s", shortSHA(headSHA), shortSHA(masterSHA))
	}
	if !options.DryRun {
		lock, err := acquireScanLock(repository.LockPath())
		if err != nil {
			return err
		}
		defer lock.Close()
	}

	findings, err := selectRecheckFindings(ctx, store, options.FindingIDs)
	if err != nil {
		return err
	}
	if len(findings) == 0 {
		fmt.Fprintln(options.Output, "No open findings.")
		return nil
	}
	previous := map[int64]struct{}{}
	promptIdentity := promptVersionOrDefault(options.PromptVersion, recheckPromptVersion)
	if !options.Force {
		previous, err = store.PreviouslyRecheckedFindingIDs(ctx, headSHA,
			options.Harness, options.Model, options.ReasoningEffort, promptIdentity)
		if err != nil {
			return err
		}
	}
	pending := make([]Finding, 0, len(findings))
	skipped := 0
	for _, finding := range findings {
		if _, exists := previous[finding.ID]; exists {
			skipped++
			continue
		}
		pending = append(pending, finding)
	}
	if options.Limit > 0 && len(pending) > options.Limit {
		pending = pending[:options.Limit]
	}
	identityLabel := options.Model
	if options.Harness != "" && options.Harness != codexReviewerName {
		identityLabel = options.Harness + ":" + identityLabel
	}
	if options.ReasoningEffort != "" {
		identityLabel += "/" + options.ReasoningEffort
	}
	if len(pending) == 0 {
		fmt.Fprintf(options.Output, "No findings need recheck at %s with %s", shortSHA(headSHA), identityLabel)
		if skipped != 0 {
			fmt.Fprintf(options.Output, " (%d already checked)", skipped)
		}
		fmt.Fprintln(options.Output, ".")
		return nil
	}
	batches := buildRecheckBatches(pending, options.BatchBy, options.BatchSize)
	if options.DryRun {
		fmt.Fprintf(options.Output, "Pending recheck at %s with %s: %d findings in %d batches (jobs: %d)",
			shortSHA(headSHA), identityLabel, len(pending), len(batches), options.Jobs)
		if skipped != 0 {
			fmt.Fprintf(options.Output, " (%d already checked)", skipped)
		}
		fmt.Fprintln(options.Output)
		if options.RetryOnError && options.RetryLimit > 0 {
			fmt.Fprintf(options.Output, "Failed batches will be retried up to %d times.\n", options.RetryLimit)
		}
		for index, batch := range batches {
			fmt.Fprintf(options.Output, "  Batch %d (%s)\n", index+1, formatRecheckBatch(batch, options.BatchBy))
		}
		return nil
	}
	if options.NewReviewer == nil {
		return errors.New("no model reviewer is configured")
	}
	// Construct each worker on the coordinator, since factories can read settings
	// from the database. Each reviewer is used by only one batch at a time.
	type worker struct {
		reviewer RecheckReviewer
		identity ReviewIdentity
	}
	workers := make([]worker, min(options.Jobs, len(batches)))
	for index := range workers {
		reviewer, identity, err := options.NewReviewer()
		if err != nil {
			return err
		}
		identity.PromptVersion = promptVersionOrDefault(identity.PromptVersion, promptIdentity)
		if identity.PromptVersion != promptIdentity {
			return fmt.Errorf("reviewer prompt identity %q does not match recheck identity %q",
				identity.PromptVersion, promptIdentity)
		}
		workers[index] = worker{reviewer: reviewer, identity: identity}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	elapsedNow := options.ElapsedNow
	if elapsedNow == nil {
		elapsedNow = time.Now
	}
	// Keep injected clocks safe even when several workers finish together.
	var clockMu sync.Mutex
	readElapsed := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return elapsedNow()
	}
	workerContext, cancelWorkers := context.WithCancel(ctx)
	var running sync.WaitGroup
	defer func() {
		cancelWorkers()
		running.Wait()
	}()
	type completion struct {
		worker int
		batch  int
		result RecheckResult
		err    error
	}
	completed := make(chan completion, len(workers))
	attempts := make([]int, len(batches))
	nextBatch, active := 0, 0
	startBatch := func(workerIndex, index int) {
		attempts[index]++
		active++
		running.Add(1)
		go func() {
			defer running.Done()
			result, err := reviewRecheckBatch(workerContext, workers[workerIndex].reviewer,
				RecheckInput{HeadSHA: headSHA, Findings: batches[index]}, readElapsed)
			// Capacity covers every active worker, including when the coordinator
			// exits on cancellation or a database error.
			completed <- completion{worker: workerIndex, batch: index, result: result, err: err}
		}()
	}
	for index := range workers {
		if err := ctx.Err(); err != nil {
			return err
		}
		startBatch(index, nextBatch)
		nextBatch++
	}
	totalResolved := 0
	totalStillPresent := 0
	totalUncertain := 0
	failureCount := 0
	successCount := 0
	var firstFailure error
	for active > 0 {
		var done completion
		select {
		case <-ctx.Done():
			return ctx.Err()
		case done = <-completed:
		}
		active--
		if err := ctx.Err(); err != nil {
			return err
		}
		batch := batches[done.batch]
		batchLabel := formatRecheckBatch(batch, options.BatchBy)
		if done.err != nil {
			fmt.Fprintf(options.Output, "Recheck batch %d failed (%s): %v\n",
				done.batch+1, batchLabel, done.err)
			if options.RetryOnError && attempts[done.batch] <= options.RetryLimit {
				if err := ctx.Err(); err != nil {
					return err
				}
				fmt.Fprintf(options.Output, "Retrying recheck batch %d (%s; retry %d/%d).\n",
					done.batch+1, batchLabel, attempts[done.batch], options.RetryLimit)
				startBatch(done.worker, done.batch)
				continue
			}
			failureCount++
			if firstFailure == nil {
				firstFailure = fmt.Errorf("recheck %s: %w", batchLabel, done.err)
			}
		} else {
			if _, err := store.ApplyRecheck(ctx, headSHA, workers[done.worker].identity, batch, done.result, now()); err != nil {
				return err
			}
			resolved, stillPresent, uncertain := countRecheckOutcomes(done.result.Output)
			totalResolved += resolved
			totalStillPresent += stillPresent
			totalUncertain += uncertain
			successCount += len(batch)
			fmt.Fprintf(options.Output,
				"Recheck batch %d (%s): %d resolved, %d still present, %d uncertain\n",
				done.batch+1, batchLabel, resolved, stillPresent, uncertain)
		}
		if nextBatch < len(batches) && (firstFailure == nil || options.ContinueOnError) {
			if err := ctx.Err(); err != nil {
				return err
			}
			startBatch(done.worker, nextBatch)
			nextBatch++
		}
	}
	fmt.Fprintf(options.Output,
		"Rechecked %d findings at %s with %s: %d resolved, %d still present, %d uncertain",
		successCount, shortSHA(headSHA), identityLabel,
		totalResolved, totalStillPresent, totalUncertain)
	if skipped != 0 {
		fmt.Fprintf(options.Output, "; %d already checked", skipped)
	}
	fmt.Fprintln(options.Output)
	if firstFailure != nil && !options.ContinueOnError {
		return firstFailure
	}
	if failureCount != 0 {
		return fmt.Errorf("%d recheck batches failed", failureCount)
	}
	return nil
}

func reviewRecheckBatch(
	ctx context.Context,
	reviewer RecheckReviewer,
	input RecheckInput,
	elapsedNow func() time.Time,
) (RecheckResult, error) {
	if err := ctx.Err(); err != nil {
		return RecheckResult{}, err
	}
	started := elapsedNow()
	result, err := reviewer.Recheck(ctx, input)
	if err != nil {
		return RecheckResult{}, err
	}
	result.Duration = elapsedNow().Sub(started)
	if result.Duration < 0 {
		return RecheckResult{}, errors.New("measure recheck duration: clock moved backwards")
	}
	if result.Usage == nil {
		return RecheckResult{}, errors.New("reviewer did not report token usage")
	}
	if err := validateTokenUsage(*result.Usage); err != nil {
		return RecheckResult{}, fmt.Errorf("invalid recheck token usage: %w", err)
	}
	if result.ReportedCostMicrousd != nil && *result.ReportedCostMicrousd < 0 {
		return RecheckResult{}, errors.New("recheck reported cost must not be negative")
	}
	if err := validateRecheckOutput(result.Output, recheckFindingIDs(input.Findings)); err != nil {
		return RecheckResult{}, err
	}
	return result, nil
}

func selectRecheckFindings(ctx context.Context, store *Store, ids []int64) ([]Finding, error) {
	if len(ids) == 0 {
		return store.OpenFindings(ctx)
	}
	seen := make(map[int64]struct{}, len(ids))
	findings := make([]Finding, 0, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("finding #%d was supplied more than once", id)
		}
		seen[id] = struct{}{}
		finding, err := store.Finding(ctx, id)
		if err != nil {
			return nil, err
		}
		if finding.ResolvedSHA != nil {
			return nil, fmt.Errorf("finding #%d is resolved; recheck accepts only open findings", id)
		}
		if finding.DismissedAt != nil {
			return nil, fmt.Errorf("finding #%d is dismissed; recheck accepts only open findings", id)
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

func validateRecheckBatchBy(value string) error {
	if value != "file" && value != "count" {
		return fmt.Errorf("invalid --batch-by %q; expected file or count", value)
	}
	return nil
}

// Selection, prior-result filtering, and the total limit are applied before
// batching so the batching mode affects grouping, not which findings are checked.
func buildRecheckBatches(findings []Finding, batchBy string, batchSize int) [][]Finding {
	groups := [][]Finding{findings}
	if batchBy == "file" {
		byFile := make(map[string][]Finding)
		for _, finding := range findings {
			file := ""
			if finding.File != nil {
				file = *finding.File
			}
			byFile[file] = append(byFile[file], finding)
		}
		paths := make([]string, 0, len(byFile))
		for file := range byFile {
			if file != "" {
				paths = append(paths, file)
			}
		}
		sort.Strings(paths)
		groups = nil
		for _, file := range paths {
			groups = append(groups, byFile[file])
		}
		if unlocated := byFile[""]; len(unlocated) != 0 {
			groups = append(groups, unlocated)
		}
	}
	var batches [][]Finding
	for _, group := range groups {
		for offset := 0; offset < len(group); offset += batchSize {
			batches = append(batches, group[offset:min(offset+batchSize, len(group))])
		}
	}
	return batches
}

func formatRecheckBatch(findings []Finding, batchBy string) string {
	ids := formatFindingIDs(findings)
	if batchBy != "file" {
		return ids
	}
	if findings[0].File == nil || *findings[0].File == "" {
		return "no file; " + ids
	}
	return fmt.Sprintf("%q; %s", *findings[0].File, ids)
}

func formatFindingIDs(findings []Finding) string {
	ids := make([]int64, 0, len(findings))
	for _, finding := range findings {
		ids = append(ids, finding.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		labels = append(labels, fmt.Sprintf("#%d", id))
	}
	return strings.Join(labels, ", ")
}
