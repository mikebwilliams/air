package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSemanticIndexResumeAndPolicyReuse(t *testing.T) {
	if _, err := exec.LookPath("clangd"); err != nil {
		t.Skip("clangd not installed")
	}
	repository, directory := newInventoryFixture(t)
	first := runReposeRecord(t, directory, "inventory", "build")
	run := func(args ...string) semanticSnapshot {
		t.Helper()
		output, err := executeReposeTest(t, directory, append(args, "--json")...)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot semanticSnapshot
		if err := json.Unmarshal([]byte(output), &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	limited := run("inventory", "index", "--path", "pcbnew", "--max-files", "1", "--timeout", "10s")
	if len(limited.Files) != 1 || limited.Files[0].Status != "indexed" || limited.Files[0].CommandID != 1 {
		t.Fatalf("limited index: %+v", limited)
	}
	finished := run("inventory", "index", "--path", "pcbnew", "--timeout", "10s")
	byPath := finished.byPath()
	if len(finished.Files) != 3 || byPath["pcbnew/main.h"].Status != "indexed" || byPath["pcbnew/main.h"].CommandID != 1 || byPath["pcbnew/platform.cpp"].Status != "unavailable" {
		t.Fatalf("resumed index: %+v", finished)
	}
	if limited.Files[0].ID != byPath["pcbnew/main.cpp"].ID {
		t.Fatal("resume replaced completed source")
	}
	if _, err := executeReposeTest(t, directory, "inventory", "group", "pcbnew", "--name", "board"); err != nil {
		t.Fatal(err)
	}
	reused := run("inventory", "index-status", "--group", "board")
	if reused.Profile.ID != finished.Profile.ID || !reflect.DeepEqual(reused.Files, finished.Files) {
		t.Fatal("policy edit invalidated semantic facts")
	}
	output, err := executeReposeTest(t, directory, "inventory", "plan", "--semantic", "--group", "board", "--json")
	var plan inventoryPlan
	if err != nil || json.Unmarshal([]byte(output), &plan) != nil || plan.Planner != "symbols-v1" || plan.SemanticProfileID != finished.Profile.ID || plan.Files != 3 {
		t.Fatalf("semantic plan: %s %v", output, err)
	}
	// Saved inspection and planning remain usable after build inputs disappear.
	if err := os.Remove(filepath.Join(directory, "build/compile_commands.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := executeReposeTest(t, directory, "inventory", "symbols", first.ID, "--path", "pcbnew"); err != nil {
		t.Fatal(err)
	}
	if _, err := executeReposeTest(t, directory, "inventory", "index", "--path", "pcbnew"); err == nil {
		t.Fatal("indexed stale build inputs")
	}
	database, err := reposeDatabasePath(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openInventoryReadOnly(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	old, err := store.Inventory(context.Background(), first.ID)
	if err != nil || old.ID != first.ID {
		t.Fatal("index altered inventory history")
	}
}

func TestSemanticPlansCoverEveryByteAndKeepContext(t *testing.T) {
	file := InventoryFile{Path: "router/large.cpp", BlobID: strings.Repeat("b", 40), Bytes: 300, Group: "router", Kind: "source", CommandIDs: []int{1}}
	record := InventoryRecord{ID: strings.Repeat("a", 64), Inventory: Inventory{SnapshotSHA: strings.Repeat("c", 40), WorkTree: "/scan", Files: []InventoryFile{file, {Path: "thirdparty/api.h", BlobID: strings.Repeat("d", 40), Group: "thirdparty", Kind: "header", Excluded: true}}}}
	snapshot := &semanticSnapshot{Profile: semanticProfile{ID: strings.Repeat("e", 64), BaseID: semanticBaseID(record.Inventory)}, Files: []semanticFileResult{{ID: "result-1", Path: file.Path, BlobID: file.BlobID, Status: "indexed", Symbols: []semanticSymbol{
		{Name: "ns", Kind: 3, StartByte: 10, EndByte: 290, Children: []semanticSymbol{{Name: "first", Kind: 12, StartByte: 20, EndByte: 80}, {Name: "large", Kind: 12, StartByte: 100, EndByte: 240}, {Name: "last", Kind: 12, StartByte: 250, EndByte: 280}}},
	}, Includes: []semanticLink{{Path: "thirdparty/api.h", SHA256: "header-hash"}}}}}
	selection := inventorySelection{Path: ".", Status: "included"}
	plan, err := planSemanticInventory(record, selection, "Check rollback.", inventoryPlanLimits{2, 100}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	cursor := int64(0)
	oversized := 0
	for _, assignment := range plan.Assignments {
		if assignment.Oversized {
			oversized++
			if len(assignment.Ranges) != 1 || assignment.Ranges[0].StartByte != 100 || assignment.Ranges[0].EndByte != 240 {
				t.Fatal("split inside oversized function")
			}
		}
		if len(assignment.Context) != 1 || assignment.Context[0].Path != "thirdparty/api.h" {
			t.Fatal("excluded include lost as context")
		}
		for _, part := range assignment.Ranges {
			if part.Path != file.Path || part.StartByte != cursor || part.EndByte <= part.StartByte {
				t.Fatalf("gap/overlap at %+v", part)
			}
			cursor = part.EndByte
		}
	}
	if cursor != file.Bytes || oversized != 1 || plan.Bytes != 300 || plan.Files != 1 {
		t.Fatal("incorrect coverage")
	}
	repeated, err := planSemanticInventory(record, selection, "Check rollback.", inventoryPlanLimits{2, 100}, snapshot)
	if err != nil || repeated.ID != plan.ID {
		t.Fatal("nondeterministic plan")
	}
	snapshot.Files[0].Status = "partial"
	partial, err := planSemanticInventory(record, selection, "Check rollback.", inventoryPlanLimits{2, 100}, snapshot)
	if err != nil || len(partial.Assignments) != 1 || !partial.Assignments[0].Oversized || len(partial.Assignments[0].Ranges) != 1 {
		t.Fatal("partial parse used for splitting")
	}
}

func TestSemanticCommandQuotingAndHeaderTransfer(t *testing.T) {
	command := InventoryCommand{Directory: "/scan/build", File: "/scan/src/my file.cpp", Command: `/usr/bin/c++ '-DNAME=two words' -I"/path with spaces" -include pch.hxx -c '../src/my file.cpp' -o file.o`}
	args, err := semanticCommandArguments(command)
	if err != nil || !slices.Equal(args, []string{"/usr/bin/c++", "-DNAME=two words", "-I/path with spaces", "-include", "pch.hxx", "-c", "../src/my file.cpp", "-o", "file.o"}) {
		t.Fatalf("arguments: %q %v", args, err)
	}
	header, err := semanticHeaderCommand(command, "/scan/include/header.h")
	if err != nil || header.File != "/scan/include/header.h" || !slices.Contains(header.Arguments, "/scan/include/header.h") || slices.Contains(header.Arguments, "../src/my file.cpp") {
		t.Fatalf("header transfer: %+v %v", header, err)
	}
	if _, err := semanticCommandArguments(InventoryCommand{Command: `c++ "unfinished`}); err == nil {
		t.Fatal("accepted unterminated command")
	}
}

func TestSemanticResultsAreImmutableAndReadOnly(t *testing.T) {
	repository, _ := newInventoryFixture(t)
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	ctx := context.Background()
	filename := filepath.Join(t.TempDir(), "repose.sqlite")
	store, err := openInventoryStore(ctx, filename, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	profile := semanticProfile{BaseID: semanticBaseID(inventory), FormatVersion: 1}
	data, _ := json.Marshal(profile)
	profile.ID = inventoryHash(data)
	if err := store.saveSemanticProfile(ctx, profile, time.Now()); err != nil {
		t.Fatal(err)
	}
	first, err := store.saveSemanticResult(ctx, semanticFileResult{ProfileID: profile.ID, Path: "pcbnew/main.cpp", Status: "partial"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Status = "indexed"
	second, err = store.saveSemanticResult(ctx, second, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("changed semantic result reused ID")
	}
	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM semantic_results").Scan(&count); err != nil || count != 2 {
		t.Fatal("old result overwritten")
	}
	reader, err := openInventoryReadOnly(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	snapshot, err := reader.semanticSnapshot(ctx, inventory, profile.ID)
	if err != nil || len(snapshot.Files) != 1 || snapshot.Files[0].ID != second.ID {
		t.Fatalf("read snapshot: %+v %v", snapshot, err)
	}
	if _, err := reader.saveSemanticResult(ctx, first, time.Now()); err == nil {
		t.Fatal("read-only semantic mutation succeeded")
	}
}

type semanticProgressFunc func([]byte) (int, error)

func (callback semanticProgressFunc) Write(data []byte) (int, error) { return callback(data) }

func TestSemanticIndexCancellationRetainsCompletedFiles(t *testing.T) {
	if _, err := exec.LookPath("clangd"); err != nil {
		t.Skip("clangd not installed")
	}
	repository, directory := newInventoryFixture(t)
	record := runReposeRecord(t, directory, "inventory", "build")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openInventoryStore(ctx, database, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = runSemanticIndex(ctx, store, repository, record, inventorySelection{Path: "pcbnew", Status: "included"}, semanticIndexOptions{Clangd: "clangd", Jobs: 1, Timeout: 10 * time.Second}, semanticProgressFunc(func(data []byte) (int, error) { cancel(); return len(data), nil }), time.Now)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted index: %v", err)
	}
	snapshot, err := store.semanticSnapshot(context.Background(), record.Inventory, "")
	if err != nil {
		t.Fatal(err)
	}
	result, exists := snapshot.byPath()["pcbnew/main.cpp"]
	if !exists || result.Status != "indexed" {
		t.Fatal("completed file lost on interruption")
	}
	if _, err := executeReposeTest(t, directory, "inventory", "index", "--path", "pcbnew"); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.semanticSnapshot(context.Background(), record.Inventory, "")
	if err != nil || resumed.byPath()["pcbnew/main.cpp"].ID != result.ID {
		t.Fatal("resume replaced completed work")
	}
}

func TestSemanticDependencyFingerprintDetectsChangedInputs(t *testing.T) {
	directory := t.TempDir()
	forced := filepath.Join(directory, "pch.hxx")
	direct := filepath.Join(directory, "generated.h")
	for _, file := range []string{forced, direct} {
		if err := os.WriteFile(file, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inputs, err := semanticForcedInputs(InventoryCommand{Directory: directory, Arguments: []string{"clang++", "-include", "pch.hxx"}})
	if err != nil {
		t.Fatal(err)
	}
	result := semanticFileResult{BuildInputs: inputs, Includes: semanticResolvedLinks(directory, []semanticLink{{Target: clangdURI(direct)}})}
	if semanticDependenciesChanged(result) {
		t.Fatal("unchanged dependencies invalidated result")
	}
	if err := os.WriteFile(forced, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !semanticDependenciesChanged(result) {
		t.Fatal("changed forced include ignored")
	}
	if err := os.WriteFile(forced, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(direct); err != nil {
		t.Fatal(err)
	}
	if !semanticDependenciesChanged(result) {
		t.Fatal("missing generated include ignored")
	}
}

func TestSemanticBrowserShowsFactsAndPlansWithSymbols(t *testing.T) {
	file := InventoryFile{Path: "router/main.cpp", BlobID: strings.Repeat("b", 40), Bytes: 50, Group: "router", Kind: "source"}
	record := InventoryRecord{ID: strings.Repeat("a", 64), Inventory: Inventory{Files: []InventoryFile{file}}}
	snapshot := &semanticSnapshot{Profile: semanticProfile{ID: strings.Repeat("c", 64), BaseID: semanticBaseID(record.Inventory)}, Files: []semanticFileResult{{ID: "result", Path: file.Path, BlobID: file.BlobID, Status: "indexed", Symbols: []semanticSymbol{{Name: "run", Kind: 12, StartByte: 1, EndByte: 49}}}}}
	model := newInventoryBrowserModel(context.Background(), inventoryBrowserOptions{Record: record, Selection: inventorySelection{Path: "router", Status: "included"}, Semantic: snapshot})
	model, _ = inventoryBrowserTestKey(t, model, "s")
	if !strings.Contains(strings.Join(model.page, "\n"), "run") {
		t.Fatal("symbol missing from browser")
	}
	model, _ = inventoryBrowserTestKey(t, model, "esc")
	model, _ = inventoryBrowserTestKey(t, model, "p")
	model, _ = inventoryBrowserTestKey(t, model, "enter")
	if model.planView == nil || model.planView.plan.SemanticProfileID != snapshot.Profile.ID || len(model.planView.items[1]) != 1 {
		t.Fatal("browser ignored semantic index")
	}
}
