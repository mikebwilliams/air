package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIBackupCreatesPrivateVerifiedSnapshot(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	now := time.Date(2026, 8, 29, 14, 15, 16, 0, time.Local)
	var stdout bytes.Buffer
	environment := cliEnvironment{
		Cwd: directory, Stdout: &stdout, Stderr: &bytes.Buffer{},
		Now: func() time.Time { return now },
	}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}

	// Keep another WAL-mode connection open after a committed write to ensure
	// the backup uses SQLite state rather than copying only the main file.
	source, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.SetConfig(ctx, "model", "backup-model"); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"backup"}, environment); err != nil {
		t.Fatalf("backup: %v", err)
	}
	destination := filepath.Join(directory, "air-backup-20260829-141516.sqlite")
	if stdout.String() != "Created AIR backup "+destination+"\n" {
		t.Fatalf("backup output = %q", stdout.String())
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions = %o", info.Mode().Perm())
	}
	backup, err := OpenStore(ctx, destination)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	value, err := backup.Config(ctx, "model")
	if closeErr := backup.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil || value != "backup-model" {
		t.Fatalf("backup model = %q, %v", value, err)
	}

	stdout.Reset()
	if err := runCLI(ctx, []string{"backup"}, environment); err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "air-backup-20260829-141516-2.sqlite")); err != nil {
		t.Fatalf("numbered backup: %v", err)
	}
}

func TestCLIBackupRefusesToOverwriteDestination(t *testing.T) {
	ctx := context.Background()
	_, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	environment := cliEnvironment{Cwd: directory, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "existing.sqlite")
	if err := os.WriteFile(destination, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runCLI(ctx, []string{"backup", "existing.sqlite"}, environment)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("backup error = %v", err)
	}
	contents, readErr := os.ReadFile(destination)
	if readErr != nil || string(contents) != "keep me" {
		t.Fatalf("existing destination = %q, %v", contents, readErr)
	}
}
