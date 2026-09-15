package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCLIHTMLExportIsSelfContainedAndIncludesFindingHistory(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.go", []byte("package app\n"), "base")
	head := testCommitFile(t, directory, "app.go", []byte("package app\n\nfunc unsafe() {\n\tpanic(\"boom\")\n}\n"), "add unsafe function")
	now := time.Date(2026, 8, 29, 16, 30, 0, 0, time.UTC)
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &stdout, Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" }, Now: func() time.Time { return now },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	line := 4
	result := cleanReview("Found two issues.")
	result.Output.NewFindings = []NewFinding{
		{
			Severity: "error", Title: `unsafe </script><script>alert("x")</script>`,
			Description: "A panic escapes to the caller.", File: stringPointer("app.go"), Line: &line,
		},
		{
			Severity: "info", Title: "accepted diagnostic", Description: "This is intentional.",
			File: stringPointer("app.go"), Line: &line,
		},
	}
	ids, err := store.ApplyReview(ctx, CommitMetadata{SHA: head, ParentSHA: base},
		ReviewIdentity{Model: modelByName("gpt-5.6-luna"), ReasoningEffort: "xhigh"}, result, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DismissFinding(ctx, ids[1], "Known development diagnostic.", now); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"export", "--format", "html"}, environment); err != nil {
		t.Fatalf("HTML export: %v", err)
	}
	output := stdout.String()
	for _, expected := range []string{
		"<!doctype html>", "AIR Findings Report", `id="search"`, `aria-label="Text search"`,
		`id="status"`, `id="severity"`, `id="tag-filter"`, `id="tag-options"`, `id="sort"`,
		`value="age"`, `value="author"`, `value="title"`, "function matchesSearch",
		"function matchesTags", "Introducing diff", "</html>",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("HTML export does not contain %q", expected)
		}
	}
	if strings.Contains(output, `</script><script>alert("x")</script>`) {
		t.Fatal("HTML export contains an unescaped script-closing finding title")
	}
	if strings.Contains(output, "<script src=") || strings.Contains(output, "<link rel=") {
		t.Fatal("HTML export references an external script or stylesheet")
	}

	const dataStart = `<script id="air-data" type="application/json">`
	start := strings.Index(output, dataStart)
	if start < 0 {
		t.Fatal("HTML export does not contain its data element")
	}
	start += len(dataStart)
	end := strings.Index(output[start:], "</script>")
	if end < 0 {
		t.Fatal("HTML export data element is not closed")
	}
	var report htmlExportReport
	if err := json.Unmarshal([]byte(output[start:start+end]), &report); err != nil {
		t.Fatalf("decode embedded report: %v", err)
	}
	if report.Version != 1 || report.Repository == "" || report.GeneratedAt != now.Format(time.RFC3339) {
		t.Fatalf("report metadata = %+v", report)
	}
	if len(report.Findings) != 2 {
		t.Fatalf("findings = %+v, want open and dismissed findings", report.Findings)
	}
	findingsByID := make(map[int64]htmlExportFinding, len(report.Findings))
	for _, finding := range report.Findings {
		findingsByID[finding.ID] = finding
	}
	open := findingsByID[ids[0]]
	if open.Disposition != "open" || open.Review == nil ||
		open.Review.Model != "gpt-5.6-luna" || open.Review.ReasoningEffort != "xhigh" ||
		open.Author != "AIR Test" || open.CommitDate == "" ||
		len(open.Events) == 0 || len(open.Diff.Lines) == 0 || open.Diff.HunkHeader == "" {
		t.Fatalf("open exported finding = %+v", open)
	}
	dismissed := findingsByID[ids[1]]
	if dismissed.Disposition != "dismissed" ||
		dismissed.DismissReason != "Known development diagnostic." || len(dismissed.Events) != 2 {
		t.Fatalf("dismissed exported finding = %+v", dismissed)
	}
}
