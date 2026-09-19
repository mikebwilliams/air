package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReposeDatabasePathWorksBeforeInitialization(t *testing.T) {
	repository, directory := newInventoryFixture(t)
	databasePath, err := reposeDatabasePath(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(databasePath); !os.IsNotExist(err) {
		t.Fatalf("database exists before command: %v", err)
	}

	caller := t.TempDir()
	relativeRepo, err := filepath.Rel(caller, directory)
	if err != nil {
		t.Fatal(err)
	}
	output, err := executeReposeTest(t, caller, "db", "path", "--repo", relativeRepo)
	if err != nil {
		t.Fatal(err)
	}
	if output != databasePath+"\n" {
		t.Fatalf("db path output = %q, want %q", output, databasePath+"\n")
	}
	if _, err := os.Stat(databasePath); !os.IsNotExist(err) {
		t.Fatalf("db path created state: %v", err)
	}
}

func TestReposeDatabasePathRejectsInvalidCommands(t *testing.T) {
	_, directory := newInventoryFixture(t)
	for _, args := range [][]string{{"db"}, {"db", "show"}, {"db", "path", "extra"}} {
		if _, err := executeReposeTest(t, directory, args...); err == nil || !strings.Contains(err.Error(), "usage: repose db path") {
			t.Errorf("command %q returned %v", args, err)
		}
	}
}
