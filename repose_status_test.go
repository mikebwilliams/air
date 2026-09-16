package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReposeStatusSummarizesProgressUsageAndTiming(t *testing.T) {
	repository, store, scan, _ := auditFixture(t, 1)
	ctx := context.Background()
	started := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if err := store.recoverAudit(ctx, scan, false, started); err != nil {
		t.Fatal(err)
	}
	completed, err := store.claimAuditTask(ctx, scan.ID, scan.Spec.Model.Timeout, started)
	if err != nil || completed == nil {
		t.Fatalf("claim completed task: %+v, %v", completed, err)
	}
	invocation := auditTestResponse(completed.Input)
	output, err := parseAuditTaskOutput([]byte(invocation.StructuredOutput), completed.Input)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.finishAuditTask(ctx, scan, *completed, "completed", invocation, &output, nil, started.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	failed, err := store.claimAuditTask(ctx, scan.ID, scan.Spec.Model.Timeout, started.Add(10*time.Minute))
	if err != nil || failed == nil {
		t.Fatalf("claim failed task: %+v, %v", failed, err)
	}
	if err := store.finishAuditTask(ctx, scan, *failed, "failed", auditInvocation{}, nil, errors.New("fixture failure"), started.Add(14*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE audit_scans SET status='paused' WHERE id=?", scan.ID); err != nil {
		t.Fatal(err)
	}

	environment := cliEnvironment{Cwd: repository.WorkTree, Now: func() time.Time { return started.Add(14 * time.Minute) }}
	var stdout, stderr bytes.Buffer
	environment.Stdout, environment.Stderr = &stdout, &stderr
	if err := runReposeCLI(ctx, []string{"status", "--jobs", "2", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var report reposeStatusReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Inventory.Approved || report.ScanScope != "all" || report.ScanCounts["paused"] != 1 || len(report.Scans) != 1 {
		t.Fatalf("status identity = %+v", report)
	}
	if report.Tasks.Total != 4 || report.Tasks.Completed != 1 || report.Tasks.Failed != 1 || report.Tasks.Pending != 2 {
		t.Fatalf("task totals = %+v", report.Tasks)
	}
	if report.Findings.Total != 1 || report.Findings.Open != 1 || report.Findings.OpenBySeverity["warning"] != 1 || report.Findings.Verification["unchecked"] != 1 {
		t.Fatalf("finding totals = %+v", report.Findings)
	}
	if report.Usage.Attempts != 2 || report.Usage.ReportedAttempts != 1 || report.Usage.InputTokens != 12 || report.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", report.Usage)
	}
	if report.Timing.WorkerMilliseconds != (14*time.Minute).Milliseconds() || report.Timing.SuccessfulSamples != 1 ||
		report.Timing.AverageSuccessfulMilliseconds == nil || *report.Timing.AverageSuccessfulMilliseconds != (10*time.Minute).Milliseconds() ||
		report.Timing.EstimatedRemainingWorkerMilliseconds == nil || *report.Timing.EstimatedRemainingWorkerMilliseconds != (20*time.Minute).Milliseconds() ||
		report.Timing.EstimatedRemainingWallMilliseconds == nil || *report.Timing.EstimatedRemainingWallMilliseconds != (10*time.Minute).Milliseconds() {
		t.Fatalf("timing = %+v", report.Timing)
	}

	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"status", "--scan", scan.ID[:12], "--jobs", "2"}, environment); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"1 paused", "1 open", "2 pending", "1 failed", "12 input tokens", "14m0s", "20m0s of worker time", "10m0s at 2 jobs"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Errorf("text status missing %q:\n%s", expected, stdout.String())
		}
	}
}

func TestReposeStatusBeforeFirstScan(t *testing.T) {
	_, directory := newInventoryFixture(t)
	record := runReposeRecord(t, directory, "inventory", "build")
	output, err := executeReposeTest(t, directory, "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var report reposeStatusReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	if report.Inventory.ID != record.ID || report.Inventory.Approved || len(report.Scans) != 0 || report.Findings.Total != 0 || report.Usage.Attempts != 0 {
		t.Fatalf("pre-scan status = %+v", report)
	}
	for _, jobs := range []string{"0", "33", "-1"} {
		if _, err := executeReposeTest(t, directory, "status", "--jobs", jobs); err == nil {
			t.Errorf("accepted --jobs %s", jobs)
		}
	}
}

func TestReposeStatusKeepsPartialRemainingEstimate(t *testing.T) {
	knownWorker, knownWall := int64(20_000), int64(10_000)
	scans := []reposeStatusScan{
		{Timing: reposeStatusTiming{RemainingTasks: 2, EstimatedRemainingWorkerMilliseconds: &knownWorker,
			EstimatedRemainingWallMilliseconds: &knownWall, RemainingEstimateComplete: true}},
		{Timing: reposeStatusTiming{RemainingTasks: 3, UnestimatedRemainingTasks: 3}},
	}
	got := aggregateReposeStatusTiming(scans, 2)
	if got.RemainingEstimateComplete || got.RemainingTasks != 5 || got.UnestimatedRemainingTasks != 3 ||
		got.EstimatedRemainingWorkerMilliseconds == nil || *got.EstimatedRemainingWorkerMilliseconds != knownWorker ||
		got.EstimatedRemainingWallMilliseconds == nil || *got.EstimatedRemainingWallMilliseconds != knownWall {
		t.Fatalf("partial estimate = %+v", got)
	}
}
