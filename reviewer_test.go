package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestHTTPReviewerUsesReadOnlyToolAndParsesResult(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change state")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := repository.CommitDiff(context.Background(), base, head)
	if err != nil {
		t.Fatal(err)
	}

	callCount := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		callCount++
		var body chatRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.URL.String() != "https://model.example/v1/chat/completions" {
			t.Fatalf("request URL = %s", request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization header = %q", request.Header.Get("Authorization"))
		}
		if callCount == 1 {
			return JSONResponse(t, map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": nil,
						"tool_calls": []any{map[string]any{
							"id":   "call-1",
							"type": "function",
							"function": map[string]any{
								"name":      "git_read_file",
								"arguments": "{\"path\":\"state.txt\",\"revision\":\"commit\"}",
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			}), nil
		}
		if len(body.Messages) == 0 {
			t.Fatal("second request had no messages")
		}
		last := body.Messages[len(body.Messages)-1]
		if last.Role != "tool" || last.ToolCallID != "call-1" {
			t.Fatalf("last message = %+v", last)
		}
		toolContent, ok := last.Content.(string)
		if !ok || !strings.Contains(toolContent, "new") {
			t.Fatalf("tool output = %#v", last.Content)
		}
		final := ReviewOutput{
			NewFindings: []NewFinding{{
				Severity:    "warning",
				Title:       "state regression",
				Description: "The new state violates the caller contract.",
				File:        stringPointer("state.txt"),
			}},
			ResolvedFindings: []ResolvedFinding{},
			Summary:          "Inspected the changed state file.",
		}
		content, _ := json.Marshal(final)
		return JSONResponse(t, map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": string(content),
				},
				"finish_reason": "stop",
			}},
		}), nil
	})}

	reviewer := &HTTPReviewer{
		Repository: repository,
		Model:      "test-model",
		BaseURL:    "https://model.example/v1",
		APIKey:     "secret",
		Client:     client,
	}
	result, err := reviewer.Review(context.Background(), ReviewInput{
		Commit:       metadata,
		Diff:         diff.Text,
		OpenFindings: []Finding{},
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("request count = %d, want 2", callCount)
	}
	if len(result.Output.NewFindings) != 1 || result.Output.NewFindings[0].Title != "state regression" {
		t.Fatalf("unexpected output: %+v", result.Output)
	}
	var transcript []json.RawMessage
	if err := json.Unmarshal([]byte(result.RawResponse), &transcript); err != nil || len(transcript) != 2 {
		t.Fatalf("raw transcript = %q, %v", result.RawResponse, err)
	}
}

func TestHTTPReviewerRepairsInvalidFindingResolution(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := repository.CommitDiff(context.Background(), base, head)
	if err != nil {
		t.Fatal(err)
	}

	responses := []ReviewOutput{
		{
			NewFindings: []NewFinding{},
			ResolvedFindings: []ResolvedFinding{{
				ID:     99,
				Reason: "invented",
			}},
			Summary: "First response.",
		},
		{
			NewFindings:      []NewFinding{},
			ResolvedFindings: []ResolvedFinding{},
			Summary:          "Corrected response.",
		},
	}
	callCount := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if callCount == 1 {
			var requestBody chatRequest
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatal(err)
			}
			last := requestBody.Messages[len(requestBody.Messages)-1]
			if last.Role != "user" || !strings.Contains(last.Content.(string), "not supplied as open") {
				t.Fatalf("missing repair instruction: %+v", last)
			}
		}
		content, _ := json.Marshal(responses[callCount])
		callCount++
		return JSONResponse(t, map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": string(content)},
				"finish_reason": "stop",
			}},
		}), nil
	})}
	reviewer := &HTTPReviewer{
		Repository: repository,
		Model:      "test-model",
		BaseURL:    "https://model.example/v1",
		APIKey:     "secret",
		Client:     client,
	}
	result, err := reviewer.Review(context.Background(), ReviewInput{
		Commit: metadata,
		Diff:   diff.Text,
		OpenFindings: []Finding{{
			ID:            1,
			IntroducedSHA: base,
			Severity:      "warning",
			Title:         "existing",
			Description:   "existing issue",
		}},
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if callCount != 2 || result.Output.Summary != "Corrected response." {
		t.Fatalf("calls=%d, output=%+v", callCount, result.Output)
	}
}

func TestReviewerToolRejectsParentTraversal(t *testing.T) {
	repository, _ := newTestGitRepository(t)
	commit := CommitMetadata{
		SHA:       strings.Repeat("a", 40),
		ParentSHA: strings.Repeat("b", 40),
	}
	reviewer := &HTTPReviewer{Repository: repository}
	result := reviewer.executeTool(context.Background(), commit, toolCall{
		ID:   "bad",
		Type: "function",
		Function: toolFunction{
			Name:      "git_read_file",
			Arguments: "{\"path\":\"../secret\",\"revision\":\"commit\"}",
		},
	})
	if !strings.Contains(result, "\"ok\":false") || !strings.Contains(result, "repository-relative") {
		t.Fatalf("unexpected tool result: %s", result)
	}
}

func JSONResponse(t *testing.T, value any) *http.Response {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
}
