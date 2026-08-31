package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	if !strings.Contains(stdout.String(), "review   codex    built-in  4") ||
		!strings.Contains(stdout.String(), "recheck  http     built-in  1") {
		t.Fatalf("prompt list:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"prompt", "show", "review", "--reviewer", "codex"}, environment); err != nil {
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
	if err := runCLI(ctx, []string{"prompt", "set", "review", "--reviewer", "codex", "--file", "review-prompt.txt"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Set review/codex prompt (custom:sha256:") {
		t.Fatalf("prompt set output: %s", stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"prompt", "show", "--reviewer", "codex", "review"}, environment); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Concentrate on transaction boundaries.\n" {
		t.Fatalf("custom prompt = %q", stdout.String())
	}
	stdout.Reset()
	if err := runCLI(ctx, []string{"prompt", "show", "review", "--full", "--reviewer", "codex"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Concentrate on transaction boundaries.\n\n") ||
		!strings.Contains(stdout.String(), "required by the supplied output schema") {
		t.Fatalf("full custom prompt:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"prompt", "reset", "review", "--reviewer", "codex"}, environment); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Reset review/codex prompt to built-in version 4\n" {
		t.Fatalf("prompt reset output = %q", stdout.String())
	}
}

func TestCLICustomPromptIsUsedAndRecorded(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	head := testCommitFile(t, directory, "app.txt", []byte("changed\n"), "change")
	var receivedSystemPrompt string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var wire chatRequest
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		if len(wire.Messages) != 2 {
			t.Fatalf("messages = %+v", wire.Messages)
		}
		receivedSystemPrompt, _ = wire.Messages[0].Content.(string)
		output, _ := json.Marshal(ReviewOutput{
			NewFindings: []NewFinding{}, ResolvedFindings: []ResolvedFinding{}, Summary: "Custom review.",
		})
		return JSONResponse(t, map[string]any{
			"usage": chatUsage(10, 0, 0, 5, 0),
			"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": string(output)},
			}},
		}), nil
	})}
	values := map[string]string{
		"AIR_MODEL": "gpt-5.6-luna", "AIR_BASE_URL": "https://model.example/v1", "AIR_API_KEY": "secret",
	}
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &stdout, Stderr: &bytes.Buffer{},
		Getenv: func(key string) string { return values[key] }, HTTPClient: client,
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	environment.Stdin = strings.NewReader("Focus only on transaction safety.\n")
	if err := runCLI(ctx, []string{"prompt", "set", "review", "--reviewer", "http", "--stdin"}, environment); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(ctx, []string{"scan", "--reviewer", "http"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(receivedSystemPrompt, "Focus only on transaction safety.\n\n") ||
		!strings.Contains(receivedSystemPrompt, `"new_findings"`) ||
		strings.Contains(receivedSystemPrompt, "Changes limited to comments") {
		t.Fatalf("received system prompt:\n%s", receivedSystemPrompt)
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
