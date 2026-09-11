package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

type inventoryUIRunner func(context.Context, io.Reader, io.Writer, inventoryBrowserOptions) error

type inventoryBrowserEdit struct{ Action, Prefix, Text string }

type inventoryBrowserOptions struct {
	Record    InventoryRecord
	Selection inventorySelection
	Editable  bool
	Save      func(context.Context, InventoryRecord, inventoryBrowserEdit) (InventoryRecord, error)
	Reload    func(context.Context) (InventoryRecord, error)
}

func runInventoryBrowser(ctx context.Context, environment cliEnvironment, databasePath string, record InventoryRecord, selection inventorySelection, editable bool) error {
	options := inventoryBrowserOptions{Record: record, Selection: selection, Editable: editable}
	options.Reload = func(ctx context.Context) (InventoryRecord, error) {
		store, err := openInventoryReadOnly(ctx, databasePath)
		if err != nil {
			return InventoryRecord{}, err
		}
		defer store.Close()
		selector := record.ID
		if editable {
			selector = "current"
		}
		return store.Inventory(ctx, selector)
	}
	options.Save = func(ctx context.Context, expected InventoryRecord, edit inventoryBrowserEdit) (InventoryRecord, error) {
		if !editable {
			return InventoryRecord{}, errors.New("historical inventory is read-only; browse current to edit")
		}
		var updated Inventory
		var err error
		switch edit.Action {
		case "exclude", "include":
			updated, _, err = editInventoryScope(expected.Inventory, []string{edit.Prefix}, edit.Action == "exclude", edit.Text)
		case "group":
			if strings.TrimSpace(edit.Text) == "" {
				return InventoryRecord{}, errors.New("group name must not be blank")
			}
			updated, err = editInventoryAnnotations(expected.Inventory, []string{edit.Prefix}, inventoryAnnotationEdit{Group: edit.Text})
		case "tag":
			updated, err = editInventoryAnnotations(expected.Inventory, []string{edit.Prefix}, inventoryAnnotationEdit{Tags: []string{edit.Text}})
		case "note":
			updated, err = editInventoryAnnotations(expected.Inventory, []string{edit.Prefix}, inventoryAnnotationEdit{Note: edit.Text})
		default:
			return InventoryRecord{}, fmt.Errorf("unknown edit %q", edit.Action)
		}
		if err != nil {
			return InventoryRecord{}, err
		}
		store, err := openInventoryStore(ctx, databasePath, false)
		if err != nil {
			return InventoryRecord{}, err
		}
		defer store.Close()
		return store.saveCurrentInventory(ctx, updated, &expected.ID, environmentNow(environment))
	}
	runner := environment.InventoryUI
	if runner == nil {
		runner = runTerminalInventoryUI
	}
	return runner(ctx, environment.Stdin, environment.Stdout, options)
}

func runTerminalInventoryUI(ctx context.Context, input io.Reader, output io.Writer, options inventoryBrowserOptions) error {
	inputFile, inputOK := input.(*os.File)
	outputFile, outputOK := output.(*os.File)
	if !inputOK || !outputOK || !term.IsTerminal(inputFile.Fd()) || !term.IsTerminal(outputFile.Fd()) {
		return errors.New("inventory browse requires an interactive terminal; use inventory tree, inspect, or plan --json")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	model := newInventoryBrowserModel(ctx, options)
	_, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("run inventory browser: %w", err)
	}
	return nil
}

type inventoryBrowserRow struct {
	Path      string
	Directory bool
	inventoryCounts
}

type inventoryBrowserLoaded struct {
	Record InventoryRecord
	Err    error
}

type inventoryBrowserModel struct {
	ctx                                   context.Context
	options                               inventoryBrowserOptions
	selection                             inventorySelection
	rows                                  []inventoryBrowserRow
	cursor                                int
	width, height                         int
	query, mode, input, editPath, message string
	pending                               bool
	page                                  []string
	scroll                                int
}

func newInventoryBrowserModel(ctx context.Context, options inventoryBrowserOptions) inventoryBrowserModel {
	m := inventoryBrowserModel{ctx: ctx, options: options, selection: options.Selection, width: 100, height: 30}
	m.refreshRows("")
	return m
}

func (m inventoryBrowserModel) Init() tea.Cmd { return nil }

func (m *inventoryBrowserModel) refreshRows(preferred string) {
	rows := map[string]*inventoryBrowserRow{}
	prefix := m.selection.Path
	rows[prefix] = &inventoryBrowserRow{Path: prefix, Directory: true}
	for _, file := range m.selection.files(m.options.Record.Inventory) {
		rows[prefix].add(file)
		if file.Path == prefix {
			rows[prefix].Directory = false
			continue
		}
		relative := file.Path
		if prefix != "." {
			relative = strings.TrimPrefix(file.Path, prefix+"/")
		}
		name, _, directory := strings.Cut(relative, "/")
		child := name
		if prefix != "." {
			child = prefix + "/" + name
		}
		row := rows[child]
		if row == nil {
			row = &inventoryBrowserRow{Path: child, Directory: directory}
			rows[child] = row
		}
		row.add(file)
	}
	m.rows = []inventoryBrowserRow{}
	for _, row := range rows {
		if m.query == "" || strings.Contains(strings.ToLower(row.Path), strings.ToLower(m.query)) {
			m.rows = append(m.rows, *row)
		}
	}
	sort.Slice(m.rows, func(i, j int) bool {
		if m.rows[i].Path == m.rows[j].Path {
			return false
		}
		if m.rows[i].Path == prefix {
			return true
		}
		if m.rows[j].Path == prefix {
			return false
		}
		if m.rows[i].Directory != m.rows[j].Directory {
			return m.rows[i].Directory
		}
		return m.rows[i].Path < m.rows[j].Path
	})
	m.cursor = min(m.cursor, max(0, len(m.rows)-1))
	for index, row := range m.rows {
		if row.Path == preferred {
			m.cursor = index
			break
		}
	}
}

func (m inventoryBrowserModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
	case inventoryBrowserLoaded:
		m.pending = false
		if message.Err != nil {
			m.message = message.Err.Error() + "; r reloads current before retrying"
		} else {
			preferred := m.selectedPath()
			m.options.Record = message.Record
			m.refreshRows(preferred)
			m.message = "Loaded inventory " + message.Record.ID[:12] + " (" + inventoryReviewLabel(message.Record.ReviewedAt != nil) + ")"
		}
	case tea.PasteMsg:
		if m.mode != "" {
			m.input += singleLine(message.Content)
		}
	case tea.KeyPressMsg:
		return m.handleKey(message.String())
	}
	return m, nil
}

func (m inventoryBrowserModel) selectedPath() string {
	if len(m.rows) == 0 {
		return ""
	}
	return m.rows[m.cursor].Path
}

func (m inventoryBrowserModel) handleKey(key string) (tea.Model, tea.Cmd) {
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if m.pending {
		return m, nil
	}
	if m.mode != "" {
		return m.handleInput(key)
	}
	if m.page != nil {
		switch key {
		case "q", "esc", "p", "?":
			m.page = nil
			m.scroll = 0
		case "down", "j":
			m.scroll++
		case "up", "k":
			m.scroll--
		case "pgdown", "ctrl+d":
			m.scroll += max(1, m.height-5)
		case "pgup", "ctrl+u":
			m.scroll -= max(1, m.height-5)
		case "home":
			m.scroll = 0
		case "end":
			m.scroll = len(m.page)
		}
		m.scroll = min(max(0, len(m.page)-max(1, m.height-5)), max(0, m.scroll))
		return m, nil
	}
	m.message = ""
	switch key {
	case "q":
		return m, tea.Quit
	case "esc":
		m.query = ""
		m.refreshRows("")
	case "down", "j":
		m.cursor = min(max(0, len(m.rows)-1), m.cursor+1)
	case "up", "k":
		m.cursor = max(0, m.cursor-1)
	case "pgdown", "ctrl+d":
		m.cursor = min(max(0, len(m.rows)-1), m.cursor+max(1, m.height-5))
	case "pgup", "ctrl+u":
		m.cursor = max(0, m.cursor-max(1, m.height-5))
	case "home":
		m.cursor = 0
	case "end":
		m.cursor = max(0, len(m.rows)-1)
	case "enter", "right", "l":
		if len(m.rows) > 0 && m.rows[m.cursor].Directory {
			m.selection.Path = m.selectedPath()
			m.cursor = 0
			m.query = ""
			m.refreshRows("")
		} else if len(m.rows) > 0 {
			m.page = m.detailLines()
			m.scroll = 0
		}
	case "left", "h", "backspace":
		old := m.selection.Path
		m.selection.Path = path.Dir(old)
		m.query = ""
		m.refreshRows(old)
	case "/":
		m.mode = "search"
		m.input = m.query
	case "a":
		if m.selection.Status == "all" {
			m.selection.Status = "included"
		} else {
			m.selection.Status = "all"
		}
		m.refreshRows(m.selectedPath())
	case "r":
		m.pending = true
		m.message = "Reloading..."
		return m, func() tea.Msg { record, err := m.options.Reload(m.ctx); return inventoryBrowserLoaded{record, err} }
	case "d":
		m.page = m.detailLines()
		m.scroll = 0
	case "g", "t", "n", "x", "i", "p":
		if m.selectedPath() == "" {
			break
		}
		if key != "p" && !m.options.Editable {
			m.message = "Historical inventory is read-only; browse current to edit."
			break
		}
		m.editPath = m.selectedPath()
		m.mode = map[string]string{"g": "group", "t": "tag", "n": "note", "x": "exclude", "i": "include", "p": "plan"}[key]
		m.input = ""
		if key == "p" {
			m.input = "Find correctness issues."
		}
	case "?":
		m.page = []string{"Inventory browser", "", "j/k or arrows: move; Enter/right: open directory or file details", "h/left/Backspace: parent; Home/End and PgUp/PgDown: navigate", "/: filter visible paths (Enter applies); Esc: clear filter", "a: toggle all files / included files; r: reload inventory", "g: set group; t: add tag; n: add note", "x: exclude with reason; i: include (optional reason)", "Edits apply to ALL paths under the selected prefix, including hidden files.", "Enter submits an edit; Esc cancels. Each edit versions the inventory.", "Explicit inventory IDs and latest open read-only.", "p: preview selected scope with a question (8 files / 65536 bytes)", "Preview pages: j/k, PgUp/PgDown scroll; Esc/q return", "CLI inventory plan --json exports previews and accepts other size limits.", "Policy export/import supports removing tags or notes.", "No scans or compiler indexing are started by this browser.", "q: quit"}
		m.page = append(m.page, "d: full details of the selected file or directory")
		m.scroll = 0
	}
	return m, nil
}

func (m inventoryBrowserModel) handleInput(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.mode, m.input = "", ""
		return m, nil
	case "backspace":
		_, size := utf8.DecodeLastRuneInString(m.input)
		m.input = m.input[:len(m.input)-size]
	case "ctrl+u":
		m.input = ""
	case "enter":
		mode := m.mode
		if mode == "search" {
			m.query = m.input
			m.mode = ""
			m.cursor = 0
			m.refreshRows("")
			return m, nil
		}
		if mode != "include" && strings.TrimSpace(m.input) == "" {
			m.message = "Enter a value, or Esc to cancel."
			return m, nil
		}
		if mode == "plan" {
			selection := m.selection
			selection.Path, selection.Status = m.editPath, "included"
			plan, err := planInventory(m.options.Record, selection, m.input, inventoryPlanLimits{8, 65536})
			m.mode = ""
			if err != nil {
				m.message = err.Error()
				return m, nil
			}
			var output bytes.Buffer
			_ = printInventoryPlan(&output, plan)
			m.page = strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
			m.scroll = 0
			return m, nil
		}
		edit := inventoryBrowserEdit{mode, m.editPath, m.input}
		m.mode = ""
		m.pending = true
		m.message = "Saving policy..."
		return m, func() tea.Msg {
			record, err := m.options.Save(m.ctx, m.options.Record, edit)
			return inventoryBrowserLoaded{record, err}
		}
	default:
		if utf8.RuneCountInString(key) == 1 {
			m.input += key
		}
		if key == "space" {
			m.input += " "
		}
	}
	return m, nil
}

func inventoryDisplay(value string) string {
	quoted := strconv.Quote(value)
	return quoted[1 : len(quoted)-1]
}

func (m inventoryBrowserModel) detailLines() []string {
	if m.selectedPath() == "" {
		return []string{"No matching paths."}
	}
	selection := m.selection
	selection.Path = m.selectedPath()
	inspection := inspectInventory(m.options.Record, selection)
	lines := []string{inventoryDisplay(selection.Path), fmt.Sprintf("%d files  %d bytes", inspection.Files, inspection.Bytes),
		fmt.Sprintf("%d sources / %d headers / %d excluded", inspection.Sources, inspection.Headers, inspection.Excluded),
		fmt.Sprintf("%d sources without commands", inspection.MissingCommand), fmt.Sprintf("%d headers await semantic association", inspection.UnmappedHeader), ""}
	if !m.rows[m.cursor].Directory && len(inspection.Largest) == 1 {
		file := inspection.Largest[0]
		lines = append(lines, "Blob: "+file.BlobID, fmt.Sprintf("Command IDs: %v", file.CommandIDs))
		if file.Excluded {
			lines = append(lines, "Excluded: "+inventoryDisplay(file.ExclusionReason))
		}
	}
	for _, group := range inspection.Groups {
		lines = append(lines, "Group: "+inventoryDisplay(group.Name), fmt.Sprintf("Tags: %q", group.Tags))
		for _, note := range group.Notes {
			lines = append(lines, "Note: "+inventoryDisplay(note))
		}
	}
	lines = append(lines, "", "Largest selected files:")
	for _, file := range inspection.Largest {
		lines = append(lines, fmt.Sprintf("%d  %s", file.Bytes, inventoryDisplay(path.Base(file.Path))))
	}
	return lines
}

func (m inventoryBrowserModel) View() tea.View {
	width, height := max(1, m.width), max(1, m.height)
	access := "current; editable"
	if !m.options.Editable {
		access = "read-only"
	}
	lines := []string{fmt.Sprintf("Repose inventory %s  %s  %s", m.options.Record.ID[:12], access, inventoryReviewLabel(m.options.Record.ReviewedAt != nil)),
		fmt.Sprintf("Path: %s  status: %s  group: %q  tag: %q  filter: %q", inventoryDisplay(m.selection.Path), m.selection.Status, m.selection.Group, m.selection.Tag, m.query)}
	bodyHeight := max(0, height-5)
	if m.page != nil {
		lines = append(lines, fitLines(m.page[min(m.scroll, len(m.page)):], bodyHeight, width)...)
	} else {
		list := []string{}
		start := max(0, m.cursor-bodyHeight+1)
		for index := start; index < min(len(m.rows), start+bodyHeight); index++ {
			row := m.rows[index]
			marker := " "
			if index == m.cursor {
				marker = ">"
			}
			name := path.Base(row.Path)
			if row.Path == m.selection.Path {
				name = "."
			}
			if row.Directory {
				name += "/"
			}
			list = append(list, fmt.Sprintf("%s %5d %8d  %s", marker, row.Files, row.Bytes, inventoryDisplay(name)))
		}
		if len(m.rows) == 0 {
			list = append(list, "No matching paths. Esc clears the filter.")
		}
		if width >= 90 {
			leftWidth := width / 2
			left := fitLines(list, bodyHeight, leftWidth)
			right := fitLines(m.detailLines(), bodyHeight, width-leftWidth-3)
			for index := range left {
				lines = append(lines, padRight(left[index], leftWidth)+" | "+right[index])
			}
		} else {
			lines = append(lines, fitLines(list, bodyHeight, width)...)
		}
	}
	message := inventoryDisplay(m.message)
	if m.mode != "" {
		if m.mode == "search" {
			message = "Filter visible paths (Enter applies)"
		} else if m.mode == "plan" {
			message = "Question for included selection under " + inventoryDisplay(m.editPath)
		} else {
			message = fmt.Sprintf("%s: ALL paths under %s", m.mode, inventoryDisplay(m.editPath))
		}
		if m.message != "" {
			message = inventoryDisplay(m.message)
		}
		lines = append(lines, message, "> "+inventoryDisplay(m.input), "Enter submit | Esc cancel | Ctrl+U clear")
	} else if m.page != nil {
		lines = append(lines, message, "j/k scroll | PgUp/PgDown | Esc/q back", "No scan is started by this browser.")
	} else {
		lines = append(lines, message, "Enter open | d details | g group | t tag | n note | x exclude | i include | p plan | ? help | q quit", "List: files / bytes / path. Edits include hidden descendants. / filter | a all | r reload")
	}
	view := tea.NewView(strings.Join(fitLines(lines, height, width), "\n"))
	view.AltScreen = true
	return view
}
