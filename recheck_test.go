package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
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
	for index, title := range []string{"one", "two", "three"} {
		file := "app.go"
		if index == 1 {
			file = "other.go"
		}
		seed.Output.NewFindings = append(seed.Output.NewFindings, NewFinding{
			Severity: "warning", Title: title, Description: "Concrete failure.", File: &file,
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
		Model: "check-model", ReasoningEffort: "high",
		BatchSize: 2, ContinueOnError: true, Output: &output,
		NewReviewer: func() (RecheckReviewer, ReviewIdentity, error) {
			return first, ReviewIdentity{Model: modelByName("check-model"), ReasoningEffort: "high"}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "1 recheck batches failed") || len(first.calls) != 2 {
		t.Fatalf("first run calls=%d err=%v output=%s", len(first.calls), err, output.String())
	}
	prior, err := store.PreviouslyRecheckedFindingIDs(ctx, head, codexReviewerName,
		"check-model", "high", recheckPromptVersion)
	if err != nil || len(prior) != 2 {
		t.Fatalf("successful first batch = %v, %v", prior, err)
	}
	otherHarness, err := store.PreviouslyRecheckedFindingIDs(ctx, head, claudeReviewerName,
		"check-model", "high", recheckPromptVersion)
	if err != nil || len(otherHarness) != 0 {
		t.Fatalf("other-harness rechecks = %v, %v", otherHarness, err)
	}

	second := &fakeRecheckReviewer{check: func(input RecheckInput) (RecheckResult, error) {
		if len(input.Findings) != 1 || input.Findings[0].ID != 2 {
			t.Fatalf("resumed batch = %+v", input.Findings)
		}
		return recheckStillPresent(input.Findings), nil
	}}
	output.Reset()
	if err := recheckRepository(ctx, repository, store, recheckOptions{
		Model: "check-model", ReasoningEffort: "high",
		BatchBy: "count", BatchSize: 2, Output: &output,
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
		Model: "check-model", ReasoningEffort: "high",
		PromptVersion: customPromptIdentity, BatchBy: "count", BatchSize: 3, Output: &output,
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
	prior, err = store.PreviouslyRecheckedFindingIDs(ctx, head, codexReviewerName,
		"check-model", "high", customPromptIdentity)
	if err != nil || len(prior) != 3 {
		t.Fatalf("custom-prompt results = %v, %v", prior, err)
	}
}

func TestRecheckBatchPlansMatchExecution(t *testing.T) {
	tests := []struct {
		name  string
		mode  string
		limit int
		ids   []int64
		args  []string
		want  [][]int64
	}{
		{
			name: "default groups by file and caps each group",
			want: [][]int64{{2, 5}, {6}, {1, 3}, {8}, {4, 7}, {9}},
		},
		{
			name: "count mixes files", mode: "count", args: []string{"--batch-by", "count"},
			want: [][]int64{{1, 2}, {3, 4}, {5, 6}, {7, 8}, {9}},
		},
		{
			name: "limit selects before grouping", mode: "file", limit: 5,
			args: []string{"--batch-by", "file", "--limit", "5"},
			want: [][]int64{{2, 5}, {1, 3}, {4}},
		},
		{
			name: "explicit IDs", ids: []int64{8, 6, 3, 1}, args: []string{"8", "6", "3", "1"},
			want: [][]int64{{6}, {3, 1}, {8}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repository, directory := newTestGitRepository(t)
			head := testCommitFile(t, directory, "app.go", []byte("package app\n"), "base")
			store, err := CreateStore(ctx, repository.DatabasePath(), head)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			seed := cleanReview("Findings across multiple files.")
			for _, file := range []string{"b.go", "a.go", "b.go", "", "a.go", "a.go", "", "c.go", ""} {
				finding := NewFinding{Severity: "warning", Title: "Failure", Description: "Concrete failure."}
				if file != "" {
					finding.File = stringPointer(file)
				}
				seed.Output.NewFindings = append(seed.Output.NewFindings, finding)
			}
			if _, err := store.ApplyReview(ctx, CommitMetadata{SHA: head},
				ReviewIdentity{Model: modelByName("seed-model")}, seed, time.Now()); err != nil {
				t.Fatal(err)
			}
			var plan bytes.Buffer
			args := append([]string{
				"recheck", "--dry-run", "--model", "check-model", "--effort", "high", "--batch-size", "2",
			}, test.args...)
			if err := runCLI(ctx, args, cliEnvironment{
				Cwd: directory, Stdout: &plan, Stderr: io.Discard,
				Getenv: func(string) string { return "" },
			}); err != nil {
				t.Fatal(err)
			}
			prior, err := store.PreviouslyRecheckedFindingIDs(ctx, head, codexReviewerName,
				"check-model", "high", recheckPromptVersion)
			if err != nil || len(prior) != 0 {
				t.Fatalf("dry run recorded checks: %v, %v", prior, err)
			}
			if test.mode != "count" && len(test.ids) == 0 {
				if !strings.Contains(plan.String(), `Batch 1 ("a.go"; #2, #5)`) ||
					!strings.Contains(plan.String(), "no file;") {
					t.Fatalf("plan is missing files or exact IDs:\n%s", plan.String())
				}
			}
			reviewer := &fakeRecheckReviewer{check: func(input RecheckInput) (RecheckResult, error) {
				if input.HeadSHA != head {
					t.Fatalf("HEAD = %s, want %s", input.HeadSHA, head)
				}
				return recheckStillPresent(input.Findings), nil
			}}
			var output bytes.Buffer
			options := recheckOptions{
				FindingIDs: test.ids, Model: "check-model", ReasoningEffort: "high", Limit: test.limit,
				BatchBy: test.mode, BatchSize: 2, Output: &output,
				NewReviewer: func() (RecheckReviewer, ReviewIdentity, error) {
					return reviewer, ReviewIdentity{Model: modelByName("check-model"), ReasoningEffort: "high"}, nil
				},
			}
			if err := recheckRepository(ctx, repository, store, options); err != nil {
				t.Fatal(err)
			}
			var actual [][]int64
			for _, call := range reviewer.calls {
				var ids []int64
				for _, finding := range call.Findings {
					ids = append(ids, finding.ID)
				}
				actual = append(actual, ids)
			}
			if !reflect.DeepEqual(actual, test.want) {
				t.Fatalf("model batches = %v, want %v", actual, test.want)
			}
			plannedBatches := 0
			for _, line := range strings.Split(plan.String(), "\n") {
				if strings.HasPrefix(line, "  Batch ") {
					plannedBatches++
					executed := "Recheck batch " + strings.TrimPrefix(line, "  Batch ") + ":"
					if !strings.Contains(output.String(), executed) {
						t.Fatalf("planned batch %q not found in execution:\n%s", line, output.String())
					}
				}
			}
			if plannedBatches != len(test.want) {
				t.Fatalf("planned %d batches, want %d:\n%s", plannedBatches, len(test.want), plan.String())
			}
		})
	}
}

func TestRecheckRejectsInvalidBatchingBeforeRepositoryAccess(t *testing.T) {
	for _, args := range [][]string{
		{"--batch-by", "directory"}, {"--batch-by", ""}, {"--batch-size", "0"}, {"--batch-size", "51"},
	} {
		err := runCLI(context.Background(), append([]string{"recheck"}, args...), cliEnvironment{
			Cwd: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard,
		})
		if err == nil || !strings.Contains(err.Error(), args[0]) {
			t.Errorf("recheck %v: expected batching error, got %v", args, err)
		}
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
