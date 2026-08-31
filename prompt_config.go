package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const maximumPromptBytes = 256 * 1024

func resolveReviewerPrompt(
	ctx context.Context,
	store *Store,
	kind string,
) (reviewerPrompt, error) {
	prompt, err := reviewerPromptSpec(kind)
	if err != nil {
		return reviewerPrompt{}, err
	}
	instructions, found, err := store.ConfigValue(ctx, prompt.ConfigKey)
	if err != nil {
		return reviewerPrompt{}, err
	}
	if !found {
		return prompt, nil
	}
	instructions, err = validatePromptInstructions(instructions)
	if err != nil {
		return reviewerPrompt{}, fmt.Errorf("invalid database %s prompt: %w", kind, err)
	}
	return prompt.withCustomInstructions(instructions), nil
}

func validatePromptInstructions(value string) (string, error) {
	value = strings.TrimRight(value, "\r\n")
	if strings.TrimSpace(value) == "" {
		return "", errors.New("instructions must not be empty")
	}
	if len(value) > maximumPromptBytes {
		return "", fmt.Errorf("instructions exceed %d KiB", maximumPromptBytes/1024)
	}
	if !utf8.ValidString(value) {
		return "", errors.New("instructions must be valid UTF-8")
	}
	return value, nil
}

func promptVersionOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
