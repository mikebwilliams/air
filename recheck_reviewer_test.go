package main

import (
	"context"
	"encoding/json"
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
