package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func auditLimitResponse(kind string) (auditInvocation, error) {
	code := "usage_limit_reached"
	if kind == auditRateLimited {
		code = "rate_limit_exceeded"
	}
	return auditInvocation{RawResponse: `{"type":"turn.failed","error":{"code":"` + code + `","message":"provider capacity unavailable"}}`}, errors.New("Codex failed: exit status 1")
}

func TestAuditProviderLimitClassification(t *testing.T) {
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, raw, stderr, kind string
		delay                   time.Duration
	}{
		{"quota code wins over HTTP 429", `{"type":"turn.failed","error":{"code":"insufficient_quota","httpStatusCode":429}}`, "", auditQuotaExhausted, 0},
		{"Codex enum", `{"type":"turn.failed","error":{"codexErrorInfo":"UsageLimitExceeded"}}`, "", auditQuotaExhausted, 0},
		{"CLI usage message", `{"type":"turn.failed","error":{"message":"You've hit your usage limit. Try again later."}}`, "", auditQuotaExhausted, 0},
		{"startup stderr", "", "ERROR: You've hit your usage limit.", auditQuotaExhausted, 0},
		{"throttle seconds", `{"type":"turn.failed","error":{"code":"rate_limit_exceeded","retry_after_seconds":45}}`, "", auditRateLimited, 45 * time.Second},
		{"throttle milliseconds", `{"type":"error","error":{"codexErrorInfo":{"HttpConnectionFailed":{"httpStatusCode":429}},"retry_after_ms":1500}}`, "", auditRateLimited, 1500 * time.Millisecond},
		{"throttle message", `{"type":"turn.failed","error":{"message":"Rate limit reached. Please try again in 2 minutes."}}`, "", auditRateLimited, 2 * time.Minute},
		{"HTTP retry date", `{"type":"turn.failed","error":{"code":"rate_limit_exceeded","retry_after":"Fri, 11 Sep 2026 18:03:00 GMT"}}`, "", auditRateLimited, 3 * time.Minute},
		{"tool output is data", `{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"You've hit your usage limit; rate_limit_exceeded"}}`, "", "", 0},
		{"model text is data", `{"type":"item.completed","item":{"type":"agent_message","text":"Rate limit reached"}}`, "", "", 0},
		{"recovered error", "{\"type\":\"error\",\"message\":\"rate limit reached\"}\n{\"type\":\"turn.completed\"}", "rate limit reached", "", 0},
		{"last error wins", "{\"type\":\"error\",\"message\":\"rate limit reached\"}\n{\"type\":\"turn.failed\",\"error\":{\"message\":\"invalid configuration\"}}", "", "", 0},
		{"unrelated failure", `{"type":"turn.failed","error":{"message":"network connection failed","httpStatusCode":503}}`, "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAuditProviderLimit("codex", auditInvocation{RawResponse: tc.raw, Stderr: tc.stderr}, now)
			if tc.kind == "" {
				if got != nil {
					t.Fatalf("misclassified data or unrelated error: %+v", got)
				}
				return
			}
			if got == nil || got.Kind != tc.kind {
				t.Fatalf("got %+v; want %s", got, tc.kind)
			}
			if tc.delay > 0 && (got.RetryAt == nil || !got.RetryAt.Equal(now.Add(tc.delay))) {
				t.Fatalf("retry time: %+v; want %s", got.RetryAt, now.Add(tc.delay))
			}
		})
	}
}

func TestAuditThrottleRoundsAndServerDelay(t *testing.T) {
	now := time.Now()
	throttle := auditThrottle{}
	for round, seconds := range []int{30, 60, 120, 240} {
		limit := &auditProviderLimit{Kind: auditRateLimited}
		if allowed := throttle.limited(limit, now); allowed != (round < 3) {
			t.Fatalf("round %d retry allowed: %v", round+1, allowed)
		}
		if !throttle.until.Equal(now.Add(time.Duration(seconds) * time.Second)) {
			t.Fatalf("round %d wrong cooldown: %s", round+1, throttle.until)
		}
		// A second failure from the same wave must not consume another retry.
		throttle.limited(&auditProviderLimit{Kind: auditRateLimited}, now)
		if throttle.rounds != round+1 {
			t.Fatal("concurrent failure counted as another retry round")
		}
		now = throttle.until
		throttle.probe = int64(round + 1)
	}
	serverTime := now.Add(10 * time.Minute)
	limit := &auditProviderLimit{Kind: auditRateLimited, RetryAt: &serverTime}
	throttle = auditThrottle{}
	throttle.limited(limit, now)
	if !throttle.until.Equal(serverTime) || !limit.RetryAt.Equal(serverTime) {
		t.Fatal("server delay was shortened")
	}
}

func waitAuditLimit(t *testing.T, store *inventoryStore, scanID, kind string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		limits, err := store.auditPendingLimits(context.Background(), scanID)
		if err != nil {
			t.Fatal(err)
		}
		for _, limit := range limits {
			if limit.Kind == kind {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("provider limit was not persisted")
}

func TestAuditQuotaPausesAndDrainsThenResumes(t *testing.T) {
	repo, store, scan, inputs := auditFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, quota, healthy := make(chan struct{}, 3), make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	runner := func(ctx context.Context, c auditModelConfig, prompt string) (auditInvocation, error) {
		entered <- struct{}{}
		gate := healthy
		if prompt == inputs[0].Prompt {
			gate = quota
		}
		select {
		case <-ctx.Done():
			return auditInvocation{}, ctx.Err()
		case <-gate:
		}
		if prompt == inputs[0].Prompt {
			return auditLimitResponse(auditQuotaExhausted)
		}
		return auditTestRunner(inputs)(ctx, c, prompt)
	}
	go func() { finished <- runAudit(ctx, store, repo, scan, auditRunOptions{Jobs: 2}, runner, io.Discard) }()
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("workers did not start")
		}
	}
	close(quota)
	waitAuditLimit(t, store, scan.ID, auditQuotaExhausted)
	close(healthy)
	if err := <-finished; err == nil || !strings.Contains(err.Error(), auditQuotaExhausted) {
		t.Fatalf("missing quota pause: %v", err)
	}
	updated, err := store.audit(ctx, scan.ID)
	if err != nil || updated.Status != "paused" || updated.Counts["pending"] != 2 || updated.Counts["completed"] != 1 || updated.Counts["failed"] != 0 || updated.Blocked == nil {
		t.Fatalf("quota lost pending work or healthy result: %+v %v", updated, err)
	}
	attempts, _ := store.auditAttempts(ctx, scan.ID)
	if len(attempts) != 2 || len(entered) != 0 || attempts[0].Status != auditQuotaExhausted || attempts[0].Invocation.RawResponse == "" {
		t.Fatalf("extra dispatch or lost quota evidence: %+v", attempts)
	}
	var status bytes.Buffer
	printAuditStatus(&status, updated)
	if !strings.Contains(status.String(), "Provider limit: quota_exhausted") {
		t.Fatal("quota pause is invisible in scan show")
	}
	// An explicit resume retries pending quota work without --retry-failed.
	if err := runAudit(ctx, store, repo, updated, auditRunOptions{Jobs: 2}, auditTestRunner(inputs), io.Discard); err != nil {
		t.Fatal(err)
	}
	updated, _ = store.audit(ctx, scan.ID)
	attempts, _ = store.auditAttempts(ctx, scan.ID)
	if updated.Status != "completed" || updated.Blocked != nil || len(attempts) != 4 {
		t.Fatalf("quota resume failed or repeated healthy work: %+v; %d attempts", updated, len(attempts))
	}
}

func TestAudit32Workers32Attempts(t *testing.T) {
	for _, kind := range []string{"completed", auditRateLimited, auditQuotaExhausted} {
		t.Run(kind, func(t *testing.T) { testAudit32WorkerBatch(t, kind) })
	}
}

func testAudit32WorkerBatch(t *testing.T, kind string) {
	t.Helper()
	repo, store, scan, inputs := auditFixture(t, 61) // 64 actual file assignments
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered, release, finished := make(chan struct{}, 64), make(chan struct{}), make(chan error, 1)
	var calls atomic.Int32
	runner := func(ctx context.Context, c auditModelConfig, prompt string) (auditInvocation, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			if kind != "completed" {
				return auditLimitResponse(kind)
			}
			return auditTestRunner(inputs)(ctx, c, prompt)
		case <-ctx.Done():
			return auditInvocation{}, ctx.Err()
		}
	}
	var stdout, stderr bytes.Buffer
	env := cliEnvironment{Cwd: repo.WorkTree, Stdout: &stdout, Stderr: &stderr, AuditRunner: runner}
	go func() {
		finished <- runReposeCLI(ctx, []string{"scan", "run", scan.ID, "--jobs", "32", "--limit", "32", "--json"}, env)
	}()
	for range 32 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("32 workers did not run concurrently")
		}
	}
	close(release)
	if err := <-finished; (err == nil) != (kind == "completed") {
		t.Fatalf("unexpected batch result for %s: %v", kind, err)
	}
	var updated auditScan
	if err := json.Unmarshal(stdout.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	attempts, _ := store.auditAttempts(ctx, scan.ID)
	completed, pending := 32, 32
	if kind != "completed" {
		completed, pending = 0, 64
		if updated.Blocked == nil || updated.Blocked.Kind != kind {
			t.Fatalf("provider pause missing from CLI JSON: %+v", updated.Blocked)
		}
	}
	if calls.Load() != 32 || len(attempts) != 32 || updated.Counts["completed"] != completed || updated.Counts["pending"] != pending || updated.Counts["running"] != 0 || updated.Counts["failed"] != 0 {
		t.Fatalf("batch exceeded its global cap: calls=%d attempts=%d counts=%v", calls.Load(), len(attempts), updated.Counts)
	}
	for _, a := range attempts {
		if a.Number != 1 || a.Status != kind {
			t.Fatal("batch repeated an assignment or recorded an incorrect outcome")
		}
	}
}

func TestAuditCodexQuotaExitPausesCoordinator(t *testing.T) {
	repo, store, scan, _ := auditFixture(t)
	invocation, _ := auditLimitResponse(auditQuotaExhausted)
	runner := newAuditRunner(repo, cliEnvironment{CodexCommand: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; printf '%s\n' "$1"; exit 1`, "fixture", invocation.RawResponse)
	}})
	ctx := context.Background()
	err := runAudit(ctx, store, repo, scan, auditRunOptions{Jobs: 1}, runner, io.Discard)
	if err == nil || !strings.Contains(err.Error(), auditQuotaExhausted) {
		t.Fatalf("CLI exit did not pause coordinator: %v", err)
	}
	attempts, _ := store.auditAttempts(ctx, scan.ID)
	if len(attempts) != 1 || attempts[0].Status != auditQuotaExhausted || !strings.Contains(attempts[0].Error, "provider capacity unavailable") {
		t.Fatalf("lost process failure diagnostics: %+v", attempts)
	}
}

func TestAuditThrottleUsesSingleProbe(t *testing.T) {
	repo, store, scan, inputs := auditFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, wave, probe, finished := make(chan int32, 8), make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var calls atomic.Int32
	runner := func(ctx context.Context, c auditModelConfig, prompt string) (auditInvocation, error) {
		n := calls.Add(1)
		entered <- n
		var gate <-chan struct{}
		if n <= 2 {
			gate = wave
		} else if n == 3 {
			gate = probe
		}
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return auditInvocation{}, ctx.Err()
			}
		}
		if n <= 2 {
			return auditLimitResponse(auditRateLimited)
		}
		return auditTestRunner(inputs)(ctx, c, prompt)
	}
	go func() {
		finished <- runAudit(ctx, store, repo, scan, auditRunOptions{Jobs: 2, throttleDelay: time.Millisecond}, runner, io.Discard)
	}()
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("initial wave did not start")
		}
	}
	close(wave)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("probe did not start")
	}
	select {
	case <-entered:
		t.Fatal("more than one worker started during the provider probe")
	case <-time.After(50 * time.Millisecond):
	}
	close(probe)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	updated, _ := store.audit(ctx, scan.ID)
	attempts, _ := store.auditAttempts(ctx, scan.ID)
	if calls.Load() != 5 || len(attempts) != 5 || updated.Status != "completed" {
		t.Fatalf("throttled assignments were not recovered: %+v; %d calls", updated, calls.Load())
	}
	for _, a := range attempts[:2] {
		if a.Status != auditRateLimited || a.Invocation.ProviderLimit.RetryAt == nil {
			t.Fatal("throttle/cooldown evidence missing")
		}
	}
}

func TestAuditThrottleRetriesAreBounded(t *testing.T) {
	for _, limit := range []int{0, 2} {
		repo, store, scan, _ := auditFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runner := func(context.Context, auditModelConfig, string) (auditInvocation, error) {
			return auditLimitResponse(auditRateLimited)
		}
		err := runAudit(ctx, store, repo, scan, auditRunOptions{Jobs: 1, Limit: limit, throttleDelay: time.Millisecond}, runner, io.Discard)
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("throttle did not pause automatically: %v", err)
		}
		attempts, _ := store.auditAttempts(ctx, scan.ID)
		want := 4 // initial attempt, then three cooldown/probe retries
		if limit > 0 {
			want = limit
		}
		updated, _ := store.audit(ctx, scan.ID)
		if len(attempts) != want || updated.Status != "paused" || updated.Counts["pending"] != 3 || updated.Counts["failed"] != 0 || updated.Blocked == nil {
			t.Fatalf("throttle exceeded retry or attempt budget: %d attempts, %+v", len(attempts), updated)
		}
	}
}

func TestAuditThrottleRetriesAfterAllPendingTasksWereDispatched(t *testing.T) {
	repo, store, scan, inputs := auditFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release, finished := make(chan struct{}, 3), make(chan struct{}), make(chan error, 1)
	var calls atomic.Int32
	runner := func(ctx context.Context, c auditModelConfig, prompt string) (auditInvocation, error) {
		if calls.Add(1) <= 3 {
			entered <- struct{}{}
			select {
			case <-release:
				return auditLimitResponse(auditRateLimited)
			case <-ctx.Done():
				return auditInvocation{}, ctx.Err()
			}
		}
		return auditTestRunner(inputs)(ctx, c, prompt)
	}
	go func() {
		finished <- runAudit(ctx, store, repo, scan, auditRunOptions{Jobs: 4, throttleDelay: time.Millisecond}, runner, io.Discard)
	}()
	for range 3 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("assignments did not start")
		}
	}
	// Let the coordinator observe an empty pending queue while all tasks run.
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	updated, err := store.audit(ctx, scan.ID)
	if err != nil || calls.Load() != 6 || updated.Status != "completed" || updated.Counts["completed"] != 3 {
		t.Fatalf("queue exhaustion prevented throttle recovery: calls=%d scan=%+v err=%v", calls.Load(), updated, err)
	}
}

func TestAuditSavedCooldownSurvivesResumeAndCanBeStopped(t *testing.T) {
	for _, stop := range []string{"cancel", "pause", "duration"} {
		t.Run(stop, func(t *testing.T) {
			repo, store, scan, _ := auditFixture(t)
			ctx := context.Background()
			task, err := store.claimAuditTask(ctx, scan.ID, scan.Spec.Model.Timeout, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			retryAt := time.Now().Add(time.Hour)
			invocation := auditInvocation{ProviderLimit: &auditProviderLimit{Kind: auditRateLimited, Message: "provider retry window", RetryAt: &retryAt}}
			if err = store.finishAuditTask(ctx, scan, *task, auditRateLimited, invocation, nil, errors.New("throttled"), time.Now()); err != nil {
				t.Fatal(err)
			}
			runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			options := auditRunOptions{Jobs: 32, Limit: 32}
			if stop == "duration" {
				options.Duration = 20 * time.Millisecond
			}
			var calls atomic.Int32
			runner := func(context.Context, auditModelConfig, string) (auditInvocation, error) {
				calls.Add(1)
				return auditLimitResponse(auditRateLimited)
			}
			done := make(chan error, 1)
			go func() { done <- runAudit(runCtx, store, repo, scan, options, runner, io.Discard) }()
			// Wait for recovery to clear old controls before requesting a pause.
			deadline := time.Now().Add(3 * time.Second)
			for {
				current, err := store.audit(ctx, scan.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.Status == "running" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("coordinator did not start")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if stop == "cancel" {
				cancel()
			} else if stop == "pause" {
				if _, err := store.db.Exec("UPDATE audit_scans SET control='pause' WHERE id=?", scan.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err = <-done; err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("coordinator did not stop during cooldown: %v", err)
			}
			attempts, _ := store.auditAttempts(ctx, scan.ID)
			if calls.Load() != 0 || len(attempts) != 1 {
				t.Fatal("resume bypassed the persisted provider cooldown")
			}
		})
	}
}
