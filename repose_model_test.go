package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestReposeModelRegistryDrivesCostAndDoctor(t *testing.T) {
	repository, store, scan, inputs := auditFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: repository.WorkTree, Stdout: &stdout, Stderr: io.Discard, Now: func() time.Time { return now }}

	if err := runReposeCLI(ctx, []string{"model", "list", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var listed []reposeModelInfo
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	observed, found := findReposeModelInfo(listed, "test-model")
	if !found || observed.Origin != "observed" || observed.PricingStatus != "unknown" || observed.ObservedScans != 1 {
		t.Fatalf("observed model = %+v, found=%t", observed, found)
	}
	if builtin, ok := findReposeModelInfo(listed, "gpt-5.6-luna"); !ok || builtin.Origin != "built-in" || builtin.Pricing == nil {
		t.Fatalf("built-in model = %+v, found=%t", builtin, ok)
	}

	stdout.Reset()
	setPricing := []string{
		"model", "set-pricing", "test-model", "--source", "fixture", "--as-of", "2026-09-01",
		"--short-input", "1", "--short-cached-input", "0.5", "--short-cache-write", "1", "--short-output", "4",
		"--long-input", "2", "--long-cached-input", "1", "--long-cache-write", "2", "--long-output", "8", "--json",
	}
	if err := runReposeCLI(ctx, setPricing, environment); err != nil {
		t.Fatal(err)
	}
	var configured reposeModelInfo
	if err := json.Unmarshal(stdout.Bytes(), &configured); err != nil {
		t.Fatal(err)
	}
	if configured.Origin != "configured" || configured.PricingStatus != "known" || configured.ObservedScans != 1 ||
		configured.Pricing == nil || configured.Pricing.ShortContext.InputNanousdPerToken != 1000 {
		t.Fatalf("configured model = %+v", configured)
	}

	if err := runAudit(ctx, store, repository, scan, auditRunOptions{Jobs: 1}, auditTestRunner(inputs), io.Discard); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"cost", "--scan", scan.ID, "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var cost reposeStatsReport
	if err := json.Unmarshal(stdout.Bytes(), &cost); err != nil {
		t.Fatal(err)
	}
	if cost.Cost.EstimatedCostAttempts != len(inputs) || cost.Cost.UnknownCostAttempts != 0 || !cost.Cost.Complete {
		t.Fatalf("configured cost = %+v", cost.Cost)
	}

	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"doctor", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var doctor doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &doctor); err != nil {
		t.Fatal(err)
	}
	if check, ok := reposeDoctorCheck(doctor, "model pricing"); !ok || check.Status != "pass" {
		t.Fatalf("doctor pricing = %+v, found=%t", check, ok)
	}

	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"model", "mark-pricing-unknown", "test-model", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	configured = reposeModelInfo{}
	if err := json.Unmarshal(stdout.Bytes(), &configured); err != nil || configured.PricingStatus != "unknown" || configured.Pricing != nil {
		t.Fatalf("unknown model = %+v, err=%v", configured, err)
	}
	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"cost", "--scan", scan.ID, "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &cost); err != nil || cost.Cost.UnpricedUsageAttempts != len(inputs) {
		t.Fatalf("unknown cost = %+v, err=%v", cost.Cost, err)
	}
}

func TestReposeModelShowAndValidation(t *testing.T) {
	repository, _, _, _ := auditFixture(t)
	ctx := context.Background()
	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: repository.WorkTree, Stdout: &stdout, Stderr: io.Discard}
	if err := runReposeCLI(ctx, []string{"model", "show", "test-model"}, environment); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Model: test-model", "Origin: observed", "Observed scans: 1", "Pricing: unknown"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Errorf("model show missing %q:\n%s", expected, stdout.String())
		}
	}
	for _, args := range [][]string{
		{"model", "show", "never-used"},
		{"model", "set-pricing", "test-model", "--short-input", "-1"},
		{"model", "mark-pricing-unknown", " "},
		{"model", "unknown-command"},
	} {
		if err := runReposeCLI(ctx, args, environment); err == nil {
			t.Errorf("accepted invalid command %v", args)
		}
	}
}

func findReposeModelInfo(models []reposeModelInfo, name string) (reposeModelInfo, bool) {
	for _, model := range models {
		if model.Name == name {
			return model, true
		}
	}
	return reposeModelInfo{}, false
}
