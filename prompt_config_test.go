package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewerPromptSpecsPreserveBuiltinPrompts(t *testing.T) {
	tests := []struct {
		kind     string
		reviewer string
		static   string
		version  string
	}{
		{"review", "codex", codexReviewerPrompt, promptVersion},
		{"review", "http", reviewerSystemPrompt, promptVersion},
		{"recheck", "codex", codexRecheckPrompt, recheckPromptVersion},
		{"recheck", "http", recheckSystemPrompt, recheckPromptVersion},
	}
	for _, test := range tests {
		prompt, err := reviewerPromptSpec(test.kind, test.reviewer)
		if err != nil {
			t.Fatal(err)
		}
		if prompt.Static != test.static || prompt.PromptVersion != test.version || prompt.Source != "built-in" {
			t.Errorf("%s/%s prompt = %+v", test.kind, test.reviewer, prompt)
		}
	}
}

func TestResolveReviewerPromptUsesStableCustomIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const instructions = "Review state-machine changes with special care."
	if err := store.SetConfig(ctx, "prompt.review.codex", instructions+"\n"); err != nil {
		t.Fatal(err)
	}
	first, err := resolveReviewerPrompt(ctx, store, "review", "codex")
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolveReviewerPrompt(ctx, store, "review", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if first.Instructions != instructions || first.Source != "database" {
		t.Fatalf("resolved prompt = %+v", first)
	}
	if first.PromptVersion != second.PromptVersion ||
		!strings.HasPrefix(first.PromptVersion, "custom:sha256:") ||
		len(strings.TrimPrefix(first.PromptVersion, "custom:sha256:")) != 64 {
		t.Fatalf("custom prompt identity = %q, second = %q", first.PromptVersion, second.PromptVersion)
	}
	if !strings.HasPrefix(first.Static, instructions+"\n\n") ||
		!strings.Contains(first.Static, "Return only the JSON object required") ||
		strings.Contains(first.Static, "Changes limited to comments") {
		t.Fatalf("custom static prompt:\n%s", first.Static)
	}

	http := first
	http.Reviewer = "http"
	if first.withCustomInstructions(instructions).PromptVersion == http.withCustomInstructions(instructions).PromptVersion {
		t.Fatal("custom identity does not account for reviewer backend")
	}
}

func TestValidatePromptInstructions(t *testing.T) {
	if value, err := validatePromptInstructions("  keep this indentation\n"); err != nil || value != "  keep this indentation" {
		t.Fatalf("validated instructions = %q, %v", value, err)
	}
	if _, err := validatePromptInstructions(" \n\t\n"); err == nil {
		t.Fatal("empty instructions were accepted")
	}
	if _, err := validatePromptInstructions(string([]byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 instructions were accepted")
	}
}
