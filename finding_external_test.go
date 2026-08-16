package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFindingExternalCommandConstruction(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	testCommitFile(t, directory, "source.go", []byte("package source\n"), "start")
	sha := testCommitFile(t, directory, "source.go", []byte("package source\n\nfunc broken() {}\n"), "introduce problem")
	testGit(t, directory, "config", "core.editor", "vim --nofork")
	line := 3
	repositoryPath := "source.go"
	finding := Finding{ID: 7, IntroducedSHA: sha, File: &repositoryPath, Line: &line}

	diffCommand, err := buildFindingDiffCommand(ctx, repository, finding, exec.CommandContext)
	if err != nil {
		t.Fatalf("buildFindingDiffCommand: %v", err)
	}
	metadata, err := repository.CommitMetadata(ctx, sha)
	if err != nil {
		t.Fatalf("CommitMetadata: %v", err)
	}
	wantDiffArgs := []string{
		"git", "-C", directory, "--literal-pathspecs", "difftool", "--no-prompt",
		metadata.ParentSHA, sha, "--",
	}
	if !reflect.DeepEqual(diffCommand.Args, wantDiffArgs) {
		t.Fatalf("diff args = %#v, want %#v", diffCommand.Args, wantDiffArgs)
	}

	openCommand, err := buildFindingOpenCommand(ctx, repository, finding, exec.CommandContext)
	if err != nil {
		t.Fatalf("buildFindingOpenCommand: %v", err)
	}
	wantOpenArgs := []string{
		"sh", "-c", `exec vim --nofork "$@"`, "air-editor", "+3", filepath.Join(directory, "source.go"),
	}
	if !reflect.DeepEqual(openCommand.Args, wantOpenArgs) {
		t.Fatalf("open args = %#v, want %#v", openCommand.Args, wantOpenArgs)
	}
	if openCommand.Dir != directory {
		t.Fatalf("open command directory = %q, want %q", openCommand.Dir, directory)
	}
}

func TestFindingOpenCommandValidatesLocation(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	sha := testCommitFile(t, directory, "source.go", []byte("package source\n"), "start")
	testGit(t, directory, "config", "core.editor", "vim")

	if _, err := buildFindingOpenCommand(ctx, repository, Finding{ID: 1, IntroducedSHA: sha}, exec.CommandContext); err == nil ||
		!strings.Contains(err.Error(), "no file location") {
		t.Fatalf("missing location error = %v", err)
	}
	invalid := "../outside.go"
	if _, err := buildFindingOpenCommand(ctx, repository,
		Finding{ID: 2, IntroducedSHA: sha, File: &invalid}, exec.CommandContext); err == nil ||
		!strings.Contains(err.Error(), "invalid file location") {
		t.Fatalf("invalid location error = %v", err)
	}
	missing := "missing.go"
	if _, err := buildFindingOpenCommand(ctx, repository,
		Finding{ID: 3, IntroducedSHA: sha, File: &missing}, exec.CommandContext); err == nil ||
		!strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("missing file error = %v", err)
	}
}

func TestEditorLocationArguments(t *testing.T) {
	line := 42
	tests := []struct {
		editor string
		want   []string
	}{
		{"code --wait", []string{"--goto", "/repo/file.go:42"}},
		{"/usr/bin/nvim -f", []string{"+42", "/repo/file.go"}},
		{"hx", []string{"/repo/file.go:42"}},
		{"kate", []string{"--line", "42", "/repo/file.go"}},
		{"custom-editor", []string{"/repo/file.go"}},
	}
	for _, test := range tests {
		t.Run(test.editor, func(t *testing.T) {
			if got := editorLocationArguments(test.editor, "/repo/file.go", &line); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("arguments = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestFindingDiffAndOpenCLICommands(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	startSHA := testCommitFile(t, directory, "start.go", []byte("package sample\n"), "start")
	sha := testCommitFile(t, directory, "issue.go", []byte("package sample\n"), "issue")
	testGit(t, directory, "config", "core.editor", "vim")
	store, err := CreateStore(ctx, repository.DatabasePath(), startSHA)
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	metadata, err := repository.CommitMetadata(ctx, sha)
	if err != nil {
		t.Fatalf("CommitMetadata: %v", err)
	}
	file := "issue.go"
	line := 1
	ids, err := store.ApplyReview(ctx, metadata,
		ReviewIdentity{Model: modelByName("external-model"), ReasoningEffort: "low"},
		ReviewResult{
			Output: ReviewOutput{NewFindings: []NewFinding{{
				Severity: "warning", Title: "external action", Description: "test", File: &file, Line: &line,
			}}, Summary: "found one"},
			RawResponse: "{}",
			Usage:       &TokenUsage{InputTokens: 2, OutputTokens: 1},
		}, time.Now())
	if err != nil {
		t.Fatalf("ApplyReview: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	t.Setenv("AIR_EXTERNAL_COMMAND_HELPER", "1")
	type call struct {
		name string
		args []string
	}
	var calls []call
	environment := cliEnvironment{
		Cwd:    directory,
		Stdin:  bytes.NewBuffer(nil),
		Stdout: io.Discard,
		Stderr: io.Discard,
		ExternalCommand: func(commandContext context.Context, name string, args ...string) *exec.Cmd {
			calls = append(calls, call{name: name, args: append([]string(nil), args...)})
			return exec.CommandContext(commandContext, os.Args[0], "-test.run=^TestFindingExternalCommandHelper$")
		},
	}
	id := strconv.FormatInt(ids[0], 10)
	if err := runCLI(ctx, []string{"finding", "diff", id}, environment); err != nil {
		t.Fatalf("finding diff: %v", err)
	}
	if err := runCLI(ctx, []string{"finding", "open", id}, environment); err != nil {
		t.Fatalf("finding open: %v", err)
	}
	if len(calls) != 2 || calls[0].name != "git" || calls[1].name != "sh" {
		t.Fatalf("external calls = %+v", calls)
	}
}

func TestFindingExternalCommandHelper(t *testing.T) {
	if os.Getenv("AIR_EXTERNAL_COMMAND_HELPER") != "1" {
		return
	}
	os.Exit(0)
}
