package main

import (
	"bytes"
	"context"
	"errors"
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

func TestCLIBackupImportConfirmsAndRestoresDatabase(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: directory, Stdin: strings.NewReader("n\n"), Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	setBackupTestConfig(t, ctx, repository.DatabasePath(), "model", "backed-up-model")
	backupPath := filepath.Join(directory, "restore.sqlite")
	if err := runCLI(ctx, []string{"backup", backupPath}, environment); err != nil {
		t.Fatal(err)
	}
	backupContents, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	setBackupTestConfig(t, ctx, repository.DatabasePath(), "model", "current-model")

	stdout.Reset()
	if err := runCLI(ctx, []string{"backup", "import", backupPath}, environment); err != nil {
		t.Fatalf("cancel import: %v", err)
	}
	if !strings.Contains(stdout.String(), "Replace AIR database ") ||
		!strings.Contains(stdout.String(), "Import cancelled.") {
		t.Fatalf("cancel output = %q", stdout.String())
	}
	if got := backupTestConfig(t, ctx, repository.DatabasePath(), "model"); got != "current-model" {
		t.Fatalf("model after cancelled import = %q", got)
	}

	stdout.Reset()
	environment.Stdin = nil
	if err := runCLI(ctx, []string{"backup", "import", "--force", backupPath}, environment); err != nil {
		t.Fatalf("forced import: %v", err)
	}
	if got := stdout.String(); got != "Imported AIR backup "+backupPath+" to "+repository.DatabasePath()+"\n" {
		t.Fatalf("import output = %q", got)
	}
	if got := backupTestConfig(t, ctx, repository.DatabasePath(), "model"); got != "backed-up-model" {
		t.Fatalf("restored model = %q", got)
	}
	afterContents, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterContents, backupContents) {
		t.Fatal("source backup changed during import")
	}
	info, err := os.Stat(repository.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("imported database permissions = %o", info.Mode().Perm())
	}
}

func TestCLIBackupImportRestoresUninitializedRepository(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	environment := cliEnvironment{Cwd: directory, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	setBackupTestConfig(t, ctx, repository.DatabasePath(), "model", "restored-model")
	backupPath := filepath.Join(directory, "restore.sqlite")
	if err := runCLI(ctx, []string{"backup", backupPath}, environment); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(ctx, []string{"reset", "--force"}, environment); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(ctx, []string{"backup", "import", backupPath}, environment); err != nil {
		t.Fatalf("import into uninitialized repository: %v", err)
	}
	if got := backupTestConfig(t, ctx, repository.DatabasePath(), "model"); got != "restored-model" {
		t.Fatalf("restored model = %q", got)
	}
}

func TestCLIBackupImportRejectsAnotherRepository(t *testing.T) {
	ctx := context.Background()
	sourceRepository, sourceDirectory := newTestGitRepository(t)
	sourceBase := testCommitFile(t, sourceDirectory, "source.txt", []byte("source\n"), "source")
	sourceEnvironment := cliEnvironment{Cwd: sourceDirectory, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := runCLI(ctx, []string{"init", sourceBase}, sourceEnvironment); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(sourceDirectory, "source.sqlite")
	if err := runCLI(ctx, []string{"backup", backupPath}, sourceEnvironment); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sourceRepository.DatabasePath()); err != nil {
		t.Fatal(err)
	}

	targetRepository, targetDirectory := newTestGitRepository(t)
	targetBase := testCommitFile(t, targetDirectory, "target.txt", []byte("target\n"), "target")
	targetEnvironment := cliEnvironment{Cwd: targetDirectory, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := runCLI(ctx, []string{"init", targetBase}, targetEnvironment); err != nil {
		t.Fatal(err)
	}
	setBackupTestConfig(t, ctx, targetRepository.DatabasePath(), "model", "target-model")
	err := runCLI(ctx, []string{"backup", "import", "--force", backupPath}, targetEnvironment)
	if err == nil || !strings.Contains(err.Error(), "is not on the first-parent history of master") {
		t.Fatalf("wrong-repository import error = %v", err)
	}
	if got := backupTestConfig(t, ctx, targetRepository.DatabasePath(), "model"); got != "target-model" {
		t.Fatalf("target model after rejected import = %q", got)
	}
}

func TestCLIBackupImportRejectsInvalidBackupAndActiveScan(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	environment := cliEnvironment{Cwd: directory, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := runCLI(ctx, []string{"init", base}, environment); err != nil {
		t.Fatal(err)
	}
	setBackupTestConfig(t, ctx, repository.DatabasePath(), "model", "current-model")
	invalidPath := filepath.Join(directory, "invalid.sqlite")
	if err := os.WriteFile(invalidPath, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(ctx, []string{"backup", "import", "--force", invalidPath}, environment); err == nil {
		t.Fatal("invalid backup import succeeded")
	}
	if got := backupTestConfig(t, ctx, repository.DatabasePath(), "model"); got != "current-model" {
		t.Fatalf("model after invalid import = %q", got)
	}

	backupPath := filepath.Join(directory, "valid.sqlite")
	if err := runCLI(ctx, []string{"backup", backupPath}, environment); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireScanLock(repository.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	err = runCLI(ctx, []string{"backup", "import", "--force", backupPath}, environment)
	if closeErr := lock.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err == nil || !strings.Contains(err.Error(), "another scan") {
		t.Fatalf("active-scan import error = %v", err)
	}
	if got := backupTestConfig(t, ctx, repository.DatabasePath(), "model"); got != "current-model" {
		t.Fatalf("model after locked import = %q", got)
	}
}

func TestReplaceAIRDatabaseRemovesOldDatabaseAndSidecars(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "reviews.sqlite")
	temporary := filepath.Join(directory, "import.sqlite")
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range sqliteSidecarSuffixes {
		if err := os.WriteFile(destination+suffix, []byte("old"+suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := replaceAIRDatabase(temporary, destination, true); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil || string(contents) != "new" {
		t.Fatalf("installed database = %q, %v", contents, err)
	}
	for _, suffix := range sqliteSidecarSuffixes {
		if _, err := os.Stat(destination + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old sidecar %s remains: %v", suffix, err)
		}
	}
}

func TestReplaceAIRDatabaseRestoresAfterInstallFailure(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "reviews.sqlite")
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range sqliteSidecarSuffixes {
		if err := os.WriteFile(destination+suffix, []byte("old"+suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := replaceAIRDatabase(filepath.Join(directory, "missing.sqlite"), destination, true)
	if err == nil || !strings.Contains(err.Error(), "install imported AIR database") {
		t.Fatalf("replacement error = %v", err)
	}
	contents, readErr := os.ReadFile(destination)
	if readErr != nil || string(contents) != "old" {
		t.Fatalf("restored database = %q, %v", contents, readErr)
	}
	for _, suffix := range sqliteSidecarSuffixes {
		contents, readErr := os.ReadFile(destination + suffix)
		if readErr != nil || string(contents) != "old"+suffix {
			t.Fatalf("restored sidecar %s = %q, %v", suffix, contents, readErr)
		}
	}
	rollbacks, err := filepath.Glob(filepath.Join(directory, ".reviews-rollback-*.sqlite*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rollbacks) != 0 {
		t.Fatalf("rollback files remain: %v", rollbacks)
	}
}

func setBackupTestConfig(t *testing.T, ctx context.Context, databasePath, key, value string) {
	t.Helper()
	store, err := OpenStore(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfig(ctx, key, value); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func backupTestConfig(t *testing.T, ctx context.Context, databasePath, key string) string {
	t.Helper()
	store, err := OpenStore(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Config(ctx, key)
	if closeErr := store.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return value
}
