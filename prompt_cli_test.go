package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIPromptManagement(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	var stdout, stderr bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &stdout, Stderr: &stderr,
		Getenv: func(string) string { return "" },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"prompt", "list"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "review   built-in  4") ||
		!strings.Contains(stdout.String(), "recheck  built-in  1") {
		t.Fatalf("prompt list:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"prompt", "show", "review"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Act as AIR") || strings.Contains(stdout.String(), "required by the supplied output schema") {
		t.Fatalf("editable prompt:\n%s", stdout.String())
	}

	customPath := filepath.Join(directory, "review-prompt.txt")
	if err := os.WriteFile(customPath, []byte("Concentrate on transaction boundaries.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"prompt", "set", "review", "--file", "review-prompt.txt"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Set review prompt (custom:sha256:") {
		t.Fatalf("prompt set output: %s", stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"prompt", "show", "review"}, environment); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Concentrate on transaction boundaries.\n" {
		t.Fatalf("custom prompt = %q", stdout.String())
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"prompt", "show", "review", "--full"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Concentrate on transaction boundaries.\n\n") ||
		!strings.Contains(stdout.String(), "required by the supplied output schema") {
		t.Fatalf("full custom prompt:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"prompt", "reset", "review"}, environment); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Reset review prompt to built-in version 4\n" {
		t.Fatalf("prompt reset output = %q", stdout.String())
	}
}

func TestCLICustomPromptIsUsedAndRecorded(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	command, invocation := newCodexTestCommand(t, ReviewOutput{
		NewFindings: []NewFinding{}, ResolvedFindings: []ResolvedFinding{}, Summary: "Custom review.",
	}, "")
	values := map[string]string{
		"AIR_MODEL": "gpt-5.6-luna", "AIR_REASONING_EFFORT": "high",
	}
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &stdout, Stderr: &bytes.Buffer{},
		Getenv: func(key string) string { return values[key] }, CodexCommand: command,
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	environment.Stdin = strings.NewReader("Focus only on transaction safety.\n")
	if err := runCLI(ctx, []string{"prompt", "set", "review", "--stdin"}, environment); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(ctx, []string{"scan"}, environment); err != nil {
		t.Fatal(err)
	}
	receivedPrompt, err := os.ReadFile(invocation.PromptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(receivedPrompt), "Focus only on transaction safety.\n\n") ||
		!strings.Contains(string(receivedPrompt), "required by the supplied output schema") ||
		strings.Contains(string(receivedPrompt), "Changes limited to comments") {
		t.Fatalf("received prompt:\n%s", receivedPrompt)
	}
	repository, err := DiscoverGitRepository(ctx, directory)
	if err != nil {
		t.Fatal(err)
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
	if !strings.HasPrefix(record.PromptVersion, "custom:sha256:") {
		t.Fatalf("recorded prompt version = %q", record.PromptVersion)
	}
	attempts, err := store.ReviewAttempts(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].PromptVersion != record.PromptVersion {
		t.Fatalf("review attempts = %+v, current prompt = %q", attempts, record.PromptVersion)
	}
}
