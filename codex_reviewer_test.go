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

type codexTestInvocation struct {
	Name          string
	Args          []string
	PromptPath    string
	SchemaPath    string
	Result        string
	FailureDetail string
	Delay         string
}

func newCodexTestCommand(t *testing.T, result any, failureDetail string) (commandContextFunc, *codexTestInvocation) {
	t.Helper()
	captureDirectory := t.TempDir()
	promptPath := filepath.Join(captureDirectory, "prompt.txt")
	schemaPath := filepath.Join(captureDirectory, "schema.json")
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	invocation := &codexTestInvocation{
		PromptPath:    promptPath,
		SchemaPath:    schemaPath,
		Result:        string(resultJSON),
		FailureDetail: failureDetail,
	}
	command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		invocation.Name = name
		invocation.Args = append([]string(nil), args...)
		helperArgs := []string{"-test.run=^TestCodexHelperProcess$", "--"}
		helperArgs = append(helperArgs, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], helperArgs...)
		cmd.Env = append(os.Environ(),
			"AIR_CODEX_HELPER=1",
			"AIR_CODEX_HELPER_PROMPT="+promptPath,
			"AIR_CODEX_HELPER_SCHEMA="+schemaPath,
			"AIR_CODEX_HELPER_RESULT="+invocation.Result,
			"AIR_CODEX_HELPER_FAILURE="+failureDetail,
			"AIR_CODEX_HELPER_DELAY="+invocation.Delay,
		)
		return cmd
	}
	return command, invocation
}

func TestCodexHelperProcess(t *testing.T) {
	if os.Getenv("AIR_CODEX_HELPER") != "1" {
		return
	}
	args := os.Args
	separator := slices.Index(args, "--")
	if separator < 0 {
		fmt.Fprintln(os.Stderr, "missing helper argument separator")
		os.Exit(90)
	}
	args = args[separator+1:]
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(91)
	}
	if err := os.WriteFile(os.Getenv("AIR_CODEX_HELPER_PROMPT"), prompt, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(92)
	}
	schemaSource := testArgumentValue(args, "--output-schema")
	schema, err := os.ReadFile(schemaSource)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(93)
	}
	if err := os.WriteFile(os.Getenv("AIR_CODEX_HELPER_SCHEMA"), schema, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(94)
	}
	if delay := os.Getenv("AIR_CODEX_HELPER_DELAY"); delay != "" {
		duration, err := time.ParseDuration(delay)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(97)
		}
		time.Sleep(duration)
	}
	if detail := os.Getenv("AIR_CODEX_HELPER_FAILURE"); detail != "" {
		fmt.Fprintln(os.Stderr, detail)
		os.Exit(42)
	}
	resultPath := testArgumentValue(args, "--output-last-message")
	if resultPath == "" {
		fmt.Fprintln(os.Stderr, "missing output path")
		os.Exit(95)
	}
	if err := os.WriteFile(resultPath, []byte(os.Getenv("AIR_CODEX_HELPER_RESULT")), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(96)
	}
	fmt.Fprintln(os.Stdout, `{"type":"thread.started","thread_id":"test"}`)
	fmt.Fprintln(os.Stdout, `{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"cache_write_tokens":2,"output_tokens":5,"reasoning_output_tokens":2}}`)
	os.Exit(0)
}

func TestCodexReviewerRunsReadOnlyEphemeralCommitReview(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change state")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	resultOutput := ReviewOutput{
		NewFindings: []NewFinding{{
			Severity:    "warning",
			Title:       "state regression",
			Description: "The new state violates the caller contract.",
			File:        stringPointer("state.txt"),
		}},
		ResolvedFindings: []ResolvedFinding{{ID: 7, Reason: "The old failure is removed."}},
		Summary:          "Inspected the state transition.",
	}
	command, invocation := newCodexTestCommand(t, resultOutput, "")
	temporaryRoot := t.TempDir()
	reviewer := &CodexReviewer{
		Repository:     repository,
		Binary:         "/custom/codex",
		Model:          "test-model",
		Effort:         "high",
		Profile:        "air-review",
		TempDir:        temporaryRoot,
		CommandContext: command,
	}
	result, err := reviewer.Review(context.Background(), ReviewInput{
		Commit: metadata,
		OpenFindings: []Finding{{
			ID:            7,
			IntroducedSHA: base,
			Severity:      "warning",
			Title:         "old failure",
			Description:   "An earlier commit introduced a failure.",
		}},
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if invocation.Name != "/custom/codex" {
		t.Fatalf("binary = %q", invocation.Name)
	}
	if len(invocation.Args) == 0 || invocation.Args[0] != "exec" {
		t.Fatalf("args = %q", invocation.Args)
	}
	assertArgumentPair(t, invocation.Args, "-C", repository.WorkTree)
	assertArgumentPair(t, invocation.Args, "--sandbox", "read-only")
	assertArgumentPair(t, invocation.Args, "--config", `approval_policy="never"`)
	assertArgumentPair(t, invocation.Args, "--config", `model_reasoning_effort="high"`)
	assertArgumentPair(t, invocation.Args, "--profile", "air-review")
	assertArgumentPair(t, invocation.Args, "--model", "test-model")
	for _, argument := range []string{"--ephemeral", "--json", "-"} {
		if !slices.Contains(invocation.Args, argument) {
			t.Fatalf("args lack %q: %q", argument, invocation.Args)
		}
	}
	for _, argument := range []string{"review", "--commit"} {
		if slices.Contains(invocation.Args, argument) {
			t.Fatalf("args contain incompatible Codex review argument %q: %q", argument, invocation.Args)
		}
	}
	prompt, err := os.ReadFile(invocation.PromptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), head) || !strings.Contains(string(prompt), "old failure") {
		t.Fatalf("prompt lacks commit context:\n%s", prompt)
	}
	schema, err := os.ReadFile(invocation.SchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(schema) || !strings.Contains(string(schema), `"additionalProperties": false`) {
		t.Fatalf("invalid or permissive schema:\n%s", schema)
	}
	if len(result.Output.NewFindings) != 1 || len(result.Output.ResolvedFindings) != 1 {
		t.Fatalf("result = %+v", result.Output)
	}
	if !strings.Contains(result.RawResponse, `"turn.completed"`) {
		t.Fatalf("raw response = %q", result.RawResponse)
	}
	if result.Usage == nil || result.Usage.InputTokens != 10 || result.Usage.CachedInputTokens != 4 ||
		result.Usage.CacheWriteTokens == nil || *result.Usage.CacheWriteTokens != 2 ||
		result.Usage.OutputTokens != 5 || result.Usage.ReasoningOutputTokens != 2 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	entries, err := os.ReadDir(temporaryRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files remain: %v, %v", entries, err)
	}
}

func TestParseCodexTokenUsageUsesLatestCompletedTurn(t *testing.T) {
	usage, err := parseCodexTokenUsage([]byte(strings.Join([]string{
		`{"type":"thread.started","thread_id":"test"}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"cache_write_tokens":2,"output_tokens":5,"reasoning_output_tokens":2}}`,
		`{"type":"turn.completed","usage":{"input_tokens":20,"cached_input_tokens":8,"cache_write_tokens":3,"output_tokens":7,"reasoning_output_tokens":3}}`,
	}, "\n")))
	if err != nil {
		t.Fatalf("parseCodexTokenUsage: %v", err)
	}
	if usage.InputTokens != 20 || usage.CachedInputTokens != 8 || usage.CacheWriteTokens == nil ||
		*usage.CacheWriteTokens != 3 || usage.OutputTokens != 7 || usage.ReasoningOutputTokens != 3 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestParseCodexTokenUsageRequiresCompletedTurn(t *testing.T) {
	_, err := parseCodexTokenUsage([]byte(`{"type":"thread.started","thread_id":"test"}`))
	if err == nil || !strings.Contains(err.Error(), "no turn.completed token usage") {
		t.Fatalf("parse error = %v", err)
	}
}

func TestCodexReviewerReportsCommandFailure(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	command, _ := newCodexTestCommand(t, ReviewOutput{}, "authentication required")
	reviewer := &CodexReviewer{Repository: repository, Model: "test-model", Effort: "low", CommandContext: command}
	_, err = reviewer.Review(context.Background(), ReviewInput{Commit: metadata})
	if err == nil || !strings.Contains(err.Error(), "Codex failed: authentication required") {
		t.Fatalf("Review error = %v", err)
	}
}

func TestCodexReviewerTimesOut(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	command, invocation := newCodexTestCommand(t, ReviewOutput{}, "")
	invocation.Delay = "5s"
	reviewer := &CodexReviewer{
		Repository:     repository,
		Model:          "test-model",
		Effort:         "low",
		Timeout:        100 * time.Millisecond,
		CommandContext: command,
	}
	_, err = reviewer.Review(context.Background(), ReviewInput{Commit: metadata})
	if err == nil || !strings.Contains(err.Error(), "Codex timed out after 100ms") {
		t.Fatalf("Review error = %v", err)
	}
}

func assertArgumentPair(t *testing.T, args []string, name, value string) {
	t.Helper()
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name && args[index+1] == value {
			return
		}
	}
	t.Fatalf("args lack %s %q: %q", name, value, args)
}

func testArgumentValue(args []string, name string) string {
	index := slices.Index(args, name)
	if index < 0 || index+1 == len(args) {
		return ""
	}
	return args[index+1]
}
