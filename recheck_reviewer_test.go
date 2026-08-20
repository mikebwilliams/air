package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestValidateRecheckOutputRequiresEverySuppliedFinding(t *testing.T) {
	allowed := map[int64]struct{}{1: {}, 2: {}}
	valid := RecheckOutput{Findings: []RecheckFindingResult{
		{ID: 1, Outcome: "resolved", Reason: "The path is repaired."},
		{ID: 2, Outcome: "uncertain", Reason: "The generated source is unavailable."},
	}, Summary: "Checked both findings."}
	if err := validateRecheckOutput(valid, allowed); err != nil {
		t.Fatalf("valid output: %v", err)
	}
	cases := []struct {
		name   string
		output RecheckOutput
		want   string
	}{
		{"missing", RecheckOutput{Findings: valid.Findings[:1], Summary: "Incomplete."}, "omitted recheck result for finding #2"},
		{"invented", RecheckOutput{Findings: []RecheckFindingResult{
			{ID: 1, Outcome: "resolved", Reason: "Fixed."},
			{ID: 3, Outcome: "still_present", Reason: "Present."},
		}, Summary: "Wrong ID."}, "was not supplied"},
		{"outcome", RecheckOutput{Findings: []RecheckFindingResult{
			{ID: 1, Outcome: "maybe", Reason: "Maybe."},
			{ID: 2, Outcome: "still_present", Reason: "Present."},
		}, Summary: "Bad outcome."}, "invalid outcome"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := validateRecheckOutput(test.output, allowed); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCodexReviewerRechecksExactHEAD(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	head := testCommitFile(t, directory, "state.go", []byte("package state\n"), "base")
	output := RecheckOutput{Findings: []RecheckFindingResult{
		{ID: 7, Outcome: "still_present", Reason: "The invalid state remains reachable."},
	}, Summary: "Checked the current implementation."}
	command, invocation := newCodexTestCommand(t, output, "")
	reviewer := &CodexReviewer{
		Repository: repository, Model: "strong-model", Effort: "xhigh",
		CommandContext: command,
	}
	result, err := reviewer.Recheck(context.Background(), RecheckInput{
		HeadSHA: head,
		Findings: []Finding{{
			ID: 7, IntroducedSHA: head, Severity: "warning", Title: "invalid state",
			Description: "The state can become invalid.", File: stringPointer("state.go"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output.Findings) != 1 || result.Output.Findings[0].Outcome != "still_present" {
		t.Fatalf("recheck result = %+v", result)
	}
	prompt, err := os.ReadFile(invocation.PromptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), head) || !strings.Contains(string(prompt), "invalid state") ||
		!strings.Contains(string(prompt), "exact Git HEAD snapshot") {
		t.Fatalf("recheck prompt:\n%s", prompt)
	}
	schema, err := os.ReadFile(invocation.SchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(schema) || !strings.Contains(string(schema), `"still_present"`) {
		t.Fatalf("recheck schema:\n%s", schema)
	}
}

func TestHTTPReviewerRechecksWithHEADOnlyTools(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	head := testCommitFile(t, directory, "state.go", []byte("package state\nconst ready = true\n"), "base")
	callCount := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		callCount++
		var body chatRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Tools) != 3 || body.Tools[0].Function.Name != "git_read_file" {
			t.Fatalf("recheck tools = %+v", body.Tools)
		}
		if callCount == 1 {
			return JSONResponse(t, map[string]any{
				"usage": chatUsage(20, 5, 0, 4, 1),
				"choices": []any{map[string]any{
					"message": map[string]any{
						"role": "assistant", "content": nil,
						"tool_calls": []any{map[string]any{
							"id": "read-head", "type": "function",
							"function": map[string]any{"name": "git_read_file", "arguments": `{"path":"state.go"}`},
						}},
					},
				}},
			}), nil
		}
		last := body.Messages[len(body.Messages)-1]
		if last.Role != "tool" || !strings.Contains(last.Content.(string), "ready = true") {
			t.Fatalf("HEAD tool result = %+v", last)
		}
		content, _ := json.Marshal(RecheckOutput{
			Findings: []RecheckFindingResult{{ID: 4, Outcome: "resolved", Reason: "HEAD initializes the state."}},
			Summary:  "Checked the state at HEAD.",
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(bytes.NewReader(mustJSON(t, map[string]any{
				"usage": chatUsage(30, 10, 0, 6, 2),
				"choices": []any{map[string]any{
					"message": map[string]any{"role": "assistant", "content": string(content)},
				}},
			}))),
		}, nil
	})}
	reviewer := &HTTPReviewer{
		Repository: repository, Model: "http-model", BaseURL: "https://example.invalid/v1",
		APIKey: "secret", Client: client,
	}
	result, err := reviewer.Recheck(context.Background(), RecheckInput{
		HeadSHA:  head,
		Findings: []Finding{{ID: 4, IntroducedSHA: head, Severity: "warning", Title: "state", Description: "State is uninitialized."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 2 || result.Output.Findings[0].Outcome != "resolved" ||
		result.Usage == nil || result.Usage.InputTokens != 50 {
		t.Fatalf("HTTP recheck calls=%d result=%+v", callCount, result)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
