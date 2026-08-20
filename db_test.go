package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreFindingLifecycleAndCleanupForeignKeys(t *testing.T) {
	ctx := context.Background()
	startSHA := strings.Repeat("0", 40)
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), startSHA)
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	defer store.Close()

	if value, err := store.Config(ctx, "start_sha"); err != nil || value != startSHA {
		t.Fatalf("start_sha = %q, %v", value, err)
	}
	commitA := testMetadata("a", "0")
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	cacheWrites := int64(10)
	newIDs, err := store.ApplyReview(ctx, commitA, ReviewIdentity{Model: modelByName("gpt-5.6-luna"), ReasoningEffort: "low"}, ReviewResult{
		Output: ReviewOutput{
			NewFindings: []NewFinding{{
				Severity:    "warning",
				Title:       "broken state",
				Description: "The state remains inconsistent after failure.",
			}},
			ResolvedFindings: []ResolvedFinding{},
			Summary:          "Reviewed the state transition.",
		},
		RawResponse: "{\"response\":\"a\"}",
		Usage: &TokenUsage{
			InputTokens:           100,
			CachedInputTokens:     40,
			CacheWriteTokens:      &cacheWrites,
			OutputTokens:          20,
			ReasoningOutputTokens: 5,
		},
	}, now)
	if err != nil {
		t.Fatalf("ApplyReview A: %v", err)
	}
	if len(newIDs) != 1 || newIDs[0] != 1 {
		t.Fatalf("new IDs = %v, want [1]", newIDs)
	}
	record, err := store.Commit(ctx, commitA.SHA)
	if err != nil {
		t.Fatal(err)
	}
	if record.Model != "gpt-5.6-luna" || record.ReasoningEffort != "low" {
		t.Fatalf("review identity = model %q, effort %q", record.Model, record.ReasoningEffort)
	}
	if record.Usage == nil || record.Usage.InputTokens != 100 || record.Usage.CachedInputTokens != 40 ||
		record.Usage.CacheWriteTokens == nil || *record.Usage.CacheWriteTokens != 10 ||
		record.Usage.OutputTokens != 20 || record.Usage.ReasoningOutputTokens != 5 {
		t.Fatalf("review usage = %+v", record.Usage)
	}
	if record.EstimatedCostMicrousd == nil || *record.EstimatedCostMicrousd != 37 ||
		record.EstimatedCostMaxMicrousd == nil || *record.EstimatedCostMaxMicrousd != 37 ||
		record.CostContext != "short" || !record.CostComplete {
		t.Fatalf("estimated cost = %v..%v, context=%q, complete=%t",
			record.EstimatedCostMicrousd, record.EstimatedCostMaxMicrousd,
			record.CostContext, record.CostComplete)
	}
	var pricingStatus string
	var shortInputRate, longOutputRate int64
	if err := store.db.QueryRowContext(ctx, `
		SELECT pricing_status, short_input_nanousd_per_token,
		       long_output_nanousd_per_token
		FROM models WHERE name = ?`, "gpt-5.6-luna").Scan(
		&pricingStatus, &shortInputRate, &longOutputRate); err != nil {
		t.Fatalf("read stored model: %v", err)
	}
	if pricingStatus != "known" || shortInputRate != 200 || longOutputRate != 1800 {
		t.Fatalf("stored pricing = status %q, short input %d, long output %d",
			pricingStatus, shortInputRate, longOutputRate)
	}
	open, err := store.OpenFindings(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("open findings = %v, %v", open, err)
	}

	commitB := testMetadata("b", "a")
	_, err = store.ApplyReview(ctx, commitB, ReviewIdentity{Model: modelByName("test-model"), ReasoningEffort: "high"}, ReviewResult{
		Output: ReviewOutput{
			NewFindings: []NewFinding{},
			ResolvedFindings: []ResolvedFinding{{
				ID:     1,
				Reason: "The state is restored on the failure path.",
			}},
			Summary: "Reviewed the fix.",
		},
		RawResponse: "{\"response\":\"b\"}",
		Usage:       &TokenUsage{InputTokens: 80, OutputTokens: 10},
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ApplyReview B: %v", err)
	}
	open, err = store.OpenFindings(ctx)
	if err != nil || len(open) != 0 {
		t.Fatalf("open findings after resolution = %v, %v", open, err)
	}

	if _, err := store.db.ExecContext(ctx, "DELETE FROM commits WHERE sha = ?", commitB.SHA); err != nil {
		t.Fatalf("delete resolving commit: %v", err)
	}
	open, err = store.OpenFindings(ctx)
	if err != nil || len(open) != 1 || open[0].ResolvedSHA != nil {
		t.Fatalf("finding was not reopened by ON DELETE SET NULL: %v, %v", open, err)
	}
	var eventCount int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM finding_events").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("event count after deleting resolution = %d, want 1", eventCount)
	}

	if _, err := store.db.ExecContext(ctx, "DELETE FROM commits WHERE sha = ?", commitA.SHA); err != nil {
		t.Fatalf("delete introducing commit: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("finding count after deleting introduction = %d, want 0", eventCount)
	}
}

func TestStoreConfigurationValues(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	defer store.Close()

	if value, found, err := store.ConfigValue(ctx, "model"); err != nil || found || value != "" {
		t.Fatalf("missing model = %q, found=%t, err=%v", value, found, err)
	}
	if err := store.SetConfig(ctx, "model", "gpt-5.6-luna"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if value, found, err := store.ConfigValue(ctx, "model"); err != nil || !found || value != "gpt-5.6-luna" {
		t.Fatalf("stored model = %q, found=%t, err=%v", value, found, err)
	}
	if err := store.SetConfig(ctx, "model", "gpt-5.6-sol"); err != nil {
		t.Fatalf("replace model: %v", err)
	}
	if value, err := store.Config(ctx, "model"); err != nil || value != "gpt-5.6-sol" {
		t.Fatalf("replaced model = %q, err=%v", value, err)
	}
	removed, err := store.UnsetConfig(ctx, "model")
	if err != nil || !removed {
		t.Fatalf("UnsetConfig = %t, %v", removed, err)
	}
	removed, err = store.UnsetConfig(ctx, "model")
	if err != nil || removed {
		t.Fatalf("second UnsetConfig = %t, %v", removed, err)
	}
}

func TestOpenStoreMigratesSchema4WithoutLosingReviewAttempts(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "air.sqlite")
	store, err := CreateStore(ctx, databasePath, strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyReview(ctx, testMetadata("a", "0"),
		ReviewIdentity{Model: modelByName("test-model")}, cleanReview("Historical review."), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE review_attempts DROP COLUMN duration_ms`); err != nil {
		t.Fatalf("recreate schema 4: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `PRAGMA user_version = 4`); err != nil {
		t.Fatalf("set schema version 4: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(ctx, databasePath)
	if err != nil {
		t.Fatalf("OpenStore migration: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	attempts, err := store.ReviewAttempts(ctx, strings.Repeat("a", 40))
	if err != nil || len(attempts) != 1 || attempts[0].DurationMilliseconds != nil {
		t.Fatalf("migrated historical attempts = %+v, %v", attempts, err)
	}
	newReview := cleanReview("Timed review.")
	newReview.Duration = 2500 * time.Millisecond
	if _, err := store.ApplyReview(ctx, testMetadata("b", "a"),
		ReviewIdentity{Model: modelByName("test-model")}, newReview, time.Now()); err != nil {
		t.Fatal(err)
	}
	attempts, err = store.ReviewAttempts(ctx, strings.Repeat("b", 40))
	if err != nil || len(attempts) != 1 || attempts[0].DurationMilliseconds == nil ||
		*attempts[0].DurationMilliseconds != 2500 {
		t.Fatalf("new timed attempts = %+v, %v", attempts, err)
	}
}

func TestApplyReviewRollsBackWholeCommit(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	defer store.Close()

	metadata := testMetadata("c", "0")
	_, err = store.ApplyReview(ctx, metadata, ReviewIdentity{Model: modelByName("test-model"), ReasoningEffort: "medium"}, ReviewResult{
		Output: ReviewOutput{
			NewFindings: []NewFinding{{
				Severity:    "error",
				Title:       "would be rolled back",
				Description: "This finding must not survive the invalid resolution.",
			}},
			ResolvedFindings: []ResolvedFinding{{
				ID:     999,
				Reason: "nonexistent",
			}},
			Summary: "Invalid mixed update.",
		},
		RawResponse: "{}",
		Usage:       &TokenUsage{InputTokens: 1, OutputTokens: 1},
	}, time.Now())
	if err == nil {
		t.Fatal("ApplyReview unexpectedly succeeded")
	}
	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM commits").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("commit count after rollback = %d, want 0", count)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM findings").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("finding count after rollback = %d, want 0", count)
	}
}

func TestSkippedCommitIsProcessed(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	defer store.Close()
	metadata := testMetadata("d", "0")
	if err := store.InsertSkipped(ctx, metadata, "binary-only diff", time.Now()); err != nil {
		t.Fatalf("InsertSkipped: %v", err)
	}
	processed, err := store.ProcessedSHAs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := processed[metadata.SHA]; !ok {
		t.Fatal("skipped SHA not returned as processed")
	}
	record, err := store.Commit(ctx, metadata.SHA)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "skipped" || record.SkipReason != "binary-only diff" {
		t.Fatalf("unexpected skipped record: %+v", record)
	}
}

func TestUnknownModelPersistsUnknownPricingAndCost(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	defer store.Close()

	metadata := testMetadata("e", "0")
	_, err = store.ApplyReview(ctx, metadata, ReviewIdentity{Model: modelByName("private-model")}, ReviewResult{
		Output: ReviewOutput{
			NewFindings:      []NewFinding{},
			ResolvedFindings: []ResolvedFinding{},
			Summary:          "Reviewed with an unpriced model.",
		},
		RawResponse: "{}",
		Usage:       &TokenUsage{InputTokens: 10, OutputTokens: 2},
	}, time.Now())
	if err != nil {
		t.Fatalf("ApplyReview: %v", err)
	}
	record, err := store.Commit(ctx, metadata.SHA)
	if err != nil {
		t.Fatal(err)
	}
	if record.EstimatedCostMicrousd != nil || record.EstimatedCostMaxMicrousd != nil {
		t.Fatalf("unknown model cost = %v..%v", record.EstimatedCostMicrousd, record.EstimatedCostMaxMicrousd)
	}
	var status string
	var populatedRates int
	if err := store.db.QueryRowContext(ctx, `
		SELECT pricing_status,
		       (short_input_nanousd_per_token IS NOT NULL) +
		       (long_output_nanousd_per_token IS NOT NULL)
		FROM models WHERE name = ?`, "private-model").Scan(&status, &populatedRates); err != nil {
		t.Fatalf("read stored model: %v", err)
	}
	if status != "unknown" || populatedRates != 0 {
		t.Fatalf("stored model = status %q, populated rates %d", status, populatedRates)
	}
}

func TestReviewAttemptsRetainHistoryAndCurrentReview(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	metadata := testMetadata("f", "0")
	firstTime := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	first := cleanReview("First review.")
	first.Output.NewFindings = []NewFinding{{
		Severity: "warning", Title: "first finding", Description: "Found by the first review.",
	}}
	firstIDs, err := store.ApplyReview(ctx, metadata,
		ReviewIdentity{Model: modelByName("first-model"), ReasoningEffort: "low"}, first, firstTime)
	if err != nil {
		t.Fatalf("first ApplyReview: %v", err)
	}
	secondTime := firstTime.Add(time.Hour)
	second := cleanReview("Second review.")
	second.Output.NewFindings = []NewFinding{{
		Severity: "error", Title: "second finding", Description: "Found by the second review.",
	}}
	secondIDs, err := store.ApplyReview(ctx, metadata,
		ReviewIdentity{Model: modelByName("second-model"), ReasoningEffort: "xhigh"}, second, secondTime)
	if err != nil {
		t.Fatalf("second ApplyReview: %v", err)
	}
	if len(firstIDs) != 1 || len(secondIDs) != 1 || firstIDs[0] == secondIDs[0] {
		t.Fatalf("finding IDs = %v then %v", firstIDs, secondIDs)
	}

	record, err := store.Commit(ctx, metadata.SHA)
	if err != nil {
		t.Fatal(err)
	}
	if record.Model != "second-model" || record.ReasoningEffort != "xhigh" ||
		record.Summary != "Second review." || record.NewCount != 1 {
		t.Fatalf("current review = %+v", record)
	}
	attempts, err := store.ReviewAttempts(ctx, metadata.SHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].Number != 1 || attempts[0].Current ||
		attempts[0].Model != "first-model" || attempts[0].Summary != "First review." ||
		attempts[1].Number != 2 || !attempts[1].Current || attempts[1].Model != "second-model" ||
		!attempts[1].ReviewedAt.Equal(secondTime) {
		t.Fatalf("review attempts = %+v", attempts)
	}
	firstFindings, err := store.FindingsIntroducedByReview(ctx, attempts[0].ID)
	if err != nil || len(firstFindings) != 1 || firstFindings[0].ID != firstIDs[0] {
		t.Fatalf("first attempt findings = %+v, %v", firstFindings, err)
	}
	secondFindings, err := store.FindingsIntroducedBy(ctx, metadata.SHA)
	if err != nil || len(secondFindings) != 1 || secondFindings[0].ID != secondIDs[0] {
		t.Fatalf("current findings = %+v, %v", secondFindings, err)
	}
	open, err := store.OpenFindings(ctx)
	if err != nil || len(open) != 2 {
		t.Fatalf("conservative open findings = %+v, %v", open, err)
	}
}

func TestReviewStatsAggregateEveryAttemptAndPreserveUnknownCosts(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	firstTime := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	known := cleanReview("Known-price review.")
	known.Usage = &TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	known.Duration = 10 * time.Second
	metadata := testMetadata("h", "0")
	if _, err := store.ApplyReview(ctx, metadata,
		ReviewIdentity{Model: modelByName("gpt-5.6-luna"), ReasoningEffort: "low"}, known, firstTime); err != nil {
		t.Fatal(err)
	}
	known.Duration = 20 * time.Second
	if _, err := store.ApplyReview(ctx, metadata,
		ReviewIdentity{Model: modelByName("gpt-5.6-luna"), ReasoningEffort: "xhigh"}, known, firstTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	unknown := cleanReview("Unknown-price review.")
	unknown.Usage = &TokenUsage{InputTokens: 7, OutputTokens: 3}
	unknown.Duration = 30 * time.Second
	if _, err := store.ApplyReview(ctx, testMetadata("i", "h"),
		ReviewIdentity{Model: modelByName("private-model")}, unknown, firstTime.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	stats, err := store.ReviewStats(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Attempts != 3 || stats.Commits != 2 || stats.InputTokens != 2_000_007 ||
		stats.OutputTokens != 2_000_003 || stats.PricedAttempts != 2 ||
		stats.UnknownCostAttempts != 1 || stats.MinimumCostMicrousd == 0 ||
		stats.MaximumCostMicrousd < stats.MinimumCostMicrousd || len(stats.Groups) != 3 ||
		stats.DurationMilliseconds != 60_000 || stats.TimedAttempts != 3 || stats.UntimedAttempts != 0 {
		t.Fatalf("review stats = %+v", stats)
	}
	since := firstTime.Add(90 * time.Minute)
	filtered, err := store.ReviewStats(ctx, "private-model", &since)
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Attempts != 1 || filtered.Commits != 1 || filtered.UnknownCostAttempts != 1 ||
		filtered.InputTokens != 7 || filtered.DurationMilliseconds != 30_000 || filtered.TimedAttempts != 1 {
		t.Fatalf("filtered review stats = %+v", filtered)
	}
}

func TestManualFindingDispositionAndNotes(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	result := cleanReview("Found an issue.")
	result.Output.NewFindings = []NewFinding{{
		Severity: "warning", Title: "triage me", Description: "This finding needs human triage.",
	}}
	now := time.Date(2026, 8, 16, 14, 0, 0, 0, time.UTC)
	ids, err := store.ApplyReview(ctx, testMetadata("g", "0"),
		ReviewIdentity{Model: modelByName("test-model")}, result, now)
	if err != nil {
		t.Fatal(err)
	}
	id := ids[0]
	if err := store.DismissFinding(ctx, id, "Accepted compatibility tradeoff.", now.Add(time.Minute)); err != nil {
		t.Fatalf("DismissFinding: %v", err)
	}
	if open, err := store.OpenFindings(ctx); err != nil || len(open) != 0 {
		t.Fatalf("open after dismissal = %+v, %v", open, err)
	}
	finding, err := store.Finding(ctx, id)
	if err != nil || finding.DismissedAt == nil || finding.DismissReason != "Accepted compatibility tradeoff." {
		t.Fatalf("dismissed finding = %+v, %v", finding, err)
	}
	if err := store.AddFindingNote(ctx, id, "Revisit after the compatibility window.", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("AddFindingNote: %v", err)
	}
	if err := store.ReopenFinding(ctx, id, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("ReopenFinding: %v", err)
	}
	if open, err := store.OpenFindings(ctx); err != nil || len(open) != 1 || open[0].ID != id {
		t.Fatalf("open after reopen = %+v, %v", open, err)
	}
	events, err := store.FindingEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Action != "opened" || events[1].Action != "dismissed" ||
		events[2].Action != "noted" || events[2].Note != "Revisit after the compatibility window." ||
		events[3].Action != "reopened" {
		t.Fatalf("finding events = %+v", events)
	}
}

func TestModelConfigurationRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	model := Model{
		Name: "private-model",
		Pricing: &ModelPricing{
			ServiceTier: "priority", Source: "operator", AsOf: "2026-08-16",
			LongContextInputTokens: 100_000,
			ShortContext:           TokenPrices{100, 20, 125, 600},
			LongContext:            TokenPrices{200, 40, 250, 900},
		},
	}
	if err := store.SaveModel(ctx, model); err != nil {
		t.Fatalf("SaveModel: %v", err)
	}
	stored, found, err := store.Model(ctx, model.Name)
	if err != nil || !found || stored.Pricing == nil ||
		stored.Pricing.ServiceTier != "priority" || stored.Pricing.ShortContext.CacheWriteNanousdPerToken != 125 ||
		stored.Pricing.LongContext.OutputNanousdPerToken != 900 {
		t.Fatalf("stored model = %+v, found=%t, err=%v", stored, found, err)
	}
	if err := store.SaveModel(ctx, Model{Name: model.Name}); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	stored, found, err = store.Model(ctx, model.Name)
	if err != nil || !found || stored.Pricing != nil {
		t.Fatalf("unknown model = %+v, found=%t, err=%v", stored, found, err)
	}
}

func testMetadata(shaCharacter, parentCharacter string) CommitMetadata {
	return CommitMetadata{
		SHA:       strings.Repeat(shaCharacter, 40),
		ParentSHA: strings.Repeat(parentCharacter, 40),
		Author:    "AIR Test <air-test@example.invalid>",
		Date:      "2026-08-14T12:00:00Z",
		Message:   "test commit",
	}
}
