package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"time"
)

const (
	maxClaudeResultBytes = 8 * 1024 * 1024
	maxClaudeStderrBytes = 64 * 1024
	defaultClaudeTimeout = 20 * time.Minute
)

var claudeAllowedTools = []string{
	"Read",
	"Glob",
	"Grep",
	"Bash(git blame *)",
	"Bash(git cat-file *)",
	"Bash(git diff *)",
	"Bash(git grep *)",
	"Bash(git log *)",
	"Bash(git ls-tree *)",
	"Bash(git merge-base *)",
	"Bash(git name-rev *)",
	"Bash(git rev-parse *)",
	"Bash(git show *)",
	"Bash(git status *)",
}

var claudeDisallowedTools = []string{
	"Edit",
	"Write",
	"WebFetch",
	"WebSearch",
	"Agent",
	"mcp__*",
}

type ClaudeReviewer struct {
	Repository     *GitRepository
	Binary         string
	Model          string
	Effort         string
	Prompt         string
	Timeout        time.Duration
	CommandContext commandContextFunc
}

type claudeInvocationResult struct {
	StructuredOutput json.RawMessage
	RawResponse      string
	Usage            TokenUsage
	ReportedCost     *int64
}

func (r *ClaudeReviewer) Review(ctx context.Context, input ReviewInput) (ReviewResult, error) {
	prompt, err := buildReviewPromptWithStatic(input, r.Prompt)
	if err != nil {
		return ReviewResult{}, err
	}
	invocation, err := r.invoke(ctx, prompt, reviewOutputSchema)
	if err != nil {
		return ReviewResult{}, err
	}
	output, err := parseReviewOutput(string(invocation.StructuredOutput))
	if err != nil {
		return ReviewResult{}, fmt.Errorf("decode Claude result: %w", err)
	}
	allowedResolutions := make(map[int64]struct{}, len(input.OpenFindings))
	for _, finding := range input.OpenFindings {
		allowedResolutions[finding.ID] = struct{}{}
	}
	if err := validateReviewOutput(output, allowedResolutions); err != nil {
		return ReviewResult{}, fmt.Errorf("validate Claude result: %w", err)
	}
	return ReviewResult{
		Output: output, RawResponse: invocation.RawResponse, Usage: &invocation.Usage,
		ReportedCostMicrousd: invocation.ReportedCost,
	}, nil
}

func (r *ClaudeReviewer) Recheck(ctx context.Context, input RecheckInput) (RecheckResult, error) {
	if err := validateRecheckInput(input); err != nil {
		return RecheckResult{}, err
	}
	prompt, err := buildRecheckPromptWithStatic(input, r.Prompt)
	if err != nil {
		return RecheckResult{}, err
	}
	invocation, err := r.invoke(ctx, prompt, recheckOutputSchema)
	if err != nil {
		return RecheckResult{}, err
	}
	output, err := parseRecheckOutput(string(invocation.StructuredOutput))
	if err != nil {
		return RecheckResult{}, fmt.Errorf("decode Claude result: %w", err)
	}
	if err := validateRecheckOutput(output, recheckFindingIDs(input.Findings)); err != nil {
		return RecheckResult{}, fmt.Errorf("validate Claude result: %w", err)
	}
	return RecheckResult{
		Output: output, RawResponse: invocation.RawResponse, Usage: &invocation.Usage,
		ReportedCostMicrousd: invocation.ReportedCost,
	}, nil
}

func (r *ClaudeReviewer) invoke(ctx context.Context, prompt, schema string) (claudeInvocationResult, error) {
	if r.Repository == nil {
		return claudeInvocationResult{}, errors.New("reviewer has no Git repository")
	}
	binary := strings.TrimSpace(r.Binary)
	if binary == "" {
		binary = "claude"
	}
	model := strings.TrimSpace(r.Model)
	if model == "" {
		return claudeInvocationResult{}, errors.New("Claude model is required")
	}
	effort := strings.TrimSpace(r.Effort)
	if effort == "" {
		return claudeInvocationResult{}, errors.New("Claude effort is required")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultClaudeTimeout
	}
	settings, err := claudeReviewSettings(r.Repository.WorkTree)
	if err != nil {
		return claudeInvocationResult{}, err
	}
	args := []string{
		"-p",
		"--model", model,
		"--fallback-model", model,
		"--effort", effort,
		"--output-format", "json",
		"--json-schema", schema,
		"--permission-mode", "dontAsk",
		"--no-session-persistence",
		"--no-chrome",
		"--disable-slash-commands",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--tools", "Read,Grep,Glob,Bash",
		"--allowedTools",
	}
	args = append(args, claudeAllowedTools...)
	args = append(args, "--disallowedTools")
	args = append(args, claudeDisallowedTools...)
	args = append(args, "--settings", settings)

	reviewContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	commandContext := r.CommandContext
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	command := commandContext(reviewContext, binary, args...)
	command.Dir = r.Repository.WorkTree
	command.Stdin = strings.NewReader(prompt)
	stdout := &limitedWriter{limit: maxClaudeResultBytes}
	stderr := &limitedWriter{limit: maxClaudeStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return claudeInvocationResult{}, ctxErr
		}
		if errors.Is(reviewContext.Err(), context.DeadlineExceeded) {
			return claudeInvocationResult{}, fmt.Errorf("Claude timed out after %s", timeout)
		}
		detail := strings.TrimSpace(formatLimitedOutput(limitedOutput{
			data: stderr.data.Bytes(), exceeded: stderr.exceeded,
		}))
		if detail == "" {
			detail = strings.TrimSpace(formatLimitedOutput(limitedOutput{
				data: stdout.data.Bytes(), exceeded: stdout.exceeded,
			}))
		}
		if detail == "" {
			detail = err.Error()
		}
		return claudeInvocationResult{}, fmt.Errorf("Claude failed: %s", detail)
	}
	if stdout.exceeded {
		return claudeInvocationResult{}, errors.New("Claude result exceeded the output limit")
	}
	structured, usage, reportedCost, err := parseClaudeResult(stdout.data.Bytes())
	if err != nil {
		return claudeInvocationResult{}, err
	}
	return claudeInvocationResult{
		StructuredOutput: structured,
		RawResponse: formatLimitedOutput(limitedOutput{
			data: stdout.data.Bytes(), exceeded: stdout.exceeded,
		}),
		Usage: usage, ReportedCost: reportedCost,
	}, nil
}

func claudeReviewSettings(repository string) (string, error) {
	settings := map[string]any{
		"disableAllHooks": true,
		"sandbox": map[string]any{
			"enabled":                  true,
			"failIfUnavailable":        true,
			"allowUnsandboxedCommands": false,
			"autoAllowBashIfSandboxed": false,
			"filesystem": map[string]any{
				"denyWrite": []string{repository},
			},
			"network": map[string]any{
				"allowedDomains": []string{},
			},
		},
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("encode Claude review settings: %w", err)
	}
	return string(encoded), nil
}

func parseClaudeResult(content []byte) (json.RawMessage, TokenUsage, *int64, error) {
	type wireUsage struct {
		InputTokens              int64 `json:"input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
	}
	type wireResult struct {
		Type             string          `json:"type"`
		Subtype          string          `json:"subtype"`
		IsError          bool            `json:"is_error"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Usage            *wireUsage      `json:"usage"`
		TotalCostUSD     json.RawMessage `json:"total_cost_usd"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	var result wireResult
	if err := decoder.Decode(&result); err != nil {
		return nil, TokenUsage{}, nil, fmt.Errorf("decode Claude JSON result: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, TokenUsage{}, nil, fmt.Errorf("decode Claude JSON result: %w", err)
	}
	if result.Type != "result" {
		return nil, TokenUsage{}, nil, fmt.Errorf("Claude output has type %q, expected result", result.Type)
	}
	if result.Subtype != "success" || result.IsError {
		detail := strings.TrimSpace(result.Result)
		if detail == "" {
			detail = result.Subtype
		}
		return nil, TokenUsage{}, nil, fmt.Errorf("Claude failed: %s", detail)
	}
	structured := bytes.TrimSpace(result.StructuredOutput)
	if len(structured) == 0 || bytes.Equal(structured, []byte("null")) {
		return nil, TokenUsage{}, nil, errors.New("Claude result omitted structured_output")
	}
	if result.Usage == nil {
		return nil, TokenUsage{}, nil, errors.New("Claude result omitted token usage")
	}
	inputTokens, err := sumClaudeTokens(
		result.Usage.InputTokens,
		result.Usage.CacheCreationInputTokens,
		result.Usage.CacheReadInputTokens,
	)
	if err != nil {
		return nil, TokenUsage{}, nil, err
	}
	cacheWrites := result.Usage.CacheCreationInputTokens
	usage := TokenUsage{
		InputTokens: inputTokens, CachedInputTokens: result.Usage.CacheReadInputTokens,
		CacheWriteTokens: &cacheWrites, OutputTokens: result.Usage.OutputTokens,
		ReasoningOutputTokens: 0, ReasoningOutputTokensUnreported: true,
	}
	if err := validateTokenUsage(usage); err != nil {
		return nil, TokenUsage{}, nil, fmt.Errorf("invalid Claude token usage: %w", err)
	}
	reportedCost, err := parseClaudeCost(result.TotalCostUSD)
	if err != nil {
		return nil, TokenUsage{}, nil, err
	}
	return append(json.RawMessage(nil), structured...), usage, reportedCost, nil
}

func sumClaudeTokens(values ...int64) (int64, error) {
	var total int64
	for _, value := range values {
		if value < 0 {
			return 0, errors.New("invalid Claude token usage: token counts must not be negative")
		}
		if value > math.MaxInt64-total {
			return 0, errors.New("invalid Claude token usage: token count exceeds storage range")
		}
		total += value
	}
	return total, nil
}

func parseClaudeCost(content json.RawMessage) (*int64, error) {
	content = bytes.TrimSpace(content)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return nil, nil
	}
	var value float64
	if err := json.Unmarshal(content, &value); err != nil {
		return nil, fmt.Errorf("decode Claude total_cost_usd: %w", err)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(math.MaxInt64)/1_000_000 {
		return nil, errors.New("Claude total_cost_usd is outside the supported range")
	}
	microusd := int64(math.Round(value * 1_000_000))
	return &microusd, nil
}
