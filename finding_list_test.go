package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCLIFindingListFiltersSortsAndLimits(t *testing.T) {
	ctx := context.Background()
	_, directory, findingIDs := testRepositoryWithFindingDispositions(t)
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &stdout, Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" },
		Now:    func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) },
	}

	if err := runCLI(ctx, []string{"finding", "list"}, environment); err != nil {
		t.Fatalf("default finding list: %v", err)
	}
	output := stdout.String()
	for _, expected := range []string{
		"2 open findings", "ID", "SEVERITY", "STATUS", "AGE", "BLAME", "LOCATION", "TITLE",
		"#4", "error", "open", "1d", "AIR Test", "pkg/c.go:5", "fourth",
		"#1", "warning", "pkg/b.go:20", "first",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("default output does not contain %q:\n%s", expected, output)
		}
	}
	if strings.Contains(output, "second") || strings.Contains(output, "third") ||
		strings.Index(output, "#4") > strings.Index(output, "#1") {
		t.Fatalf("default finding selection/order:\n%s", output)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"finding", "list", "--all", "--sort", "severity", "--limit", "3"}, environment); err != nil {
		t.Fatalf("limited severity list: %v", err)
	}
	output = stdout.String()
	if !strings.Contains(output, "3 of 4 findings") || strings.Contains(output, "third") {
		t.Fatalf("limited severity output:\n%s", output)
	}
	assertTextOrder(t, output, "#4", "#2", "#1")

	stdout.Reset()
	if err := runCLI(ctx, []string{
		"finding", "list", "--status", "dismissed", "--severity", "error", "--sort", "file",
	}, environment); err != nil {
		t.Fatalf("filtered file list: %v", err)
	}
	output = stdout.String()
	if !strings.Contains(output, "1 dismissed error finding") ||
		!strings.Contains(output, "#2") || !strings.Contains(output, "dismissed") ||
		strings.Contains(output, "#1") || strings.Contains(output, "#3") || strings.Contains(output, "#4") {
		t.Fatalf("filtered file output:\n%s", output)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{
		"finding", "list", "--all", "--sort", "severity", "--limit", "1", "--json",
	}, environment); err != nil {
		t.Fatalf("JSON finding list: %v", err)
	}
	var decoded findingListJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode JSON finding list: %v\n%s", err, stdout.String())
	}
	if decoded.Status != "all" || decoded.Severity != "all" || decoded.Sort != "severity" ||
		decoded.Total != 4 || len(decoded.Findings) != 1 || decoded.Findings[0].ID != findingIDs[3] ||
		decoded.Findings[0].Status != "open" || decoded.Findings[0].Review.Model != "list-model" ||
		decoded.Findings[0].Review.ReasoningEffort != "high" || decoded.Findings[0].Review.Number != 1 ||
		decoded.Findings[0].Blame != "AIR Test" ||
		!decoded.Findings[0].CommitDate.Equal(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("JSON finding list = %+v", decoded)
	}
}

func TestCLIFindingListRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--all", "--status", "all"}, "cannot be used together"},
		{[]string{"--status", "closed"}, "invalid --status"},
		{[]string{"--severity", "critical"}, "invalid --severity"},
		{[]string{"--sort", "title"}, "invalid --sort"},
		{[]string{"--limit", "-1"}, "must not be negative"},
		{[]string{"unexpected"}, "usage: air finding list"},
	}
	for _, test := range tests {
		environment := cliEnvironment{
			Cwd: t.TempDir(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
			Getenv: func(string) string { return "" },
		}
		err := runFindingList(context.Background(), test.args, environment)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("runFindingList(%q) error = %v, want %q", test.args, err, test.want)
		}
	}
}

func TestWriteFindingListKeepsColumnsStable(t *testing.T) {
	file := "a/very/long/repository/path/that/is/not/truncated.go"
	line := 91
	findings := []Finding{
		{ID: 123, Severity: "warning", Title: "wide ID", File: &file, Line: &line},
		{ID: 7, Severity: "info", Title: "narrow ID"},
	}
	commitDate := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	display := map[int64]findingDisplayMetadata{
		123: {CommitDate: commitDate, Blame: "A Very Long Contributor Name"},
		7:   {CommitDate: commitDate.Add(-30 * 24 * time.Hour), Blame: "Sam"},
	}
	items := makeFindingListDisplayItems(findings, display)
	var output bytes.Buffer
	writeFindingList(&output, items, len(findings), "open", "all", commitDate.Add(48*time.Hour))
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("finding list lines = %q", lines)
	}
	severityColumn := strings.Index(lines[3], "warning")
	if severityColumn < 0 || strings.Index(lines[4], "info") != severityColumn {
		t.Fatalf("severity column is not stable:\n%s", output.String())
	}
	if !strings.Contains(lines[3], file+":"+strconv.Itoa(line)) {
		t.Fatalf("long location was truncated:\n%s", output.String())
	}
	if !strings.Contains(lines[3], "2d") || !strings.Contains(lines[3], "A Very Long Contr…") ||
		!strings.Contains(lines[4], "1mo") {
		t.Fatalf("age or blame columns are missing:\n%s", output.String())
	}

	output.Reset()
	writeFindingList(&output, nil, 0, "open", "all", commitDate)
	if output.String() != "No open findings.\n" {
		t.Fatalf("empty finding list = %q", output.String())
	}
}

func testRepositoryWithFindingDispositions(t *testing.T) (*GitRepository, string, []int64) {
	t.Helper()
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "base.txt", []byte("base\n"), "base")
	commit := testCommitFile(t, directory, "change.txt", []byte("change\n"), "find issues")
	testGit(t, directory, "commit", "--amend", "--no-edit", "--date=2026-08-31T12:00:00Z")
	commit = strings.TrimSpace(testGit(t, directory, "rev-parse", "HEAD"))
	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatal(err)
	}
	fileA, fileB, fileC := "pkg/a.go", "pkg/b.go", "pkg/c.go"
	line5, line10, line20 := 5, 10, 20
	metadata, err := repository.CommitMetadata(ctx, commit)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	reviewedAt := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	ids, err := store.ApplyReview(ctx, metadata,
		ReviewIdentity{Model: modelByName("list-model"), ReasoningEffort: "high"},
		ReviewResult{
			Output: ReviewOutput{NewFindings: []NewFinding{
				{Severity: "warning", Title: "first", Description: "first description", File: &fileB, Line: &line20},
				{Severity: "error", Title: "second", Description: "second description", File: &fileA, Line: &line10},
				{Severity: "info", Title: "third", Description: "third description"},
				{Severity: "error", Title: "fourth", Description: "fourth description", File: &fileC, Line: &line5},
			}, Summary: "Found four problems."},
			RawResponse: "{}", Usage: &TokenUsage{InputTokens: 20, OutputTokens: 5},
		}, reviewedAt)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.DismissFinding(ctx, ids[1], "accepted behavior", reviewedAt.Add(time.Minute)); err != nil {
		store.Close()
		t.Fatal(err)
	}
	resolveCommit := testCommitFile(t, directory, "resolve.txt", []byte("resolve\n"), "resolve issue")
	resolveMetadata, err := repository.CommitMetadata(ctx, resolveCommit)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.ApplyReview(ctx, resolveMetadata,
		ReviewIdentity{Model: modelByName("resolver-model"), ReasoningEffort: "low"},
		ReviewResult{
			Output: ReviewOutput{
				ResolvedFindings: []ResolvedFinding{{ID: ids[2], Reason: "fixed"}},
				Summary:          "Resolved one problem.",
			},
			RawResponse: "{}", Usage: &TokenUsage{InputTokens: 10, OutputTokens: 2},
		}, reviewedAt.Add(2*time.Minute)); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return repository, directory, ids
}

func assertTextOrder(t *testing.T, value string, expected ...string) {
	t.Helper()
	last := -1
	for _, item := range expected {
		position := strings.Index(value, item)
		if position < 0 || position <= last {
			t.Fatalf("%q is not after the previous item in:\n%s", item, value)
		}
		last = position
	}
}
