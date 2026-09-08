package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

type precheckReviewerFunc func(context.Context, ReviewInput) (ReviewResult, error)

func (reviewer precheckReviewerFunc) Review(ctx context.Context, input ReviewInput) (ReviewResult, error) {
	return reviewer(ctx, input)
}

func TestCLIPrecheckReviewsUnpushedCommitsWithoutWritingStore(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.go", []byte("package app\n"), "base")
	testGit(t, directory, "config", "remote.origin.url", ".")
	testGit(t, directory, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	testGit(t, directory, "update-ref", "refs/remotes/origin/master", base)
	testGit(t, directory, "config", "branch.master.remote", "origin")
	testGit(t, directory, "config", "branch.master.merge", "refs/heads/master")
	head := testCommitFile(t, directory, "app.go", []byte("package app\nvar broken = true\n"), "break app")

	command, invocation := newCodexTestCommand(t, ReviewOutput{
		NewFindings: []NewFinding{{
			Severity: "warning", Title: "broken behavior", Description: "The new path fails.",
			File: stringPointer("app.go"),
		}},
		ResolvedFindings: []ResolvedFinding{}, Summary: "Reviewed local commits.",
	}, "")
	var output bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &output, Stderr: io.Discard,
		Getenv: func(string) string { return "" }, CodexCommand: command,
		Now: func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	err := runCLI(ctx, []string{
		"precheck", "--model", "gpt-5.6-luna", "--effort", "low",
		"--format", "json", "--fail-on", "warning",
	}, environment)
	if err == nil || !strings.Contains(err.Error(), "precheck found 1 open finding") {
		t.Fatalf("precheck error = %v", err)
	}
	var report precheckReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode precheck output: %v\n%s", err, output.String())
	}
	if report.Mode != "unpushed" || report.FromSHA != base || report.ToSHA != head ||
		len(report.Attempts) != 1 || len(report.Findings) != 1 || report.Findings[0].Title != "broken behavior" {
		t.Fatalf("precheck report = %+v", report)
	}
	prompt, err := os.ReadFile(invocation.PromptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), head) || strings.Contains(string(prompt), "<precheck_target>") {
		t.Fatalf("unexpected commit precheck prompt:\n%s", prompt)
	}
	store, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	processed, err := store.ProcessedSHAs(ctx)
	if err != nil || len(processed) != 0 {
		t.Fatalf("precheck changed processed commits: %v, %v", processed, err)
	}
	findings, err := store.AllFindings(ctx)
	if err != nil || len(findings) != 0 {
		t.Fatalf("precheck changed findings: %+v, %v", findings, err)
	}
}

func TestPrecheckCommitSeriesReconcilesFindingsWithoutWritingStore(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.go", []byte("package app\n"), "base")
	first := testCommitFile(t, directory, "app.go", []byte("package app\nvar one = 1\n"), "first")
	second := testCommitFile(t, directory, "app.go", []byte("package app\nvar one = 1\nvar two = 2\n"), "second")
	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	file := "app.go"
	existing := Finding{ID: 1, IntroducedSHA: base, Severity: "warning", Title: "existing", Description: "existing issue", File: &file}
	calls := 0
	reviewer := precheckReviewerFunc(func(_ context.Context, input ReviewInput) (ReviewResult, error) {
		calls++
		result := cleanReview("checked")
		result.Usage = &TokenUsage{InputTokens: 100, CachedInputTokens: 20, OutputTokens: 10}
		switch calls {
		case 1:
			if input.Commit.SHA != first || input.Staged || len(input.OpenFindings) != 1 || input.OpenFindings[0].ID != 1 {
				t.Fatalf("first input = %+v", input)
			}
			result.Output.ResolvedFindings = []ResolvedFinding{{ID: 1, Reason: "fixed existing issue"}}
			result.Output.NewFindings = []NewFinding{{
				Severity: "warning", Title: "transient", Description: "fixed by the next commit", File: &file,
			}}
		case 2:
			if input.Commit.SHA != second || len(input.OpenFindings) != 1 || input.OpenFindings[0].Title != "transient" {
				t.Fatalf("second input = %+v", input)
			}
			result.Output.ResolvedFindings = []ResolvedFinding{{ID: input.OpenFindings[0].ID, Reason: "fixed later"}}
			result.Output.NewFindings = []NewFinding{{
				Severity: "error", Title: "outstanding", Description: "still broken", File: &file,
			}}
		default:
			t.Fatalf("unexpected review call %d", calls)
		}
		return result, nil
	})
	clock := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	elapsed := func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	report, err := precheckRepository(ctx, repository, precheckOptions{
		Plan:         precheckPlan{Mode: "range", FromSHA: base, ToSHA: second, Commits: []string{first, second}},
		OpenFindings: []Finding{existing},
		Identity:     ReviewIdentity{Harness: codexReviewerName, Model: modelByName("gpt-5.6-luna"), ReasoningEffort: "high"},
		NewReviewer: func() (Reviewer, ReviewIdentity, error) {
			return reviewer, ReviewIdentity{Harness: codexReviewerName, Model: modelByName("gpt-5.6-luna"), ReasoningEffort: "high"}, nil
		},
		Now: func() time.Time { return clock }, ElapsedNow: elapsed,
	})
	if err != nil {
		t.Fatalf("precheckRepository: %v", err)
	}
	if calls != 2 || len(report.Attempts) != 2 || len(report.Findings) != 1 ||
		report.Findings[0].Title != "outstanding" || len(report.PredictedResolutions) != 2 ||
		!report.PredictedResolutions[0].Existing || report.PredictedResolutions[1].Existing {
		t.Fatalf("report = %+v", report)
	}
	if report.Accounting.Attempts != 2 || report.Accounting.InputTokens != 200 ||
		report.Accounting.CachedInputTokens != 40 || report.Accounting.OutputTokens != 20 ||
		report.Accounting.CacheWritesUnreported != 2 || report.Accounting.PricedAttempts != 2 ||
		report.Accounting.DurationMilliseconds != 2_000 {
		t.Fatalf("accounting = %+v", report.Accounting)
	}
	processed, err := store.ProcessedSHAs(ctx)
	if err != nil || len(processed) != 0 {
		t.Fatalf("precheck changed processed commits: %v, %v", processed, err)
	}
	findings, err := store.AllFindings(ctx)
	if err != nil || len(findings) != 0 {
		t.Fatalf("precheck changed findings: %+v, %v", findings, err)
	}
}

func TestStagedPrecheckUsesOnlyGitIndex(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.go", []byte("package app\nvar value = 0\n"), "base")
	if err := writeTestFile(directory, "app.go", []byte("package app\nvar value = 1\n")); err != nil {
		t.Fatal(err)
	}
	testGit(t, directory, "add", "--", "app.go")
	if err := writeTestFile(directory, "app.go", []byte("package app\nvar value = 2\n")); err != nil {
		t.Fatal(err)
	}
	diff, err := repository.StagedDiff(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Text, "value = 1") || strings.Contains(diff.Text, "value = 2") {
		t.Fatalf("staged diff includes wrong snapshot:\n%s", diff.Text)
	}
	plan, err := buildPrecheckPlan(ctx, repository, true, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	reviewer := precheckReviewerFunc(func(_ context.Context, input ReviewInput) (ReviewResult, error) {
		seen = true
		if !input.Staged || input.Commit.SHA != stagedPrecheckSHA || input.Commit.ParentSHA != base {
			t.Fatalf("staged input = %+v", input)
		}
		return cleanReview("staged snapshot is clean"), nil
	})
	if _, err := precheckRepository(ctx, repository, precheckOptions{
		Plan: plan, Identity: ReviewIdentity{Model: modelByName("gpt-5.6-luna")},
		NewReviewer: func() (Reviewer, ReviewIdentity, error) {
			return reviewer, ReviewIdentity{Model: modelByName("gpt-5.6-luna")}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("staged reviewer was not called")
	}
}

func TestPrecheckDefaultsToMastersConfiguredUpstream(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.go", []byte("base\n"), "base")
	testGit(t, directory, "config", "remote.origin.url", ".")
	testGit(t, directory, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	testGit(t, directory, "update-ref", "refs/remotes/origin/master", base)
	testGit(t, directory, "config", "branch.master.remote", "origin")
	testGit(t, directory, "config", "branch.master.merge", "refs/heads/master")
	first := testCommitFile(t, directory, "app.go", []byte("first\n"), "first")
	second := testCommitFile(t, directory, "app.go", []byte("second\n"), "second")
	plan, err := buildPrecheckPlan(ctx, repository, false, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "unpushed" || plan.FromSHA != base || plan.ToSHA != second ||
		len(plan.Commits) != 2 || plan.Commits[0] != first || plan.Commits[1] != second {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPrecheckFormatsAndFailureThreshold(t *testing.T) {
	repository := &GitRepository{WorkTree: "/tmp/example"}
	file := "app.go"
	report := precheckReport{
		Mode: "staged", GeneratedAt: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
		Attempts: []precheckAttempt{{Commit: CommitMetadata{
			SHA: stagedPrecheckSHA, Author: "AIR Test <test@example.invalid>", Date: "2026-09-08T11:00:00Z",
		}, Status: "reviewed"}},
		Findings: []Finding{{
			ID: 1, IntroducedSHA: stagedPrecheckSHA, Severity: "warning",
			Title: "unsafe <value>", Description: "broken", File: &file,
		}},
	}
	for _, format := range []string{"json", "sarif", "html", "text"} {
		t.Run(format, func(t *testing.T) {
			var output bytes.Buffer
			if err := writePrecheckReport(&output, repository, report, format); err != nil {
				t.Fatal(err)
			}
			if output.Len() == 0 {
				t.Fatal("empty output")
			}
			switch format {
			case "json":
				var decoded precheckReport
				if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || len(decoded.Findings) != 1 {
					t.Fatalf("JSON output: %+v, %v", decoded, err)
				}
			case "sarif":
				if !strings.Contains(output.String(), `"version": "2.1.0"`) {
					t.Fatalf("SARIF output:\n%s", output.String())
				}
			case "html":
				if !strings.Contains(output.String(), "AIR Precheck Report") ||
					strings.Contains(output.String(), "unsafe <value>") {
					t.Fatalf("HTML output:\n%s", output.String())
				}
			case "text":
				if !strings.Contains(output.String(), "Outstanding findings:") {
					t.Fatalf("text output:\n%s", output.String())
				}
			}
		})
	}
	if precheckFailureCount(report.Findings, "error") != 0 ||
		precheckFailureCount(report.Findings, "warning") != 1 ||
		precheckFailureCount(report.Findings, "info") != 1 ||
		precheckFailureCount(report.Findings, "") != 0 {
		t.Fatal("unexpected fail-on threshold behavior")
	}
}

func TestPrecheckRejectsConflictingAndInvalidOptions(t *testing.T) {
	environment := cliEnvironment{Cwd: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard}
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--staged", "HEAD~1..HEAD"}, "cannot be used together"},
		{[]string{"--format", "yaml"}, "invalid --format"},
		{[]string{"--fail-on", "critical"}, "invalid --fail-on"},
	}
	for _, test := range tests {
		err := runPrecheck(context.Background(), test.args, environment)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("runPrecheck(%q) error = %v, want %q", test.args, err, test.want)
		}
	}
}
