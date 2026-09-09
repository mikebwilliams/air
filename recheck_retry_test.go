package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParallelRecheckRetrySavesOnlySuccessfulAttempt(t *testing.T) {
	repository, store, head := newParallelRecheckFixture(t, 3)
	started := make(chan int64, 4)
	releaseFailure, releaseRetry, releaseOther := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls [4]atomic.Int32
	var active atomic.Int32
	var original RecheckInput
	check := func(ctx context.Context, input RecheckInput) (RecheckResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		if current > 2 || input.HeadSHA != head || len(input.Findings) != 1 {
			return RecheckResult{}, errors.New("invalid concurrency, target, or batch")
		}
		id := input.Findings[0].ID
		attempt := calls[id].Add(1)
		started <- id
		var release <-chan struct{}
		if id == 1 {
			if attempt == 1 {
				original = input
				release = releaseFailure
			} else {
				if attempt != 2 || !reflect.DeepEqual(input, original) {
					return RecheckResult{}, errors.New("retry changed the batch or exceeded two attempts")
				}
				release = releaseRetry
			}
		} else if id == 2 {
			release = releaseOther
		}
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return RecheckResult{}, ctx.Err()
			}
		}
		if id == 1 && attempt == 1 {
			return RecheckResult{}, errors.New("temporary model failure")
		}
		result := recheckStillPresent(input.Findings)
		result.Output.Findings[0].Outcome = "resolved"
		return result, nil
	}
	var output bytes.Buffer
	_, wait := startParallelRecheckTest(t, repository, store, recheckOptions{
		Model: "check-model", ReasoningEffort: "high", BatchSize: 20, Jobs: 2,
		RetryOnError: true, RetryLimit: 1, Output: &output, NewReviewer: parallelRecheckFactory(check),
	})
	requireInitialRecheckJobs(t, started)
	close(releaseFailure)
	if id := receiveRecheckStart(t, started); id != 1 {
		t.Fatalf("expected retry of batch 1, got %d", id)
	}
	prior, err := store.PreviouslyRecheckedFindingIDs(context.Background(), head,
		codexReviewerName, "check-model", "high", recheckPromptVersion)
	if err != nil || len(prior) != 0 {
		t.Fatalf("failed or unfinished attempt was recorded: %v, %v", prior, err)
	}
	close(releaseRetry)
	if id := receiveRecheckStart(t, started); id != 3 {
		t.Fatalf("expected batch 3 after successful retry, got %d", id)
	}
	finding, err := store.Finding(context.Background(), 1)
	if err != nil || finding.ResolvedSHA == nil || *finding.ResolvedSHA != head {
		t.Fatalf("retry resolution was not saved: %+v, %v", finding, err)
	}
	close(releaseOther)
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if calls[1].Load() != 2 || calls[2].Load() != 1 || calls[3].Load() != 1 || active.Load() != 0 {
		t.Fatalf("attempts = [%d %d %d], active=%d", calls[1].Load(), calls[2].Load(), calls[3].Load(), active.Load())
	}
	stats, err := store.ReviewStats(context.Background(), "check-model", nil)
	if err != nil || stats.RecheckAttempts != 3 || stats.InputTokens != 30 || stats.OutputTokens != 6 {
		t.Fatalf("successful retry accounting = %+v, %v", stats, err)
	}
	for _, want := range []string{"temporary model failure", "Retrying recheck batch 1", "retry 1/1", "Rechecked 3 findings"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
}

func TestRecheckRetryExhaustionHonorsContinueOnError(t *testing.T) {
	for _, continueOnError := range []bool{false, true} {
		for _, invalidOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("continue=%t/invalid=%t", continueOnError, invalidOutput), func(t *testing.T) {
				repository, store, head := newParallelRecheckFixture(t, 2)
				var calls [3]int
				var output bytes.Buffer
				err := recheckRepository(context.Background(), repository, store, recheckOptions{
					Model: "check-model", ReasoningEffort: "high", BatchSize: 20,
					RetryOnError: true, RetryLimit: 3, ContinueOnError: continueOnError, Output: &output,
					NewReviewer: parallelRecheckFactory(func(_ context.Context, input RecheckInput) (RecheckResult, error) {
						id := input.Findings[0].ID
						calls[id]++
						result := recheckStillPresent(input.Findings)
						if id == 1 {
							if !invalidOutput {
								return RecheckResult{}, context.DeadlineExceeded // An attempt timeout, not parent cancellation.
							}
							result.Output.Findings[0].ID = 99
						}
						return result, nil
					}),
				})
				if err == nil {
					t.Fatal("exhausted retry returned success")
				}
				wantOtherCalls := 0
				if continueOnError {
					wantOtherCalls = 1
					if !strings.Contains(err.Error(), "1 recheck batches failed") {
						t.Fatalf("failed attempts were counted as separate batches: %v", err)
					}
				} else if !invalidOutput && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lost final retry error: %v", err)
				}
				if calls[1] != 4 || calls[2] != wantOtherCalls {
					t.Fatalf("calls = %v, want batch 1 four times and batch 2 %d times", calls, wantOtherCalls)
				}
				if strings.Count(output.String(), "Recheck batch 1 failed") != 4 ||
					strings.Count(output.String(), "Retrying recheck batch 1") != 3 {
					t.Fatalf("unexpected retry output:\n%s", output.String())
				}
				prior, err := store.PreviouslyRecheckedFindingIDs(context.Background(), head,
					codexReviewerName, "check-model", "high", recheckPromptVersion)
				if err != nil || len(prior) != wantOtherCalls {
					t.Fatalf("completed rechecks = %v, %v", prior, err)
				}
				if _, failedRecorded := prior[1]; failedRecorded {
					t.Fatal("exhausted retry was marked complete")
				}
			})
		}
	}
}

func TestCLIRecheckRetriesHarnessFailure(t *testing.T) {
	repository, _, _ := newParallelRecheckFixture(t, 1)
	fail, _ := newCodexTestCommand(t, RecheckOutput{}, "temporary harness failure")
	succeed, _ := newCodexTestCommand(t, RecheckOutput{
		Findings: []RecheckFindingResult{{ID: 1, Outcome: "still_present", Reason: "The failure remains."}},
		Summary:  "Checked the current implementation.",
	}, "")
	var calls atomic.Int32
	var output bytes.Buffer
	environment := cliEnvironment{
		Cwd: repository.WorkTree, Stdout: &output, Stderr: io.Discard,
		Getenv: func(string) string { return "" },
		CodexCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if calls.Add(1) == 1 {
				return fail(ctx, name, args...)
			}
			return succeed(ctx, name, args...)
		},
	}
	args := []string{"recheck", "--model", "check-model", "--effort", "high"}
	if err := runCLI(context.Background(), append(args, "--dry-run"), environment); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || !strings.Contains(output.String(), "Failed batches will be retried up to 3 times.") {
		t.Fatalf("dry run calls=%d output=%s", calls.Load(), output.String())
	}
	output.Reset()
	if err := runCLI(context.Background(), args, environment); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !strings.Contains(output.String(), "temporary harness failure") ||
		!strings.Contains(output.String(), "Retrying recheck batch 1") {
		t.Fatalf("retry calls=%d output=%s", calls.Load(), output.String())
	}
}

func TestCLIRecheckRetryDefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		calls int32
		want  string
	}{
		{"defaults", nil, 8, "2 recheck batches failed"},
		{"custom retry limit", []string{"--retry-limit", "1"}, 4, "2 recheck batches failed"},
		{"retry disabled", []string{"--retry-on-error=false"}, 2, "2 recheck batches failed"},
		{"zero retry limit", []string{"--retry-limit", "0"}, 2, "2 recheck batches failed"},
		{"stop after retry exhaustion", []string{"--continue-on-error=false"}, 4, "temporary harness failure"},
		{"stop immediately", []string{"--retry-on-error=false", "--continue-on-error=false"}, 1, "temporary harness failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, store, head := newParallelRecheckFixture(t, 2)
			command, _ := newCodexTestCommand(t, RecheckOutput{}, "temporary harness failure")
			var calls atomic.Int32
			environment := cliEnvironment{
				Cwd: repository.WorkTree, Stdout: io.Discard, Stderr: io.Discard,
				Getenv: func(string) string { return "" },
				CodexCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
					calls.Add(1)
					return command(ctx, name, args...)
				},
			}
			args := append([]string{"recheck", "--model", "check-model", "--effort", "high"}, test.args...)
			err := runCLI(context.Background(), args, environment)
			if err == nil || !strings.Contains(err.Error(), test.want) || calls.Load() != test.calls {
				t.Fatalf("calls=%d error=%v; want %d calls and %q", calls.Load(), err, test.calls, test.want)
			}
			prior, err := store.PreviouslyRecheckedFindingIDs(context.Background(), head,
				codexReviewerName, "check-model", "high", recheckPromptVersion)
			if err != nil || len(prior) != 0 {
				t.Fatalf("failed retries were marked complete: %v, %v", prior, err)
			}
		})
	}
}
