package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReposeBackupCreatesPrivateVerifiedSnapshot(t *testing.T) {
	repository, _, scan, backend, findings := auditTagFixture(t)
	ctx := context.Background()
	if _, err := backend.TagFindings(ctx, []int64{findings[0].ID}, []string{"backup-check"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	caller := t.TempDir()
	now := time.Date(2026, 9, 15, 14, 15, 16, 0, time.Local)
	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: caller, Stdout: &stdout, Stderr: &bytes.Buffer{}, Now: func() time.Time { return now }}
	if err := runReposeCLI(ctx, []string{"backup", "--repo", repository.WorkTree}, environment); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(caller, "repose-backup-20260915-141516.sqlite")
	if stdout.String() != "Created Repose backup "+destination+"\n" {
		t.Fatalf("backup output = %q", stdout.String())
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions = %v, %v", info, err)
	}
	for _, suffix := range sqliteSidecarSuffixes {
		if _, err := os.Stat(destination + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("standalone backup left sidecar %s: %v", suffix, err)
		}
	}
	backup, err := openInventoryReadOnly(ctx, destination)
	if err != nil {
		t.Fatal(err)
	}
	backedUpFindings, err := (&auditFindingStore{reader: backup, scanID: scan.ID}).AllFindings(ctx)
	closeErr := backup.Close()
	if err != nil || closeErr != nil || len(backedUpFindings) != len(findings) || !slices.Contains(backedUpFindings[0].Tags, "backup-check") {
		t.Fatalf("backup contents: %+v, %v, %v", backedUpFindings, err, closeErr)
	}
	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"backup", "--repo", repository.WorkTree}, environment); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(caller, "repose-backup-20260915-141516-2.sqlite")); err != nil {
		t.Fatal("second default backup did not choose a new filename")
	}
	existing := filepath.Join(caller, "existing.sqlite")
	if err := os.WriteFile(existing, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runReposeCLI(ctx, []string{"backup", existing, "--repo", repository.WorkTree}, environment); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing destination error = %v", err)
	}
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "keep me" {
		t.Fatal("backup changed an existing destination")
	}
}

func TestReposeBackupPreservesOlderReadableSchema(t *testing.T) {
	repository, directory := newInventoryFixture(t)
	runReposeRecord(t, directory, "inventory", "build")
	ctx := context.Background()
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openInventoryStore(ctx, database, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "DROP INDEX audit_finding_tags_by_tag; DROP TABLE audit_finding_tags; DROP INDEX audit_finding_attributions_author; DROP TABLE audit_finding_attributions; DROP TABLE models; DROP TABLE config; PRAGMA user_version=6"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "schema-6.sqlite")
	if _, err := executeReposeTest(t, directory, "backup", destination); err != nil {
		t.Fatal(err)
	}
	backup, err := openInventoryReadOnly(ctx, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if backup.version != 6 {
		t.Fatalf("backup schema = %d, want unchanged version 6", backup.version)
	}
	var sourceVersion int
	if err := backup.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&sourceVersion); err != nil || sourceVersion != 6 {
		t.Fatalf("backup schema version = %d, %v", sourceVersion, err)
	}
}

func TestReposeBackupImportConfirmsRestoresAndLocks(t *testing.T) {
	repository, store, scan, backend, findings := auditTagFixture(t)
	ctx := context.Background()
	if _, err := backend.TagFindings(ctx, []int64{findings[0].ID}, []string{"backed-up"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	caller := t.TempDir()
	backupPath := filepath.Join(caller, "restore.sqlite")
	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: caller, Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := runReposeCLI(ctx, []string{"backup", backupPath, "--repo", repository.WorkTree}, environment); err != nil {
		t.Fatal(err)
	}
	backupContents, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.UntagFindings(ctx, []int64{findings[0].ID}, []string{"backed-up"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	environment.Stdin = strings.NewReader("n\n")
	if err := runReposeCLI(ctx, []string{"backup", "import", backupPath, "--repo", repository.WorkTree}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Replace Repose database ") || !strings.Contains(stdout.String(), "Import cancelled.") {
		t.Fatalf("confirmation output = %q", stdout.String())
	}
	if current := loadReposeBackupTestFindings(t, repository, scan.ID); slices.Contains(current[0].Tags, "backed-up") {
		t.Fatal("cancelled import changed the database")
	}
	stdout.Reset()
	environment.Stdin = nil
	if err := runReposeCLI(ctx, []string{"backup", "import", "--force", backupPath, "--repo", repository.WorkTree}, environment); err != nil {
		t.Fatal(err)
	}
	database, _ := reposeDatabasePath(ctx, repository)
	if stdout.String() != fmt.Sprintf("Imported Repose backup %s to %s\n", backupPath, database) {
		t.Fatalf("import output = %q", stdout.String())
	}
	current := loadReposeBackupTestFindings(t, repository, scan.ID)
	if !slices.Contains(current[0].Tags, "backed-up") {
		t.Fatal("import did not restore backed-up findings")
	}
	afterContents, err := os.ReadFile(backupPath)
	if err != nil || !bytes.Equal(afterContents, backupContents) {
		t.Fatal("import modified the source backup")
	}
	info, err := os.Stat(database)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("imported database permissions = %v, %v", info, err)
	}
	lock, err := acquireScanLock(filepath.Join(filepath.Dir(database), "scan.lock"))
	if err != nil {
		t.Fatal(err)
	}
	err = runReposeCLI(ctx, []string{"backup", "import", "--force", backupPath, "--repo", repository.WorkTree}, environment)
	if closeErr := lock.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err == nil || !strings.Contains(err.Error(), "another scan") {
		t.Fatalf("active scan import error = %v", err)
	}
	rollbacks, err := filepath.Glob(filepath.Join(filepath.Dir(database), ".repose-rollback-*.sqlite*"))
	if err != nil || len(rollbacks) != 0 {
		t.Fatalf("import left rollback files: %v, %v", rollbacks, err)
	}
}

func TestReposeBackupImportRejectsInvalidAndWrongRepository(t *testing.T) {
	ctx := context.Background()
	targetRepository, targetDirectory := newInventoryFixture(t)
	runReposeRecord(t, targetDirectory, "inventory", "build")
	invalid := filepath.Join(t.TempDir(), "invalid.sqlite")
	if err := os.WriteFile(invalid, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeReposeTest(t, targetDirectory, "backup", "import", "--force", invalid); err == nil {
		t.Fatal("invalid backup was imported")
	}
	_, sourceDirectory := newInventoryFixture(t)
	writeInventoryFixtureFile(t, filepath.Join(sourceDirectory, "unique.cpp"), []byte("int unique_backup_snapshot;\n"))
	testGit(t, sourceDirectory, "add", "unique.cpp")
	testGit(t, sourceDirectory, "commit", "-m", "unique backup snapshot")
	runReposeRecord(t, sourceDirectory, "inventory", "build")
	backupPath := filepath.Join(t.TempDir(), "other-repository.sqlite")
	if _, err := executeReposeTest(t, sourceDirectory, "backup", backupPath); err != nil {
		t.Fatal(err)
	}
	targetDatabase, _ := reposeDatabasePath(ctx, targetRepository)
	before, err := os.ReadFile(targetDatabase)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executeReposeTest(t, targetDirectory, "backup", "import", "--force", backupPath)
	if err == nil || !strings.Contains(err.Error(), "snapshot") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("wrong repository error = %v", err)
	}
	after, err := os.ReadFile(targetDatabase)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected import changed the target database")
	}
}

func TestReposeBackupImportsIntoRepositoryWithoutState(t *testing.T) {
	repository, directory := newInventoryFixture(t)
	record := runReposeRecord(t, directory, "inventory", "build")
	ctx := context.Background()
	backupPath := filepath.Join(t.TempDir(), "restore.sqlite")
	if _, err := executeReposeTest(t, directory, "backup", backupPath); err != nil {
		t.Fatal(err)
	}
	database, _ := reposeDatabasePath(ctx, repository)
	if err := removeDatabaseFiles(database); err != nil {
		t.Fatal(err)
	}
	if _, err := executeReposeTest(t, directory, "backup", "import", backupPath); err != nil {
		t.Fatal(err)
	}
	reader, err := openInventoryReadOnly(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	restored, err := reader.Inventory(ctx, "current")
	if err != nil || restored.ID != record.ID {
		t.Fatalf("restored inventory = %+v, %v", restored, err)
	}
}

func loadReposeBackupTestFindings(t *testing.T, repository *GitRepository, scanID string) []Finding {
	t.Helper()
	database, err := reposeDatabasePath(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := openInventoryReadOnly(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	findings, err := (&auditFindingStore{reader: reader, scanID: scanID}).AllFindings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

func TestReposeBackupHelpAndArgumentValidation(t *testing.T) {
	output, err := executeReposeTest(t, t.TempDir(), "backup", "--help")
	if err != nil || !strings.Contains(output, "repose backup [PATH]") || !strings.Contains(output, "repose backup import [OPTIONS]") {
		t.Fatalf("backup help = %q, %v", output, err)
	}
	output, err = executeReposeTest(t, t.TempDir(), "backup", "import", "--help")
	if err != nil || !strings.Contains(output, "repose backup import [OPTIONS] PATH") || !strings.Contains(output, "--force") {
		t.Fatalf("backup import help = %q, %v", output, err)
	}
	for _, args := range [][]string{{"backup", "one", "two"}, {"backup", "import"}, {"backup", "import", "one", "two"}} {
		if _, err := executeReposeTest(t, t.TempDir(), args...); err == nil {
			t.Fatalf("accepted invalid arguments %v", args)
		}
	}
}
