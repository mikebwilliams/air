package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runReposeRecord(t *testing.T, directory string, args ...string) InventoryRecord {
	t.Helper()
	output, err := executeReposeTest(t, directory, append(args, "--json")...)
	if err != nil {
		t.Fatal(err)
	}
	var record InventoryRecord
	if err := json.Unmarshal([]byte(output), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func runReposePolicyEdit(t *testing.T, directory string, args ...string) InventoryPolicyResult {
	t.Helper()
	output, err := executeReposeTest(t, directory, append(args, "--json")...)
	if err != nil {
		t.Fatal(err)
	}
	var result InventoryPolicyResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestInventoryPolicyCommandsPersistAndReuseSavedMap(t *testing.T) {
	_, directory := newInventoryFixture(t)
	first := runReposeRecord(t, directory, "inventory", "build")
	if _, err := executeReposeTest(t, directory, "inventory", "approve", first.ID); err != nil {
		t.Fatal(err)
	}
	// Deriving scope from the saved map must work without build inputs.
	compilePath := filepath.Join(directory, "build/compile_commands.json")
	data, err := os.ReadFile(compilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(compilePath); err != nil {
		t.Fatal(err)
	}
	excluded := runReposePolicyEdit(t, directory, "inventory", "exclude", "pcbnew", "--reason", "Outside audit scope")
	if excluded.PreviousID != first.ID || excluded.Record.ID == first.ID || excluded.Record.ReviewedAt != nil || len(excluded.Changes) != 3 {
		t.Fatalf("exclusion = %+v", excluded)
	}
	for _, file := range excluded.Record.Inventory.Files {
		if strings.HasPrefix(file.Path, "pcbnew/") && !file.Excluded {
			t.Fatalf("still included: %+v", file)
		}
	}
	include := runReposePolicyEdit(t, directory, "inventory", "include", "pcbnew/main.cpp")
	if len(include.Changes) != 1 || inventoryFileByPath(t, include.Record.Inventory, "pcbnew/main.cpp").Excluded {
		t.Fatalf("include override = %+v", include)
	}
	repeat := runReposePolicyEdit(t, directory, "inventory", "include", "pcbnew/main.cpp")
	if repeat.Record.ID != include.Record.ID || len(repeat.Changes) != 0 {
		t.Fatal("repeated include created a different version")
	}
	old := runReposeRecord(t, directory, "inventory", "show", first.ID)
	if old.ReviewedAt == nil || inventoryFileByPath(t, old.Inventory, "pcbnew/main.h").Excluded {
		t.Fatal("old inventory or approval changed")
	}
	writeInventoryFixtureFile(t, compilePath, data)
	rebuilt := runReposeRecord(t, directory, "inventory", "build")
	if rebuilt.ID != include.Record.ID {
		t.Fatal("rebuild failed to inherit current policy")
	}
	exported, err := executeReposeTest(t, directory, "inventory", "policy", "export")
	if err != nil {
		t.Fatal(err)
	}
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	writeInventoryFixtureFile(t, policyFile, []byte(exported))
	imported := runReposePolicyEdit(t, directory, "inventory", "policy", "import", policyFile)
	if imported.Record.ID != rebuilt.ID {
		t.Fatal("policy export/import failed to round-trip")
	}
	output, err := executeReposeTest(t, directory, "inventory", "exclusions")
	if err != nil || !strings.Contains(output, "thirdparty") || !strings.Contains(output, "pcbnew/main.cpp") || !strings.Contains(output, "EFFECTIVE") {
		t.Fatalf("exclusions = %s, %v", output, err)
	}
	// Returning to an existing document must make it current even if not newest.
	writeInventoryFixtureFile(t, policyFile, []byte(`{"rules":[]}`))
	reset := runReposePolicyEdit(t, directory, "inventory", "policy", "import", policyFile)
	if reset.Record.ID != first.ID || reset.Record.ReviewedAt == nil {
		t.Fatal("reset failed to reuse original immutable document")
	}
	current := runReposeRecord(t, directory, "inventory", "show")
	latest := runReposeRecord(t, directory, "inventory", "show", "latest")
	if current.ID != first.ID || latest.ID != include.Record.ID {
		t.Fatalf("current/newest selection: %s, %s", current.ID, latest.ID)
	}
	rebuilt = runReposeRecord(t, directory, "inventory", "build")
	if rebuilt.ID != first.ID {
		t.Fatal("build inherited the newest version instead of current policy")
	}
}

func TestInventoryPolicySurvivesDeletedAndNewPathsInLaterSnapshots(t *testing.T) {
	_, directory := newInventoryFixture(t)
	runReposeRecord(t, directory, "inventory", "build")
	runReposePolicyEdit(t, directory, "inventory", "exclude", "pcbnew/platform.cpp", "--reason", "Separate platform audit")
	testGit(t, directory, "rm", "pcbnew/platform.cpp")
	testGit(t, directory, "commit", "-m", "remove platform source")
	output, err := executeReposeTest(t, directory, "inventory", "build")
	if err != nil || !strings.Contains(output, "Retained policy prefixes without matches") {
		t.Fatalf("inherited unmatched policy should remain visible: %s, %v", output, err)
	}
	testCommitFile(t, directory, "pcbnew/platform.cpp", []byte("int restored;\n"), "restore platform source")
	rebuilt := runReposeRecord(t, directory, "inventory", "build")
	if !inventoryFileByPath(t, rebuilt.Inventory, "pcbnew/platform.cpp").Excluded {
		t.Fatal("persistent path rule was lost when the file disappeared")
	}
}

func TestInvalidInventoryPolicyEditDoesNotChangeCurrentVersion(t *testing.T) {
	repository, directory := newInventoryFixture(t)
	first := runReposeRecord(t, directory, "inventory", "build")
	for _, args := range [][]string{
		{"exclude", "pcbnew"}, {"exclude", "pcbnew", "no-such-path", "--reason", "Outside scope"},
		{"include", "README.md"}, {"include"}, {"policy", "import"}, {"policy", "export", "extra", "args"},
		{"approve", "current"},
	} {
		if _, err := executeReposeTest(t, directory, append([]string{"inventory"}, args...)...); err == nil {
			t.Errorf("accepted invalid edit %q", args)
		}
	}
	current := runReposeRecord(t, directory, "inventory", "show")
	if current.ID != first.ID {
		t.Fatal("failed edit changed current inventory")
	}
	filename, err := reposeDatabasePath(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openInventoryReadOnly(context.Background(), filename)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	list, err := store.ListInventories(context.Background())
	if err != nil || len(list) != 1 || !list[0].Current {
		t.Fatalf("failed edit left inventory rows: %+v, %v", list, err)
	}
}
