package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type HTTPReviewer struct {
	Repository *GitRepository
	Model      string
	BaseURL    string
	APIKey     string
	Client     *http.Client
	MaxRounds  int
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string             `json:"type"`
	Function chatToolDefinition `json:"function"`
}

type chatToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []toolCall      `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		PromptDetails    struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func (r *HTTPReviewer) Review(ctx context.Context, input ReviewInput) (ReviewResult, error) {
	if r.Repository == nil {
		return ReviewResult{}, errors.New("reviewer has no Git repository")
	}
	if strings.TrimSpace(r.Model) == "" {
		return ReviewResult{}, errors.New("model is required")
	}
	if strings.TrimSpace(r.BaseURL) == "" {
		return ReviewResult{}, errors.New("base URL is required")
	}
	if strings.TrimSpace(r.APIKey) == "" {
		return ReviewResult{}, errors.New("API key is required")
	}
	prompt, err := buildReviewPrompt(input)
	if err != nil {
		return ReviewResult{}, err
	}
	messages := []chatMessage{
		{Role: "system", Content: reviewerSystemPrompt},
		{Role: "user", Content: prompt},
	}
	allowedResolutions := make(map[int64]struct{}, len(input.OpenFindings))
	for _, finding := range input.OpenFindings {
		allowedResolutions[finding.ID] = struct{}{}
	}
	maxRounds := r.MaxRounds
	if maxRounds <= 0 {
		maxRounds = 8
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	var rawResponses []json.RawMessage
	var totalUsage TokenUsage
	repairAttempted := false

	for round := 0; round < maxRounds; round++ {
		response, raw, err := r.complete(ctx, client, messages)
		if err != nil {
			return ReviewResult{}, err
		}
		rawResponses = append(rawResponses, append(json.RawMessage(nil), raw...))
		if response.Usage == nil {
			return ReviewResult{}, errors.New("model response omitted token usage")
		}
		responseUsage := TokenUsage{
			InputTokens:           response.Usage.PromptTokens,
			CachedInputTokens:     response.Usage.PromptDetails.CachedTokens,
			OutputTokens:          response.Usage.CompletionTokens,
			ReasoningOutputTokens: response.Usage.CompletionDetails.ReasoningTokens,
		}
		totalUsage, err = addTokenUsage(totalUsage, responseUsage)
		if err != nil {
			return ReviewResult{}, fmt.Errorf("invalid model token usage: %w", err)
		}
		if len(response.Choices) == 0 {
			return ReviewResult{}, errors.New("model response contained no choices")
		}
		choice := response.Choices[0]
		content, err := extractMessageContent(choice.Message.Content)
		if err != nil {
			return ReviewResult{}, fmt.Errorf("decode model message: %w", err)
		}
		assistantMessage := chatMessage{
			Role:      "assistant",
			Content:   content,
			ToolCalls: choice.Message.ToolCalls,
		}
		messages = append(messages, assistantMessage)

		if len(choice.Message.ToolCalls) > 0 {
			for _, call := range choice.Message.ToolCalls {
				toolResult := r.executeTool(ctx, input.Commit, call)
				messages = append(messages, chatMessage{
					Role:       "tool",
					Content:    toolResult,
					ToolCallID: call.ID,
				})
			}
			continue
		}

		output, err := parseReviewOutput(content)
		if err == nil {
			err = validateReviewOutput(output, allowedResolutions)
		}
		if err == nil {
			rawTranscript, marshalErr := json.Marshal(rawResponses)
			if marshalErr != nil {
				return ReviewResult{}, fmt.Errorf("encode raw model responses: %w", marshalErr)
			}
			return ReviewResult{Output: output, RawResponse: string(rawTranscript), Usage: &totalUsage}, nil
		}
		if repairAttempted {
			return ReviewResult{}, fmt.Errorf("invalid model output after repair: %w", err)
		}
		repairAttempted = true
		messages = append(messages, chatMessage{
			Role: "user",
			Content: "Your final response was invalid: " + err.Error() +
				". Return one corrected JSON object only, without commentary.",
		})
	}
	return ReviewResult{}, fmt.Errorf("model exceeded the limit of %d review rounds", maxRounds)
}

func (r *HTTPReviewer) complete(
	ctx context.Context,
	client *http.Client,
	messages []chatMessage,
) (chatResponse, []byte, error) {
	requestBody := chatRequest{
		Model:       r.Model,
		Messages:    messages,
		Tools:       reviewerTools(),
		ToolChoice:  "auto",
		Temperature: 0,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return chatResponse{}, nil, fmt.Errorf("encode model request: %w", err)
	}
	endpoint, err := chatCompletionsURL(r.BaseURL)
	if err != nil {
		return chatResponse{}, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return chatResponse{}, nil, fmt.Errorf("create model request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+r.APIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return chatResponse{}, nil, fmt.Errorf("call model: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil {
		return chatResponse{}, nil, fmt.Errorf("read model response: %w", err)
	}
	if len(raw) > 2*1024*1024 {
		return chatResponse{}, nil, errors.New("model response exceeded 2 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return chatResponse{}, nil, fmt.Errorf("model returned HTTP %d: %s", response.StatusCode, truncateText(string(raw), 512))
	}
	var decoded chatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return chatResponse{}, nil, fmt.Errorf("decode model response: %w", err)
	}
	if decoded.Error != nil {
		return chatResponse{}, nil, fmt.Errorf("model error: %s", decoded.Error.Message)
	}
	return decoded, raw, nil
}

func chatCompletionsURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid model base URL %q", baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("model base URL must use http or https")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/chat/completions") {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	} else {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/chat/completions"
	}
	return parsed.String(), nil
}

func extractMessageContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("content is neither text nor a text-part array")
	}
	var result strings.Builder
	for _, part := range parts {
		if part.Type == "text" || part.Type == "output_text" || part.Type == "" {
			result.WriteString(part.Text)
		}
	}
	return result.String(), nil
}

func parseReviewOutput(content string) (ReviewOutput, error) {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		firstNewline := strings.IndexByte(content, '\n')
		lastFence := strings.LastIndex(content, "```")
		if firstNewline >= 0 && lastFence > firstNewline {
			content = strings.TrimSpace(content[firstNewline+1 : lastFence])
		}
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var output ReviewOutput
	if err := decoder.Decode(&output); err != nil {
		return ReviewOutput{}, fmt.Errorf("response is not valid review JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return ReviewOutput{}, err
	}
	if output.NewFindings == nil {
		output.NewFindings = []NewFinding{}
	}
	if output.ResolvedFindings == nil {
		output.ResolvedFindings = []ResolvedFinding{}
	}
	return output, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("response contains more than one JSON value")
		}
		return fmt.Errorf("invalid trailing response data: %w", err)
	}
	return nil
}

func reviewerTools() []chatTool {
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{
			"type":                 "object",
			"properties":           properties,
			"required":             required,
			"additionalProperties": false,
		}
	}
	stringProperty := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	revisionProperty := map[string]any{
		"type":        "string",
		"enum":        []string{"commit", "parent"},
		"description": "Repository state to inspect.",
	}
	return []chatTool{
		{
			Type: "function",
			Function: chatToolDefinition{
				Name:        "git_read_file",
				Description: "Read a repository file at the reviewed commit or its first parent. Output is capped.",
				Parameters: object(map[string]any{
					"path":     stringProperty("Clean repository-relative file path."),
					"revision": revisionProperty,
				}, "path", "revision"),
			},
		},
		{
			Type: "function",
			Function: chatToolDefinition{
				Name:        "git_grep",
				Description: "Search for a fixed string at the reviewed commit or its first parent. Output is capped.",
				Parameters: object(map[string]any{
					"pattern":  stringProperty("Literal fixed-string pattern, at most 256 bytes."),
					"revision": revisionProperty,
					"path":     stringProperty("Optional clean repository-relative path to restrict the search."),
				}, "pattern", "revision"),
			},
		},
		{
			Type: "function",
			Function: chatToolDefinition{
				Name:        "git_diff_file",
				Description: "Show this commit's first-parent diff for one repository path. Output is capped.",
				Parameters: object(map[string]any{
					"path": stringProperty("Clean repository-relative file path."),
				}, "path"),
			},
		},
		{
			Type: "function",
			Function: chatToolDefinition{
				Name:        "git_log",
				Description: "Show recent commit metadata at or before the reviewed commit, optionally for one path.",
				Parameters: object(map[string]any{
					"path": stringProperty("Optional clean repository-relative path."),
					"max_count": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     20,
						"description": "Number of commits to return.",
					},
				}, "max_count"),
			},
		},
	}
}

func (r *HTTPReviewer) executeTool(ctx context.Context, commit CommitMetadata, call toolCall) string {
	if call.Type != "function" {
		return toolError(fmt.Errorf("unsupported tool call type %q", call.Type))
	}
	var output string
	var err error
	switch call.Function.Name {
	case "git_read_file":
		var args struct {
			Path     string `json:"path"`
			Revision string `json:"revision"`
		}
		if err = decodeToolArguments(call.Function.Arguments, &args); err == nil {
			var sha string
			sha, err = toolRevision(commit, args.Revision)
			if err == nil {
				output, err = r.Repository.ReadFile(ctx, sha, args.Path)
			}
		}
	case "git_grep":
		var args struct {
			Pattern  string `json:"pattern"`
			Revision string `json:"revision"`
			Path     string `json:"path"`
		}
		if err = decodeToolArguments(call.Function.Arguments, &args); err == nil {
			var sha string
			sha, err = toolRevision(commit, args.Revision)
			if err == nil {
				output, err = r.Repository.Grep(ctx, sha, args.Pattern, args.Path)
			}
		}
	case "git_diff_file":
		var args struct {
			Path string `json:"path"`
		}
		if err = decodeToolArguments(call.Function.Arguments, &args); err == nil {
			output, err = r.Repository.DiffFile(ctx, commit.ParentSHA, commit.SHA, args.Path)
		}
	case "git_log":
		var args struct {
			Path     string `json:"path"`
			MaxCount int    `json:"max_count"`
		}
		if err = decodeToolArguments(call.Function.Arguments, &args); err == nil {
			output, err = r.Repository.Log(ctx, commit.SHA, args.Path, args.MaxCount)
		}
	default:
		err = fmt.Errorf("unknown tool %q", call.Function.Name)
	}
	if err != nil {
		return toolError(err)
	}
	encoded, _ := json.Marshal(map[string]any{"ok": true, "output": output})
	return string(encoded)
}

func decodeToolArguments(arguments string, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return ensureJSONEOF(decoder)
}

func toolRevision(commit CommitMetadata, revision string) (string, error) {
	switch revision {
	case "commit":
		return commit.SHA, nil
	case "parent":
		return commit.ParentSHA, nil
	default:
		return "", errors.New("revision must be commit or parent")
	}
}

func toolError(err error) string {
	encoded, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	return string(encoded)
}

func truncateText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum] + "..."
}
