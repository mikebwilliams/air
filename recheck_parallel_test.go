package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type recheckReviewerFunc func(context.Context, RecheckInput) (RecheckResult, error)

func (reviewer recheckReviewerFunc) Recheck(ctx context.Context, input RecheckInput) (RecheckResult, error) {
	return reviewer(ctx, input)
}

func newParallelRecheckFixture(t *testing.T, count int) (*GitRepository, *Store, string) {
	t.Helper()
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	head := testCommitFile(t, directory, "app.go", []byte("package app\n"), "base")
	store, err := CreateStore(ctx, repository.DatabasePath(), head)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seed := cleanReview("Findings across files.")
	for index := 1; index <= count; index++ {
		seed.Output.NewFindings = append(seed.Output.NewFindings, NewFinding{
			Severity: "warning", Title: "Failure", Description: "Concrete failure.",
			File: stringPointer(fmt.Sprintf("file-%02d.go", index)),
		})
	}
	if _, err := store.ApplyReview(ctx, CommitMetadata{SHA: head},
		ReviewIdentity{Model: modelByName("seed-model")}, seed, time.Now()); err != nil {
		t.Fatal(err)
	}
	return repository, store, head
}

func parallelRecheckFactory(check recheckReviewerFunc) recheckReviewerFactory {
	return func() (RecheckReviewer, ReviewIdentity, error) {
		var busy atomic.Bool
		reviewer := recheckReviewerFunc(func(ctx context.Context, input RecheckInput) (RecheckResult, error) {
			if !busy.CompareAndSwap(false, true) {
				return RecheckResult{}, errors.New("reviewer was used concurrently by multiple batches")
			}
			defer busy.Store(false)
			return check(ctx, input)
		})
		return reviewer, ReviewIdentity{Model: modelByName("check-model"), ReasoningEffort: "high"}, nil
	}
}

// Waiting on a closed channel lets both the test and cleanup join the run.
func startParallelRecheckTest(t *testing.T, repository *GitRepository, store *Store, options recheckOptions) (context.CancelFunc, func() error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan struct{})
	var result error
	go func() {
		defer close(done)
		result = recheckRepository(ctx, repository, store, options)
	}()
	wait := func() error {
		t.Helper()
		select {
		case <-done:
			return result
		case <-time.After(15 * time.Second):
			t.Fatal("parallel recheck did not finish")
			return nil
		}
	}
	t.Cleanup(func() { cancel(); _ = wait() })
	return cancel, wait
}

func receiveRecheckStart(t *testing.T, started <-chan int64) int64 {
	t.Helper()
	select {
	case id := <-started:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("expected another recheck batch to start")
		return 0
	}
}

func requireInitialRecheckJobs(t *testing.T, started <-chan int64) {
	t.Helper()
	ids := []int64{receiveRecheckStart(t, started), receiveRecheckStart(t, started)}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if !reflect.DeepEqual(ids, []int64{1, 2}) {
		t.Fatalf("initial batches = %v, want [1 2]", ids)
	}
}

func TestParallelRecheckSavesOutOfOrderResultsAndBoundsConcurrency(t *testing.T) {
	repository, store, head := newParallelRecheckFixture(t, 4)
	started := make(chan int64, 4)
	release := make([]chan struct{}, 5)
	for index := range release {
		release[index] = make(chan struct{})
	}
	var active, peak atomic.Int32
	check := func(ctx context.Context, input RecheckInput) (RecheckResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
		}
		if current > 2 || input.HeadSHA != head || len(input.Findings) != 1 {
			return RecheckResult{}, errors.New("invalid concurrency, target, or batch")
		}
		id := input.Findings[0].ID
		started <- id
		select {
		case <-release[id]:
		case <-ctx.Done():
			return RecheckResult{}, ctx.Err()
		}
		result := recheckStillPresent(input.Findings)
		result.Output.Findings[0].Outcome = "resolved"
		return result, nil
	}
	clock := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	_, wait := startParallelRecheckTest(t, repository, store, recheckOptions{
		Model: "check-model", ReasoningEffort: "high", BatchSize: 20, Jobs: 2, Output: &output,
		NewReviewer: parallelRecheckFactory(check),
		ElapsedNow:  func() time.Time { clock = clock.Add(time.Second); return clock },
	})
	requireInitialRecheckJobs(t, started)
	close(release[2])
	if id := receiveRecheckStart(t, started); id != 3 {
		t.Fatalf("next batch = %d, want 3", id)
	}
	// Batch 2 must be durable before batch 1 has finished or batch 3 is released.
	finding, err := store.Finding(context.Background(), 2)
	if err != nil || finding.ResolvedSHA == nil || *finding.ResolvedSHA != head {
		t.Fatalf("completed batch was not saved immediately: %+v, %v", finding, err)
	}
	finding, err = store.Finding(context.Background(), 1)
	if err != nil || finding.ResolvedSHA != nil {
		t.Fatalf("unfinished batch changed state: %+v, %v", finding, err)
	}
	close(release[3])
	if id := receiveRecheckStart(t, started); id != 4 {
		t.Fatalf("next batch = %d, want 4", id)
	}
	close(release[4])
	close(release[1])
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 || active.Load() != 0 {
		t.Fatalf("peak=%d active=%d", peak.Load(), active.Load())
	}
	findings, err := store.OpenFindings(context.Background())
	if err != nil || len(findings) != 0 {
		t.Fatalf("open findings after parallel resolution: %v, %v", findings, err)
	}
	stats, err := store.ReviewStats(context.Background(), "check-model", nil)
	if err != nil || stats.RecheckAttempts != 4 || stats.InputTokens != 40 || stats.OutputTokens != 8 {
		t.Fatalf("parallel accounting = %+v, %v", stats, err)
	}
	var timed int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM recheck_attempts WHERE duration_ms > 0`).Scan(&timed); err != nil || timed != 4 {
		t.Fatalf("timed rechecks = %d, %v", timed, err)
	}
	if strings.Index(output.String(), "Recheck batch 2 (") > strings.Index(output.String(), "Recheck batch 1 (") {
		t.Fatalf("completion output was held in batch order:\n%s", output.String())
	}
}

type recheckFailureWriter struct {
	bytes.Buffer
	failed chan struct{}
}

func (output *recheckFailureWriter) Write(data []byte) (int, error) {
	n, err := output.Buffer.Write(data)
	if bytes.Contains(data, []byte("failed (")) {
		select {
		case output.failed <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestParallelRecheckFailureSchedulingAndResume(t *testing.T) {
	for _, continueOnError := range []bool{false, true} {
		for _, invalidOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("continue=%t/invalid=%t", continueOnError, invalidOutput), func(t *testing.T) {
				repository, store, head := newParallelRecheckFixture(t, 4)
				started := make(chan int64, 4)
				releaseFailure, releaseRunning := make(chan struct{}), make(chan struct{})
				check := func(ctx context.Context, input RecheckInput) (RecheckResult, error) {
					id := input.Findings[0].ID
					started <- id
					if id <= 2 {
						release := releaseRunning
						if id == 1 {
							release = releaseFailure
						}
						select {
						case <-release:
						case <-ctx.Done():
							return RecheckResult{}, ctx.Err()
						}
					}
					result := recheckStillPresent(input.Findings)
					if id == 1 {
						if !invalidOutput {
							return RecheckResult{}, errors.New("temporary model failure")
						}
						result.Output.Findings[0].ID = 4 // Not in this batch.
					}
					return result, nil
				}
				output := &recheckFailureWriter{failed: make(chan struct{}, 1)}
				_, wait := startParallelRecheckTest(t, repository, store, recheckOptions{
					Model: "check-model", ReasoningEffort: "high", BatchSize: 20, Jobs: 2,
					ContinueOnError: continueOnError, Output: output, NewReviewer: parallelRecheckFactory(check),
				})
				requireInitialRecheckJobs(t, started)
				close(releaseFailure)
				select {
				case <-output.failed:
				case <-time.After(5 * time.Second):
					t.Fatal("failed batch was not reported")
				}
				if continueOnError {
					for _, want := range []int64{3, 4} {
						if id := receiveRecheckStart(t, started); id != want {
							t.Fatalf("next batch = %d, want %d", id, want)
						}
					}
				}
				close(releaseRunning)
				if err := wait(); err == nil {
					t.Fatal("failed run returned success")
				}
				select {
				case id := <-started:
					t.Fatalf("unexpected batch started after failure: %d", id)
				default:
				}
				prior, err := store.PreviouslyRecheckedFindingIDs(context.Background(), head,
					codexReviewerName, "check-model", "high", recheckPromptVersion)
				wantCompleted := 1
				wantRetry := []int64{1, 3, 4}
				if continueOnError {
					wantCompleted = 3
					wantRetry = []int64{1}
				}
				if err != nil || len(prior) != wantCompleted {
					t.Fatalf("completed rechecks = %v, %v", prior, err)
				}
				if _, failedRecorded := prior[1]; failedRecorded {
					t.Fatal("failed batch was recorded as complete")
				}
				if _, runningSaved := prior[2]; !runningSaved {
					t.Fatal("running batch's successful result was lost")
				}
				var retried []int64
				if err := recheckRepository(context.Background(), repository, store, recheckOptions{
					Model: "check-model", ReasoningEffort: "high", BatchSize: 20, Output: io.Discard,
					NewReviewer: parallelRecheckFactory(func(_ context.Context, input RecheckInput) (RecheckResult, error) {
						for _, finding := range input.Findings {
							retried = append(retried, finding.ID)
						}
						return recheckStillPresent(input.Findings), nil
					}),
				}); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(retried, wantRetry) {
					t.Fatalf("retry checked %v, want %v", retried, wantRetry)
				}
			})
		}
	}
}

func TestParallelRecheckAbortWaitsForWorkers(t *testing.T) {
	for _, databaseError := range []bool{false, true} {
		t.Run(fmt.Sprintf("database-error=%t", databaseError), func(t *testing.T) {
			repository, store, head := newParallelRecheckFixture(t, 4)
			if databaseError {
				if _, err := store.db.Exec(`CREATE TRIGGER fail_recheck BEFORE INSERT ON recheck_attempts
					BEGIN SELECT RAISE(ABORT, 'test database failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			started := make(chan int64, 4)
			releaseSave := make(chan struct{})
			var exited atomic.Int32
			cancel, wait := startParallelRecheckTest(t, repository, store, recheckOptions{
				Model: "check-model", ReasoningEffort: "high", BatchSize: 20, Jobs: 2,
				ContinueOnError: true, RetryOnError: true, RetryLimit: 3, Output: io.Discard,
				NewReviewer: parallelRecheckFactory(func(ctx context.Context, input RecheckInput) (RecheckResult, error) {
					defer exited.Add(1)
					started <- input.Findings[0].ID
					if databaseError && input.Findings[0].ID == 2 {
						select {
						case <-releaseSave:
							return recheckStillPresent(input.Findings), nil
						case <-ctx.Done():
						}
					}
					<-ctx.Done()
					return RecheckResult{}, ctx.Err()
				}),
			})
			requireInitialRecheckJobs(t, started)
			if databaseError {
				close(releaseSave)
			} else {
				cancel()
			}
			err := wait()
			if databaseError {
				if err == nil || !strings.Contains(err.Error(), "test database failure") {
					t.Fatalf("database failure error = %v", err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled recheck error = %v", err)
			}
			if exited.Load() != 2 {
				t.Fatalf("returned before workers exited: %d", exited.Load())
			}
			select {
			case id := <-started:
				t.Fatalf("started batch %d after abort", id)
			default:
			}
			prior, err := store.PreviouslyRecheckedFindingIDs(context.Background(), head,
				codexReviewerName, "check-model", "high", recheckPromptVersion)
			if err != nil || len(prior) != 0 {
				t.Fatalf("aborted run wrote results: %v, %v", prior, err)
			}
			lock, err := acquireScanLock(repository.LockPath())
			if err != nil {
				t.Fatalf("aborted recheck retained its lock: %v", err)
			}
			_ = lock.Close()
		})
	}
}
