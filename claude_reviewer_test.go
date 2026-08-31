package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type claudeTestInvocation struct {
	Name             string
	Args             []string
	PromptPath       string
	WorkingDirectory string
	Result           string
	FailureDetail    string
	Delay            string
}

func newClaudeTestCommand(t *testing.T, structured any, failureDetail string) (commandContextFunc, *claudeTestInvocation) {
	t.Helper()
	captureDirectory := t.TempDir()
	promptPath := filepath.Join(captureDirectory, "prompt.txt")
	structuredJSON, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	envelope := fmt.Sprintf(`{
		"type":"result","subtype":"success","is_error":false,
		"structured_output":%s,
		"usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":4,"output_tokens":5},
		"total_cost_usd":0.001234
	}`, structuredJSON)
	invocation := &claudeTestInvocation{
		PromptPath: promptPath, Result: envelope, FailureDetail: failureDetail,
	}
	command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		invocation.Name = name
		invocation.Args = append([]string(nil), args...)
		helperArgs := []string{"-test.run=^TestClaudeHelperProcess$", "--"}
		helperArgs = append(helperArgs, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], helperArgs...)
		cmd.Env = append(os.Environ(),
			"AIR_CLAUDE_HELPER=1",
			"AIR_CLAUDE_HELPER_PROMPT="+promptPath,
			"AIR_CLAUDE_HELPER_RESULT="+invocation.Result,
			"AIR_CLAUDE_HELPER_FAILURE="+failureDetail,
			"AIR_CLAUDE_HELPER_DELAY="+invocation.Delay,
		)
		return cmd
	}
	return command, invocation
}

func TestClaudeHelperProcess(t *testing.T) {
	if os.Getenv("AIR_CLAUDE_HELPER") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		fmt.Fprintln(os.Stderr, "missing helper argument separator")
		os.Exit(90)
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(91)
	}
	if err := os.WriteFile(os.Getenv("AIR_CLAUDE_HELPER_PROMPT"), prompt, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(92)
	}
	if delay := os.Getenv("AIR_CLAUDE_HELPER_DELAY"); delay != "" {
		duration, err := time.ParseDuration(delay)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(93)
		}
		time.Sleep(duration)
	}
	if detail := os.Getenv("AIR_CLAUDE_HELPER_FAILURE"); detail != "" {
		fmt.Fprintln(os.Stderr, detail)
		os.Exit(42)
	}
	fmt.Fprintln(os.Stdout, os.Getenv("AIR_CLAUDE_HELPER_RESULT"))
	os.Exit(0)
}

func TestClaudeReviewerRunsReadOnlyCommitReview(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change state")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	structured := ReviewOutput{
		NewFindings: []NewFinding{{
			Severity: "warning", Title: "state regression",
			Description: "The new state violates the caller contract.", File: stringPointer("state.txt"),
		}},
		ResolvedFindings: []ResolvedFinding{{ID: 7, Reason: "The old failure is removed."}},
		Summary:          "Inspected the state transition.",
	}
	command, invocation := newClaudeTestCommand(t, structured, "")
	reviewer := &ClaudeReviewer{
		Repository: repository, Binary: "/custom/claude", Model: "claude-test-model",
		Effort: "high", CommandContext: command,
	}
	result, err := reviewer.Review(context.Background(), ReviewInput{
		Commit: metadata,
		OpenFindings: []Finding{{
			ID: 7, IntroducedSHA: base, Severity: "warning", Title: "old failure",
			Description: "An earlier commit introduced a failure.",
		}},
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if invocation.Name != "/custom/claude" {
		t.Fatalf("binary = %q", invocation.Name)
	}
	for _, argument := range []string{
		"-p", "--no-session-persistence", "--no-chrome", "--disable-slash-commands", "--strict-mcp-config",
	} {
		if !slices.Contains(invocation.Args, argument) {
			t.Fatalf("args lack %q: %q", argument, invocation.Args)
		}
	}
	assertArgumentPair(t, invocation.Args, "--model", "claude-test-model")
	assertArgumentPair(t, invocation.Args, "--fallback-model", "claude-test-model")
	assertArgumentPair(t, invocation.Args, "--effort", "high")
	assertArgumentPair(t, invocation.Args, "--output-format", "json")
	assertArgumentPair(t, invocation.Args, "--permission-mode", "dontAsk")
	assertArgumentPair(t, invocation.Args, "--mcp-config", `{"mcpServers":{}}`)
	assertArgumentPair(t, invocation.Args, "--tools", "Read,Grep,Glob,Bash")
	for _, allowed := range []string{"Read", "Glob", "Grep", "Bash(git diff *)", "Bash(git show *)"} {
		if !slices.Contains(invocation.Args, allowed) {
			t.Fatalf("args lack allowed tool %q: %q", allowed, invocation.Args)
		}
	}
	for _, denied := range claudeDisallowedTools {
		if !slices.Contains(invocation.Args, denied) {
			t.Fatalf("args lack denied tool %q: %q", denied, invocation.Args)
		}
	}
	var settings struct {
		DisableAllHooks bool `json:"disableAllHooks"`
		Sandbox         struct {
			Enabled                  bool `json:"enabled"`
			FailIfUnavailable        bool `json:"failIfUnavailable"`
			AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
			Filesystem               struct {
				DenyWrite []string `json:"denyWrite"`
			} `json:"filesystem"`
			Network struct {
				AllowedDomains []string `json:"allowedDomains"`
			} `json:"network"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(testArgumentValue(invocation.Args, "--settings")), &settings); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if !settings.DisableAllHooks || !settings.Sandbox.Enabled || !settings.Sandbox.FailIfUnavailable ||
		settings.Sandbox.AllowUnsandboxedCommands ||
		len(settings.Sandbox.Filesystem.DenyWrite) != 1 ||
		settings.Sandbox.Filesystem.DenyWrite[0] != repository.WorkTree ||
		len(settings.Sandbox.Network.AllowedDomains) != 0 {
		t.Fatalf("unsafe Claude settings: %+v", settings)
	}
	schema := testArgumentValue(invocation.Args, "--json-schema")
	if !json.Valid([]byte(schema)) || !strings.Contains(schema, `"additionalProperties": false`) {
		t.Fatalf("invalid or permissive schema: %s", schema)
	}
	prompt, err := os.ReadFile(invocation.PromptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), head) || !strings.Contains(string(prompt), "old failure") {
		t.Fatalf("prompt lacks review context:\n%s", prompt)
	}
	if len(result.Output.NewFindings) != 1 || len(result.Output.ResolvedFindings) != 1 {
		t.Fatalf("result = %+v", result.Output)
	}
	if result.Usage == nil || result.Usage.InputTokens != 16 || result.Usage.CachedInputTokens != 4 ||
		result.Usage.CacheWriteTokens == nil || *result.Usage.CacheWriteTokens != 2 ||
		result.Usage.OutputTokens != 5 || result.Usage.ReasoningOutputTokens != 0 ||
		!result.Usage.ReasoningOutputTokensUnreported {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if result.ReportedCostMicrousd == nil || *result.ReportedCostMicrousd != 1234 {
		t.Fatalf("reported cost = %v", result.ReportedCostMicrousd)
	}
	if !strings.Contains(result.RawResponse, `"structured_output"`) {
		t.Fatalf("raw response = %q", result.RawResponse)
	}
}

func TestClaudeReviewerRechecksExactHEAD(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	head := testCommitFile(t, directory, "state.go", []byte("package state\n"), "base")
	structured := RecheckOutput{Findings: []RecheckFindingResult{{
		ID: 7, Outcome: "still_present", Reason: "The invalid state remains reachable.",
	}}, Summary: "Checked the current implementation."}
	command, invocation := newClaudeTestCommand(t, structured, "")
	reviewer := &ClaudeReviewer{
		Repository: repository, Model: "claude-strong-model", Effort: "xhigh",
		CommandContext: command,
	}
	result, err := reviewer.Recheck(context.Background(), RecheckInput{
		HeadSHA: head,
		Findings: []Finding{{
			ID: 7, IntroducedSHA: head, Severity: "warning", Title: "invalid state",
			Description: "The state can become invalid.", File: stringPointer("state.go"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output.Findings) != 1 || result.Output.Findings[0].Outcome != "still_present" {
		t.Fatalf("recheck result = %+v", result)
	}
	prompt, err := os.ReadFile(invocation.PromptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), head) || !strings.Contains(string(prompt), "exact Git HEAD snapshot") {
		t.Fatalf("recheck prompt:\n%s", prompt)
	}
	if schema := testArgumentValue(invocation.Args, "--json-schema"); !json.Valid([]byte(schema)) || !strings.Contains(schema, `"still_present"`) {
		t.Fatalf("recheck schema: %s", schema)
	}
}

func TestClaudeReviewerReportsCommandFailure(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	command, _ := newClaudeTestCommand(t, ReviewOutput{}, "authentication required")
	reviewer := &ClaudeReviewer{
		Repository: repository, Model: "claude-test-model", Effort: "low", CommandContext: command,
	}
	_, err = reviewer.Review(context.Background(), ReviewInput{Commit: metadata})
	if err == nil || !strings.Contains(err.Error(), "Claude failed: authentication required") {
		t.Fatalf("Review error = %v", err)
	}
}

func TestClaudeReviewerTimesOut(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	command, invocation := newClaudeTestCommand(t, ReviewOutput{}, "")
	invocation.Delay = "5s"
	reviewer := &ClaudeReviewer{
		Repository: repository, Model: "claude-test-model", Effort: "low",
		Timeout: 100 * time.Millisecond, CommandContext: command,
	}
	_, err = reviewer.Review(context.Background(), ReviewInput{Commit: metadata})
	if err == nil || !strings.Contains(err.Error(), "Claude timed out after 100ms") {
		t.Fatalf("Review error = %v", err)
	}
}

func TestParseClaudeResultRequiresStructuredOutputAndUsage(t *testing.T) {
	for _, test := range []struct {
		name string
		json string
		want string
	}{
		{"structured output", `{"type":"result","subtype":"success","usage":{}}`, "omitted structured_output"},
		{"usage", `{"type":"result","subtype":"success","structured_output":{}}`, "omitted token usage"},
		{"failed", `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"model failed"}`, "Claude failed: model failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, err := parseClaudeResult([]byte(test.json))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
