package main

import (
	"strings"
	"testing"
)

func TestReviewerPromptsEnforceReviewScope(t *testing.T) {
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
				"complete set of resolution candidates",
				"inspect AIR's",
				"database or return any finding ID",
			} {
				if !strings.Contains(prompt, phrase) {
					t.Fatalf("prompt does not contain %q:\n%s", phrase, prompt)
				}
			}
		})
	}
	if promptVersion != "4" {
		t.Fatalf("prompt version = %q, want 4", promptVersion)
	}
}

func TestRecheckPromptsRequireExactCompleteOutcomes(t *testing.T) {
	for name, prompt := range map[string]string{
		"http":  recheckSystemPrompt,
		"codex": codexRecheckPrompt,
	} {
		t.Run(name, func(t *testing.T) {
			for _, phrase := range []string{
				"exact Git HEAD snapshot",
				"still_present",
				"uncertain",
				"Do not search for or report new defects",
				"every supplied finding",
			} {
				if !strings.Contains(prompt, phrase) {
					t.Fatalf("recheck prompt does not contain %q:\n%s", phrase, prompt)
				}
			}
		})
	}
	if recheckPromptVersion != "1" {
		t.Fatalf("recheck prompt version = %q, want 1", recheckPromptVersion)
	}
}
