package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	maxGeminiResultBytes = 8 * 1024 * 1024
	maxGeminiStderrBytes = 64 * 1024
	defaultGeminiTimeout = 20 * time.Minute
)

var geminiReadOnlyGitCommands = []string{
	"git blame",
	"git cat-file",
	"git diff",
	"git grep",
	"git log",
	"git ls-tree",
	"git merge-base",
	"git name-rev",
	"git rev-parse",
	"git show",
	"git status",
}

const geminiReviewPolicy = `
[[rule]]
toolName = "*"
decision = "deny"
priority = 900
modes = ["plan"]
denyMessage = "AIR reviews repositories with read-only tools only"

[[rule]]
toolName = ["read_file", "list_directory", "glob", "grep_search"]
decision = "allow"
priority = 999
modes = ["plan"]

[[rule]]
toolName = "run_shell_command"
commandPrefix = [
  "git blame",
  "git cat-file",
  "git diff",
  "git grep",
  "git log",
  "git ls-tree",
  "git merge-base",
  "git name-rev",
  "git rev-parse",
  "git show",
  "git status",
]
decision = "allow"
priority = 999
modes = ["plan"]

[[rule]]
toolName = "*"
mcpName = "*"
decision = "deny"
priority = 999
modes = ["plan"]
`

type GeminiReviewer struct {
	Repository         *GitRepository
	Binary             string
	Model              string
	Effort             string
	Prompt             string
	Timeout            time.Duration
	TempDir            string
	SystemSettingsPath string
	CommandContext     commandContextFunc
}

type geminiInvocationResult struct {
	StructuredOutput json.RawMessage
	RawResponse      string
	Usage            TokenUsage
}

func (r *GeminiReviewer) Review(ctx context.Context, input ReviewInput) (ReviewResult, error) {
	prompt, err := buildCodexReviewPromptWithStatic(input, r.Prompt)
	if err != nil {
		return ReviewResult{}, err
	}
	prompt = appendGeminiOutputSchema(prompt, codexReviewOutputSchema)
	invocation, err := r.invoke(ctx, prompt)
	if err != nil {
		return ReviewResult{}, err
	}
	output, err := parseReviewOutput(string(invocation.StructuredOutput))
	if err != nil {
		return ReviewResult{}, fmt.Errorf("decode Gemini result: %w", err)
	}
	allowedResolutions := make(map[int64]struct{}, len(input.OpenFindings))
	for _, finding := range input.OpenFindings {
		allowedResolutions[finding.ID] = struct{}{}
	}
	if err := validateReviewOutput(output, allowedResolutions); err != nil {
		return ReviewResult{}, fmt.Errorf("validate Gemini result: %w", err)
	}
	return ReviewResult{
		Output: output, RawResponse: invocation.RawResponse, Usage: &invocation.Usage,
	}, nil
}

func (r *GeminiReviewer) Recheck(ctx context.Context, input RecheckInput) (RecheckResult, error) {
	if err := validateRecheckInput(input); err != nil {
		return RecheckResult{}, err
	}
	prompt, err := buildRecheckPromptWithStatic(input, r.Prompt)
	if err != nil {
		return RecheckResult{}, err
	}
	prompt = appendGeminiOutputSchema(prompt, codexRecheckOutputSchema)
	invocation, err := r.invoke(ctx, prompt)
	if err != nil {
		return RecheckResult{}, err
	}
	output, err := parseRecheckOutput(string(invocation.StructuredOutput))
	if err != nil {
		return RecheckResult{}, fmt.Errorf("decode Gemini result: %w", err)
	}
	if err := validateRecheckOutput(output, recheckFindingIDs(input.Findings)); err != nil {
		return RecheckResult{}, fmt.Errorf("validate Gemini result: %w", err)
	}
	return RecheckResult{
		Output: output, RawResponse: invocation.RawResponse, Usage: &invocation.Usage,
	}, nil
}

func (r *GeminiReviewer) invoke(ctx context.Context, prompt string) (geminiInvocationResult, error) {
	if r.Repository == nil {
		return geminiInvocationResult{}, errors.New("reviewer has no Git repository")
	}
	binary := strings.TrimSpace(r.Binary)
	if binary == "" {
		binary = "gemini"
	}
	model := strings.TrimSpace(r.Model)
	if model == "" {
		return geminiInvocationResult{}, errors.New("Gemini model is required")
	}
	if effort := strings.TrimSpace(r.Effort); effort != "default" {
		return geminiInvocationResult{}, fmt.Errorf(
			"Gemini CLI does not expose per-invocation reasoning effort; use effort %q", "default")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultGeminiTimeout
	}
	temporaryDirectory, err := os.MkdirTemp(r.TempDir, "air-gemini-")
	if err != nil {
		return geminiInvocationResult{}, fmt.Errorf("create Gemini policy directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	policyPath := filepath.Join(temporaryDirectory, "air-read-only.toml")
	if err := os.WriteFile(policyPath, []byte(geminiReviewPolicy), 0o600); err != nil {
		return geminiInvocationResult{}, fmt.Errorf("write Gemini review policy: %w", err)
	}
	settingsPath := filepath.Join(temporaryDirectory, "system-settings.json")
	settings, err := r.geminiSystemSettings()
	if err != nil {
		return geminiInvocationResult{}, err
	}
	if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
		return geminiInvocationResult{}, fmt.Errorf("write Gemini review settings: %w", err)
	}

	args := []string{
		"--model", model,
		"--prompt", "",
		"--output-format", "json",
		"--approval-mode", "plan",
		"--skip-trust",
		"--extensions", "none",
		"--allowed-mcp-server-names", "",
		"--policy", policyPath,
	}
	reviewContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	commandContext := r.CommandContext
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	command := commandContext(reviewContext, binary, args...)
	command.Dir = r.Repository.WorkTree
	command.Stdin = strings.NewReader(prompt)
	command.Env = replaceEnvironmentValue(command.Environ(),
		"GEMINI_CLI_SYSTEM_SETTINGS_PATH", settingsPath)
	stdout := &limitedWriter{limit: maxGeminiResultBytes}
	stderr := &limitedWriter{limit: maxGeminiStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return geminiInvocationResult{}, ctxErr
		}
		if errors.Is(reviewContext.Err(), context.DeadlineExceeded) {
			return geminiInvocationResult{}, fmt.Errorf("Gemini timed out after %s", timeout)
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
		return geminiInvocationResult{}, fmt.Errorf("Gemini failed: %s", detail)
	}
	if stdout.exceeded {
		return geminiInvocationResult{}, errors.New("Gemini result exceeded the output limit")
	}
	structured, usage, err := parseGeminiResult(stdout.data.Bytes(), model)
	if err != nil {
		return geminiInvocationResult{}, err
	}
	return geminiInvocationResult{
		StructuredOutput: structured,
		RawResponse: formatLimitedOutput(limitedOutput{
			data: stdout.data.Bytes(), exceeded: stdout.exceeded,
		}),
		Usage: usage,
	}, nil
}

func (r *GeminiReviewer) geminiSystemSettings() ([]byte, error) {
	settings := make(map[string]any)
	path, required := r.baseGeminiSystemSettingsPath()
	if path != "" {
		contents, err := os.ReadFile(path)
		if err != nil {
			if required || !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("read existing Gemini system settings: %w", err)
			}
		} else if err := json.Unmarshal(contents, &settings); err != nil {
			return nil, fmt.Errorf("decode existing Gemini system settings: %w", err)
		}
	}
	setGeminiSetting(settings, []string{"hooksConfig", "enabled"}, false)
	setGeminiSetting(settings, []string{"skills", "enabled"}, false)
	setGeminiSetting(settings, []string{"general", "plan", "modelRouting"}, false)
	setGeminiSetting(settings, []string{"admin", "secureModeEnabled"}, true)
	setGeminiSetting(settings, []string{"admin", "extensions", "enabled"}, false)
	setGeminiSetting(settings, []string{"admin", "mcp", "enabled"}, false)
	setGeminiSetting(settings, []string{"admin", "skills", "enabled"}, false)
	setGeminiSetting(settings, []string{"mcp", "allowed"}, []string{})
	setGeminiSetting(settings, []string{"useWriteTodos"}, false)
	setGeminiSetting(settings, []string{"security", "toolSandboxing"}, true)
	setGeminiSetting(settings, []string{"security", "disableYoloMode"}, true)
	setGeminiSetting(settings, []string{"security", "disableAlwaysAllow"}, true)
	setGeminiSetting(settings, []string{"tools", "sandboxNetworkAccess"}, false)
	setGeminiSetting(settings, []string{"tools", "shell", "enableInteractiveShell"}, false)
	setGeminiSetting(settings, []string{"advanced", "ignoreLocalEnv"}, true)
	tools := []string{"read_file", "list_directory", "glob", "grep_search"}
	for _, command := range geminiReadOnlyGitCommands {
		tools = append(tools, "run_shell_command("+command+")")
	}
	setGeminiSetting(settings, []string{"tools", "core"}, tools)
	contents, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Gemini review settings: %w", err)
	}
	return append(contents, '\n'), nil
}

func (r *GeminiReviewer) baseGeminiSystemSettingsPath() (string, bool) {
	if path := strings.TrimSpace(r.SystemSettingsPath); path != "" {
		return path, true
	}
	if path := strings.TrimSpace(os.Getenv("GEMINI_CLI_SYSTEM_SETTINGS_PATH")); path != "" {
		return path, true
	}
	switch runtime.GOOS {
	case "linux":
		return "/etc/gemini-cli/settings.json", false
	case "darwin":
		return "/Library/Application Support/GeminiCli/settings.json", false
	case "windows":
		if programData := os.Getenv("ProgramData"); programData != "" {
			return filepath.Join(programData, "gemini-cli", "settings.json"), false
		}
	}
	return "", false
}

func setGeminiSetting(settings map[string]any, path []string, value any) {
	current := settings
	for _, key := range path[:len(path)-1] {
		nested, ok := current[key].(map[string]any)
		if !ok {
			nested = make(map[string]any)
			current[key] = nested
		}
		current = nested
	}
	current[path[len(path)-1]] = value
}

func replaceEnvironmentValue(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

type geminiTokenMetrics struct {
	Input      int64 `json:"input"`
	Prompt     int64 `json:"prompt"`
	Candidates int64 `json:"candidates"`
	Total      int64 `json:"total"`
	Cached     int64 `json:"cached"`
	Thoughts   int64 `json:"thoughts"`
	Tool       int64 `json:"tool"`
}

func appendGeminiOutputSchema(prompt, schema string) string {
	return prompt + "\n\nReturn only one JSON object matching this schema exactly:\n" +
		"<output_schema>\n" + schema + "\n</output_schema>\n"
}

func parseGeminiResult(content []byte, requestedModel string) (json.RawMessage, TokenUsage, error) {
	type modelStats struct {
		Tokens *geminiTokenMetrics `json:"tokens"`
	}
	type wireResult struct {
		Response *string `json:"response"`
		Stats    *struct {
			Models map[string]modelStats `json:"models"`
		} `json:"stats"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	var result wireResult
	if err := decoder.Decode(&result); err != nil {
		return nil, TokenUsage{}, fmt.Errorf("decode Gemini JSON result: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, TokenUsage{}, fmt.Errorf("decode Gemini JSON result: %w", err)
	}
	if result.Error != nil {
		detail := strings.TrimSpace(result.Error.Message)
		if detail == "" {
			detail = strings.TrimSpace(result.Error.Type)
		}
		if detail == "" {
			detail = "unknown error"
		}
		return nil, TokenUsage{}, fmt.Errorf("Gemini failed: %s", detail)
	}
	if result.Response == nil || strings.TrimSpace(*result.Response) == "" {
		return nil, TokenUsage{}, errors.New("Gemini result omitted response")
	}
	if result.Stats == nil || len(result.Stats.Models) == 0 {
		return nil, TokenUsage{}, errors.New("Gemini result omitted per-model token usage")
	}
	if len(result.Stats.Models) != 1 {
		return nil, TokenUsage{}, fmt.Errorf(
			"Gemini used %d models; AIR requires exactly one for unambiguous provenance",
			len(result.Stats.Models))
	}
	metrics, found := result.Stats.Models[requestedModel]
	if !found {
		actual := ""
		for model := range result.Stats.Models {
			actual = model
		}
		return nil, TokenUsage{}, fmt.Errorf(
			"Gemini reported model %q instead of requested model %q", actual, requestedModel)
	}
	if metrics.Tokens == nil {
		return nil, TokenUsage{}, errors.New("Gemini result omitted model token counts")
	}
	promptBreakdown, err := sumGeminiTokens(metrics.Tokens.Input, metrics.Tokens.Cached)
	if err != nil {
		return nil, TokenUsage{}, err
	}
	if promptBreakdown != metrics.Tokens.Prompt {
		return nil, TokenUsage{}, fmt.Errorf(
			"invalid Gemini token usage: prompt is %d but input plus cached is %d",
			metrics.Tokens.Prompt, promptBreakdown)
	}
	inputTokens, err := sumGeminiTokens(metrics.Tokens.Prompt, metrics.Tokens.Tool)
	if err != nil {
		return nil, TokenUsage{}, err
	}
	outputTokens, err := sumGeminiTokens(metrics.Tokens.Candidates, metrics.Tokens.Thoughts)
	if err != nil {
		return nil, TokenUsage{}, err
	}
	usage := TokenUsage{
		InputTokens: inputTokens, CachedInputTokens: metrics.Tokens.Cached,
		CacheWriteTokens: nil, OutputTokens: outputTokens,
		ReasoningOutputTokens: metrics.Tokens.Thoughts,
	}
	if err := validateTokenUsage(usage); err != nil {
		return nil, TokenUsage{}, fmt.Errorf("invalid Gemini token usage: %w", err)
	}
	if metrics.Tokens.Total != 0 && metrics.Tokens.Total != inputTokens+outputTokens {
		return nil, TokenUsage{}, fmt.Errorf(
			"invalid Gemini token usage: total is %d but categorized total is %d",
			metrics.Tokens.Total, inputTokens+outputTokens)
	}
	structured, err := extractGeminiStructuredJSON(*result.Response)
	if err != nil {
		return nil, TokenUsage{}, err
	}
	return structured, usage, nil
}

func sumGeminiTokens(values ...int64) (int64, error) {
	var total int64
	for _, value := range values {
		if value < 0 {
			return 0, errors.New("invalid Gemini token usage: token counts must not be negative")
		}
		if value > math.MaxInt64-total {
			return 0, errors.New("invalid Gemini token usage: token count exceeds storage range")
		}
		total += value
	}
	return total, nil
}

func extractGeminiStructuredJSON(response string) (json.RawMessage, error) {
	value := strings.TrimSpace(response)
	if strings.HasPrefix(value, "```") {
		firstNewline := strings.IndexByte(value, '\n')
		if firstNewline < 0 || !strings.HasSuffix(value, "```") {
			return nil, errors.New("Gemini response contains an incomplete JSON code fence")
		}
		language := strings.TrimSpace(strings.TrimPrefix(value[:firstNewline], "```"))
		if language != "" && !strings.EqualFold(language, "json") {
			return nil, fmt.Errorf("Gemini response used unexpected %q code fence", language)
		}
		value = strings.TrimSpace(strings.TrimSuffix(value[firstNewline+1:], "```"))
	}
	if !json.Valid([]byte(value)) {
		return nil, errors.New("Gemini response is not valid JSON")
	}
	return json.RawMessage(append([]byte(nil), value...)), nil
}
