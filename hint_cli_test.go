package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCLIHintManagementAndPromptInspection(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.go", []byte("package app\n"), "base")
	var output bytes.Buffer
	environment := cliEnvironment{Cwd: directory, Stdout: &output, Stderr: io.Discard}
	run := func(args ...string) string {
		t.Helper()
		output.Reset()
		if err := runCLI(ctx, args, environment); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}
	run("init", base)
	if got := run("hint", "list"); got != "No active hints.\n" {
		t.Fatalf("initial list = %q", got)
	}
	if got := run("hint", "add", "Protocol is a WIP."); !strings.Contains(got, "Added hint #1") {
		t.Fatalf("add = %q", got)
	}
	if got := run("hint", "list"); !strings.Contains(got, "#1  Protocol is a WIP.") {
		t.Fatalf("list = %q", got)
	}
	if got := run("prompt", "show", "review"); strings.Contains(got, "Protocol is a WIP.") {
		t.Fatal("editable prompt contains hints")
	}
	if got := run("prompt", "show", "--full", "review"); !strings.Contains(got, "Protocol is a WIP.") {
		t.Fatal("full prompt omitted hints")
	}
	run("prompt", "reset", "review")
	if got := run("hint", "list"); !strings.Contains(got, "Protocol is a WIP.") {
		t.Fatal("prompt reset removed hints")
	}
	run("hint", "remove", "1")
	if got := run("hint", "add", "Replacement."); !strings.Contains(got, "Added hint #2") {
		t.Fatalf("reused removed ID: %q", got)
	}
	for _, args := range [][]string{
		{"hint"}, {"hint", "missing"}, {"hint", "add"}, {"hint", "add", ""},
		{"hint", "add", "unquoted", "words"}, {"hint", "remove", "0"},
		{"hint", "remove", "-1"}, {"hint", "remove", "abc"}, {"hint", "remove", "1"},
		{"hint", "list", "extra"}, {"hint", "list", "--prompt", ""},
		{"hint", "list", "--prompt", "hints:sha256:unknown"},
	} {
		if err := runCLI(ctx, args, environment); err == nil {
			t.Errorf("accepted invalid command: %v", args)
		}
	}
}

func TestCLIHintScanFreezesAndRetainsSnapshots(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.go", []byte("package app\n"), "base")
	first := testCommitFile(t, directory, "app.go", []byte("package app\nvar a = 1\n"), "first")
	second := testCommitFile(t, directory, "app.go", []byte("package app\nvar a = 2\n"), "second")
	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.AddHint(ctx, "Original project hint."); err != nil {
		t.Fatal(err)
	}
	commands := make([]commandContextFunc, 3)
	invocations := make([]*codexTestInvocation, 3)
	for index := range commands {
		commands[index], invocations[index] = newCodexTestCommand(t, cleanReview("ok").Output, "")
	}
	calls := 0
	var output bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &output, Stderr: io.Discard,
		Getenv: func(string) string { return "" },
		CodexCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if calls == 0 {
				// Simulate the user editing hints after review configuration was
				// frozen but before even the first model call finishes.
				if err := store.RemoveHint(ctx, 1); err != nil {
					t.Fatal(err)
				}
				if _, err := store.AddHint(ctx, "Next invocation only."); err != nil {
					t.Fatal(err)
				}
			}
			index := calls
			calls++
			if index >= len(commands) {
				t.Fatal("unexpected harness call")
			}
			return commands[index](ctx, name, args...)
		},
	}
	args := []string{"scan", "--model", "test-model", "--effort", "high",
		"--hint", "One-off, with a comma.", "--hint", "Another one-off."}
	if err := runCLI(ctx, append(append([]string(nil), args...), "--dry-run"), environment); err != nil {
		t.Fatal(err)
	}
	var snapshots int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM config WHERE key LIKE 'hint.snapshot.%'`).Scan(&snapshots); err != nil || snapshots != 0 || calls != 0 {
		t.Fatalf("dry run wrote snapshots or called harness: %d/%d, %v", snapshots, calls, err)
	}
	if err := runCLI(ctx, args, environment); err != nil {
		t.Fatal(err)
	}
	wantHints := []ReviewHint{{ID: 1, Text: "Original project hint."}, {Text: "One-off, with a comma."}, {Text: "Another one-off."}}
	var version string
	for index, sha := range []string{first, second} {
		record, err := store.Commit(ctx, sha)
		if err != nil || !reflect.DeepEqual(record.Hints, wantHints) {
			t.Fatalf("commit snapshot = %+v, %v", record.Hints, err)
		}
		if index == 0 {
			version = record.PromptVersion
		} else if record.PromptVersion != version {
			t.Fatal("hint identity changed within scan")
		}
		prompt, err := os.ReadFile(invocations[index].PromptPath)
		if err != nil || !strings.Contains(string(prompt), "Original project hint.") ||
			!strings.Contains(string(prompt), "One-off, with a comma.") || strings.Contains(string(prompt), "Next invocation only.") {
			t.Fatalf("unexpected prompt: %s, %v", prompt, err)
		}
	}
	if err := runCLI(ctx, args, environment); err != nil || calls != 2 {
		t.Fatalf("changed hints silently rescanned commits: %d, %v", calls, err)
	}
	if err := runCLI(ctx, []string{"rescan", first, "--model", "test-model", "--effort", "high"}, environment); err != nil {
		t.Fatal(err)
	}
	attempts, err := store.ReviewAttempts(ctx, first)
	if err != nil || len(attempts) != 2 || !reflect.DeepEqual(attempts[0].Hints, wantHints) ||
		!reflect.DeepEqual(attempts[1].Hints, []ReviewHint{{ID: 2, Text: "Next invocation only."}}) {
		t.Fatalf("retained attempts: %+v, %v", attempts, err)
	}
	output.Reset()
	if err := runCLI(ctx, []string{"show", first, "--review", "1"}, environment); err != nil ||
		!strings.Contains(output.String(), "Hints used:") || !strings.Contains(output.String(), "One-off, with a comma.") {
		t.Fatalf("historical show: %s, %v", output.String(), err)
	}
	output.Reset()
	if err := runCLI(ctx, []string{"show", first, "--review", "1", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var shown showJSONOutput
	if err := json.Unmarshal(output.Bytes(), &shown); err != nil || shown.Review == nil || !reflect.DeepEqual(shown.Review.Hints, wantHints) {
		t.Fatalf("historical JSON: %s, %v", output.String(), err)
	}
	output.Reset()
	if err := runCLI(ctx, []string{"hint", "list", "--prompt", version}, environment); err != nil || !strings.Contains(output.String(), "Original project hint.") {
		t.Fatalf("historical hints: %s, %v", output.String(), err)
	}
	backupPath := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := store.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	backup, err := OpenStore(ctx, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if hints, err := backup.HintsForPrompt(ctx, version); err != nil || !reflect.DeepEqual(hints, wantHints) {
		t.Fatalf("backup snapshot = %+v, %v", hints, err)
	}
	if hints, err := backup.Hints(ctx); err != nil || len(hints) != 1 || hints[0].ID != 2 {
		t.Fatalf("backup active hints = %+v, %v", hints, err)
	}
}

func TestCLIHintPrecheckIsNonPersistent(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.go", []byte("package app\n"), "base")
	store, err := CreateStore(ctx, repository.DatabasePath(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.AddHint(ctx, "Protocol is a WIP."); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "app.go"), []byte("package app\nvar a = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, directory, "add", "app.go")
	command, invocation := newCodexTestCommand(t, cleanReview("ok").Output, "")
	var output bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &output, Stderr: io.Discard,
		Getenv: func(string) string { return "" }, CodexCommand: command,
	}
	if err := runCLI(ctx, []string{"precheck", "--staged", "--format", "json", "--model", "test-model", "--effort", "high", "--hint", "One-off."}, environment); err != nil {
		t.Fatal(err)
	}
	var report precheckReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || len(report.Hints) != 2 || !strings.HasPrefix(report.PromptVersion, "hints:sha256:") {
		t.Fatalf("precheck report = %+v, %v", report, err)
	}
	prompt, err := os.ReadFile(invocation.PromptPath)
	if err != nil || !strings.Contains(string(prompt), "Protocol is a WIP.") || !strings.Contains(string(prompt), "One-off.") {
		t.Fatalf("precheck prompt = %s, %v", prompt, err)
	}
	var snapshots int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM config WHERE key LIKE 'hint.snapshot.%'`).Scan(&snapshots); err != nil || snapshots != 0 {
		t.Fatalf("precheck wrote snapshots: %d, %v", snapshots, err)
	}
	if hints, err := store.Hints(ctx); err != nil || len(hints) != 1 {
		t.Fatalf("precheck saved one-off hint: %+v, %v", hints, err)
	}
}

func TestCLIHintRecheckRetriesFrozenHintsAndResumesByIdentity(t *testing.T) {
	ctx := context.Background()
	repository, store, head := newParallelRecheckFixture(t, 1)
	if _, err := store.AddHint(ctx, "Original recheck hint."); err != nil {
		t.Fatal(err)
	}
	prompt, err := resolveReviewerPrompt(ctx, store, "recheck", "One-off.")
	if err != nil {
		t.Fatal(err)
	}
	fail, failedInvocation := newCodexTestCommand(t, RecheckOutput{}, "temporary failure")
	succeed, successInvocation := newCodexTestCommand(t, RecheckOutput{
		Findings: []RecheckFindingResult{{ID: 1, Outcome: "still_present", Reason: "Still broken."}}, Summary: "Checked.",
	}, "")
	calls := 0
	var output bytes.Buffer
	environment := cliEnvironment{
		Cwd: repository.WorkTree, Stdout: &output, Stderr: io.Discard,
		Getenv: func(string) string { return "" },
		CodexCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			calls++
			if calls == 1 {
				if err := store.RemoveHint(ctx, 1); err != nil {
					t.Error(err)
				}
				if _, err := store.AddHint(ctx, "Next recheck only."); err != nil {
					t.Error(err)
				}
				return fail(ctx, name, args...)
			}
			return succeed(ctx, name, args...)
		},
	}
	args := []string{"recheck", "--model", "test-model", "--effort", "high", "--hint", "One-off."}
	if err := runCLI(ctx, args, environment); err != nil || calls != 2 {
		t.Fatalf("retry calls = %d, %v", calls, err)
	}
	for _, invocation := range []*codexTestInvocation{failedInvocation, successInvocation} {
		text, err := os.ReadFile(invocation.PromptPath)
		if err != nil || !strings.HasPrefix(string(text), prompt.Static) || strings.Contains(string(text), "Next recheck only.") {
			t.Fatalf("retry used changed hints: %s, %v", text, err)
		}
	}
	hints, err := store.HintsForPrompt(ctx, prompt.PromptVersion)
	if err != nil || !reflect.DeepEqual(hints, prompt.Hints) {
		t.Fatalf("recheck snapshot = %+v, %v", hints, err)
	}
	prior, err := store.PreviouslyRecheckedFindingIDs(ctx, head, codexReviewerName, "test-model", "high", prompt.PromptVersion)
	if err != nil || len(prior) != 1 {
		t.Fatalf("recheck identity not recorded: %v, %v", prior, err)
	}
	// The changed active hints make this finding eligible even at the same HEAD.
	if err := runCLI(ctx, args, environment); err != nil || calls != 3 {
		t.Fatalf("changed hints did not recheck: %d, %v", calls, err)
	}
	if err := runCLI(ctx, args, environment); err != nil || calls != 3 {
		t.Fatalf("identical hints did not resume: %d, %v", calls, err)
	}
	if _, err := store.AddHint(ctx, "Temporarily different."); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runCLI(ctx, append(append([]string(nil), args...), "--dry-run"), environment); err != nil || !strings.Contains(output.String(), "1 findings in 1 batches") {
		t.Fatalf("changed hint dry run: %s, %v", output.String(), err)
	}
	if err := store.RemoveHint(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(ctx, args, environment); err != nil || calls != 3 {
		t.Fatalf("restoring hints did not resume prior checks: %d, %v", calls, err)
	}
}

func TestCLIHintParallelRecheckFreezesAllWorkersAndRetries(t *testing.T) {
	ctx := context.Background()
	repository, store, _ := newParallelRecheckFixture(t, 3)
	if _, err := store.AddHint(ctx, "Original parallel hint."); err != nil {
		t.Fatal(err)
	}
	prompt, err := resolveReviewerPrompt(ctx, store, "recheck", "One-off.")
	if err != nil {
		t.Fatal(err)
	}
	// Six independent helper invocations avoid sharing capture paths between
	// workers: three batches, each with an initial attempt and one retry.
	commands := make([]commandContextFunc, 6)
	invocations := make([]*codexTestInvocation, 6)
	for index := range commands {
		commands[index], invocations[index] = newCodexTestCommand(t, RecheckOutput{}, "temporary failure")
	}
	var calls atomic.Int32
	environment := cliEnvironment{
		Cwd: repository.WorkTree, Stdout: io.Discard, Stderr: io.Discard,
		Getenv: func(string) string { return "" },
		CodexCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			index := int(calls.Add(1)) - 1
			if index == 0 {
				if err := store.RemoveHint(ctx, 1); err != nil {
					t.Error(err)
				}
				if _, err := store.AddHint(ctx, "Only for future invocations."); err != nil {
					t.Error(err)
				}
			}
			if index >= len(commands) {
				t.Error("unexpected extra harness invocation")
				return exec.CommandContext(ctx, "air-test-nonexistent-executable")
			}
			return commands[index](ctx, name, args...)
		},
	}
	err = runCLI(ctx, []string{"recheck", "--jobs", "2", "--retry-limit", "1",
		"--model", "test-model", "--effort", "high", "--hint", "One-off."}, environment)
	if err == nil || !strings.Contains(err.Error(), "3 recheck batches failed") || calls.Load() != 6 {
		t.Fatalf("parallel retries = %d, %v", calls.Load(), err)
	}
	for _, invocation := range invocations {
		text, err := os.ReadFile(invocation.PromptPath)
		if err != nil || !strings.HasPrefix(string(text), prompt.Static) || strings.Contains(string(text), "Only for future invocations.") {
			t.Fatalf("worker or retry changed hints: %s, %v", text, err)
		}
	}
	if _, found, err := store.ConfigValue(ctx, hintSnapshotPrefix+prompt.PromptVersion); err != nil || found {
		t.Fatalf("failed recheck retained snapshot: %t, %v", found, err)
	}
}
