package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func newAuditRunner(repository *GitRepository, environment cliEnvironment) auditRunner {
	return newAuditRunnerWithSchema(repository, environment, auditOutputSchema)
}

func newAuditRunnerForScan(repository *GitRepository, environment cliEnvironment, scan auditScan) auditRunner {
	if environment.AuditRunner != nil {
		return environment.AuditRunner
	}
	schema := auditOutputSchema
	if scan.Spec.Recheck != nil {
		schema = auditRecheckOutputSchema
		if scan.Spec.PromptVersion == auditRecheckPromptVersionV1 {
			schema = auditRecheckOutputSchemaV1
		}
	}
	return newAuditRunnerWithSchema(repository, environment, schema)
}

func newAuditRunnerWithSchema(repository *GitRepository, environment cliEnvironment, schema string) auditRunner {
	return func(ctx context.Context, config auditModelConfig, prompt string) (auditInvocation, error) {
		switch config.Harness {
		case "codex":
			return invokeAuditCodexWithSchema(ctx, repository, config, prompt, environment.CodexCommand, schema)
		case "claude":
			var command *exec.Cmd
			r := ClaudeReviewer{Repository: repository, Binary: config.Binary, Model: config.Model, Effort: config.Effort, Timeout: config.Timeout, CommandContext: auditCaptureCommand(environment.ClaudeCommand, &command)}
			result, err := r.invoke(ctx, prompt, schema)
			invocation := auditInvocation{StructuredOutput: string(result.StructuredOutput), RawResponse: result.RawResponse, ReportedCostMicrousd: result.ReportedCost}
			if err == nil {
				invocation.Usage = &result.Usage
			}
			captureAuditStreams(&invocation, command)
			return invocation, err
		case "gemini":
			var command *exec.Cmd
			r := GeminiReviewer{Repository: repository, Binary: config.Binary, Model: config.Model, Effort: config.Effort, Timeout: config.Timeout, CommandContext: auditCaptureCommand(environment.GeminiCommand, &command)}
			result, err := r.invoke(ctx, appendGeminiOutputSchema(prompt, schema))
			invocation := auditInvocation{StructuredOutput: string(result.StructuredOutput), RawResponse: result.RawResponse}
			if err == nil {
				invocation.Usage = &result.Usage
			}
			captureAuditStreams(&invocation, command)
			return invocation, err
		default:
			return auditInvocation{}, fmt.Errorf("unknown runner %q", config.Harness)
		}
	}
}

func auditCaptureCommand(base commandContextFunc, captured **exec.Cmd) commandContextFunc {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*captured = auditCommandContext(base)(ctx, name, args...)
		return *captured
	}
}

func captureAuditStreams(invocation *auditInvocation, command *exec.Cmd) {
	if command == nil {
		return
	}
	if stdout, ok := command.Stdout.(*limitedWriter); ok {
		invocation.RawResponse = formatLimitedOutput(limitedOutput{data: stdout.data.Bytes(), exceeded: stdout.exceeded})
	}
	if stderr, ok := command.Stderr.(*limitedWriter); ok {
		invocation.Stderr = formatLimitedOutput(limitedOutput{data: stderr.data.Bytes(), exceeded: stderr.exceeded})
	}
}

func auditCommandContext(base commandContextFunc) commandContextFunc {
	if base == nil {
		base = exec.CommandContext
	}
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		command := base(ctx, name, args...)
		configureAuditProcess(command)
		return command
	}
}

// Reuse AIR's bounded capture and usage parser with an assignment-specific
// prompt/schema. Preserve capture even on cancellation or malformed output.
func invokeAuditCodex(ctx context.Context, repository *GitRepository, config auditModelConfig, prompt string, commandContext commandContextFunc) (result auditInvocation, returnedErr error) {
	return invokeAuditCodexWithSchema(ctx, repository, config, prompt, commandContext, auditOutputSchema)
}

func invokeAuditCodexWithSchema(ctx context.Context, repository *GitRepository, config auditModelConfig, prompt string, commandContext commandContextFunc, schema string) (result auditInvocation, returnedErr error) {
	callContext, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()
	temporary, err := os.MkdirTemp("", "repose-codex-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(temporary)
	schemaPath, resultPath := filepath.Join(temporary, "schema.json"), filepath.Join(temporary, "result.json")
	if err = os.WriteFile(schemaPath, []byte(schema), 0o600); err != nil {
		return result, err
	}
	args := []string{"exec", "-C", repository.WorkTree, "--sandbox", "read-only", "--ignore-user-config", "--ignore-rules",
		"--config", `approval_policy="never"`, "--config", "project_doc_max_bytes=0", "--config", `web_search="disabled"`, "--config", "features.multi_agent=false",
		"--model", config.Model, "--config", fmt.Sprintf("model_reasoning_effort=%q", config.Effort),
		"--ephemeral", "--color", "never", "--json", "--output-schema", schemaPath, "--output-last-message", resultPath, "-"}
	command := auditCommandContext(commandContext)(callContext, config.Binary, args...)
	command.Stdin = strings.NewReader(prompt)
	stdout, stderr := &limitedWriter{limit: maxCodexTranscriptBytes}, &limitedWriter{limit: maxCodexStderrBytes}
	command.Stdout, command.Stderr = stdout, stderr
	runErr := command.Run()
	result.RawResponse = formatLimitedOutput(limitedOutput{data: stdout.data.Bytes(), exceeded: stdout.exceeded})
	result.Stderr = formatLimitedOutput(limitedOutput{data: stderr.data.Bytes(), exceeded: stderr.exceeded})
	if usage, err := parseCodexTokenUsage(stdout.data.Bytes()); err == nil {
		result.Usage = &usage
	}
	data, readErr := readLimitedFile(resultPath, maxCodexResultBytes)
	if readErr == nil {
		result.StructuredOutput = string(data)
	}
	if runErr != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if errors.Is(callContext.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("Codex timed out after %s", config.Timeout)
		}
		return result, fmt.Errorf("Codex failed: %w: %s", runErr, strings.TrimSpace(result.Stderr))
	}
	if readErr != nil {
		return result, fmt.Errorf("read Codex result: %w", readErr)
	}
	if stdout.exceeded {
		return result, errors.New("Codex transcript exceeded capture limit")
	}
	return result, nil
}
