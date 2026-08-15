package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCLIInitScanAndQueries(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	var stdout, stderr bytes.Buffer
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		output := ReviewOutput{
			NewFindings: []NewFinding{{
				Severity:    "warning",
				Title:       "test finding",
				Description: "The changed value violates the test contract.",
				File:        stringPointer("app.txt"),
			}},
			ResolvedFindings: []ResolvedFinding{},
			Summary:          "Reviewed app.txt.",
		}
		content, _ := json.Marshal(output)
		return JSONResponse(t, map[string]any{
			"usage": chatUsage(100, 25, 20, 5),
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": string(content)},
				"finish_reason": "stop",
			}},
		}), nil
	})}
	values := map[string]string{
		"AIR_MODEL":                        "test-model",
		"AIR_BASE_URL":                     "https://model.example/v1",
		"AIR_API_KEY":                      "secret",
		"AIR_INPUT_USD_PER_MILLION":        "10",
		"AIR_CACHED_INPUT_USD_PER_MILLION": "1",
		"AIR_OUTPUT_USD_PER_MILLION":       "20",
	}
	environment := cliEnvironment{
		Cwd:        directory,
		Stdout:     &stdout,
		Stderr:     &stderr,
		Getenv:     func(key string) string { return values[key] },
		HTTPClient: client,
		Now:        func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(stdout.String(), "Baseline: "+base) {
		t.Fatalf("init output:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"scan", "--reviewer", "http"}, environment); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !strings.Contains(stdout.String(), shortSHA(head)+"  1 new, 0 resolved") {
		t.Fatalf("scan output:\n%s", stdout.String())
	}
	if status := strings.TrimSpace(testGit(t, directory, "status", "--porcelain")); status != "" {
		t.Fatalf("AIR modified tracked worktree state: %q", status)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"status"}, environment); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(stdout.String(), "1 open findings") || !strings.Contains(stdout.String(), "test finding") {
		t.Fatalf("status output:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"show", head}, environment); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(stdout.String(), "Model: test-model") ||
		!strings.Contains(stdout.String(), "Tokens: 120 total (100 input, 25 cached input, 20 output, 5 reasoning output)") ||
		!strings.Contains(stdout.String(), "Estimated cost: $0.001175 USD") ||
		!strings.Contains(stdout.String(), "Reviewed app.txt.") ||
		!strings.Contains(stdout.String(), "#1 warning") {
		t.Fatalf("show output:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"finding", "1"}, environment); err != nil {
		t.Fatalf("finding: %v", err)
	}
	if !strings.Contains(stdout.String(), "Introduced:") || !strings.Contains(stdout.String(), "test finding") {
		t.Fatalf("finding output:\n%s", stdout.String())
	}
}

func TestCLIScanDefaultsToCodex(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	resultOutput := ReviewOutput{
		NewFindings:      []NewFinding{},
		ResolvedFindings: []ResolvedFinding{},
		Summary:          "Reviewed with local Codex.",
	}
	command, invocation := newCodexTestCommand(t, resultOutput, "")
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd:          directory,
		Stdout:       &stdout,
		Stderr:       &bytes.Buffer{},
		Getenv:       func(string) string { return "" },
		CodexCommand: command,
		Now:          func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatalf("init: %v", err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"scan", "--model", "test-codex-model", "--effort", "low"}, environment); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if invocation.Name != "codex" || !strings.Contains(stdout.String(), shortSHA(head)+"  0 new, 0 resolved") {
		t.Fatalf("invocation=%+v, output=%q", invocation, stdout.String())
	}
	store, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, err := store.Commit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if record.Model != "test-codex-model" || record.ReasoningEffort != "low" {
		t.Fatalf("review identity = model %q, effort %q", record.Model, record.ReasoningEffort)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"show", head}, environment); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(stdout.String(), "Model: test-codex-model") ||
		!strings.Contains(stdout.String(), "Reasoning effort: low") ||
		!strings.Contains(stdout.String(), "Tokens: 15 total (10 input, 4 cached input, 5 output, 2 reasoning output)") ||
		!strings.Contains(stdout.String(), "Estimated cost: unavailable") {
		t.Fatalf("show output lacks review identity:\n%s", stdout.String())
	}
}

func TestCLICodexRequiresExplicitReviewIdentity(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	environment := cliEnvironment{
		Cwd:    directory,
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := runCLI(ctx, []string{"scan", "--model", "test-model"}, environment); err == nil ||
		!strings.Contains(err.Error(), "reasoning effort is required") {
		t.Fatalf("missing effort error = %v", err)
	}
	if err := runCLI(ctx, []string{"scan", "--effort", "low"}, environment); err == nil ||
		!strings.Contains(err.Error(), "model is required") {
		t.Fatalf("missing model error = %v", err)
	}
}

func TestCLIRejectsInitOutsideMasterFirstParent(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	testCommitFile(t, directory, "base.txt", []byte("base\n"), "base")
	testGit(t, directory, "switch", "-c", "feature")
	feature := testCommitFile(t, directory, "feature.txt", []byte("feature\n"), "feature")
	testGit(t, directory, "switch", "master")
	environment := cliEnvironment{
		Cwd:    directory,
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" },
	}
	err := runCLI(ctx, []string{"init", feature}, environment)
	if err == nil || !strings.Contains(err.Error(), "not on the first-parent history") {
		t.Fatalf("init error = %v", err)
	}
}
