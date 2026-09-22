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
	for _, want := range []string{"1 of 2 open findings", "VERIFICATION", "LINE AGE", "AUTHOR", "AIR Test", "LOCATION", "Fixture finding"} {
		if !strings.Contains(output, want) {
			t.Errorf("text list missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "not actionable") {
		t.Fatalf("default list included dismissed finding:\n%s", output)
	}

	stdout.Reset()
	if err := runReposeCLI(ctx, []string{
		"finding", "list", "--scan", scan.ID, "--all", "--search", "  NOT ACTIONABLE  ", "--json",
	}, environment); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode searched list: %v\n%s", err, stdout.String())
	}
	if report.Search != "NOT ACTIONABLE" || report.Total != 1 || len(report.Findings) != 1 ||
		report.Findings[0].ID != findings[1].ID {
		t.Fatalf("finding search report = %+v", report)
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
		{[]string{"--sort", "bogus"}, "invalid --sort"},
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

func TestAuditFindingBulkDismissIsAtomic(t *testing.T) {
	repository, _, scan, backend, findings := auditTagFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC)
	first, second, remaining := findings[0].ID, findings[1].ID, findings[2].ID
	if _, err := backend.DismissFindings(ctx, []int64{first, 9999999}, "must be atomic", false, now); err == nil {
		t.Fatal("bulk dismissal accepted a missing finding")
	}
	loaded, err := backend.AllFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range loaded {
		if finding.DismissedAt != nil {
			t.Fatalf("failed bulk dismissal changed finding #%d", finding.ID)
		}
	}

	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: repository.WorkTree, Stdout: &stdout, Stderr: io.Discard, Now: func() time.Time { return now }}
	args := []string{"finding", "dismiss", strconv.FormatInt(first, 10), strconv.FormatInt(second, 10),
		"--scan", scan.ID, "--reason", "Addressed together in fix #4"}
	if err := runReposeCLI(ctx, args, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Dismissed 2 findings") {
		t.Fatalf("bulk dismissal output = %q", stdout.String())
	}
	loaded, err = backend.AllFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[int64]Finding, len(loaded))
	for _, finding := range loaded {
		byID[finding.ID] = finding
	}
	if byID[first].DismissedAt == nil || byID[second].DismissedAt == nil || byID[remaining].DismissedAt != nil ||
		byID[first].DismissReason != "Addressed together in fix #4" || byID[second].DismissReason != "Addressed together in fix #4" {
		t.Fatalf("bulk dismissal state = %+v", loaded)
	}
	for _, id := range []int64{first, second} {
		events, err := backend.FindingEvents(ctx, id)
		if err != nil || len(events) == 0 || events[len(events)-1].Action != "dismissed" || events[len(events)-1].Note != "Addressed together in fix #4" {
			t.Fatalf("finding #%d events = %+v, err=%v", id, events, err)
		}
	}
	if _, err := backend.DismissFindings(ctx, []int64{remaining, first}, "must roll back", false, now.Add(time.Minute)); err == nil {
		t.Fatal("bulk dismissal accepted an already dismissed finding")
	}
	loaded, err = backend.AllFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range loaded {
		if finding.ID == remaining && finding.DismissedAt != nil {
			t.Fatal("validation failure partially dismissed the remaining finding")
		}
	}

	firstEvents, err := backend.FindingEvents(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	args = []string{"finding", "dismiss", strconv.FormatInt(remaining, 10), strconv.FormatInt(first, 10),
		"--scan", scan.ID, "--reason", "Only close open findings", "--ignore-closed"}
	if err := runReposeCLI(ctx, args, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Dismissed 1 finding; skipped 1 already dismissed") {
		t.Fatalf("ignore-closed output = %q", stdout.String())
	}
	loaded, err = backend.AllFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID = make(map[int64]Finding, len(loaded))
	for _, finding := range loaded {
		byID[finding.ID] = finding
	}
	if byID[remaining].DismissedAt == nil || byID[remaining].DismissReason != "Only close open findings" ||
		byID[first].DismissReason != "Addressed together in fix #4" {
		t.Fatalf("ignore-closed state = %+v", loaded)
	}
	afterFirstEvents, err := backend.FindingEvents(ctx, first)
	if err != nil || len(afterFirstEvents) != len(firstEvents) {
		t.Fatalf("ignored finding received an event: before=%+v after=%+v err=%v", firstEvents, afterFirstEvents, err)
	}
	remainingEvents, err := backend.FindingEvents(ctx, remaining)
	if err != nil || len(remainingEvents) == 0 || remainingEvents[len(remainingEvents)-1].Note != "Only close open findings" {
		t.Fatalf("dismissed finding events = %+v, err=%v", remainingEvents, err)
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
