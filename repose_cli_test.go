package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func executeReposeTest(t *testing.T, cwd string, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runReposeCLI(context.Background(), args, cliEnvironment{
		Cwd: cwd, Stdout: &stdout, Stderr: &stderr,
	})
	if stderr.Len() != 0 {
		t.Logf("stderr: %s", stderr.String())
	}
	return stdout.String(), err
}

func TestReposeInventoryReviewWorkflow(t *testing.T) {
	_, directory := newInventoryFixture(t)
	caller := t.TempDir()
	output, err := executeReposeTest(t, caller, "inventory", "build", "--repo", directory, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var first InventoryRecord
	if err := json.Unmarshal([]byte(output), &first); err != nil {
		t.Fatal(err)
	}
	output, err = executeReposeTest(t, directory, "inventory", "show")
	if err != nil || !strings.Contains(output, "awaiting review") || !strings.Contains(output, "2 in-scope sources without commands") ||
		!strings.Contains(output, "1 external and 1 untracked") {
		t.Fatalf("summary = %s, %v", output, err)
	}
	output, err = executeReposeTest(t, directory, "inventory", "files", "--path", "pcbnew", "--status", "missing-command", "--json")
	var files []InventoryFile
	if err != nil || json.Unmarshal([]byte(output), &files) != nil || len(files) != 1 || files[0].Path != "pcbnew/platform.cpp" {
		t.Fatalf("subset = %s, %v", output, err)
	}
	if _, err := executeReposeTest(t, directory, "inventory", "approve", "latest"); err == nil {
		t.Fatal("approval accepted mutable latest selector")
	}
	if _, err := executeReposeTest(t, directory, "inventory", "approve", first.ID[:12]); err != nil {
		t.Fatal(err)
	}
	if _, err := executeReposeTest(t, directory, "inventory", "check", first.ID[:12]); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(caller, "policy.json")
	writeInventoryFixtureFile(t, policy, []byte(`{"rules":[{"prefix":"pcbnew","group":"board","tags":["undo"],"note":"Check undo behavior."}]}`))
	output, err = executeReposeTest(t, caller, "inventory", "build", "--repo", directory, "--policy", "policy.json", "--json")
	var second InventoryRecord
	if err != nil || json.Unmarshal([]byte(output), &second) != nil || second.ID == first.ID || second.ReviewedAt != nil {
		t.Fatalf("policy rebuild = %s, %v", output, err)
	}
	output, err = executeReposeTest(t, directory, "inventory", "files", "--group", "board", "--tag", "undo", "--json")
	if err != nil || json.Unmarshal([]byte(output), &files) != nil || len(files) != 3 {
		t.Fatalf("annotated subset = %s, %v", output, err)
	}
	output, err = executeReposeTest(t, directory, "inventory", "list", "--json")
	var list []InventoryListing
	if err != nil || json.Unmarshal([]byte(output), &list) != nil || len(list) != 2 || list[1].ReviewedAt == nil {
		t.Fatalf("version list = %s, %v", output, err)
	}
	// Inspecting historical inventories does not depend on the current checkout.
	writeInventoryFixtureFile(t, filepath.Join(directory, "pcbnew/main.cpp"), []byte("changed\n"))
	if _, err := executeReposeTest(t, directory, "inventory", "show", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := executeReposeTest(t, directory, "inventory", "approve", second.ID); err == nil {
		t.Fatal("approved inventory after checkout changed")
	}
}

func TestReposeRejectsBadPolicyBeforeCreatingState(t *testing.T) {
	repository, directory := newInventoryFixture(t)
	for _, contents := range []string{
		`{"rulez":[]}`,
		`{"rules":[{"prefix":"pcbnew","exclude":true}]}`,
		`{"rules":[{"prefix":"pcbnew/**"}]}`,
		`{"rules":[{"prefix":"../outside"}]}`,
		`{"rules":[]} {"rules":[]}`,
	} {
		policy := filepath.Join(t.TempDir(), "policy.json")
		writeInventoryFixtureFile(t, policy, []byte(contents))
		if _, err := executeReposeTest(t, directory, "inventory", "build", "--policy", policy); err == nil {
			t.Errorf("accepted policy %s", contents)
		}
	}
	databasePath, err := reposeDatabasePath(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(databasePath); !os.IsNotExist(err) {
		t.Fatalf("invalid build created state: %v", err)
	}
}

func TestReposeEntryPointExposesInventoryInsteadOfCommitReview(t *testing.T) {
	directory := t.TempDir()
	for _, args := range [][]string{nil, {"--help"}, {"inventory", "--help"}, {"inventory", "build", "--help"}} {
		output, err := executeReposeTest(t, directory, args...)
		if err != nil || !strings.Contains(output, "repose inventory build") || strings.Contains(output, "air scan") {
			t.Fatalf("help = %s, %v", output, err)
		}
	}
	for _, args := range [][]string{{"version"}, {"--version"}} {
		output, err := executeReposeTest(t, directory, args...)
		if err != nil || output != "repose "+reposeVersion+"\n" {
			t.Fatalf("version = %s, %v", output, err)
		}
	}
	for _, args := range [][]string{{"scan"}, {"init", "HEAD"}, {"inventory", "bad"}, {"inventory", "build", "--unknown"}, {"inventory", "files", "--status", "bad"}} {
		if _, err := executeReposeTest(t, directory, args...); err == nil {
			t.Errorf("accepted unsupported command %q", args)
		}
	}
}
