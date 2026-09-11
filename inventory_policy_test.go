package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func inventoryFileByPath(t *testing.T, inventory Inventory, name string) InventoryFile {
	t.Helper()
	for _, file := range inventory.Files {
		if file.Path == name {
			return file
		}
	}
	t.Fatalf("file %q absent", name)
	return InventoryFile{}
}

func TestInventoryScopeEditsPreserveFactsAndAnnotations(t *testing.T) {
	repository, _ := newInventoryFixture(t)
	exclude := true
	inventory := mustBuildInventory(t, repository, InventoryPolicy{Rules: []InventoryRule{
		{Prefix: "pcbnew", Exclude: &exclude, Reason: "Later", Group: "board", Tags: []string{"undo"}, Note: "Check undo invariants."},
	}})
	original, _ := json.Marshal(inventory)
	updated, changes, err := editInventoryScope(inventory, []string{"pcbnew/main.cpp"}, false, "Review this first")
	if err != nil || len(changes) != 1 || changes[0].Path != "pcbnew/main.cpp" {
		t.Fatalf("include = %+v, %v", changes, err)
	}
	main := inventoryFileByPath(t, updated, "pcbnew/main.cpp")
	if main.Excluded || main.Group != "board" || !reflect.DeepEqual(main.Tags, []string{"undo"}) || len(main.Notes) != 1 || len(main.CommandIDs) != 2 {
		t.Fatalf("include lost context: %+v", main)
	}
	if !inventoryFileByPath(t, updated, "pcbnew/main.h").Excluded || inventoryFileByPath(t, updated, "pcbnewish/other.cpp").Excluded {
		t.Fatal("prefix override leaked to other paths")
	}
	if !reflect.DeepEqual(inventory.Commands, updated.Commands) || !reflect.DeepEqual(inventory.BuildInputs, updated.BuildInputs) {
		t.Fatal("policy edit changed compiler facts")
	}
	after, _ := json.Marshal(inventory)
	if string(original) != string(after) {
		t.Fatal("policy edit mutated original inventory")
	}
	rows := inventoryExclusionRules(updated)
	for _, row := range rows {
		if row.Prefix == "pcbnew" && (row.Matched != 3 || row.Effective != 2) {
			t.Fatalf("parent rule counts: %+v", row)
		}
		if row.Prefix == "pcbnew/main.cpp" && (row.Action != "include" || row.Effective != 1) {
			t.Fatalf("include override counts: %+v", row)
		}
	}
	// Re-including the parent replaces its exclusion while keeping its annotation.
	updated, _, err = editInventoryScope(updated, []string{"pcbnew"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	main = inventoryFileByPath(t, updated, "pcbnew/main.h")
	if main.Excluded || main.Group != "board" || len(main.Notes) != 1 {
		t.Fatalf("parent annotation lost: %+v", main)
	}
	// Importing an empty policy removes annotations as well as explicit scopes.
	reset, err := inventoryWithPolicy(updated, InventoryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	main = inventoryFileByPath(t, reset, "pcbnew/main.cpp")
	if main.Excluded || main.Group != "pcbnew" || len(main.Tags) != 0 || len(main.Notes) != 0 {
		t.Fatalf("policy reset left annotations: %+v", main)
	}
}

func TestInventoryScopeEditsAreIdempotentAndValidateAllPrefixes(t *testing.T) {
	repository, _ := newInventoryFixture(t)
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	first, _, err := editInventoryScope(inventory, []string{"pcbnew", "thirdparty"}, true, "Outside this audit")
	if err != nil {
		t.Fatal(err)
	}
	second, changes, err := editInventoryScope(first, []string{"pcbnew", "thirdparty"}, true, "Outside this audit")
	if err != nil || len(changes) != 0 || !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated edit changed inventory: %v, %v", changes, err)
	}
	for _, prefix := range []string{"not-found", "README.md", "pcbnew/**", "../outside"} {
		if _, _, err := editInventoryScope(inventory, []string{"pcbnew", prefix}, true, "Outside scope"); err == nil {
			t.Errorf("accepted invalid multi-prefix edit %q", prefix)
		}
	}
	if _, _, err := editInventoryScope(inventory, []string{"pcbnew"}, true, " "); err == nil {
		t.Fatal("excluded without a reason")
	}
	if _, err := readInventoryPolicy(strings.NewReader("null")); err == nil {
		t.Fatal("accepted null policy")
	}
}
