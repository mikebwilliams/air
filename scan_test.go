package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeReviewer struct {
	calls  []ReviewInput
	review func(ReviewInput) (ReviewResult, error)
}

func (reviewer *fakeReviewer) Review(_ context.Context, input ReviewInput) (ReviewResult, error) {
	reviewer.calls = append(reviewer.calls, input)
	return reviewer.review(input)
}

func TestScanRepositoryLifecycleSkipsAndResume(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	first := testCommitFile(t, directory, "app.txt", []byte("base\nbug\n"), "introduce bug")
	binary := testCommitFile(t, directory, "asset.bin", []byte{0, 1, 2, 0, 3}, "binary asset")
	second := testCommitFile(t, directory, "app.txt", []byte("base\nfixed\n"), "fix bug")

	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	defer store.Close()

	reviewer := &fakeReviewer{}
	reviewer.review = func(input ReviewInput) (ReviewResult, error) {
		switch len(reviewer.calls) {
		case 1:
			if input.Commit.SHA != first || len(input.OpenFindings) != 0 {
				t.Fatalf("first review input = %+v", input)
			}
			return ReviewResult{
				Output: ReviewOutput{
					NewFindings: []NewFinding{{
						Severity:    "warning",
						Title:       "bug",
						Description: "The new path leaves broken state.",
						File:        stringPointer("app.txt"),
					}},
					ResolvedFindings: []ResolvedFinding{},
					Summary:          "Reviewed the bug.",
				},
				RawResponse: "[]",
				Usage:       &TokenUsage{InputTokens: 10, OutputTokens: 2},
			}, nil
		case 2:
			if input.Commit.SHA != second || len(input.OpenFindings) != 1 {
				t.Fatalf("second review input = %+v", input)
			}
			return ReviewResult{
				Output: ReviewOutput{
					NewFindings: []NewFinding{},
					ResolvedFindings: []ResolvedFinding{{
						ID:     input.OpenFindings[0].ID,
						Reason: "The broken state is now restored.",
					}},
					Summary: "Reviewed the fix.",
				},
				RawResponse: "[]",
				Usage:       &TokenUsage{InputTokens: 12, OutputTokens: 3},
			}, nil
		default:
			t.Fatalf("unexpected reviewer call %d", len(reviewer.calls))
			return ReviewResult{}, nil
		}
	}
	var output bytes.Buffer
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	err = scanRepository(ctx, repository, store, scanOptions{
		Output: &output,
		Now:    func() time.Time { now = now.Add(time.Second); return now },
		NewReviewer: func() (Reviewer, ReviewIdentity, error) {
			return reviewer, ReviewIdentity{Model: modelByName("fake-model"), ReasoningEffort: "low"}, nil
		},
	})
	if err != nil {
		t.Fatalf("scanRepository: %v", err)
	}
	if len(reviewer.calls) != 2 {
		t.Fatalf("reviewer calls = %d, want 2", len(reviewer.calls))
	}
	if !strings.Contains(output.String(), shortSHA(binary)+"  skipped: binary-only diff") {
		t.Fatalf("scan output lacks binary skip:\n%s", output.String())
	}
	open, err := store.OpenFindings(ctx)
	if err != nil || len(open) != 0 {
		t.Fatalf("open findings = %v, %v", open, err)
	}
	records, err := store.Log(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("log has %d records, want 3", len(records))
	}
	binaryRecord, err := store.Commit(ctx, binary)
	if err != nil || binaryRecord.Status != "skipped" {
		t.Fatalf("binary record = %+v, %v", binaryRecord, err)
	}

	output.Reset()
	if err := scanRepository(ctx, repository, store, scanOptions{
		Output: &output,
		NewReviewer: func() (Reviewer, ReviewIdentity, error) {
			return reviewer, ReviewIdentity{Model: modelByName("fake-model"), ReasoningEffort: "low"}, nil
		},
	}); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if output.String() != "No unprocessed commits.\n" {
		t.Fatalf("second scan output = %q", output.String())
	}
	if len(reviewer.calls) != 2 {
		t.Fatalf("resume repeated model request; calls = %d", len(reviewer.calls))
	}
}

func TestScanStopsAtFailureAndRetriesFailedCommit(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	first := testCommitFile(t, directory, "app.txt", []byte("first\n"), "first")
	second := testCommitFile(t, directory, "app.txt", []byte("second\n"), "second")
	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	reviewer := &fakeReviewer{}
	reviewer.review = func(input ReviewInput) (ReviewResult, error) {
		if input.Commit.SHA == second {
			return ReviewResult{}, errors.New("temporary model failure")
		}
		return cleanReview("Reviewed first."), nil
	}
	err = scanRepository(ctx, repository, store, scanOptions{
		Output: &bytes.Buffer{},
		NewReviewer: func() (Reviewer, ReviewIdentity, error) {
			return reviewer, ReviewIdentity{Model: modelByName("fake-model"), ReasoningEffort: "low"}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "temporary model failure") {
		t.Fatalf("first scan error = %v", err)
	}
	if _, err := store.Commit(ctx, first); err != nil {
		t.Fatalf("first commit not retained: %v", err)
	}
	if _, err := store.Commit(ctx, second); err == nil {
		t.Fatal("failed commit was recorded")
	}
	failures, err := store.ScanFailures(ctx)
	if err != nil || len(failures) != 1 || failures[0].SHA != second ||
		failures[0].AttemptCount != 1 || failures[0].Model != "fake-model" {
		t.Fatalf("stored failure = %+v, %v", failures, err)
	}

	successReviewer := &fakeReviewer{review: func(input ReviewInput) (ReviewResult, error) {
		if input.Commit.SHA != second {
			t.Fatalf("resume reviewed %s, want %s", input.Commit.SHA, second)
		}
		return cleanReview("Reviewed retry."), nil
	}}
	if err := scanRepository(ctx, repository, store, scanOptions{
		Output: &bytes.Buffer{},
		NewReviewer: func() (Reviewer, ReviewIdentity, error) {
			return successReviewer, ReviewIdentity{Model: modelByName("fake-model"), ReasoningEffort: "low"}, nil
		},
	}); err != nil {
		t.Fatalf("resume scan: %v", err)
	}
	if len(successReviewer.calls) != 1 {
		t.Fatalf("resume calls = %d, want 1", len(successReviewer.calls))
	}
	if failures, err := store.ScanFailures(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("failure survived successful retry = %+v, %v", failures, err)
	}
}

func TestScanContinueOnErrorProcessesLaterCommits(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	first := testCommitFile(t, directory, "app.txt", []byte("first\n"), "first")
	second := testCommitFile(t, directory, "app.txt", []byte("second\n"), "second")
	third := testCommitFile(t, directory, "app.txt", []byte("third\n"), "third")
	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reviewer := &fakeReviewer{review: func(input ReviewInput) (ReviewResult, error) {
		if input.Commit.SHA == first || input.Commit.SHA == third {
			return ReviewResult{}, errors.New("transient backend failure")
		}
		return cleanReview("Reviewed middle commit."), nil
	}}
	var output bytes.Buffer
	err = scanRepository(ctx, repository, store, scanOptions{
		ContinueOnError: true,
		Output:          &output,
		NewReviewer: func() (Reviewer, ReviewIdentity, error) {
			return reviewer, ReviewIdentity{Model: modelByName("fake-model"), ReasoningEffort: "high"}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "2 commits failed") {
		t.Fatalf("continued scan error = %v", err)
	}
	if _, err := store.Commit(ctx, second); err != nil {
		t.Fatalf("middle commit was not retained: %v", err)
	}
	if _, err := store.Commit(ctx, first); err == nil {
		t.Fatal("first failed commit was recorded as processed")
	}
	failures, err := store.ScanFailures(ctx)
	if err != nil || len(failures) != 2 || failures[0].ReasoningEffort != "high" {
		t.Fatalf("continued failures = %+v, %v", failures, err)
	}
	if !strings.Contains(output.String(), shortSHA(first)+"  failed:") ||
		!strings.Contains(output.String(), shortSHA(second)+"  0 new, 0 resolved") ||
		!strings.Contains(output.String(), shortSHA(third)+"  failed:") {
		t.Fatalf("continued scan output:\n%s", output.String())
	}
}

func TestScanLimitProcessesOldestCommitsAndResumes(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	first := testCommitFile(t, directory, "app.txt", []byte("first\n"), "first")
	second := testCommitFile(t, directory, "app.txt", []byte("second\n"), "second")
	third := testCommitFile(t, directory, "app.txt", []byte("third\n"), "third")
	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	reviewer := &fakeReviewer{review: func(input ReviewInput) (ReviewResult, error) {
		return cleanReview("Reviewed " + shortSHA(input.Commit.SHA) + "."), nil
	}}
	newReviewer := func() (Reviewer, ReviewIdentity, error) {
		return reviewer, ReviewIdentity{Model: modelByName("fake-model"), ReasoningEffort: "low"}, nil
	}
	if err := scanRepository(ctx, repository, store, scanOptions{
		Limit:       2,
		Output:      &bytes.Buffer{},
		NewReviewer: newReviewer,
	}); err != nil {
		t.Fatalf("limited scan: %v", err)
	}
	if len(reviewer.calls) != 2 || reviewer.calls[0].Commit.SHA != first || reviewer.calls[1].Commit.SHA != second {
		t.Fatalf("reviewed commits = %+v", reviewer.calls)
	}
	if _, err := store.Commit(ctx, third); err == nil {
		t.Fatal("third commit was processed despite the limit")
	}

	if err := scanRepository(ctx, repository, store, scanOptions{
		Limit:       2,
		Output:      &bytes.Buffer{},
		NewReviewer: newReviewer,
	}); err != nil {
		t.Fatalf("resumed scan: %v", err)
	}
	if len(reviewer.calls) != 3 || reviewer.calls[2].Commit.SHA != third {
		t.Fatalf("reviewed commits after resume = %+v", reviewer.calls)
	}
}

func TestScanRejectsNegativeLimit(t *testing.T) {
	repository, _ := newTestGitRepository(t)
	err := scanRepository(context.Background(), repository, nil, scanOptions{Limit: -1})
	if err == nil || !strings.Contains(err.Error(), "limit must not be negative") {
		t.Fatalf("scan error = %v", err)
	}
}

func TestForcedScanCreatesReviewAttemptAndExcludesSameCommitFindings(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	firstReviewer := &fakeReviewer{review: func(input ReviewInput) (ReviewResult, error) {
		result := cleanReview("Initial review.")
		result.Output.NewFindings = []NewFinding{{
			Severity: "warning", Title: "initial finding", Description: "Initial review finding.",
		}}
		return result, nil
	}}
	if err := scanRepository(ctx, repository, store, scanOptions{
		Output: &bytes.Buffer{},
		NewReviewer: func() (Reviewer, ReviewIdentity, error) {
			return firstReviewer, ReviewIdentity{Model: modelByName("first-model"), ReasoningEffort: "low"}, nil
		},
	}); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	secondReviewer := &fakeReviewer{review: func(input ReviewInput) (ReviewResult, error) {
		if len(input.OpenFindings) != 0 {
			t.Fatalf("rescan received findings introduced by the same commit: %+v", input.OpenFindings)
		}
		return cleanReview("High-effort review."), nil
	}}
	if err := scanRepository(ctx, repository, store, scanOptions{
		Commits: []string{head},
		Force:   true,
		Output:  &bytes.Buffer{},
		NewReviewer: func() (Reviewer, ReviewIdentity, error) {
			return secondReviewer, ReviewIdentity{Model: modelByName("second-model"), ReasoningEffort: "xhigh"}, nil
		},
	}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	attempts, err := store.ReviewAttempts(ctx, head)
	if err != nil || len(attempts) != 2 || attempts[1].Model != "second-model" || !attempts[1].Current {
		t.Fatalf("review attempts = %+v, %v", attempts, err)
	}
}

func TestDryRunClassifiesPendingCommitsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	textCommit := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "text")
	binaryCommit := testCommitFile(t, directory, "image.bin", []byte{0, 1, 2, 0, 3}, "binary")
	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var output bytes.Buffer
	if err := scanRepository(ctx, repository, store, scanOptions{
		DryRun: true,
		Output: &output,
		NewReviewer: func() (Reviewer, ReviewIdentity, error) {
			t.Fatal("dry run constructed a reviewer")
			return nil, ReviewIdentity{}, nil
		},
	}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(output.String(), shortSHA(textCommit)+"  review") ||
		!strings.Contains(output.String(), shortSHA(binaryCommit)+"  skip: binary-only diff") ||
		!strings.Contains(output.String(), "Pending: 2 commits (1 reviewable, 1 skipped)") {
		t.Fatalf("dry-run output:\n%s", output.String())
	}
	processed, err := store.ProcessedSHAs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(processed) != 0 {
		t.Fatalf("dry run recorded commits: %v", processed)
	}
}

func cleanReview(summary string) ReviewResult {
	return ReviewResult{
		Output: ReviewOutput{
			NewFindings:      []NewFinding{},
			ResolvedFindings: []ResolvedFinding{},
			Summary:          summary,
		},
		RawResponse: "[]",
		Usage:       &TokenUsage{InputTokens: 10, OutputTokens: 2},
	}
}
