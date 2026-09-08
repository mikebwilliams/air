package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCLIHelpWorksWithoutRepositoryState(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		contains []string
		excludes []string
	}{
		{
			name: "global", args: []string{"help"},
			contains: []string{"Usage:\n  air COMMAND [OPTIONS]", "air --version", "Getting started:", "Commit review:", "scan       Review unprocessed commits on master.", "version    Print the AIR version."},
		},
		{
			name: "global flag", args: []string{"--help"},
			contains: []string{"History and reports:", `Run "air help COMMAND" for command details.`},
		},
		{
			name: "help command flag", args: []string{"help", "--help"},
			contains: []string{"Usage:\n  air COMMAND [OPTIONS]", "Finding review:"},
		},
		{
			name: "command topic", args: []string{"help", "scan"},
			contains: []string{"air scan [OPTIONS] [FROM..TO]", "--stop-on-error", "--harness HARNESS", "--codex-timeout DURATION", "--claude-timeout DURATION", "--gemini-timeout DURATION"},
			excludes: []string{"Getting started:", "flag: help requested"},
		},
		{
			name: "command flag", args: []string{"scan", "--help"},
			contains: []string{"air scan [OPTIONS] [FROM..TO]", "--model MODEL", "-h, --help"},
		},
		{
			name: "short command flag", args: []string{"scan", "-h"},
			contains: []string{"air scan [OPTIONS] [FROM..TO]", "--limit N"},
		},
		{
			name: "flag after another option", args: []string{"scan", "--model", "test-model", "--help"},
			contains: []string{"air scan [OPTIONS] [FROM..TO]", "--model MODEL"},
		},
		{
			name: "parent command", args: []string{"config", "--help"},
			contains: []string{"air config COMMAND [ARGUMENTS]", "Commands:", "get", "set", "unset", "list"},
		},
		{
			name: "nested topic", args: []string{"help", "finding", "dismiss"},
			contains: []string{"air finding dismiss --reason TEXT FINDING_ID", "Dismiss an open finding", "--reason TEXT"},
		},
		{
			name: "finding list topic", args: []string{"finding", "list", "--help"},
			contains: []string{"air finding list [OPTIONS]", "--status STATUS", "--severity SEVERITY", "--sort SORT", "--json"},
		},
		{
			name: "nested flag", args: []string{"finding", "dismiss", "--help"},
			contains: []string{"air finding dismiss --reason TEXT FINDING_ID", "Dismiss an open finding"},
		},
		{
			name: "database nested flag", args: []string{"model", "set-pricing", "--help"},
			contains: []string{"air model set-pricing [OPTIONS] NAME", "--short-cached-input PRICE", "--long-output PRICE"},
		},
		{
			name: "prompt nested flag", args: []string{"prompt", "show", "--help"},
			contains: []string{"air prompt show [--full] KIND", "--full"},
		},
		{
			name: "backup import flag", args: []string{"backup", "import", "--help"},
			contains: []string{"air backup import [OPTIONS] PATH", "Validate and import", "--force"},
		},
		{
			name: "version command", args: []string{"version", "--help"},
			contains: []string{"Usage:\n  air version", "Print AIR's release version and exit."},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			environment := cliEnvironment{
				Cwd: t.TempDir(), Stdout: &stdout, Stderr: &stderr,
				Getenv: func(string) string { return "" },
			}
			if err := runCLI(context.Background(), test.args, environment); err != nil {
				t.Fatalf("runCLI(%q): %v", test.args, err)
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

func TestCLIHelpRejectsUnknownTopics(t *testing.T) {
	environment := cliEnvironment{
		Cwd: t.TempDir(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" },
	}
	for _, args := range [][]string{{"help", "missing"}, {"help", "finding", "missing"}} {
		err := runCLI(context.Background(), args, environment)
		if err == nil || !strings.Contains(err.Error(), "unknown help topic") {
			t.Errorf("runCLI(%q) error = %v", args, err)
		}
	}
}

func TestCLICommandRegistryIsComplete(t *testing.T) {
	categories := make(map[string]bool, len(commandCategoryOrder))
	for _, category := range commandCategoryOrder {
		if category == "" || categories[category] {
			t.Fatalf("invalid command category %q", category)
		}
		categories[category] = true
	}
	seen := make(map[string]bool, len(cliCommands))
	for index := range cliCommands {
		command := &cliCommands[index]
		if command.Name == "" || seen[command.Name] {
			t.Errorf("invalid or duplicate command name %q", command.Name)
		}
		seen[command.Name] = true
		if !categories[command.Category] {
			t.Errorf("command %q has unknown category %q", command.Name, command.Category)
		}
		if command.Summary == "" || len(command.Usage) == 0 || command.Run == nil {
			t.Errorf("incomplete command specification: %+v", command)
		}
		validateCLIHelpChildren(t, command.Name, command.Children)
	}
	expected := []string{
		"init", "doctor", "status", "version", "pending", "scan", "precheck", "retry", "failures", "rescan", "skip", "clean",
		"recheck", "findings", "finding", "log", "show", "stats", "cost", "export", "prompt", "config", "model", "db", "backup", "reset",
	}
	for _, name := range expected {
		if !seen[name] {
			t.Errorf("command %q is not registered", name)
		}
	}
	if len(seen) != len(expected) {
		t.Errorf("registered commands = %d, expected %d", len(seen), len(expected))
	}
}

func validateCLIHelpChildren(t *testing.T, parent string, children []cliCommandSpec) {
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
		validateCLIHelpChildren(t, parent+" "+child.Name, child.Children)
	}
}
