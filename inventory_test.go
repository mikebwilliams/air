package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newInventoryFixture(t *testing.T) (*GitRepository, string) {
	t.Helper()
	repository, directory := newTestGitRepository(t)
	files := map[string]string{
		".gitignore":            "build/\n",
		"pcbnew/main.cpp":       "#include \"main.h\"\nint main() { return 0; }\n",
		"pcbnew/main.h":         "#pragma once\n",
		"pcbnew/platform.cpp":   "int platform() { return 1; }\n",
		"pcbnewish/other.cpp":   "int other() { return 0; }\n",
		"thirdparty/vendor.cpp": "int vendor() { return 0; }\n",
		"README.md":             "A small C++ project\n",
	}
	for name, contents := range files {
		writeInventoryFixtureFile(t, filepath.Join(directory, name), []byte(contents))
	}
	testGit(t, directory, "add", ".")
	testGit(t, directory, "commit", "-m", "initial source snapshot")
	build := filepath.Join(directory, "build")
	commands := []InventoryCommand{
		{Directory: build, File: "../pcbnew/main.cpp", Arguments: []string{"clang++", "-DFIRST=1", "-c", "../pcbnew/main.cpp"}},
		{Directory: build, File: filepath.Join(directory, "pcbnew/main.cpp"), Command: "clang++ -DSECOND=1 -c ../pcbnew/main.cpp", Output: "second.o"},
		{Directory: build, File: "generated.cpp", Arguments: []string{"clang++", "-c", "generated.cpp"}},
		{Directory: build, File: filepath.Join(t.TempDir(), "external.cpp"), Arguments: []string{"clang++", "-c", "external.cpp"}},
	}
	data, err := json.Marshal(commands)
	if err != nil {
		t.Fatal(err)
	}
	writeInventoryFixtureFile(t, filepath.Join(build, "compile_commands.json"), data)
	writeInventoryFixtureFile(t, filepath.Join(build, "CMakeCache.txt"), []byte("CMAKE_BUILD_TYPE:STRING=Debug\n"))
	return repository, directory
}

func writeInventoryFixtureFile(t *testing.T, filename string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, contents, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustBuildInventory(t *testing.T, repository *GitRepository, policy InventoryPolicy) Inventory {
	t.Helper()
	inventory, err := buildInventory(context.Background(), repository, inventoryBuildOptions{Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return inventory
}

func TestInventoryAccountsForTrackedTreeAndBuildCoverage(t *testing.T) {
	repository, directory := newInventoryFixture(t)
	// No master-branch or first-parent requirement; a detached snapshot is valid.
	testGit(t, directory, "checkout", "--detach")
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	if len(inventory.Files) != 7 || len(inventory.Commands) != 4 || len(inventory.BuildInputs) != 2 {
		t.Fatalf("inventory counts: files=%d commands=%d inputs=%d", len(inventory.Files), len(inventory.Commands), len(inventory.BuildInputs))
	}
	files := map[string]InventoryFile{}
	for _, file := range inventory.Files {
		files[file.Path] = file
	}
	main := files["pcbnew/main.cpp"]
	if main.Excluded || main.Kind != "source" || main.Group != "pcbnew" || len(main.CommandIDs) != 2 || main.CommandIDs[0] != 1 || main.CommandIDs[1] != 2 {
		t.Fatalf("source associations: %+v", main)
	}
	if main.Bytes != int64(len("#include \"main.h\"\nint main() { return 0; }\n")) || !isHexObjectID(main.BlobID) {
		t.Fatalf("source identity/size: %+v", main)
	}
	if !inventoryFileMatchesStatus(files["pcbnew/platform.cpp"], "missing-command") ||
		!inventoryFileMatchesStatus(files["pcbnew/main.h"], "header-unmapped") {
		t.Fatal("sources outside the build and unassociated headers must remain in scope")
	}
	if !files["thirdparty/vendor.cpp"].Excluded || !files["README.md"].Excluded {
		t.Fatal("default exclusions must remain visible")
	}
	if inventory.Commands[0].File != filepath.Join(directory, "pcbnew/main.cpp") || inventory.Commands[1].Command == "" {
		t.Fatal("relative paths or command variants were lost")
	}
}

func TestInventoryPolicyGroupsTagsAndIncludesWithLiteralBoundaries(t *testing.T) {
	repository, _ := newInventoryFixture(t)
	exclude, include := true, false
	policy := InventoryPolicy{Rules: []InventoryRule{
		{Prefix: "pcbnew", Group: "board", Tags: []string{"ownership"}, Note: "Check ownership through undo."},
		{Prefix: "pcbnew/platform.cpp", Exclude: &exclude, Reason: "Review in the Windows pass."},
		{Prefix: "thirdparty", Exclude: &include, Group: "dependencies"},
	}}
	inventory := mustBuildInventory(t, repository, policy)
	for _, file := range inventory.Files {
		switch file.Path {
		case "pcbnew/main.cpp":
			if file.Group != "board" || len(file.Tags) != 1 || len(file.Notes) != 1 {
				t.Fatalf("annotations: %+v", file)
			}
		case "pcbnew/platform.cpp":
			if !file.Excluded || file.ExclusionReason != "Review in the Windows pass." {
				t.Fatalf("exclusion: %+v", file)
			}
		case "pcbnewish/other.cpp":
			if file.Group != "pcbnewish" || len(file.Tags) != 0 {
				t.Fatal("directory prefix matched a sibling")
			}
		case "thirdparty/vendor.cpp":
			if file.Excluded || file.ExclusionReason != "" || file.Group != "dependencies" {
				t.Fatalf("include override: %+v", file)
			}
		}
	}
	_, err := buildInventory(context.Background(), repository, inventoryBuildOptions{Policy: InventoryPolicy{Rules: []InventoryRule{{Prefix: "misspelled"}}}})
	if err == nil || !strings.Contains(err.Error(), "matches no tracked paths") {
		t.Fatalf("unmatched rule error: %v", err)
	}
}

func TestInventoryRejectsChangedSnapshotAndBuildInputs(t *testing.T) {
	for _, change := range []string{"working-tree", "index", "commit", "commands", "cache"} {
		t.Run(change, func(t *testing.T) {
			repository, directory := newInventoryFixture(t)
			inventory := mustBuildInventory(t, repository, InventoryPolicy{})
			want := "tracked changes"
			switch change {
			case "working-tree", "index", "commit":
				writeInventoryFixtureFile(t, filepath.Join(directory, "pcbnew/main.cpp"), []byte("int main() { return 1; }\n"))
				if change != "working-tree" {
					testGit(t, directory, "add", "pcbnew/main.cpp")
				}
				if change == "commit" {
					testGit(t, directory, "commit", "-m", "another snapshot")
					want = "inventory requires"
				}
			case "commands":
				writeInventoryFixtureFile(t, filepath.Join(directory, "build/compile_commands.json"), []byte("[]"))
				want = "build input changed"
			case "cache":
				writeInventoryFixtureFile(t, filepath.Join(directory, "build/CMakeCache.txt"), []byte("CMAKE_BUILD_TYPE:STRING=Release\n"))
				want = "build input changed"
			}
			if err := checkInventory(context.Background(), repository, inventory); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("check error = %v, want %q", err, want)
			}
			if change == "index" || change == "working-tree" {
				if _, err := buildInventory(context.Background(), repository, inventoryBuildOptions{}); err == nil {
					t.Fatal("built an inventory from a dirty checkout")
				}
			}
		})
	}
}

func TestInventoryRejectsCompilationDatabaseFromAnotherCheckout(t *testing.T) {
	repository, directory := newInventoryFixture(t)
	commands := []InventoryCommand{{Directory: t.TempDir(), File: "main.cpp", Command: "clang++ -c main.cpp"}}
	data, _ := json.Marshal(commands)
	writeInventoryFixtureFile(t, filepath.Join(directory, "build/compile_commands.json"), data)
	if _, err := buildInventory(context.Background(), repository, inventoryBuildOptions{}); err == nil || !strings.Contains(err.Error(), "no compilation commands match") {
		t.Fatalf("wrong-checkout error: %v", err)
	}
}

func TestInventoryCommandsRejectMalformedInputsWithoutExecution(t *testing.T) {
	for _, contents := range []string{
		"null", "[]", "{}", "[{}]", "[] trailing",
		`[{"directory":"relative","file":"a.cpp","command":"clang++"}]`,
		`[{"directory":"/tmp","file":"a.cpp"}]`,
		`[{"directory":"/tmp","file":"a.cpp","arguments":[""]}]`,
		`[{"directory":"/tmp","file":"","command":"clang++"}]`,
	} {
		filename := filepath.Join(t.TempDir(), "compile_commands.json")
		writeInventoryFixtureFile(t, filename, []byte(contents))
		if _, _, err := readInventoryCommands(filename); err == nil {
			t.Errorf("accepted invalid database: %s", contents)
		}
	}
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	filename := filepath.Join(t.TempDir(), "compile_commands.json")
	data, _ := json.Marshal([]InventoryCommand{{Directory: "/tmp", File: "a.cpp", Command: "touch " + marker}})
	writeInventoryFixtureFile(t, filename, data)
	if _, _, err := readInventoryCommands(filename); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("inventory building executed a compilation command")
	}
}

func TestInventoryTreePreservesUnusualPathsAndSymlinks(t *testing.T) {
	repository, directory := newInventoryFixture(t)
	name := "pcbnew/space tab\tnewline\n.cpp"
	testCommitFile(t, directory, name, []byte("int unusual;\n"), "unusual filename")
	if err := os.Symlink("main.h", filepath.Join(directory, "pcbnew/link.h")); err != nil {
		t.Fatal(err)
	}
	testGit(t, directory, "add", "pcbnew/link.h")
	testGit(t, directory, "commit", "-m", "header link")
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	found := false
	for _, file := range inventory.Files {
		if file.Path == name {
			found = true
		}
		if file.Path == "pcbnew/link.h" && (file.Kind != "symlink" || !file.Excluded) {
			t.Fatalf("symlink must have explicit exclusion: %+v", file)
		}
	}
	if !found {
		t.Fatal("Git path was split or unquoted incorrectly")
	}
}
