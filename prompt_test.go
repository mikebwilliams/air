package main

import (
	"strings"
	"testing"
)

func TestReviewerPromptsExcludeNonExecutableContent(t *testing.T) {
	for name, prompt := range map[string]string{
		"http":  reviewerSystemPrompt,
		"codex": codexReviewerPrompt,
	} {
		t.Run(name, func(t *testing.T) {
			for _, phrase := range []string{
				"comments, string contents, translations",
				"ignore the excluded",
				"Do not resolve an open finding",
				"without inspecting unrelated",
			} {
				if !strings.Contains(prompt, phrase) {
					t.Fatalf("prompt does not contain %q:\n%s", phrase, prompt)
				}
			}
		})
	}
	if promptVersion != "3" {
		t.Fatalf("prompt version = %q, want 3", promptVersion)
	}
}
