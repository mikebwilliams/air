package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAuditFindingListFiltersAndReportsScope(t *testing.T) {
	repository, _, scan, backend, findings := auditTagFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	if _, err := backend.TagFindings(ctx, []int64{findings[0].ID, findings[1].ID}, []string{"triage:high-value"}, now); err != nil {
		t.Fatal(err)
	}
	if err := backend.DismissFinding(ctx, findings[1].ID, "not actionable", now); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: repository.WorkTree, Stdout: &stdout, Stderr: io.Discard, Now: func() time.Time { return now }}
	args := []string{
		"finding", "list", "--scan", "latest", "--all", "--tag", "triage:high-value",
		"--path", path.Dir(*findings[0].File), "--sort", "status", "--json",
	}
	if err := runReposeCLI(ctx, args, environment); err != nil {
		t.Fatal(err)
	}
	var report auditFindingListOutput
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode list: %v\n%s", err, stdout.String())
	}
	if report.Version != 1 || report.ScanID != scan.ID || report.Total != 2 || len(report.Findings) != 2 ||
		report.Findings[0].Status != "open" || report.Findings[1].Status != "dismissed" ||
		report.Findings[0].Verification != "unchecked" || report.Findings[0].Tags[0] != "triage:high-value" {
		t.Fatalf("finding list report = %+v", report)
	}

	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"finding", "list", "--scan", scan.ID, "--limit", "1"}, environment); err != nil {
		t.Fatal(err)
	}
	output := stdout.String()
	for _, want := range []string{"1 of 2 open findings", "VERIFICATION", "SCAN", "LOCATION", "Fixture finding"} {
		if !strings.Contains(output, want) {
			t.Errorf("text list missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "not actionable") {
		t.Fatalf("default list included dismissed finding:\n%s", output)
	}
}

func TestAuditFindingListRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--all", "--status", "all"}, "cannot be used together"},
		{[]string{"--status", "resolved"}, "invalid --status"},
		{[]string{"--verification", "maybe"}, "verification must be"},
		{[]string{"--sort", "author"}, "invalid --sort"},
		{[]string{"--limit", "-1"}, "must not be negative"},
		{[]string{"--path", "../outside"}, "invalid --path"},
		{[]string{"unexpected"}, "usage: repose finding list"},
	}
	for _, test := range tests {
		err := runAuditFindingListCLI(context.Background(), test.args, cliEnvironment{
			Cwd: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard,
		})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("runAuditFindingListCLI(%q) error = %v, want %q", test.args, err, test.want)
		}
	}
}

func TestAuditFindingDetailAliasAndJSON(t *testing.T) {
	repository, _, scan, backend, findings := auditTagFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	if _, err := backend.TagFindings(ctx, []int64{findings[0].ID}, []string{"ownership"}, now); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: repository.WorkTree, Stdout: &stdout, Stderr: io.Discard}
	id := formatFindingID(findings[0].ID)
	if err := runReposeCLI(ctx, []string{"finding", id}, environment); err != nil {
		t.Fatal(err)
	}
	output := stdout.String()
	for _, want := range []string{
		"#" + id + " WARNING  open", "Fixture finding", "Observed: " + findings[0].ObservedSHA,
		"Scan: " + scan.ID, "Model: codex/test-model/high", "Tags: ownership", "Verification:\n    unchecked", "History:", "observed",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("finding detail missing %q:\n%s", want, output)
		}
	}
	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"finding", "show", id, "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var finding Finding
	if err := json.Unmarshal(stdout.Bytes(), &finding); err != nil || finding.ID != findings[0].ID || finding.ScanID != scan.ID {
		t.Fatalf("finding JSON = %+v, %v\n%s", finding, err, stdout.String())
	}
}

func TestAuditFindingSourceReadsRecordedSnapshot(t *testing.T) {
	repository, _, _, _, findings := auditTagFixture(t)
	ctx := context.Background()
	finding := findings[0]
	filename := repository.WorkTree + "/" + *finding.File
	original, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var before bytes.Buffer
	environment := cliEnvironment{Cwd: repository.WorkTree, Stdout: &before, Stderr: io.Discard}
	args := []string{"finding", "source", formatFindingID(finding.ID), "--context", "0"}
	if err := runReposeCLI(ctx, args, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before.String(), "Finding #"+formatFindingID(finding.ID)) ||
		!strings.Contains(before.String(), "Snapshot "+shortSHA(finding.ObservedSHA)) ||
		!strings.Contains(before.String(), "> ") {
		t.Fatalf("source output:\n%s", before.String())
	}
	if err := os.WriteFile(filename, append(original, []byte("// changed after scan\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	var after bytes.Buffer
	environment.Stdout = &after
	if err := runReposeCLI(ctx, args, environment); err != nil {
		t.Fatal(err)
	}
	if after.String() != before.String() {
		t.Fatalf("working-tree edit changed recorded source output:\nbefore:\n%s\nafter:\n%s", before.String(), after.String())
	}
}

func TestAuditFindingOpenRequiresExactSnapshot(t *testing.T) {
	repository, _, _, _, findings := auditTagFixture(t)
	ctx := context.Background()
	finding := findings[0]
	testGit(t, repository.WorkTree, "config", "core.editor", "vim --nofork")
	t.Setenv("AIR_EXTERNAL_COMMAND_HELPER", "1")
	var calls int
	environment := cliEnvironment{
		Cwd: repository.WorkTree, Stdin: bytes.NewBuffer(nil), Stdout: io.Discard, Stderr: io.Discard,
		ExternalCommand: func(commandContext context.Context, name string, args ...string) *exec.Cmd {
			calls++
			if name != "sh" || len(args) < 4 || !strings.Contains(strings.Join(args, " "), "vim --nofork") {
				t.Fatalf("unexpected editor command: %s %q", name, args)
			}
			return exec.CommandContext(commandContext, os.Args[0], "-test.run=^TestFindingExternalCommandHelper$")
		},
	}
	args := []string{"finding", "open", formatFindingID(finding.ID)}
	if err := runReposeCLI(ctx, args, environment); err != nil {
		t.Fatalf("open matching snapshot: %v", err)
	}
	if calls != 1 {
		t.Fatalf("editor calls = %d, want 1", calls)
	}

	filename := repository.WorkTree + "/" + *finding.File
	original, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, append(original, []byte("// local edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runReposeCLI(ctx, args, environment); err == nil || !strings.Contains(err.Error(), "differs from the recorded snapshot") {
		t.Fatalf("modified source open error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("modified source launched editor: %d calls", calls)
	}
	if err := os.WriteFile(filename, original, 0o644); err != nil {
		t.Fatal(err)
	}
	testCommitFile(t, repository.WorkTree, "after-scan.txt", []byte("later\n"), "move checkout")
	if err := runReposeCLI(ctx, args, environment); err == nil || !strings.Contains(err.Error(), "checkout is at") || !strings.Contains(err.Error(), "finding source") {
		t.Fatalf("moved checkout open error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("moved checkout launched editor: %d calls", calls)
	}
}

func formatFindingID(id int64) string {
	return strconv.FormatInt(id, 10)
}
