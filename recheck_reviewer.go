package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (r *CodexReviewer) Recheck(ctx context.Context, input RecheckInput) (RecheckResult, error) {
	if r.Repository == nil {
		return RecheckResult{}, errors.New("reviewer has no Git repository")
	}
	if err := validateRecheckInput(input); err != nil {
		return RecheckResult{}, err
	}
	binary := strings.TrimSpace(r.Binary)
	if binary == "" {
		binary = "codex"
	}
	model := strings.TrimSpace(r.Model)
	if model == "" {
		return RecheckResult{}, errors.New("Codex model is required")
	}
	effort := strings.TrimSpace(r.Effort)
	if effort == "" {
		return RecheckResult{}, errors.New("Codex reasoning effort is required")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultCodexTimeout
	}
	recheckContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	prompt, err := buildRecheckPromptWithStatic(input, r.Prompt)
	if err != nil {
		return RecheckResult{}, err
	}
	temporaryDirectory, err := os.MkdirTemp(r.TempDir, "air-codex-recheck-")
	if err != nil {
		return RecheckResult{}, fmt.Errorf("create Codex output directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	schemaPath := filepath.Join(temporaryDirectory, "recheck-schema.json")
	resultPath := filepath.Join(temporaryDirectory, "recheck-result.json")
	if err := os.WriteFile(schemaPath, []byte(recheckOutputSchema), 0o600); err != nil {
		return RecheckResult{}, fmt.Errorf("write Codex output schema: %w", err)
	}
	args := []string{
		"exec", "-C", r.Repository.WorkTree,
		"--sandbox", "read-only", "--config", `approval_policy="never"`,
	}
	if profile := strings.TrimSpace(r.Profile); profile != "" {
		args = append(args, "--profile", profile)
	}
	args = append(args,
		"--model", model,
		"--config", fmt.Sprintf("model_reasoning_effort=%q", effort),
		"--ephemeral", "--color", "never", "--json",
		"--output-schema", schemaPath,
		"--output-last-message", resultPath,
		"-",
	)
	commandContext := r.CommandContext
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	command := commandContext(recheckContext, binary, args...)
	command.Stdin = strings.NewReader(prompt)
	stdout := &limitedWriter{limit: maxCodexTranscriptBytes}
	stderr := &limitedWriter{limit: maxCodexStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return RecheckResult{}, ctxErr
		}
		if errors.Is(recheckContext.Err(), context.DeadlineExceeded) {
			return RecheckResult{}, fmt.Errorf("Codex timed out after %s", timeout)
		}
		detail := strings.TrimSpace(formatLimitedOutput(limitedOutput{data: stderr.data.Bytes(), exceeded: stderr.exceeded}))
		if detail == "" {
			detail = strings.TrimSpace(formatLimitedOutput(limitedOutput{data: stdout.data.Bytes(), exceeded: stdout.exceeded}))
		}
		if detail == "" {
			detail = err.Error()
		}
		return RecheckResult{}, fmt.Errorf("Codex failed: %s", detail)
	}
	resultJSON, err := readLimitedFile(resultPath, maxCodexResultBytes)
	if err != nil {
		return RecheckResult{}, fmt.Errorf("read Codex result: %w", err)
	}
	output, err := parseRecheckOutput(string(resultJSON))
	if err != nil {
		return RecheckResult{}, fmt.Errorf("decode Codex result: %w", err)
	}
	if err := validateRecheckOutput(output, recheckFindingIDs(input.Findings)); err != nil {
		return RecheckResult{}, fmt.Errorf("validate Codex result: %w", err)
	}
	if stdout.exceeded {
		return RecheckResult{}, errors.New("Codex transcript exceeded the limit before token usage could be recorded")
	}
	usage, err := parseCodexTokenUsage(stdout.data.Bytes())
	if err != nil {
		return RecheckResult{}, err
	}
	return RecheckResult{
		Output: output,
		RawResponse: formatLimitedOutput(limitedOutput{
			data: stdout.data.Bytes(), exceeded: stdout.exceeded,
		}),
		Usage: &usage,
	}, nil
}

func validateRecheckInput(input RecheckInput) error {
	if !isHexObjectID(input.HeadSHA) {
		return errors.New("recheck HEAD must be a full Git object ID")
	}
	if len(input.Findings) == 0 {
		return errors.New("recheck requires at least one finding")
	}
	ids := recheckFindingIDs(input.Findings)
	if len(ids) != len(input.Findings) {
		return errors.New("recheck input contains an invalid or duplicate finding ID")
	}
	return nil
}

func recheckFindingIDs(findings []Finding) map[int64]struct{} {
	ids := make(map[int64]struct{}, len(findings))
	for _, finding := range findings {
		if finding.ID > 0 {
			ids[finding.ID] = struct{}{}
		}
	}
	return ids
}

func parseRecheckOutput(content string) (RecheckOutput, error) {
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
	var output RecheckOutput
	if err := decoder.Decode(&output); err != nil {
		return RecheckOutput{}, fmt.Errorf("response is not valid recheck JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return RecheckOutput{}, err
	}
	if output.Findings == nil {
		output.Findings = []RecheckFindingResult{}
	}
	return output, nil
}

func validateRecheckOutput(output RecheckOutput, allowed map[int64]struct{}) error {
	if strings.TrimSpace(output.Summary) == "" {
		return errors.New("model recheck summary must not be empty")
	}
	seen := make(map[int64]struct{}, len(output.Findings))
	for index, result := range output.Findings {
		if result.ID <= 0 || strings.TrimSpace(result.Reason) == "" {
			return fmt.Errorf("findings[%d] requires a positive ID and reason", index)
		}
		if result.Outcome != "resolved" && result.Outcome != "still_present" && result.Outcome != "uncertain" {
			return fmt.Errorf("findings[%d] has invalid outcome %q", index, result.Outcome)
		}
		if _, duplicate := seen[result.ID]; duplicate {
			return fmt.Errorf("recheck output contains duplicate finding #%d", result.ID)
		}
		seen[result.ID] = struct{}{}
		if allowed != nil {
			if _, exists := allowed[result.ID]; !exists {
				return fmt.Errorf("model rechecked finding #%d, which was not supplied", result.ID)
			}
		}
	}
	if allowed != nil {
		if len(seen) != len(allowed) {
			for id := range allowed {
				if _, exists := seen[id]; !exists {
					return fmt.Errorf("model omitted recheck result for finding #%d", id)
				}
			}
		}
	}
	return nil
}

const recheckOutputSchema = `{
  "type": "object",
  "properties": {
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "integer", "minimum": 1},
          "outcome": {"type": "string", "enum": ["resolved", "still_present", "uncertain"]},
          "reason": {"type": "string", "minLength": 1}
        },
        "required": ["id", "outcome", "reason"],
        "additionalProperties": false
      }
    },
    "summary": {"type": "string", "minLength": 1}
  },
  "required": ["findings", "summary"],
  "additionalProperties": false
}`
