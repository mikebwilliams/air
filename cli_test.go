package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
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
	if err := runCLI(ctx, []string{"status", "--json"}, environment); err != nil {
		t.Fatalf("status JSON: %v", err)
	}
	var statusJSON struct {
		OpenFindings []Finding `json:"open_findings"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &statusJSON); err != nil || len(statusJSON.OpenFindings) != 1 ||
		statusJSON.OpenFindings[0].Title != "test finding" {
		t.Fatalf("status JSON = %+v, %v; output=%s", statusJSON, err, stdout.String())
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
	if err := runCLI(ctx, []string{"show", head, "--reviews", "--json"}, environment); err != nil {
		t.Fatalf("show JSON: %v", err)
	}
	var showJSON showJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &showJSON); err != nil || showJSON.Commit.SHA != head ||
		showJSON.Record.Model != "gpt-5.6-luna" || len(showJSON.IntroducedFindings) != 1 ||
		len(showJSON.Reviews) != 1 || showJSON.Reviews[0].Usage.InputTokens != 100 {
		t.Fatalf("show JSON = %+v, %v; output=%s", showJSON, err, stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"export", "--format", "sarif"}, environment); err != nil {
		t.Fatalf("SARIF export: %v", err)
	}
	var sarif sarifLog
	if err := json.Unmarshal(stdout.Bytes(), &sarif); err != nil || sarif.Version != "2.1.0" ||
		len(sarif.Runs) != 1 || len(sarif.Runs[0].Results) != 1 ||
		sarif.Runs[0].Results[0].Level != "warning" ||
		sarif.Runs[0].Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI != "app.txt" {
		t.Fatalf("SARIF = %+v, %v; output=%s", sarif, err, stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"finding", "1"}, environment); err != nil {
		t.Fatalf("finding: %v", err)
	}
	if !strings.Contains(stdout.String(), "Introduced:") || !strings.Contains(stdout.String(), "test finding") {
		t.Fatalf("finding output:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"stats"}, environment); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(stdout.String(), "Reviews: 1 attempts across 1 commits") ||
		!strings.Contains(stdout.String(), "Tokens: 100 input (25 cached), 10 cache writes, 20 output (5 reasoning)") ||
		!strings.Contains(stdout.String(), "Estimated cost: $0.000040 USD") ||
		!strings.Contains(stdout.String(), "gpt-5.6-luna: 1 attempts") {
		t.Fatalf("stats output:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"cost", "--model", "gpt-5.6-luna", "--since", "2026-08-14"}, environment); err != nil {
		t.Fatalf("cost: %v", err)
	}
	if !strings.Contains(stdout.String(), "Estimated cost: $0.000040 USD") ||
		!strings.Contains(stdout.String(), "Reviews: 1 attempts across 1 commits") {
		t.Fatalf("cost output:\n%s", stdout.String())
	}
}

func TestCLIDatabasePathWorksBeforeInitialization(t *testing.T) {
	_, directory := newTestGitRepository(t)
	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: directory, Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := runCLI(context.Background(), []string{"db", "path"}, environment); err != nil {
		t.Fatal(err)
	}
	repository, err := DiscoverGitRepository(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != repository.DatabasePath()+"\n" {
		t.Fatalf("db path output = %q, want %q", stdout.String(), repository.DatabasePath()+"\n")
	}
}

func TestCLIDoctorChecksCodexConfigurationAndAuthentication(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var invokedName string
	var invokedArgs []string
	environment := cliEnvironment{
		Cwd: directory, Stdout: &stdout, Stderr: &bytes.Buffer{}, Getenv: func(string) string { return "" },
		CodexCommand: func(commandContext context.Context, name string, args ...string) *exec.Cmd {
			invokedName = name
			invokedArgs = append([]string(nil), args...)
			command := exec.CommandContext(commandContext, os.Args[0], "-test.run=^TestDoctorLoginHelper$")
			command.Env = append(os.Environ(), "AIR_DOCTOR_HELPER=1")
			return command
		},
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	for _, setting := range [][2]string{
		{"model", "gpt-5.6-luna"},
		{"effort", "xhigh"},
		{"codex-bin", executable},
	} {
		if err := runCLI(ctx, []string{"config", "set", setting[0], setting[1]}, environment); err != nil {
			t.Fatalf("set %s: %v", setting[0], err)
		}
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"doctor"}, environment); err != nil {
		t.Fatalf("doctor: %v\n%s", err, stdout.String())
	}
	if invokedName != executable || len(invokedArgs) != 2 || invokedArgs[0] != "login" || invokedArgs[1] != "status" {
		t.Fatalf("doctor Codex invocation = %q %q", invokedName, invokedArgs)
	}
	if !strings.Contains(stdout.String(), "PASS repository") ||
		!strings.Contains(stdout.String(), "PASS database integrity") ||
		!strings.Contains(stdout.String(), "PASS Codex authentication") ||
		!strings.Contains(stdout.String(), "0 warnings, 0 failed") {
		t.Fatalf("doctor output:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"doctor", "--json"}, environment); err != nil {
		t.Fatalf("doctor JSON: %v", err)
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || !report.OK || report.Failed != 0 ||
		report.DatabasePath == "" || len(report.Checks) < 8 {
		t.Fatalf("doctor report = %+v, %v; output=%s", report, err, stdout.String())
	}
}

func TestDoctorLoginHelper(t *testing.T) {
	if os.Getenv("AIR_DOCTOR_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "Logged in for AIR test")
	os.Exit(0)
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

func TestCLIFindingTriageCommands(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	now := time.Date(2026, 8, 16, 15, 0, 0, 0, time.UTC)
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
	result := cleanReview("Finding for CLI triage.")
	result.Output.NewFindings = []NewFinding{{
		Severity: "warning", Title: "manual triage", Description: "Exercise manual disposition.",
	}}
	ids, err := store.ApplyReview(ctx, CommitMetadata{SHA: head, ParentSHA: base},
		ReviewIdentity{Model: modelByName("test-model")}, result, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(ids[0], 10)
	if err := runCLI(ctx, []string{"finding", "dismiss", id, "--reason", "accepted risk"}, environment); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if err := runCLI(ctx, []string{"finding", "note", id, "check next release"}, environment); err != nil {
		t.Fatalf("note: %v", err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"finding", id}, environment); err != nil {
		t.Fatalf("show finding: %v", err)
	}
	if !strings.Contains(stdout.String(), "dismissed") || !strings.Contains(stdout.String(), "accepted risk") ||
		!strings.Contains(stdout.String(), "check next release") {
		t.Fatalf("dismissed finding output:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"status"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "0 open findings") {
		t.Fatalf("status after dismissal:\n%s", stdout.String())
	}
	if err := runCLI(ctx, []string{"finding", "reopen", id}, environment); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"status"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "1 open findings") {
		t.Fatalf("status after reopen:\n%s", stdout.String())
	}
}

func TestCLIPendingAndScanDryRun(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &stdout, Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"pending"}, environment); err != nil {
		t.Fatalf("pending: %v", err)
	}
	if !strings.Contains(stdout.String(), shortSHA(head)+"  review") ||
		!strings.Contains(stdout.String(), "Pending: 1 commit (1 reviewable, 0 skipped)") {
		t.Fatalf("pending output:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"scan", "--dry-run"}, environment); err != nil {
		t.Fatalf("scan --dry-run: %v", err)
	}
	if !strings.Contains(stdout.String(), shortSHA(head)+"  review") {
		t.Fatalf("scan --dry-run output:\n%s", stdout.String())
	}
	store, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Commit(ctx, head); err == nil {
		t.Fatal("dry-run commit was recorded")
	}
}

func TestCLICleanPrunesCommitsOutsideMaster(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "base.txt", []byte("base\n"), "base")
	testGit(t, directory, "switch", "-c", "discarded")
	stale := testCommitFile(t, directory, "stale.txt", []byte("stale\n"), "stale")
	testGit(t, directory, "switch", "master")
	live := testCommitFile(t, directory, "live.txt", []byte("live\n"), "live")
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	staleReview := cleanReview("Stale review.")
	staleReview.Output.NewFindings = []NewFinding{{
		Severity: "warning", Title: "stale finding", Description: "Should cascade with stale history.",
	}}
	if _, err := store.ApplyReview(ctx, CommitMetadata{SHA: stale, ParentSHA: base},
		ReviewIdentity{Model: modelByName("test-model")}, staleReview, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyReview(ctx, CommitMetadata{SHA: live, ParentSHA: base},
		ReviewIdentity{Model: modelByName("test-model")}, cleanReview("Live review."), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"clean", "--dry-run"}, environment); err != nil {
		t.Fatalf("clean dry run: %v", err)
	}
	if !strings.Contains(stdout.String(), shortSHA(stale)+"  would remove") ||
		strings.Contains(stdout.String(), shortSHA(live)) {
		t.Fatalf("clean dry-run output:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"clean"}, environment); err != nil {
		t.Fatalf("clean: %v", err)
	}
	store, err = OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Commit(ctx, stale); err == nil {
		t.Fatal("stale commit survived clean")
	}
	if _, err := store.Commit(ctx, live); err != nil {
		t.Fatalf("master commit was removed: %v", err)
	}
	if open, err := store.OpenFindings(ctx); err != nil || len(open) != 0 {
		t.Fatalf("stale findings survived clean: %+v, %v", open, err)
	}
}

func TestCLIFailuresAndRetryIncludingFailedRescan(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	command, _ := newCodexTestCommand(t, ReviewOutput{
		NewFindings:      []NewFinding{},
		ResolvedFindings: []ResolvedFinding{},
		Summary:          "Successful retry.",
	}, "")
	now := time.Date(2026, 8, 16, 17, 0, 0, 0, time.UTC)
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &stdout, Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" }, CodexCommand: command,
		Now: func() time.Time { now = now.Add(time.Minute); return now },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	identity := ReviewIdentity{Model: modelByName("retry-model"), ReasoningEffort: "low"}
	if err := store.RecordScanFailure(ctx, head, base, identity, false,
		errors.New("temporary failure"), now); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"failures"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "1 failed commits") ||
		!strings.Contains(stdout.String(), "retry-model/low") ||
		!strings.Contains(stdout.String(), "temporary failure") {
		t.Fatalf("failures output:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"failures", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var failureJSON struct {
		Failures []ScanFailure `json:"failures"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &failureJSON); err != nil ||
		len(failureJSON.Failures) != 1 || failureJSON.Failures[0].SHA != head {
		t.Fatalf("failure JSON = %+v, %v; output=%s", failureJSON, err, stdout.String())
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"retry", "--model", "retry-model", "--effort", "low"}, environment); err != nil {
		t.Fatalf("retry: %v", err)
	}
	store, err = OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := store.ReviewAttempts(ctx, head)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("retry attempts = %+v, %v", attempts, err)
	}
	if err := store.RecordScanFailure(ctx, head, base, identity, true,
		errors.New("failed high-effort rescan"), now); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"retry", "--model", "retry-model", "--effort", "low"}, environment); err != nil {
		t.Fatalf("rescan retry: %v", err)
	}
	store, err = OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	attempts, err = store.ReviewAttempts(ctx, head)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("rescan retry attempts = %+v, %v", attempts, err)
	}
	if failures, err := store.ScanFailures(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("successful retries left failures = %+v, %v", failures, err)
	}
}

func TestCLIResetRequiresConfirmationAndRemovesStateDirectory(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdin: strings.NewReader("no\n"), Stdout: &stdout, Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"reset"}, environment); err != nil {
		t.Fatalf("cancel reset: %v", err)
	}
	if !strings.Contains(stdout.String(), "Reset cancelled") {
		t.Fatalf("cancel output = %q", stdout.String())
	}
	if _, err := os.Stat(repository.DatabasePath()); err != nil {
		t.Fatalf("cancelled reset removed database: %v", err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"reset", "--force"}, environment); err != nil {
		t.Fatalf("forced reset: %v", err)
	}
	if _, err := os.Stat(repository.StateDirectory()); !os.IsNotExist(err) {
		t.Fatalf("state directory remains after reset: %v", err)
	}
}

func TestCLIModelPricingManagementControlsReviewCost(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	command, _ := newCodexTestCommand(t, ReviewOutput{
		NewFindings:      []NewFinding{},
		ResolvedFindings: []ResolvedFinding{},
		Summary:          "Custom-priced review.",
	}, "")
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &stdout, Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" }, CodexCommand: command,
		Now: func() time.Time { return time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC) },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	pricingArgs := []string{
		"model", "set-pricing", "custom-model",
		"--service-tier", "standard", "--source", "test", "--as-of", "2026-08-16",
		"--long-context-threshold", "100000",
		"--short-input", "1", "--short-cached-input", "0.5",
		"--short-cache-write", "1.25", "--short-output", "2",
		"--long-input", "2", "--long-cached-input", "1",
		"--long-cache-write", "2.5", "--long-output", "3",
	}
	if err := runCLI(ctx, pricingArgs, environment); err != nil {
		t.Fatalf("set pricing: %v", err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"model", "show", "custom-model"}, environment); err != nil {
		t.Fatalf("show model: %v", err)
	}
	if !strings.Contains(stdout.String(), "Pricing: known") ||
		!strings.Contains(stdout.String(), "short input 1.000") {
		t.Fatalf("model output:\n%s", stdout.String())
	}
	if err := runCLI(ctx, []string{"config", "set", "model", "custom-model"}, environment); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(ctx, []string{"config", "set", "effort", "low"}, environment); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(ctx, []string{"scan"}, environment); err != nil {
		t.Fatalf("scan: %v", err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"show", head}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Estimated cost: $0.000019 USD") {
		t.Fatalf("custom cost output:\n%s", stdout.String())
	}
	if err := runCLI(ctx, []string{"model", "mark-pricing-unknown", "custom-model"}, environment); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	if err := runCLI(ctx, []string{"rescan", head}, environment); err != nil {
		t.Fatalf("rescan unknown pricing: %v", err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"show", head}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Estimated cost: unavailable") {
		t.Fatalf("unknown cost output:\n%s", stdout.String())
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
