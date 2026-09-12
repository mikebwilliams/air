package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func inventoryPlanBrowserFixture(t *testing.T) (InventoryRecord, inventoryPlanSession) {
	t.Helper()
	file := InventoryFile{Path: "router/large.cpp", BlobID: strings.Repeat("b", 40), Bytes: 300, Group: "router", Kind: "source", CommandIDs: []int{1}}
	record := InventoryRecord{ID: strings.Repeat("a", 64), Inventory: Inventory{SnapshotSHA: strings.Repeat("c", 40), Files: []InventoryFile{file,
		{Path: "thirdparty/api.h", BlobID: strings.Repeat("d", 40), Excluded: true, ExclusionReason: "vendor", Group: "thirdparty"}}}}
	snapshot := &semanticSnapshot{Profile: semanticProfile{ID: strings.Repeat("e", 64), BaseID: semanticBaseID(record.Inventory)}, Files: []semanticFileResult{{
		ID: "saved-result", Path: file.Path, BlobID: file.BlobID, Status: "indexed", CommandID: 1, CommandSource: file.Path, CommandArguments: []string{"clang++", "-DNAME=hello world"},
		Symbols: []semanticSymbol{{Name: "ns", Kind: 3, StartByte: 10, EndByte: 290, Children: []semanticSymbol{
			{Name: "first", Kind: 12, StartByte: 20, EndByte: 80}, {Name: "large", Kind: 12, StartByte: 100, EndByte: 240},
			{Name: "last", Detail: "void last(int argument)", Kind: 12, StartByte: 250, EndByte: 280, Range: semanticRange{Start: semanticPosition{Line: 20}, End: semanticPosition{Line: 25}}},
		}}}, Includes: []semanticLink{{Path: "thirdparty/api.h", SHA256: "header-hash"}},
		Diagnostics: []semanticDiagnostic{{Severity: 2, Message: "unused value", Range: semanticRange{Start: semanticPosition{Line: 2}}}},
	}}}
	plan, err := planSemanticInventory(record, inventorySelection{Path: "router", Status: "included"}, "Check ownership.", inventoryPlanLimits{2, 100}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for i := range plan.Assignments {
		plan.Assignments[i].Context = append(plan.Assignments[i].Context, inventoryContextFile{Path: "thirdparty/api.h", Reason: "another includer"})
	}
	return record, inventoryPlanSession{Plan: plan, Semantic: snapshot}
}

func TestInventoryPlanBrowserAssignmentSymbolsAndContext(t *testing.T) {
	record, session := inventoryPlanBrowserFixture(t)
	m := newInventoryPlanBrowser(session, record.Inventory)
	originalID := m.plan.ID
	m.handleKey("end", 120, 30)
	lastOrdinal := m.visible[m.cursor]
	if lastOrdinal == 0 {
		t.Fatal("assignment navigation did not move")
	}
	m.handleKey("/", 120, 30)
	m.input = "NS::LAST"
	m.handleKey("enter", 120, 30)
	if len(m.visible) != 1 || m.visible[0] != lastOrdinal || m.plan.ID != originalID {
		t.Fatalf("filter changed plan identity or original ordinals: %+v", m.visible)
	}
	m.handleKey("s", 120, 30)
	if !m.detailFocus || len(m.items[1]) != 2 {
		t.Fatalf("expected enclosing namespace and last function: %+v", m.items[1])
	}
	if !strings.Contains(strings.Join(m.items[1][0].Details, "\n"), "extends outside") || !strings.Contains(m.items[1][1].Label, "ns::last") {
		t.Fatal("symbol containment was lost")
	}
	m.handleKey("j", 120, 30)
	m.handleKey("enter", 120, 30)
	details := strings.Join(m.page, "\n")
	for _, expected := range []string{"void last(int argument)", "21:1", "[250, 280)", "Fully inside", "Command 1 from router/large.cpp", "saved-result"} {
		if !strings.Contains(details, expected) {
			t.Errorf("missing %q in symbol details: %s", expected, details)
		}
	}
	m.handleKey("esc", 120, 30)
	m.handleKey("c", 120, 30)
	if len(m.items[2]) != 1 {
		t.Fatal("context path duplicated")
	}
	m.handleKey("enter", 120, 30)
	details = strings.Join(m.page, "\n")
	for _, expected := range []string{"thirdparty/api.h", "resolved include from router/large.cpp", "another includer", "header-hash", "Excluded from review targets: vendor"} {
		if !strings.Contains(details, expected) {
			t.Errorf("missing %q in context: %s", expected, details)
		}
	}
	m.handleKey("esc", 120, 30)
	m.handleKey("d", 120, 30)
	m.handleKey("enter", 120, 30)
	if !strings.Contains(strings.Join(m.page, "\n"), "File-level parse diagnostic") {
		t.Fatal("diagnostic scope unclear")
	}
	m.handleKey("esc", 120, 30)
	m.handleKey("esc", 120, 30)
	if len(m.visible) != len(m.plan.Assignments) || m.visible[m.cursor] != lastOrdinal {
		t.Fatal("clearing filter lost assignment selection")
	}
	m.handleKey("/", 120, 30)
	m.input = "no such symbol"
	m.handleKey("enter", 120, 30)
	for _, key := range []string{"end", "s", "j", "enter", "t", "home"} {
		m.handleKey(key, 120, 30)
	}
	if m.assignment() != nil || len(m.items[0]) != 0 || m.page != nil {
		t.Fatal("empty filter retained an unrelated assignment")
	}
}

func TestInventoryPlanBrowserFallbackAndPartialFacts(t *testing.T) {
	record, session := inventoryPlanBrowserFixture(t)
	plan, err := planInventory(record, session.Plan.Selection, session.Plan.Goal, session.Plan.Limits)
	if err != nil {
		t.Fatal(err)
	}
	m := newInventoryPlanBrowser(inventoryPlanSession{Plan: plan}, record.Inventory)
	if len(m.items[0]) != 1 || !strings.Contains(m.items[0][0].Label, "[0,300)") || len(m.items[1]) != 0 {
		t.Fatal("file fallback lost whole-file target")
	}
	m.handleKey("s", 80, 24)
	if !strings.Contains(m.view(80, 24, true).Content, "No semantic index") || len(m.items[3]) == 0 {
		t.Fatal("missing indexing was hidden")
	}
	session.Semantic.Files[0].Status = "partial"
	session.Semantic.Files[0].Error = "unknown type"
	plan, err = planSemanticInventory(record, session.Plan.Selection, session.Plan.Goal, session.Plan.Limits, session.Semantic)
	if err != nil {
		t.Fatal(err)
	}
	m = newInventoryPlanBrowser(inventoryPlanSession{Plan: plan, Semantic: session.Semantic}, record.Inventory)
	if len(m.items[0]) != 1 || len(m.items[1]) != 4 || !strings.Contains(strings.Join(m.items[1][1].Details, "\n"), "Semantic status: partial") {
		t.Fatal("partial facts not labeled or whole-file fallback lost")
	}
	if !strings.Contains(strings.Join(m.items[3][0].Details, "\n"), "unknown type") || !m.assignment().Oversized {
		t.Fatal("parse error or oversized warning hidden")
	}
}

func TestInventoryPlanBrowserParentNavigationAndRendering(t *testing.T) {
	record, session := inventoryPlanBrowserFixture(t)
	session.Plan.Goal = "check\x1b[2J\n" + strings.Repeat("界", 80)
	session.Semantic.Files[0].Symbols[0].Detail = strings.Repeat("LongQualifiedName::", 100) + "tail-marker\x1b[2J"
	m := newInventoryBrowserModel(context.Background(), inventoryBrowserOptions{Record: record, Selection: session.Plan.Selection, Semantic: session.Semantic, InitialPlan: &session})
	m, _ = inventoryBrowserTestKey(t, m, "s")
	m, _ = inventoryBrowserTestKey(t, m, "enter")
	wrapped := inventoryPlanWrap(m.planView.page, 60)
	if !strings.Contains(strings.Join(wrapped, ""), "tail-marker") {
		t.Fatal("long signature was truncated")
	}
	m, _ = inventoryBrowserTestKey(t, m, "end")
	if m.planView.scroll == 0 {
		t.Fatal("long details did not scroll")
	}
	for _, page := range []bool{true, false} {
		if !page {
			m, _ = inventoryBrowserTestKey(t, m, "esc")
		}
		for _, size := range []tea.WindowSizeMsg{{Width: 140, Height: 30}, {Width: 60, Height: 15}, {Width: 10, Height: 3}, {Width: 1, Height: 1}} {
			next, _ := m.Update(size)
			m = next.(inventoryBrowserModel)
			view := m.View()
			lines := strings.Split(view.Content, "\n")
			if !view.AltScreen || len(lines) > size.Height {
				t.Fatal("view exceeded terminal height")
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size.Width || strings.ContainsRune(line, '\x1b') {
					t.Fatalf("unsafe or oversized line: %q", line)
				}
			}
		}
	}
	m, _ = inventoryBrowserTestKey(t, m, "/")
	next, _ := m.Update(tea.PasteMsg{Content: "ns::last\n"})
	m = next.(inventoryBrowserModel)
	m, _ = inventoryBrowserTestKey(t, m, "enter")
	if len(m.planView.visible) != 1 {
		t.Fatal("pasted search did not filter")
	}
	m, command := inventoryBrowserTestKey(t, m, "q")
	if command == nil {
		t.Fatal("standalone browser did not quit")
	}
	m.options.InitialPlan = nil
	selection, selected := m.selection, m.selectedPath()
	m, command = inventoryBrowserTestKey(t, m, "q")
	if command != nil || m.planView != nil || m.selection != selection || m.selectedPath() != selected {
		t.Fatal("embedded browser did not return to inventory selection")
	}
}

func TestInventoryPlanTUIUsesSavedIndexAndCLISelection(t *testing.T) {
	repository, directory := newInventoryFixture(t)
	record := runReposeRecord(t, directory, "inventory", "build")
	ctx := context.Background()
	var output bytes.Buffer
	var opened *inventoryPlanSession
	environment := cliEnvironment{Cwd: directory, Stdout: &output, Stderr: &output, InventoryUI: func(_ context.Context, _ io.Reader, _ io.Writer, options inventoryBrowserOptions) error {
		opened = options.InitialPlan
		if options.Editable || opened == nil || opened.Plan.Selection.Path != "pcbnew" || opened.Plan.Goal != "Check lifetime." || opened.Plan.Limits.MaxFiles != 1 {
			t.Fatalf("wrong TUI options: %+v", options)
		}
		return nil
	}}
	args := []string{"inventory", "plan", "--tui", "--path", "pcbnew", "--goal", "Check lifetime.", "--max-files", "1"}
	if err := runReposeCLI(ctx, args, environment); err != nil || opened.Plan.Planner != "files-v1" || opened.Semantic != nil {
		t.Fatalf("unindexed fallback: %+v %v", opened, err)
	}
	if err := runReposeCLI(ctx, append(args, "--semantic"), environment); err == nil {
		t.Fatal("explicit semantic planning accepted a missing index")
	}
	if err := runReposeCLI(ctx, append(args, "--json"), environment); err == nil || !strings.Contains(err.Error(), "--tui and --json") {
		t.Fatal("conflicting outputs were accepted")
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openInventoryStore(ctx, database, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	profile := semanticProfile{BaseID: semanticBaseID(record.Inventory), FormatVersion: 1}
	data, _ := json.Marshal(profile)
	profile.ID = inventoryHash(data)
	if err := store.saveSemanticProfile(ctx, profile, time.Now()); err != nil {
		t.Fatal(err)
	}
	file := inventoryFileByPath(t, record.Inventory, "pcbnew/main.cpp")
	result, err := store.saveSemanticResult(ctx, semanticFileResult{ProfileID: profile.ID, Path: file.Path, BlobID: file.BlobID, Status: "indexed", Symbols: []semanticSymbol{{Name: "run", Kind: 12, StartByte: 0, EndByte: file.Bytes}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Inspection must use only persisted facts, even if the build disappears.
	if err := os.Remove(filepath.Join(directory, "build/compile_commands.json")); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--index", profile.ID}} {
		if err := runReposeCLI(ctx, append(args, extra...), environment); err != nil {
			t.Fatal(err)
		}
		if opened.Plan.Planner != "symbols-v1" || opened.Semantic.Profile.ID != profile.ID || opened.Plan.SemanticResults[0].ID != result.ID || opened.Semantic.Files[0].ID != result.ID {
			t.Fatal("plan and browser loaded different semantic facts")
		}
	}
	result.Status = "partial"
	if _, err := store.saveSemanticResult(ctx, result, time.Now()); err != nil {
		t.Fatal(err)
	}
	if opened.Semantic.Files[0].Status != "indexed" {
		t.Fatal("index progress relabeled an open plan")
	}
	if current := runReposeRecord(t, directory, "inventory", "show"); current.ID != record.ID || current.ReviewedAt != nil {
		t.Fatal("browsing changed inventory or approval")
	}
}
