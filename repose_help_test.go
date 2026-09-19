package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestReposeHelpWorksWithoutRepositoryState(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		contains []string
		excludes []string
	}{
		{
			name: "global", args: []string{"help"},
			contains: []string{"Usage:\n  repose COMMAND [OPTIONS]", "Repository inventory:", "Repository review:", "inventory  Build, inspect", "version    Print the Repose version."},
		},
		{
			name: "global flag", args: []string{"--help"},
			contains: []string{"Finding review:", "Reports:", `Run "repose help COMMAND" for command details.`},
		},
		{
			name: "help command flag", args: []string{"help", "--help"},
			contains: []string{"Usage:\n  repose COMMAND [OPTIONS]", "Configuration and maintenance:"},
		},
		{
			name: "scan create topic", args: []string{"help", "scan", "create"},
			contains: []string{"repose scan create --model MODEL [OPTIONS]", "--inventory ID", "--instructions FILE", "--hint TEXT", "--timeout DURATION"},
			excludes: []string{"Repository inventory:", "flag: help requested"},
		},
		{
			name: "nested command flag", args: []string{"scan", "create", "--help"},
			contains: []string{"repose scan create --model MODEL [OPTIONS]", "--harness HARNESS", "-h, --help"},
		},
		{
			name: "short command flag", args: []string{"recheck", "-h"},
			contains: []string{"repose recheck [OPTIONS] [FINDING_ID ...]", "--batch-max N", "--create-only"},
		},
		{
			name: "flag after option", args: []string{"recheck", "--model", "test-model", "--help"},
			contains: []string{"repose recheck [OPTIONS] [FINDING_ID ...]", "--model MODEL"},
		},
		{
			name: "inventory parent", args: []string{"inventory", "--help"},
			contains: []string{"repose inventory COMMAND [ARGUMENTS]", "Commands:", "build", "index", "policy"},
		},
		{
			name: "inventory build", args: []string{"inventory", "build", "--help"},
			contains: []string{"repose inventory build [OPTIONS]", "--compile-commands FILE", "--policy FILE"},
		},
		{
			name: "deep topic", args: []string{"help", "inventory", "policy", "import"},
			contains: []string{"repose inventory policy import FILE [OPTIONS]", "Replace current policy"},
		},
		{
			name: "finding source", args: []string{"finding", "source", "--help"},
			contains: []string{"repose finding source ID [OPTIONS]", "--context N", "recorded snapshot"},
		},
		{
			name: "model pricing", args: []string{"model", "set-pricing", "--help"},
			contains: []string{"repose model set-pricing NAME [OPTIONS]", "--short-cached-input PRICE", "--long-output PRICE"},
		},
		{
			name: "prompt show", args: []string{"prompt", "show", "--help"},
			contains: []string{"repose prompt show [--full] scan|recheck [OPTIONS]", "--full"},
		},
		{
			name: "hint list", args: []string{"hint", "list", "--help"},
			contains: []string{"repose hint list [OPTIONS]", "--scan ID|latest"},
		},
		{
			name: "backup import", args: []string{"backup", "import", "--help"},
			contains: []string{"repose backup import [OPTIONS] PATH", "--force", "Validate schema"},
		},
		{
			name: "database path", args: []string{"db", "path", "--help"},
			contains: []string{"repose db path [OPTIONS]", "--repo DIR", "before Repose has created"},
		},
		{
			name: "version", args: []string{"version", "--help"},
			contains: []string{"Usage:\n  repose version", "Print the Repose version."},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			environment := cliEnvironment{Cwd: t.TempDir(), Stdout: &stdout, Stderr: &stderr}
			if err := runReposeCLI(context.Background(), test.args, environment); err != nil {
				t.Fatalf("runReposeCLI(%q): %v", test.args, err)
			}
			for _, expected := range test.contains {
				if !strings.Contains(stdout.String(), expected) {
					t.Errorf("help output does not contain %q:\n%s", expected, stdout.String())
				}
			}
			for _, excluded := range test.excludes {
				if strings.Contains(stdout.String(), excluded) {
					t.Errorf("help output unexpectedly contains %q:\n%s", excluded, stdout.String())
				}
			}
			if stderr.Len() != 0 {
				t.Errorf("help wrote stderr: %s", stderr.String())
			}
		})
	}
}

func TestReposeHelpRejectsUnknownTopics(t *testing.T) {
	environment := cliEnvironment{Cwd: t.TempDir(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	for _, args := range [][]string{{"help", "missing"}, {"help", "inventory", "missing"}, {"help", "inventory", "policy", "missing"}} {
		err := runReposeCLI(context.Background(), args, environment)
		if err == nil || !strings.Contains(err.Error(), "unknown help topic") {
			t.Errorf("runReposeCLI(%q) error = %v", args, err)
		}
	}
}

func TestReposeHelpRegistryIsComplete(t *testing.T) {
	categories := make(map[string]bool, len(reposeCategoryOrder))
	for _, category := range reposeCategoryOrder {
		if category == "" || categories[category] {
			t.Fatalf("invalid command category %q", category)
		}
		categories[category] = true
	}
	expected := map[string][]string{
		"doctor": nil, "status": nil, "version": nil,
		"inventory": {"build", "list", "show", "files", "tree", "groups", "inspect", "group", "annotate", "plan", "browse", "index", "index-status", "symbols", "includes", "check", "approve", "exclude", "include", "exclusions", "policy"},
		"scan":      {"create", "run", "resume", "list", "show", "tasks", "attempts", "prompt", "pause", "interrupt"},
		"recheck":   nil,
		"findings":  nil,
		"finding":   {"list", "show", "dismiss", "reopen", "note", "source", "open", "tag", "untag"},
		"tags":      nil,
		"stats":     nil,
		"cost":      nil,
		"export":    nil,
		"prompt":    {"list", "show", "set", "reset"},
		"hint":      {"add", "list", "remove"},
		"model":     {"list", "show", "set-pricing", "mark-pricing-unknown"},
		"db":        {"path"},
		"backup":    {"import"},
	}
	seen := make(map[string]bool, len(reposeCLICommands))
	for index := range reposeCLICommands {
		command := &reposeCLICommands[index]
		if command.Name == "" || seen[command.Name] {
			t.Errorf("invalid or duplicate command name %q", command.Name)
		}
		seen[command.Name] = true
		if !categories[command.Category] || command.Summary == "" || len(command.Usage) == 0 || command.Run == nil {
			t.Errorf("incomplete command specification: %+v", command)
		}
		want, exists := expected[command.Name]
		if !exists {
			t.Errorf("unexpected command %q", command.Name)
			continue
		}
		validateReposeHelpChildren(t, command.Name, command.Children)
		got := make([]string, 0, len(command.Children))
		for _, child := range command.Children {
			got = append(got, child.Name)
		}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s children = %q, want %q", command.Name, got, want)
		}
	}
	if len(seen) != len(expected) {
		t.Errorf("registered commands = %d, expected %d", len(seen), len(expected))
	}
}

func TestEveryReposeHelpTopicRoutesWithoutRepositoryState(t *testing.T) {
	var visit func([]string, []cliCommandSpec)
	visit = func(parent []string, commands []cliCommandSpec) {
		for _, command := range commands {
			path := append(append([]string(nil), parent...), command.Name)
			t.Run(strings.Join(path, "_"), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				environment := cliEnvironment{Cwd: t.TempDir(), Stdout: &stdout, Stderr: &stderr}
				args := append(append([]string(nil), path...), "--help")
				if err := runReposeCLI(context.Background(), args, environment); err != nil {
					t.Fatalf("runReposeCLI(%q): %v", args, err)
				}
				if !strings.Contains(stdout.String(), command.Usage[0]) {
					t.Errorf("help does not contain usage %q:\n%s", command.Usage[0], stdout.String())
				}
				if stderr.Len() != 0 {
					t.Errorf("help wrote stderr: %s", stderr.String())
				}
			})
			visit(path, command.Children)
		}
	}
	visit(nil, reposeCLICommands)
}

func validateReposeHelpChildren(t *testing.T, parent string, children []cliCommandSpec) {
	t.Helper()
	seen := make(map[string]bool, len(children))
	for _, child := range children {
		if child.Name == "" || seen[child.Name] {
			t.Errorf("%s has invalid or duplicate child name %q", parent, child.Name)
		}
		seen[child.Name] = true
		if child.Summary == "" || len(child.Usage) == 0 {
			t.Errorf("incomplete help specification for %s %s", parent, child.Name)
		}
		validateReposeHelpChildren(t, parent+" "+child.Name, child.Children)
	}
}
