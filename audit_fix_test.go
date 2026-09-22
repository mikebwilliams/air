package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAuditFixQueueStorageCLIAndTUI(t *testing.T) {
	repository, store, _, backend, findings := auditTagFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	model := newFindingsModel(ctx, findingExternalCommands{snapshot: true}, backend, findings, auditFindingDisplay(findings), true, func() time.Time { return now })
	updated, _ := model.handleKey("F")
	model = updated.(findingsModel)
	if !strings.Contains(model.message, "no pending fix") {
		t.Fatalf("missing previous-fix TUI message = %q", model.message)
	}
	firstID := model.selectedID()
	updated, _ = model.handleKey("f")
	model = updated.(findingsModel)
	if !strings.Contains(model.message, "Created fix #1") {
		t.Fatalf("new-fix TUI message = %q", model.message)
	}
	updated, _ = model.handleKey("down")
	model = updated.(findingsModel)
	secondID := model.selectedID()
	if secondID == firstID {
		t.Fatal("TUI did not select a second finding")
	}
	updated, _ = model.handleKey("F")
	model = updated.(findingsModel)
	if !strings.Contains(model.message, "Added finding") || !strings.Contains(model.message, "fix #1") {
		t.Fatalf("previous-fix TUI message = %q", model.message)
	}
	updated, _ = model.handleKey("F")
	model = updated.(findingsModel)
	if !strings.Contains(model.message, "already in fix #1") {
		t.Fatalf("idempotent TUI message = %q", model.message)
	}
	fixes, err := store.auditFixes(ctx)
	if err != nil || len(fixes) != 1 || !slices.Equal(formatFixIDs(fixes[0]), []int64{firstID, secondID}) {
		t.Fatalf("TUI fix = %+v, err=%v", fixes, err)
	}

	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: repository.WorkTree, Stdout: &stdout, Stderr: io.Discard, Now: func() time.Time { return now.Add(time.Minute) }}
	thirdID := findings[0].ID
	if thirdID == firstID || thirdID == secondID {
		thirdID = findings[2].ID
	}
	if err := runReposeCLI(ctx, []string{"fix", "create", fmt.Sprint(secondID), fmt.Sprint(thirdID)}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Created fix #2 with 2 findings") {
		t.Fatalf("fix create output = %q", stdout.String())
	}
	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"fix", "list"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "#1") || !strings.Contains(stdout.String(), "#2") || !strings.Contains(stdout.String(), "pending") {
		t.Fatalf("fix list output = %q", stdout.String())
	}
	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"fix", "show", "2", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var shown auditFix
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil || shown.ID != 2 || !slices.Equal(formatFixIDs(shown), []int64{secondID, thirdID}) {
		t.Fatalf("fix show = %+v, err=%v", shown, err)
	}
	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"fix", "delete", "2"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Deleted pending fix #2") {
		t.Fatalf("fix delete output = %q", stdout.String())
	}
	if err := runReposeCLI(ctx, []string{"fix", "append", "1", fmt.Sprint(thirdID)}, environment); err == nil || !strings.Contains(err.Error(), "unknown fix command") {
		t.Fatalf("unexpected append command result: %v", err)
	}
}

func formatFixIDs(fix auditFix) []int64 {
	ids := make([]int64, len(fix.Findings))
	for index := range fix.Findings {
		ids[index] = fix.Findings[index].ID
	}
	return ids
}

func cloneFixWorktree(t *testing.T, source string) *GitRepository {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "development")
	command := exec.Command("git", "clone", "--quiet", source, destination)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("clone development checkout: %v\n%s", err, output)
	}
	repository, err := DiscoverGitRepository(context.Background(), destination)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func TestAuditFixRunnerIsSequentialDurableAndStopsAtFailures(t *testing.T) {
	scanRepository, store, _, backend, findings := auditTagFixture(t)
	ctx := context.Background()
	development := cloneFixWorktree(t, scanRepository.WorkTree)
	nowValue := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
	now := func() time.Time {
		nowValue = nowValue.Add(time.Second)
		return nowValue
	}
	first, err := backend.CreateFix(ctx, []int64{findings[0].ID, findings[1].ID}, now())
	if err != nil {
		t.Fatal(err)
	}
	second, err := backend.CreateFix(ctx, []int64{findings[2].ID}, now())
	if err != nil {
		t.Fatal(err)
	}
	config := auditModelConfig{Harness: codexReviewerName, Model: "fixer", Effort: "xhigh", Binary: "/bin/true", Timeout: time.Minute}
	var calls []int64
	runner := func(_ context.Context, _ auditModelConfig, prompt string) (auditInvocation, error) {
		fixID := int64(0)
		for _, fix := range []auditFix{first, second} {
			if strings.Contains(prompt, fmt.Sprintf("entry #%d", fix.ID)) {
				fixID = fix.ID
			}
		}
		if fixID == 0 || !strings.Contains(prompt, "untrusted review data") {
			return auditInvocation{}, errors.New("unexpected fix prompt")
		}
		calls = append(calls, fixID)
		output, _ := json.Marshal(auditFixOutput{Status: "completed", Summary: fmt.Sprintf("fixed #%d", fixID), Tests: []string{"focused test passed"}})
		return auditInvocation{StructuredOutput: string(output), Usage: &TokenUsage{InputTokens: 10, OutputTokens: 2}}, nil
	}
	var progress bytes.Buffer
	if err := runAuditFixQueue(ctx, store, scanRepository, development, config, 0, false, runner, &progress, now); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []int64{first.ID, second.ID}) {
		t.Fatalf("fix execution order = %v", calls)
	}
	loaded, err := store.auditFixes(ctx)
	if err != nil || len(loaded) != 2 || loaded[0].Status != "completed" || loaded[1].Status != "completed" ||
		len(loaded[0].Attempts) != 1 || loaded[0].Attempts[0].Output == nil ||
		loaded[0].Attempts[0].PromptVersion != auditFixPromptVersion || !strings.Contains(loaded[0].Attempts[0].Prompt, "Findings:") {
		t.Fatalf("completed fixes = %+v, err=%v", loaded, err)
	}
	events, err := backend.FindingEvents(ctx, findings[0].ID)
	if err != nil || len(events) == 0 || events[len(events)-1].Action != "fix_completed" {
		t.Fatalf("finding fix history = %+v, err=%v", events, err)
	}

	failed, err := backend.CreateFix(ctx, []int64{findings[0].ID}, now())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := backend.CreateFix(ctx, []int64{findings[1].ID}, now())
	if err != nil {
		t.Fatal(err)
	}
	failing := func(context.Context, auditModelConfig, string) (auditInvocation, error) {
		return auditInvocation{Stderr: "fixture failure"}, errors.New("fixture failure")
	}
	if err := runAuditFixQueue(ctx, store, scanRepository, development, config, 0, false, failing, io.Discard, now); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("fix #%d failed", failed.ID)) {
		t.Fatalf("failed fix did not stop queue: %v", err)
	}
	loadedFailed, _ := store.auditFix(ctx, pending.ID)
	if loadedFailed.Status != "pending" {
		t.Fatalf("later fix ran after failure: %+v", loadedFailed)
	}
	calledWithoutRetry := false
	if err := runAuditFixQueue(ctx, store, scanRepository, development, config, 0, false,
		func(context.Context, auditModelConfig, string) (auditInvocation, error) {
			calledWithoutRetry = true
			return auditInvocation{}, nil
		}, io.Discard, now); err == nil || !strings.Contains(err.Error(), "--retry-failed") || calledWithoutRetry {
		t.Fatalf("failed entry did not block later queue work: called=%t err=%v", calledWithoutRetry, err)
	}
	var retried int
	if err := runAuditFixQueue(ctx, store, scanRepository, development, config, 0, true,
		func(context.Context, auditModelConfig, string) (auditInvocation, error) {
			retried++
			output, _ := json.Marshal(auditFixOutput{Status: "completed", Summary: "fixed on retry", Tests: []string{"test passed"}})
			return auditInvocation{StructuredOutput: string(output)}, nil
		}, io.Discard, now); err != nil || retried != 2 {
		t.Fatalf("retry did not finish failed and pending entries: calls=%d err=%v", retried, err)
	}
}

func TestInvokeAuditFixCodexUsesWritableSandbox(t *testing.T) {
	repository, _ := newTestGitRepository(t)
	command := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--sandbox workspace-write") || strings.Contains(joined, "--sandbox read-only") {
			t.Errorf("fix invocation sandbox = %s", joined)
		}
		result := ""
		for index, arg := range args {
			if arg == "--output-last-message" {
				result = args[index+1]
			}
		}
		response := `{"status":"completed","summary":"done","tests":["test passed"]}`
		return exec.CommandContext(ctx, "sh", "-c", `cat >/dev/null; printf '%s' "$1" > "$2"; printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":1}}'`, "fixture", response, result)
	}
	invocation, err := invokeAuditFixCodex(context.Background(), repository,
		auditModelConfig{Harness: codexReviewerName, Model: "fixer", Effort: "high", Binary: "fixture", Timeout: time.Minute},
		"fix this", command)
	if err != nil {
		t.Fatal(err)
	}
	output, err := parseAuditFixOutput(invocation.StructuredOutput)
	if err != nil || output.Status != "completed" || invocation.Usage == nil || invocation.Usage.InputTokens != 3 {
		t.Fatalf("fix invocation = %+v, output=%+v, err=%v", invocation, output, err)
	}
}
