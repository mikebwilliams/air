package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeRecheckReviewer struct {
	calls []RecheckInput
	check func(RecheckInput) (RecheckResult, error)
}

func (r *fakeRecheckReviewer) Recheck(_ context.Context, input RecheckInput) (RecheckResult, error) {
	r.calls = append(r.calls, input)
	return r.check(input)
}

func TestRecheckContinuesAndResumesSuccessfulBatches(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.go", []byte("package app\n"), "base")
	head := testCommitFile(t, directory, "app.go", []byte("package app\nfunc head() {}\n"), "head")
	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seed := cleanReview("Three findings.")
	for _, title := range []string{"one", "two", "three"} {
		seed.Output.NewFindings = append(seed.Output.NewFindings, NewFinding{
			Severity: "warning", Title: title, Description: "Concrete failure.", File: stringPointer("app.go"),
		})
	}
	if _, err := store.ApplyReview(ctx, CommitMetadata{SHA: base},
		ReviewIdentity{Model: modelByName("seed-model")}, seed, time.Now()); err != nil {
		t.Fatal(err)
	}
	call := 0
	first := &fakeRecheckReviewer{check: func(input RecheckInput) (RecheckResult, error) {
		call++
		if input.HeadSHA != head {
			t.Fatalf("HEAD = %s, want %s", input.HeadSHA, head)
		}
		if call == 2 {
			return RecheckResult{}, errors.New("temporary model failure")
		}
		return recheckStillPresent(input.Findings), nil
	}}
	var output bytes.Buffer
	err = recheckRepository(ctx, repository, store, recheckOptions{
		Reviewer: "codex", Model: "check-model", ReasoningEffort: "high",
		BatchSize: 2, ContinueOnError: true, Output: &output,
		NewReviewer: func() (RecheckReviewer, ReviewIdentity, error) {
			return first, ReviewIdentity{Model: modelByName("check-model"), ReasoningEffort: "high"}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "1 recheck batches failed") || len(first.calls) != 2 {
		t.Fatalf("first run calls=%d err=%v output=%s", len(first.calls), err, output.String())
	}
	prior, err := store.PreviouslyRecheckedFindingIDs(ctx, head, "codex", "check-model", "high", recheckPromptVersion)
	if err != nil || len(prior) != 2 {
		t.Fatalf("successful first batch = %v, %v", prior, err)
	}

	second := &fakeRecheckReviewer{check: func(input RecheckInput) (RecheckResult, error) {
		if len(input.Findings) != 1 || input.Findings[0].ID != 3 {
			t.Fatalf("resumed batch = %+v", input.Findings)
		}
		return recheckStillPresent(input.Findings), nil
	}}
	output.Reset()
	if err := recheckRepository(ctx, repository, store, recheckOptions{
		Reviewer: "codex", Model: "check-model", ReasoningEffort: "high",
		BatchSize: 2, Output: &output,
		NewReviewer: func() (RecheckReviewer, ReviewIdentity, error) {
			return second, ReviewIdentity{Model: modelByName("check-model"), ReasoningEffort: "high"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(second.calls) != 1 || !strings.Contains(output.String(), "2 already checked") {
		t.Fatalf("resume calls=%d output=%s", len(second.calls), output.String())
	}

	const customPromptIdentity = "custom:sha256:test-recheck-prompt"
	third := &fakeRecheckReviewer{check: func(input RecheckInput) (RecheckResult, error) {
		if len(input.Findings) != 3 {
			t.Fatalf("custom-prompt batch = %+v", input.Findings)
		}
		return recheckStillPresent(input.Findings), nil
	}}
	output.Reset()
	if err := recheckRepository(ctx, repository, store, recheckOptions{
		Reviewer: "codex", Model: "check-model", ReasoningEffort: "high",
		PromptVersion: customPromptIdentity, BatchSize: 3, Output: &output,
		NewReviewer: func() (RecheckReviewer, ReviewIdentity, error) {
			return third, ReviewIdentity{
				Model: modelByName("check-model"), ReasoningEffort: "high",
				PromptVersion: customPromptIdentity,
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(third.calls) != 1 || strings.Contains(output.String(), "already checked") {
		t.Fatalf("custom prompt calls=%d output=%s", len(third.calls), output.String())
	}
	prior, err = store.PreviouslyRecheckedFindingIDs(ctx, head, "codex", "check-model", "high", customPromptIdentity)
	if err != nil || len(prior) != 3 {
		t.Fatalf("custom-prompt results = %v, %v", prior, err)
	}
}

func recheckStillPresent(findings []Finding) RecheckResult {
	results := make([]RecheckFindingResult, 0, len(findings))
	for _, finding := range findings {
		results = append(results, RecheckFindingResult{
			ID: finding.ID, Outcome: "still_present", Reason: "The failure remains at HEAD.",
		})
	}
	return RecheckResult{
		Output:      RecheckOutput{Findings: results, Summary: "Checked current HEAD."},
		RawResponse: "recheck", Usage: &TokenUsage{InputTokens: 10, OutputTokens: 2},
	}
}
