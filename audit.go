package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const auditPromptVersion = "assignment-v1"
const defaultAuditTimeout = 30 * time.Minute

type auditModelConfig struct {
	Harness string        `json:"harness"`
	Model   string        `json:"model"`
	Effort  string        `json:"effort"`
	Binary  string        `json:"binary"`
	Timeout time.Duration `json:"timeout_ns"`
}

type auditSpec struct {
	Plan          inventoryPlan     `json:"plan"`
	Model         auditModelConfig  `json:"model"`
	PromptVersion string            `json:"prompt_version"`
	Instructions  string            `json:"instructions"`
	Recheck       *auditRecheckSpec `json:"recheck,omitempty"`
}

type auditScan struct {
	ID        string              `json:"id"`
	CreatedAt time.Time           `json:"created_at"`
	Status    string              `json:"status"`
	Control   string              `json:"control,omitempty"`
	Spec      auditSpec           `json:"spec"`
	Counts    map[string]int      `json:"counts"`
	Blocked   *auditProviderLimit `json:"blocked,omitempty"`
	Verdicts  map[string]int      `json:"verdicts,omitempty"`
}

type auditTarget struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	StartByte int64  `json:"start_byte"`
	EndByte   int64  `json:"end_byte"`
}

type auditTaskInput struct {
	Assignment   inventoryAssignment   `json:"assignment"`
	Targets      []auditTarget         `json:"targets"`
	Prompt       string                `json:"prompt"`
	Recheck      *auditRecheckFinding  `json:"recheck,omitempty"`
	RecheckBatch []auditRecheckFinding `json:"recheck_batch,omitempty"`
}

type auditTask struct {
	ID        string         `json:"id"`
	Ordinal   int            `json:"ordinal"`
	Status    string         `json:"status"`
	AttemptID int64          `json:"attempt_id,omitempty"`
	Input     auditTaskInput `json:"input"`
}

type auditOutput struct {
	Status       string               `json:"status"`
	Summary      string               `json:"summary"`
	Findings     []NewFinding         `json:"findings"`
	Recheck      *auditRecheckOutput  `json:"recheck,omitempty"`
	RecheckBatch []auditRecheckOutput `json:"recheck_batch,omitempty"`
}

type auditInvocation struct {
	StructuredOutput     string              `json:"structured_output,omitempty"`
	RawResponse          string              `json:"raw_response,omitempty"`
	Stderr               string              `json:"stderr,omitempty"`
	Usage                *TokenUsage         `json:"usage,omitempty"`
	ReportedCostMicrousd *int64              `json:"reported_cost_microusd,omitempty"`
	ProviderLimit        *auditProviderLimit `json:"provider_limit,omitempty"`
}

type auditAttempt struct {
	ID                   int64           `json:"id"`
	TaskID               string          `json:"task_id"`
	Number               int             `json:"number"`
	Status               string          `json:"status"`
	StartedAt            time.Time       `json:"started_at"`
	Timeout              time.Duration   `json:"timeout_ns,omitempty"`
	FinishedAt           *time.Time      `json:"finished_at,omitempty"`
	DurationMilliseconds int64           `json:"duration_ms"`
	Error                string          `json:"error,omitempty"`
	Invocation           auditInvocation `json:"invocation"`
	Output               *auditOutput    `json:"output,omitempty"`
}

type auditRunner func(context.Context, auditModelConfig, string) (auditInvocation, error)

var errAuditStaleAttempt = errors.New("attempt no longer owns this assignment")

func parseAuditOutput(raw []byte, input auditTaskInput) (auditOutput, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var output auditOutput
	if err := decoder.Decode(&output); err != nil {
		return output, fmt.Errorf("invalid assignment response: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return output, err
	}
	if output.Recheck != nil || output.RecheckBatch != nil {
		return output, errors.New("review assignments cannot return recheck results")
	}
	if output.Status != "completed" && output.Status != "unable_to_assess" {
		return output, errors.New("response status must be completed or unable_to_assess")
	}
	if output.Findings == nil {
		return output, errors.New("response must include findings array")
	}
	if err := validateReviewOutput(ReviewOutput{Summary: output.Summary, NewFindings: output.Findings}, nil); err != nil {
		return output, err
	}
	for _, finding := range output.Findings {
		if finding.File == nil || finding.Line == nil {
			return output, errors.New("each finding requires a target file and line")
		}
		inScope := false
		for _, target := range input.Targets {
			if *finding.File == target.Path && *finding.Line >= target.StartLine && *finding.Line <= target.EndLine {
				inScope = true
			}
		}
		if !inScope {
			return output, fmt.Errorf("finding location %s:%d is outside assignment target lines", *finding.File, *finding.Line)
		}
	}
	return output, nil
}

const auditOutputSchema = `{
 "type":"object", "additionalProperties":false,
 "required":["status","summary","findings"],
 "properties":{
  "status":{"type":"string","enum":["completed","unable_to_assess"]},
  "summary":{"type":"string","minLength":1},
  "findings":{"type":"array","items":{"type":"object","additionalProperties":false,
   "required":["severity","title","description","file","line","symbol"],
   "properties":{
    "severity":{"type":"string","enum":["info","warning","error"]},
    "title":{"type":"string","minLength":1},
    "description":{"type":"string","minLength":1},
    "file":{"type":"string","minLength":1},
    "line":{"type":"integer","minimum":1},
    "symbol":{"type":["string","null"]}
   }}}
 }
}`
