package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func inventoryBrowserTestKey(t *testing.T, model inventoryBrowserModel, key string) (inventoryBrowserModel, tea.Cmd) {
	t.Helper()
	next, command := model.handleKey(key)
	return next.(inventoryBrowserModel), command
}

func TestInventoryBrowserNavigationAndEditing(t *testing.T) {
	_, directory := newInventoryFixture(t)
	first := runReposeRecord(t, directory, "inventory", "build")
	var output bytes.Buffer
	environment := cliEnvironment{Cwd: directory, Stdout: &output, Stderr: &output}
	environment.InventoryUI = func(ctx context.Context, _ io.Reader, _ io.Writer, options inventoryBrowserOptions) error {
		model := newInventoryBrowserModel(ctx, options)
		if !options.Editable || model.selectedPath() != "." || len(model.rows) != 3 {
			t.Fatalf("initial rows = %+v", model.rows)
		}
		model, _ = inventoryBrowserTestKey(t, model, "j")
		if model.selectedPath() != "pcbnew" {
			t.Fatal("navigation did not select pcbnew")
		}
		model, _ = inventoryBrowserTestKey(t, model, "enter")
		if model.selection.Path != "pcbnew" || len(model.rows) != 4 {
			t.Fatal("did not descend into directory")
		}
		model, _ = inventoryBrowserTestKey(t, model, "x")
		model, _ = inventoryBrowserTestKey(t, model, "esc")
		if model.mode != "" || model.options.Record.ID != first.ID {
			t.Fatal("cancel changed inventory")
		}
		model, _ = inventoryBrowserTestKey(t, model, "g")
		next, _ := model.Update(tea.PasteMsg{Content: "board"})
		model = next.(inventoryBrowserModel)
		var command tea.Cmd
		model, command = inventoryBrowserTestKey(t, model, "enter")
		if command == nil || !model.pending {
			t.Fatal("save was not dispatched")
		}
		next, _ = model.Update(command())
		model = next.(inventoryBrowserModel)
		if model.pending || model.options.Record.ID == first.ID || inventoryFileByPath(t, model.options.Record.Inventory, "pcbnew/main.cpp").Group != "board" {
			t.Fatal("save did not refresh model")
		}
		// An independent CLI edit makes this browser stale; it must not erase it.
		if _, err := executeReposeTest(t, directory, "inventory", "annotate", "pcbnew", "--tag", "cli"); err != nil {
			t.Fatal(err)
		}
		model, _ = inventoryBrowserTestKey(t, model, "n")
		model.input = "browser note"
		model, command = inventoryBrowserTestKey(t, model, "enter")
		next, _ = model.Update(command())
		model = next.(inventoryBrowserModel)
		if !strings.Contains(model.message, "changed") || model.pending {
			t.Fatalf("missing conflict: %s", model.message)
		}
		model, command = inventoryBrowserTestKey(t, model, "r")
		next, _ = model.Update(command())
		model = next.(inventoryBrowserModel)
		model, _ = inventoryBrowserTestKey(t, model, "n")
		model.input = "browser note"
		model, command = inventoryBrowserTestKey(t, model, "enter")
		next, _ = model.Update(command())
		model = next.(inventoryBrowserModel)
		file := inventoryFileByPath(t, model.options.Record.Inventory, "pcbnew/main.cpp")
		if len(file.Tags) != 1 || file.Tags[0] != "cli" || len(file.Notes) != 1 || file.Notes[0] != "browser note" {
			t.Fatalf("edits lost: %+v", file)
		}
		model, _ = inventoryBrowserTestKey(t, model, "p")
		model, _ = inventoryBrowserTestKey(t, model, "enter")
		if model.page == nil || !strings.Contains(strings.Join(model.page, "\n"), "3 files") {
			t.Fatal("preview missing")
		}
		model, _ = inventoryBrowserTestKey(t, model, "esc")
		model, _ = inventoryBrowserTestKey(t, model, "/")
		model.input = "main.h"
		model, _ = inventoryBrowserTestKey(t, model, "enter")
		if len(model.rows) != 1 || model.selectedPath() != "pcbnew/main.h" {
			t.Fatalf("filter = %+v", model.rows)
		}
		model, _ = inventoryBrowserTestKey(t, model, "esc")
		model, _ = inventoryBrowserTestKey(t, model, "h")
		model, _ = inventoryBrowserTestKey(t, model, "a")
		if model.selection.Path != "." || model.selection.Status != "all" || len(model.rows) != 6 {
			t.Fatalf("parent/all rows = %+v", model.rows)
		}
		return nil
	}
	if err := runReposeCLI(context.Background(), []string{"inventory", "browse"}, environment); err != nil {
		t.Fatal(err)
	}
	old := runReposeRecord(t, directory, "inventory", "show", first.ID)
	if inventoryFileByPath(t, old.Inventory, "pcbnew/main.cpp").Group != "pcbnew" {
		t.Fatal("browser changed history")
	}
}

func TestInventoryBrowserReadOnlyHistoryAndTerminalRequirement(t *testing.T) {
	_, directory := newInventoryFixture(t)
	first := runReposeRecord(t, directory, "inventory", "build")
	var output bytes.Buffer
	environment := cliEnvironment{Cwd: directory, Stdout: &output, Stderr: &output}
	if err := runReposeCLI(context.Background(), []string{"inventory", "browse"}, environment); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("nonterminal = %v", err)
	}
	environment.InventoryUI = func(ctx context.Context, _ io.Reader, _ io.Writer, options inventoryBrowserOptions) error {
		if options.Editable {
			t.Fatal("explicit selector editable")
		}
		model := newInventoryBrowserModel(ctx, options)
		model, command := inventoryBrowserTestKey(t, model, "x")
		if command != nil || model.mode != "" || !strings.Contains(model.message, "read-only") {
			t.Fatal("history accepted edit")
		}
		if _, err := options.Save(ctx, options.Record, inventoryBrowserEdit{"group", "pcbnew", "board"}); err == nil {
			t.Fatal("save callback failed to enforce read-only")
		}
		return nil
	}
	for _, selector := range []string{first.ID, "latest"} {
		if err := runReposeCLI(context.Background(), []string{"inventory", "browse", selector}, environment); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInventoryBrowserResizeInputAndErrorRendering(t *testing.T) {
	repository, _ := newInventoryFixture(t)
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	inventory.Files[0].Notes = []string{"note\x1b[2J\nwith control characters"}
	model := newInventoryBrowserModel(context.Background(), inventoryBrowserOptions{Record: InventoryRecord{ID: strings.Repeat("a", 64), Inventory: inventory}, Selection: inventorySelection{Path: ".", Status: "all"}, Editable: true})
	for _, size := range []tea.WindowSizeMsg{{Width: 120, Height: 30}, {Width: 60, Height: 15}, {Width: 10, Height: 3}, {Width: 1, Height: 1}} {
		next, _ := model.Update(size)
		model = next.(inventoryBrowserModel)
		view := model.View()
		lines := strings.Split(view.Content, "\n")
		if !view.AltScreen || len(lines) > size.Height {
			t.Fatal("view exceeded terminal height")
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size.Width || strings.ContainsRune(line, '\x1b') {
				t.Fatalf("unsafe/oversized line: %q", line)
			}
		}
	}
	model, _ = inventoryBrowserTestKey(t, model, "n")
	model.input = "é"
	model, _ = inventoryBrowserTestKey(t, model, "backspace")
	model, _ = inventoryBrowserTestKey(t, model, "backspace")
	if model.input != "" {
		t.Fatal("unicode backspace failed")
	}
	model, _ = inventoryBrowserTestKey(t, model, "enter")
	if model.mode != "note" {
		t.Fatal("blank note submitted")
	}
	model, _ = inventoryBrowserTestKey(t, model, "esc")
	next, _ := model.Update(inventoryBrowserLoaded{Err: errors.New("write failed")})
	model = next.(inventoryBrowserModel)
	if model.pending || !strings.Contains(model.message, "write failed") {
		t.Fatal("save error not surfaced")
	}
}
