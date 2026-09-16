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

func TestReposeStatsAndCostAccounting(t *testing.T) {
	repository, store, scan, _ := auditFixture(t, 1)
	ctx := context.Background()
	scan.Spec.Model.Model = "gpt-5.6-luna"
	document, err := json.Marshal(scan.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE audit_scans SET document=? WHERE id=?", string(document), scan.ID); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if err := store.recoverAudit(ctx, scan, false, started); err != nil {
		t.Fatal(err)
	}

	completed, err := store.claimAuditTask(ctx, scan.ID, scan.Spec.Model.Timeout, started)
	if err != nil || completed == nil {
		t.Fatalf("claim completed: %+v, %v", completed, err)
	}
	invocation := auditTestResponse(completed.Input)
	cacheWrites := int64(10)
	invocation.Usage = &TokenUsage{InputTokens: 100, CachedInputTokens: 25, CacheWriteTokens: &cacheWrites, OutputTokens: 20, ReasoningOutputTokens: 5}
	output, err := parseAuditTaskOutput([]byte(invocation.StructuredOutput), completed.Input)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.finishAuditTask(ctx, scan, *completed, "completed", invocation, &output, nil, started.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	reported, err := store.claimAuditTask(ctx, scan.ID, scan.Spec.Model.Timeout, started.Add(10*time.Minute))
	if err != nil || reported == nil {
		t.Fatalf("claim reported: %+v, %v", reported, err)
	}
	reportedCost := int64(1234)
	if err := store.finishAuditTask(ctx, scan, *reported, "failed", auditInvocation{ReportedCostMicrousd: &reportedCost}, nil,
		errors.New("reported failure"), started.Add(14*time.Minute)); err != nil {
		t.Fatal(err)
	}

	missing, err := store.claimAuditTask(ctx, scan.ID, scan.Spec.Model.Timeout, started.Add(14*time.Minute))
	if err != nil || missing == nil {
		t.Fatalf("claim missing: %+v, %v", missing, err)
	}
	if err := store.finishAuditTask(ctx, scan, *missing, "failed", auditInvocation{}, nil,
		errors.New("missing accounting"), started.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}

	environment := cliEnvironment{Cwd: repository.WorkTree, Now: func() time.Time { return started.Add(15 * time.Minute) }}
	var stdout, stderr bytes.Buffer
	environment.Stdout, environment.Stderr = &stdout, &stderr
	if err := runReposeCLI(ctx, []string{"stats", "--scan", scan.ID[:12], "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var report reposeStatsReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Scans.Total != 1 || report.Attempts.Total != 3 || report.Attempts.Reviews != 3 ||
		report.Attempts.Outcomes["completed"] != 1 || report.Attempts.Outcomes["failed"] != 2 {
		t.Fatalf("attempt counts = %+v", report)
	}
	if report.Usage.ReportedAttempts != 1 || report.Usage.InputTokens != 100 || report.Usage.CacheWriteTokens != 10 || report.Usage.OutputTokens != 20 {
		t.Fatalf("usage = %+v", report.Usage)
	}
	if report.Timing.WorkerMilliseconds != (15*time.Minute).Milliseconds() || report.Timing.TimedAttempts != 3 ||
		report.Timing.AverageFinishedMilliseconds == nil || *report.Timing.AverageFinishedMilliseconds != (5*time.Minute).Milliseconds() {
		t.Fatalf("timing = %+v", report.Timing)
	}
	if report.Cost.MinimumMicrousd != 1274 || report.Cost.MaximumMicrousd != 1274 || report.Cost.AccountedAttempts != 2 ||
		report.Cost.ReportedCostAttempts != 1 || report.Cost.EstimatedCostAttempts != 1 || report.Cost.UnknownCostAttempts != 1 ||
		report.Cost.NoAccountingDataAttempts != 1 || report.Cost.Complete {
		t.Fatalf("cost = %+v", report.Cost)
	}
	if report.Findings.Total != 1 || report.Findings.ScopeScanID != scan.ID || len(report.Groups) != 1 {
		t.Fatalf("scope or groups = %+v", report)
	}

	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"cost", "--scan", scan.ID, "--since", started.Add(9 * time.Minute).Format(time.RFC3339)}, environment); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"$0.001234 USD", "1 harness-reported", "1 attempt with unknown cost", "neither token usage nor reported cost"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Errorf("cost output missing %q:\n%s", expected, stdout.String())
		}
	}
}

func TestReposeCostDistinguishesUnknownPricing(t *testing.T) {
	usage := &TokenUsage{InputTokens: 100, OutputTokens: 10}
	cost, err := reposeAttemptCost("private-model", usage, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cost.UnknownCostAttempts != 1 || cost.UnpricedUsageAttempts != 1 || cost.NoAccountingDataAttempts != 0 || cost.Complete {
		t.Fatalf("unknown pricing = %+v", cost)
	}
}

func TestReposeStatsRejectsInvalidFilters(t *testing.T) {
	_, directory := newInventoryFixture(t)
	runReposeRecord(t, directory, "inventory", "build")
	for _, args := range [][]string{
		{"stats", "--since", "yesterday"},
		{"stats", "--model", " padded "},
		{"cost", "extra"},
	} {
		if _, err := executeReposeTest(t, directory, args...); err == nil {
			t.Errorf("accepted invalid arguments %v", args)
		}
	}
}
