package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInventoryCurationCLI(t *testing.T) {
	_, directory := newInventoryFixture(t)
	first := runReposeRecord(t, directory, "inventory", "build")
	if _, err := executeReposeTest(t, directory, "inventory", "approve", first.ID); err != nil {
		t.Fatal(err)
	}
	// All curation and previews must operate on saved facts, including historical
	// inventories when the checkout/build environment has moved on.
	if err := os.Remove(filepath.Join(directory, "build/compile_commands.json")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"inventory", "group", "pcbnew", "--name", "board"},
		{"inventory", "annotate", "pcbnew", "--tag", "ownership", "--tag", "undo", "--note", "Check rollback."},
	} {
		if _, err := executeReposeTest(t, directory, args...); err != nil {
			t.Fatal(err)
		}
	}
	current := runReposeRecord(t, directory, "inventory", "show")
	if current.ID == first.ID || current.ReviewedAt != nil {
		t.Fatal("curation did not create an unreviewed version")
	}
	for _, args := range [][]string{
		{"inventory", "annotate", "pcbnew", "--tag", "undo", "--tag", "ownership", "--note", "Check rollback."},
		{"inventory", "group", "pcbnew", "--name", "board"},
		{"inventory", "group", "pcbnew", "--name", "board"},
	} {
		if _, err := executeReposeTest(t, directory, args...); err != nil {
			t.Fatal(err)
		}
	}
	stable := runReposeRecord(t, directory, "inventory", "show")
	if stable.ID != current.ID {
		t.Fatal("repeated annotations or group changed the version")
	}
	if _, err := executeReposeTest(t, directory, "inventory", "group", "pcbnew", "--name", "board"); err != nil {
		t.Fatal(err)
	}
	if runReposeRecord(t, directory, "inventory", "show").ID != stable.ID {
		t.Fatal("group edit is not idempotent")
	}
	for _, file := range current.Inventory.Files {
		if inventoryPrefixMatches(file.Path, "pcbnew") {
			if file.Group != "board" || !slices.Equal(file.Tags, []string{"ownership", "undo"}) || !slices.Equal(file.Notes, []string{"Check rollback."}) {
				t.Fatalf("annotations lost: %+v", file)
			}
		} else if file.Group == "board" {
			t.Fatalf("prefix leaked to %s", file.Path)
		}
	}
	output, err := executeReposeTest(t, directory, "inventory", "inspect", "--group", "board", "--tag", "undo", "--json")
	var inspection inventoryInspection
	if err != nil || json.Unmarshal([]byte(output), &inspection) != nil || inspection.Files != 3 || inspection.MissingCommand != 1 || inspection.UnmappedHeader != 1 || len(inspection.Groups) != 1 {
		t.Fatalf("inspection = %s, %v", output, err)
	}
	output, err = executeReposeTest(t, directory, "inventory", "tree", "--path", "pcbnew", "--depth", "0", "--json")
	var tree struct {
		Rows []inventoryTreeRow `json:"rows"`
	}
	if err != nil || json.Unmarshal([]byte(output), &tree) != nil || len(tree.Rows) != 1 || tree.Rows[0].Files != 3 {
		t.Fatalf("tree = %s, %v", output, err)
	}
	old := runReposeRecord(t, directory, "inventory", "show", first.ID)
	if old.ReviewedAt == nil || inventoryFileByPath(t, old.Inventory, "pcbnew/main.cpp").Group != "pcbnew" {
		t.Fatal("historical inventory changed")
	}
	for _, args := range [][]string{
		{"group", "pcbnew", "--name", " "}, {"group", "pcbnew", "absent", "--name", "oops"},
		{"annotate", "pcbnew"}, {"annotate", "pcbnew", "--tag", " "}, {"annotate", "pcbnew", "--note", " "},
		{"tree", "--path", "../outside"}, {"tree", "--depth", "-1"}, {"tree", "--status", "wrong"},
		{"plan", "--path", "thirdparty"}, {"plan", "--goal", " "}, {"plan", "--max-files", "0"}, {"plan", "--max-bytes", "0"},
	} {
		if _, err := executeReposeTest(t, directory, append([]string{"inventory"}, args...)...); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	if runReposeRecord(t, directory, "inventory", "show").ID != stable.ID {
		t.Fatal("failed command changed current version")
	}
}

func TestInventoryPlanCoverageAndBounds(t *testing.T) {
	repository, _ := newInventoryFixture(t)
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	inventory.Files = append(inventory.Files,
		InventoryFile{Path: "pcbnew/a/huge.cpp", Group: "pcbnew", Kind: "source", Bytes: 1000},
		InventoryFile{Path: "pcbnew/a/small.cpp", Group: "pcbnew", Kind: "source", Bytes: 1},
		InventoryFile{Path: "pcbnew/b/main.cpp", Group: "pcbnew", Kind: "source", Bytes: 1},
	)
	record := InventoryRecord{ID: strings.Repeat("a", 64), Inventory: inventory}
	selection := inventorySelection{Path: "pcbnew", Status: "included"}
	plan, err := planInventory(record, selection, "Check undo.", inventoryPlanLimits{2, 80})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	oversized, warnings := 0, 0
	for _, assignment := range plan.Assignments {
		if len(assignment.Files) > 2 {
			t.Fatal("exceeded file limit")
		}
		if assignment.Oversized {
			oversized++
			if len(assignment.Files) != 1 || assignment.Files[0].Bytes <= 80 {
				t.Fatal("invalid oversized singleton")
			}
		} else if assignment.Bytes > 80 {
			t.Fatal("exceeded byte limit")
		}
		warnings += len(assignment.Warnings)
		for _, file := range assignment.Files {
			if seen[file.Path] || file.Excluded || !inventoryPrefixMatches(file.Path, "pcbnew") {
				t.Fatalf("invalid target %s", file.Path)
			}
			seen[file.Path] = true
			if file.Group != assignment.Group || filepath.Dir(file.Path) != assignment.Directory {
				t.Fatal("crossed grouping boundary")
			}
		}
	}
	if len(seen) != 6 || plan.Files != 6 || oversized != 1 || warnings < 3 {
		t.Fatalf("incomplete coverage: %+v", plan)
	}
	// Iteration order and review acknowledgement don't change a frozen preview.
	slices.Reverse(record.Inventory.Files)
	repeated, err := planInventory(record, selection, "Check undo.", inventoryPlanLimits{2, 80})
	if err != nil || repeated.ID != plan.ID {
		t.Fatalf("nondeterministic preview: %v", err)
	}
	changed, err := planInventory(record, selection, "Check ownership.", inventoryPlanLimits{2, 80})
	if err != nil || changed.ID == plan.ID || changed.Assignments[0].ID == plan.Assignments[0].ID {
		t.Fatal("goal change reused preview identity")
	}
	rows := inventoryTree(selection.files(inventory), "pcbnew", 1)
	if len(rows) != 3 || rows[0].Files != 6 || rows[1].Files != 2 || rows[2].Files != 1 {
		t.Fatalf("directory rollups = %+v", rows)
	}
}

func TestInventoryPlanCLIIsReadOnlyAndRepeatable(t *testing.T) {
	_, directory := newInventoryFixture(t)
	first := runReposeRecord(t, directory, "inventory", "build")
	output, err := executeReposeTest(t, directory, "inventory", "plan", "--path", "pcbnew", "--max-files", "2", "--json")
	var plan inventoryPlan
	if err != nil || json.Unmarshal([]byte(output), &plan) != nil || plan.InventoryID != first.ID || plan.Files != 3 || len(plan.Assignments) != 2 {
		t.Fatalf("plan = %s, %v", output, err)
	}
	repeated, err := executeReposeTest(t, directory, "inventory", "plan", first.ID, "--path", "pcbnew", "--max-files", "2", "--json")
	if err != nil || repeated != output {
		t.Fatal("CLI preview changed")
	}
	list, err := executeReposeTest(t, directory, "inventory", "list", "--json")
	var records []InventoryListing
	if err != nil || json.Unmarshal([]byte(list), &records) != nil || len(records) != 1 || records[0].ReviewedAt != nil {
		t.Fatal("planning altered stored inventories")
	}
}

func TestInventoryTreeKeepsSubtreesTogether(t *testing.T) {
	files := []InventoryFile{{Path: "!first/file.cpp"}, {Path: "a/sub/file.cpp"}, {Path: "a.b/file.cpp"}}
	rows := inventoryTree(files, ".", 3)
	paths := []string{}
	for _, row := range rows {
		paths = append(paths, row.Path)
	}
	if !slices.Equal(paths, []string{".", "!first", "a", "a/sub", "a.b"}) {
		t.Fatalf("tree order = %q", paths)
	}
}
