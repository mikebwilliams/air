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
			"usage": chatUsage(100, 25, 10, 20, 5),
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": string(content)},
				"finish_reason": "stop",
			}},
		}), nil
	})}
	values := map[string]string{
		"AIR_MODEL":    "gpt-5.6-luna",
		"AIR_BASE_URL": "https://model.example/v1",
		"AIR_API_KEY":  "secret",
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
	if !strings.Contains(stdout.String(), "Model: gpt-5.6-luna") ||
		!strings.Contains(stdout.String(), "Tokens: 120 total (100 input, 25 cached input, 10 cache writes, 20 output, 5 reasoning output)") ||
		!strings.Contains(stdout.String(), "Estimated cost: $0.000040 USD (short context)") ||
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
		!strings.Contains(stdout.String(), "Tokens: 15 total (10 input, 4 cached input, 2 cache writes, 5 output, 2 reasoning output)") ||
		!strings.Contains(stdout.String(), "Estimated cost: unavailable") {
		t.Fatalf("show output lacks review identity:\n%s", stdout.String())
	}
}

func TestCLIStoredConfigurationDrivesScanAndRedactsSecrets(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	command, invocation := newCodexTestCommand(t, ReviewOutput{
		NewFindings:      []NewFinding{},
		ResolvedFindings: []ResolvedFinding{},
		Summary:          "Reviewed using stored configuration.",
	}, "")
	var stdout bytes.Buffer
	values := map[string]string{}
	environment := cliEnvironment{
		Cwd:          directory,
		Stdout:       &stdout,
		Stderr:       &bytes.Buffer{},
		Getenv:       func(key string) string { return values[key] },
		CodexCommand: command,
		Now:          func() time.Time { return time.Date(2026, 8, 15, 13, 0, 0, 0, time.UTC) },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, setting := range [][2]string{
		{"model", "stored-model"},
		{"effort", "xhigh"},
		{"codex-bin", "/stored/codex"},
		{"codex-profile", "air-review"},
		{"codex-timeout", "3m"},
	} {
		if err := runCLI(ctx, []string{"config", "set", setting[0], setting[1]}, environment); err != nil {
			t.Fatalf("set %s: %v", setting[0], err)
		}
	}
	environment.Stdin = strings.NewReader("database-secret\n")
	if err := runCLI(ctx, []string{"config", "set", "--stdin", "api-key"}, environment); err != nil {
		t.Fatalf("set api-key: %v", err)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"config", "list", "--effective"}, environment); err != nil {
		t.Fatalf("config list: %v", err)
	}
	configOutput := stdout.String()
	if !strings.Contains(configOutput, "model") || !strings.Contains(configOutput, "stored-model") ||
		!strings.Contains(configOutput, "database") || !strings.Contains(configOutput, "reviewer") ||
		!strings.Contains(configOutput, "built-in") || !strings.Contains(configOutput, "<redacted>") ||
		strings.Contains(configOutput, "database-secret") {
		t.Fatalf("effective configuration output:\n%s", configOutput)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"scan"}, environment); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if invocation.Name != "/stored/codex" {
		t.Fatalf("Codex binary = %q", invocation.Name)
	}
	assertArgumentPair(t, invocation.Args, "--model", "stored-model")
	assertArgumentPair(t, invocation.Args, "--config", `model_reasoning_effort="xhigh"`)
	assertArgumentPair(t, invocation.Args, "--profile", "air-review")

	secondHead := testCommitFile(t, directory, "app.txt", []byte("changed again\n"), "change again")
	values["AIR_MODEL"] = "environment-model"
	values["AIR_REASONING_EFFORT"] = "medium"
	stdout.Reset()
	if err := runCLI(ctx, []string{"config", "get", "--effective", "model"}, environment); err != nil {
		t.Fatalf("get effective model: %v", err)
	}
	if !strings.Contains(stdout.String(), "environment-model\tAIR_MODEL") {
		t.Fatalf("effective model output = %q", stdout.String())
	}
	if err := runCLI(ctx, []string{"scan", "--model", "command-model", "--effort", "high"}, environment); err != nil {
		t.Fatalf("scan with flags: %v", err)
	}
	assertArgumentPair(t, invocation.Args, "--model", "command-model")
	assertArgumentPair(t, invocation.Args, "--config", `model_reasoning_effort="high"`)

	recordStore, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer recordStore.Close()
	record, err := recordStore.Commit(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if record.Model != "stored-model" || record.ReasoningEffort != "xhigh" {
		t.Fatalf("stored review identity = model %q, effort %q", record.Model, record.ReasoningEffort)
	}
	record, err = recordStore.Commit(ctx, secondHead)
	if err != nil {
		t.Fatal(err)
	}
	if record.Model != "command-model" || record.ReasoningEffort != "high" {
		t.Fatalf("overridden review identity = model %q, effort %q", record.Model, record.ReasoningEffort)
	}
}

func TestCLIStoredHTTPConfiguration(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://stored.example/v1/chat/completions" {
			t.Fatalf("request URL = %s", request.URL)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer stored-secret" {
			t.Fatalf("Authorization = %q", authorization)
		}
		output, _ := json.Marshal(ReviewOutput{
			NewFindings:      []NewFinding{},
			ResolvedFindings: []ResolvedFinding{},
			Summary:          "Reviewed with stored HTTP configuration.",
		})
		return JSONResponse(t, map[string]any{
			"usage": chatUsage(10, 0, 0, 2, 0),
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": string(output)},
				"finish_reason": "stop",
			}},
		}), nil
	})}
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd:        directory,
		Stdout:     &stdout,
		Stderr:     &bytes.Buffer{},
		Getenv:     func(string) string { return "" },
		HTTPClient: client,
		Now:        func() time.Time { return time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC) },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, setting := range [][2]string{
		{"reviewer", "http"},
		{"model", "stored-http-model"},
		{"base-url", "https://stored.example/v1"},
		{"api-key", "stored-secret"},
	} {
		if err := runCLI(ctx, []string{"config", "set", setting[0], setting[1]}, environment); err != nil {
			t.Fatalf("set %s: %v", setting[0], err)
		}
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"scan"}, environment); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !strings.Contains(stdout.String(), "0 new, 0 resolved") {
		t.Fatalf("scan output:\n%s", stdout.String())
	}
}

func TestCLIConfigGetUnsetAndValidation(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd:    directory,
		Stdout: &stdout,
		Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatalf("init: %v", err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"config", "list"}, environment); err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if !strings.Contains(stdout.String(), "No reviewer configuration stored") {
		t.Fatalf("empty list output = %q", stdout.String())
	}
	if err := runCLI(ctx, []string{"config", "set", "model", "test-model"}, environment); err != nil {
		t.Fatalf("set model: %v", err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"config", "get", "model"}, environment); err != nil {
		t.Fatalf("get model: %v", err)
	}
	if strings.TrimSpace(stdout.String()) != "test-model" {
		t.Fatalf("get model output = %q", stdout.String())
	}
	if err := runCLI(ctx, []string{"config", "unset", "model"}, environment); err != nil {
		t.Fatalf("unset model: %v", err)
	}
	if err := runCLI(ctx, []string{"config", "get", "model"}, environment); err == nil ||
		!strings.Contains(err.Error(), "not set in the database") {
		t.Fatalf("missing model error = %v", err)
	}
	if err := runCLI(ctx, []string{"config", "set", "codex-timeout", "never"}, environment); err == nil ||
		!strings.Contains(err.Error(), "positive duration") {
		t.Fatalf("invalid timeout error = %v", err)
	}
	if err := runCLI(ctx, []string{"config", "set", "mystery", "value"}, environment); err == nil ||
		!strings.Contains(err.Error(), "unknown configuration setting") {
		t.Fatalf("unknown setting error = %v", err)
	}
}

func TestCLIRescanAndReviewAttemptDisplay(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	command, _ := newCodexTestCommand(t, ReviewOutput{
		NewFindings:      []NewFinding{},
		ResolvedFindings: []ResolvedFinding{},
		Summary:          "Retained review.",
	}, "")
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd:          directory,
		Stdout:       &stdout,
		Stderr:       &bytes.Buffer{},
		Getenv:       func(string) string { return "" },
		CodexCommand: command,
		Now:          func() time.Time { return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC) },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := runCLI(ctx, []string{"scan", "--model", "first-model", "--effort", "low"}, environment); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := runCLI(ctx, []string{"rescan", head, "--model", "second-model", "--effort", "xhigh"}, environment); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"show", head, "--reviews"}, environment); err != nil {
		t.Fatalf("show reviews: %v", err)
	}
	if !strings.Contains(stdout.String(), "Review attempts:") ||
		!strings.Contains(stdout.String(), "#1") || !strings.Contains(stdout.String(), "first-model/low") ||
		!strings.Contains(stdout.String(), "#2") || !strings.Contains(stdout.String(), "second-model/xhigh") ||
		!strings.Contains(stdout.String(), "current") {
		t.Fatalf("review list output:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"show", head, "--review", "1"}, environment); err != nil {
		t.Fatalf("show first review: %v", err)
	}
	if !strings.Contains(stdout.String(), "Review attempt #1") ||
		!strings.Contains(stdout.String(), "Model: first-model") ||
		strings.Contains(stdout.String(), "Model: second-model") {
		t.Fatalf("first review output:\n%s", stdout.String())
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
