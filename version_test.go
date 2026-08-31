package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCLIVersionWorksWithoutRepositoryState(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			environment := cliEnvironment{
				Cwd: t.TempDir(), Stdout: &stdout, Stderr: &stderr,
				Getenv: func(string) string { return "" },
			}
			if err := runCLI(context.Background(), args, environment); err != nil {
				t.Fatalf("runCLI(%q): %v", args, err)
			}
			if stdout.String() != "air 0.1\n" {
				t.Errorf("version output = %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("version wrote stderr: %s", stderr.String())
			}
		})
	}
}

func TestCLIVersionRejectsArguments(t *testing.T) {
	environment := cliEnvironment{
		Cwd: t.TempDir(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Getenv: func(string) string { return "" },
	}
	for _, args := range [][]string{{"version", "extra"}, {"--version", "extra"}} {
		err := runCLI(context.Background(), args, environment)
		if err == nil || err.Error() != "usage: air version" {
			t.Errorf("runCLI(%q) error = %v", args, err)
		}
	}
}
