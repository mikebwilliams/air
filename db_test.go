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
	newIDs, err := store.ApplyReview(ctx, commitA, ReviewIdentity{Model: "test-model", ReasoningEffort: "low"}, ReviewResult{
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
			OutputTokens:          20,
			ReasoningOutputTokens: 5,
		},
		EstimatedCostMicrousd: int64Pointer(37),
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
	if record.Model != "test-model" || record.ReasoningEffort != "low" {
		t.Fatalf("review identity = model %q, effort %q", record.Model, record.ReasoningEffort)
	}
	if record.Usage == nil || *record.Usage != (TokenUsage{100, 40, 20, 5}) {
		t.Fatalf("review usage = %+v", record.Usage)
	}
	if record.EstimatedCostMicrousd == nil || *record.EstimatedCostMicrousd != 37 {
		t.Fatalf("estimated cost = %v", record.EstimatedCostMicrousd)
	}
	open, err := store.OpenFindings(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("open findings = %v, %v", open, err)
	}

	commitB := testMetadata("b", "a")
	_, err = store.ApplyReview(ctx, commitB, ReviewIdentity{Model: "test-model", ReasoningEffort: "high"}, ReviewResult{
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

func TestApplyReviewRollsBackWholeCommit(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	defer store.Close()

	metadata := testMetadata("c", "0")
	_, err = store.ApplyReview(ctx, metadata, ReviewIdentity{Model: "test-model", ReasoningEffort: "medium"}, ReviewResult{
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

func testMetadata(shaCharacter, parentCharacter string) CommitMetadata {
	return CommitMetadata{
		SHA:       strings.Repeat(shaCharacter, 40),
		ParentSHA: strings.Repeat(parentCharacter, 40),
		Author:    "AIR Test <air-test@example.invalid>",
		Date:      "2026-08-14T12:00:00Z",
		Message:   "test commit",
	}
}
