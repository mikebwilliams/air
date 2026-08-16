package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxCodexResultBytes     = 2 * 1024 * 1024
	maxCodexTranscriptBytes = 8 * 1024 * 1024
	maxCodexStderrBytes     = 64 * 1024
	defaultCodexTimeout     = 20 * time.Minute
)

type commandContextFunc func(context.Context, string, ...string) *exec.Cmd

type CodexReviewer struct {
	Repository     *GitRepository
	Binary         string
	Model          string
	Effort         string
	Profile        string
	TempDir        string
	Timeout        time.Duration
	CommandContext commandContextFunc
}

func (r *CodexReviewer) Review(ctx context.Context, input ReviewInput) (ReviewResult, error) {
	if r.Repository == nil {
		return ReviewResult{}, errors.New("reviewer has no Git repository")
	}
	binary := strings.TrimSpace(r.Binary)
	if binary == "" {
		binary = "codex"
	}
	model := strings.TrimSpace(r.Model)
	if model == "" {
		return ReviewResult{}, errors.New("Codex model is required")
	}
	effort := strings.TrimSpace(r.Effort)
	if effort == "" {
		return ReviewResult{}, errors.New("Codex reasoning effort is required")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultCodexTimeout
	}
	reviewContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	prompt, err := buildCodexReviewPrompt(input)
	if err != nil {
		return ReviewResult{}, err
	}

	temporaryDirectory, err := os.MkdirTemp(r.TempDir, "air-codex-")
	if err != nil {
		return ReviewResult{}, fmt.Errorf("create Codex output directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)

	schemaPath := filepath.Join(temporaryDirectory, "review-schema.json")
	resultPath := filepath.Join(temporaryDirectory, "review-result.json")
	if err := os.WriteFile(schemaPath, []byte(codexReviewOutputSchema), 0o600); err != nil {
		return ReviewResult{}, fmt.Errorf("write Codex output schema: %w", err)
	}

	args := []string{
		"exec",
		"-C", r.Repository.WorkTree,
		"--sandbox", "read-only",
		"--config", `approval_policy="never"`,
	}
	if profile := strings.TrimSpace(r.Profile); profile != "" {
		args = append(args, "--profile", profile)
	}
	args = append(args,
		"--model", model,
		"--config", fmt.Sprintf("model_reasoning_effort=%q", effort),
	)
	args = append(args,
		"--ephemeral",
		"--color", "never",
		"--json",
		"--output-schema", schemaPath,
		"--output-last-message", resultPath,
		"-",
	)

	commandContext := r.CommandContext
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	command := commandContext(reviewContext, binary, args...)
	command.Stdin = strings.NewReader(prompt)
	stdout := &limitedWriter{limit: maxCodexTranscriptBytes}
	stderr := &limitedWriter{limit: maxCodexStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ReviewResult{}, ctxErr
		}
		if errors.Is(reviewContext.Err(), context.DeadlineExceeded) {
			return ReviewResult{}, fmt.Errorf("Codex timed out after %s", timeout)
		}
		detail := strings.TrimSpace(formatLimitedOutput(limitedOutput{
			data:     stderr.data.Bytes(),
			exceeded: stderr.exceeded,
		}))
		if detail == "" {
			detail = strings.TrimSpace(formatLimitedOutput(limitedOutput{
				data:     stdout.data.Bytes(),
				exceeded: stdout.exceeded,
			}))
		}
		if detail == "" {
			detail = err.Error()
		}
		return ReviewResult{}, fmt.Errorf("Codex failed: %s", detail)
	}

	resultJSON, err := readLimitedFile(resultPath, maxCodexResultBytes)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("read Codex result: %w", err)
	}
	output, err := parseReviewOutput(string(resultJSON))
	if err != nil {
		return ReviewResult{}, fmt.Errorf("decode Codex result: %w", err)
	}
	allowedResolutions := make(map[int64]struct{}, len(input.OpenFindings))
	for _, finding := range input.OpenFindings {
		allowedResolutions[finding.ID] = struct{}{}
	}
	if err := validateReviewOutput(output, allowedResolutions); err != nil {
		return ReviewResult{}, fmt.Errorf("validate Codex result: %w", err)
	}
	if stdout.exceeded {
		return ReviewResult{}, errors.New("Codex transcript exceeded the limit before token usage could be recorded")
	}
	usage, err := parseCodexTokenUsage(stdout.data.Bytes())
	if err != nil {
		return ReviewResult{}, err
	}

	return ReviewResult{
		Output: output,
		RawResponse: formatLimitedOutput(limitedOutput{
			data:     stdout.data.Bytes(),
			exceeded: stdout.exceeded,
		}),
		Usage: &usage,
	}, nil
}

func parseCodexTokenUsage(transcript []byte) (TokenUsage, error) {
	type wireTokenUsage struct {
		InputTokens           int64  `json:"input_tokens"`
		CachedInputTokens     int64  `json:"cached_input_tokens"`
		CacheWriteTokens      *int64 `json:"cache_write_tokens"`
		OutputTokens          int64  `json:"output_tokens"`
		ReasoningOutputTokens int64  `json:"reasoning_output_tokens"`
		InputTokenDetails     *struct {
			CacheWriteTokens *int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
	}
	type wireEvent struct {
		Type  string          `json:"type"`
		Usage *wireTokenUsage `json:"usage"`
	}

	var latest *TokenUsage
	scanner := bufio.NewScanner(bytes.NewReader(transcript))
	scanner.Buffer(make([]byte, 64*1024), maxCodexTranscriptBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event wireEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return TokenUsage{}, fmt.Errorf("decode Codex JSON event: %w", err)
		}
		if event.Type != "turn.completed" {
			continue
		}
		if event.Usage == nil {
			return TokenUsage{}, errors.New("Codex turn.completed event omitted token usage")
		}
		candidate := TokenUsage{
			InputTokens:           event.Usage.InputTokens,
			CachedInputTokens:     event.Usage.CachedInputTokens,
			CacheWriteTokens:      event.Usage.CacheWriteTokens,
			OutputTokens:          event.Usage.OutputTokens,
			ReasoningOutputTokens: event.Usage.ReasoningOutputTokens,
		}
		if candidate.CacheWriteTokens == nil && event.Usage.InputTokenDetails != nil {
			candidate.CacheWriteTokens = event.Usage.InputTokenDetails.CacheWriteTokens
		}
		if err := validateTokenUsage(candidate); err != nil {
			return TokenUsage{}, fmt.Errorf("invalid Codex token usage: %w", err)
		}
		latest = &candidate
	}
	if err := scanner.Err(); err != nil {
		return TokenUsage{}, fmt.Errorf("read Codex JSON events: %w", err)
	}
	if latest == nil {
		return TokenUsage{}, errors.New("Codex output contained no turn.completed token usage")
	}
	return *latest, nil
}

func readLimitedFile(filename string, maximum int64) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var contents bytes.Buffer
	if _, err := io.CopyN(&contents, file, maximum+1); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(contents.Len()) > maximum {
		return nil, fmt.Errorf("output exceeded %d bytes", maximum)
	}
	return contents.Bytes(), nil
}

const codexReviewOutputSchema = `{
  "type": "object",
  "properties": {
    "new_findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "severity": {"type": "string", "enum": ["info", "warning", "error"]},
          "title": {"type": "string", "minLength": 1},
          "description": {"type": "string", "minLength": 1},
          "file": {"type": ["string", "null"]},
          "line": {"type": ["integer", "null"], "minimum": 1},
          "symbol": {"type": ["string", "null"]}
        },
        "required": ["severity", "title", "description", "file", "line", "symbol"],
        "additionalProperties": false
      }
    },
    "resolved_findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "integer", "minimum": 1},
          "reason": {"type": "string", "minLength": 1}
        },
        "required": ["id", "reason"],
        "additionalProperties": false
      }
    },
    "summary": {"type": "string", "minLength": 1}
  },
  "required": ["new_findings", "resolved_findings", "summary"],
  "additionalProperties": false
}`
