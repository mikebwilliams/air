package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The plan and facts travel together so a preview never mixes result versions.
type inventoryPlanSession struct {
	Plan     inventoryPlan
	Semantic *semanticSnapshot
}

type inventoryPlanItem struct {
	Label   string
	Details []string
}

type inventoryPlanBrowser struct {
	plan        inventoryPlan
	indexed     map[string]semanticFileResult
	files       map[string]InventoryFile
	visible     []int // Original assignment ordinals, preserved when filtering.
	cursor      int
	tab         int
	item        int
	detailFocus bool
	items       [4][]inventoryPlanItem
	query       string
	input       string
	searching   bool
	page        []string
	scroll      int
}

var inventoryPlanTabs = [...]string{"Targets", "Symbols", "Context", "Diagnostics"}

func newInventoryPlanBrowser(session inventoryPlanSession, inventory Inventory) *inventoryPlanBrowser {
	m := &inventoryPlanBrowser{plan: session.Plan, indexed: session.Semantic.byPath(), files: map[string]InventoryFile{}}
	for _, file := range inventory.Files {
		m.files[file.Path] = file
	}
	m.filterAssignments()
	return m
}

func (m *inventoryPlanBrowser) assignment() *inventoryAssignment {
	if len(m.visible) == 0 {
		return nil
	}
	return &m.plan.Assignments[m.visible[m.cursor]]
}

func inventoryAssignmentRanges(assignment inventoryAssignment) []inventoryTargetRange {
	if len(assignment.Ranges) != 0 {
		return assignment.Ranges
	}
	ranges := make([]inventoryTargetRange, 0, len(assignment.Files))
	for _, file := range assignment.Files {
		ranges = append(ranges, inventoryTargetRange{Path: file.Path, BlobID: file.BlobID, EndByte: file.Bytes})
	}
	return ranges
}

// Visit only declarations overlapping target bytes, retaining enclosing scopes
// and explicitly distinguishing them from declarations wholly inside a target.
func visitInventoryTargetSymbols(ranges []inventoryTargetRange, filename string, symbols []semanticSymbol, parent string, visit func(semanticSymbol, string, bool)) {
	for _, symbol := range symbols {
		name := symbol.Name
		if parent != "" {
			name = parent + "::" + name
		}
		overlaps, contained := false, false
		for _, part := range ranges {
			if part.Path != filename {
				continue
			}
			if symbol.StartByte < part.EndByte && symbol.EndByte > part.StartByte ||
				symbol.StartByte == symbol.EndByte && symbol.StartByte >= part.StartByte && symbol.StartByte < part.EndByte {
				overlaps = true
				contained = contained || symbol.StartByte >= part.StartByte && symbol.EndByte <= part.EndByte
			}
		}
		if overlaps {
			visit(symbol, name, contained)
		}
		visitInventoryTargetSymbols(ranges, filename, symbol.Children, name, visit)
	}
}

func (m *inventoryPlanBrowser) filterAssignments() {
	previous := -1
	if len(m.visible) > 0 {
		previous = m.visible[m.cursor]
	}
	m.visible = nil
	query := strings.ToLower(m.query)
	for index, assignment := range m.plan.Assignments {
		match := query == "" || strings.Contains(strings.ToLower(assignment.ID+" "+assignment.Group+" "+assignment.Directory), query)
		ranges := inventoryAssignmentRanges(assignment)
		for _, file := range assignment.Files {
			match = match || strings.Contains(strings.ToLower(file.Path), query)
			if !match {
				visitInventoryTargetSymbols(ranges, file.Path, m.indexed[file.Path].Symbols, "", func(symbol semanticSymbol, name string, _ bool) {
					match = match || strings.Contains(strings.ToLower(name+" "+symbol.Detail), query)
				})
			}
		}
		if match {
			m.visible = append(m.visible, index)
		}
	}
	m.cursor = 0
	for cursor, index := range m.visible {
		if index == previous {
			m.cursor = cursor
		}
	}
	m.refreshItems()
}

func inventorySemanticProvenance(result semanticFileResult, exists bool) []string {
	if !exists {
		return []string{"Semantic status: pending (no saved result)."}
	}
	lines := []string{"Semantic status: " + inventoryDisplay(result.Status), "Result: " + inventoryDisplay(result.ID)}
	if result.CommandSource != "" {
		lines = append(lines, fmt.Sprintf("Command %d from %s", result.CommandID, inventoryDisplay(result.CommandSource)))
	}
	if result.Error != "" {
		lines = append(lines, "Index error: "+inventoryDisplay(result.Error))
	}
	return lines
}

func (m *inventoryPlanBrowser) refreshItems() {
	m.items = [4][]inventoryPlanItem{}
	m.item = 0
	assignment := m.assignment()
	if assignment == nil {
		return
	}
	ranges := inventoryAssignmentRanges(*assignment)
	for _, part := range ranges {
		result, exists := m.indexed[part.Path]
		details := []string{inventoryDisplay(part.Path), fmt.Sprintf("Target bytes [%d, %d) of %d (zero-based, end-exclusive)", part.StartByte, part.EndByte, m.files[part.Path].Bytes), "Blob: " + inventoryDisplay(part.BlobID)}
		if len(part.Symbols) > 0 {
			details = append(details, "Planner boundaries:")
			for _, name := range part.Symbols {
				details = append(details, "  "+inventoryDisplay(name))
			}
		}
		details = append(details, inventorySemanticProvenance(result, exists)...)
		if len(result.CommandArguments) > 0 {
			details = append(details, "Command arguments:")
			for _, argument := range result.CommandArguments {
				details = append(details, "  "+inventoryDisplay(argument))
			}
		}
		m.items[0] = append(m.items[0], inventoryPlanItem{fmt.Sprintf("%s [%d,%d)", inventoryDisplay(part.Path), part.StartByte, part.EndByte), details})
	}
	for _, file := range assignment.Files {
		result, exists := m.indexed[file.Path]
		visitInventoryTargetSymbols(ranges, file.Path, result.Symbols, "", func(symbol semanticSymbol, name string, contained bool) {
			coverage := "Fully inside target bytes."
			if !contained {
				coverage = "Overlaps target bytes; this declaration extends outside the assignment."
			}
			details := []string{inventoryDisplay(name)}
			if symbol.Detail != "" {
				details = append(details, inventoryDisplay(symbol.Detail))
			}
			details = append(details, inventoryDisplay(file.Path), "Kind: "+semanticSymbolKind(symbol.Kind),
				fmt.Sprintf("Lines/columns [%d:%d, %d:%d) (one-based, end-exclusive)", symbol.Range.Start.Line+1, symbol.Range.Start.Character+1, symbol.Range.End.Line+1, symbol.Range.End.Character+1),
				fmt.Sprintf("Symbol bytes [%d, %d)", symbol.StartByte, symbol.EndByte), coverage)
			details = append(details, inventorySemanticProvenance(result, exists)...)
			m.items[1] = append(m.items[1], inventoryPlanItem{fmt.Sprintf("%s %s :%d", semanticSymbolKind(symbol.Kind), inventoryDisplay(name), symbol.Range.Start.Line+1), details})
		})
		if !exists || result.Status != "indexed" {
			details := append([]string{inventoryDisplay(file.Path)}, inventorySemanticProvenance(result, exists)...)
			details = append(details, "Partial or missing facts do not establish semantic coverage; this file remains a whole-file target.")
			status := "pending"
			if exists {
				status = result.Status
			}
			m.items[3] = append(m.items[3], inventoryPlanItem{inventoryDisplay(file.Path) + ": " + inventoryDisplay(status), details})
		}
		for _, diagnostic := range result.Diagnostics {
			severity := map[int]string{1: "error", 2: "warning", 3: "information", 4: "hint"}[diagnostic.Severity]
			if severity == "" {
				severity = "diagnostic"
			}
			details := []string{fmt.Sprintf("%s:%d:%d (%s)", inventoryDisplay(file.Path), diagnostic.Range.Start.Line+1, diagnostic.Range.Start.Character+1, severity), inventoryDisplay(diagnostic.Message),
				"File-level parse diagnostic; it may lie outside this assignment's target bytes."}
			if len(diagnostic.Code) > 0 {
				details = append(details, "Code: "+inventoryDisplay(string(diagnostic.Code)))
			}
			details = append(details, inventorySemanticProvenance(result, true)...)
			m.items[3] = append(m.items[3], inventoryPlanItem{fmt.Sprintf("%s %s:%d %s", severity, inventoryDisplay(file.Path), diagnostic.Range.Start.Line+1, inventoryDisplay(diagnostic.Message)), details})
		}
	}
	contexts := map[string][]inventoryContextFile{}
	paths := []string{}
	for _, dependency := range assignment.Context {
		if _, exists := contexts[dependency.Path]; !exists {
			paths = append(paths, dependency.Path)
		}
		contexts[dependency.Path] = append(contexts[dependency.Path], dependency)
	}
	sort.Strings(paths)
	for _, filename := range paths {
		details := []string{inventoryDisplay(filename), "Context dependency; these bytes do not count against target limits."}
		for _, dependency := range contexts[filename] {
			details = append(details, "Reason: "+inventoryDisplay(dependency.Reason))
			if dependency.BlobID != "" {
				details = append(details, "Blob: "+inventoryDisplay(dependency.BlobID))
			}
			if dependency.SHA256 != "" {
				details = append(details, "SHA256: "+inventoryDisplay(dependency.SHA256))
			}
		}
		if file, exists := m.files[filename]; exists {
			details = append(details, "Group: "+inventoryDisplay(file.Group))
			if file.Excluded {
				details = append(details, "Excluded from review targets: "+inventoryDisplay(file.ExclusionReason))
			}
		}
		m.items[2] = append(m.items[2], inventoryPlanItem{inventoryDisplay(filename), details})
	}
	for _, warning := range assignment.Warnings {
		m.items[3] = append(m.items[3], inventoryPlanItem{"Plan warning: " + inventoryDisplay(warning), []string{"Plan warning", inventoryDisplay(warning)}})
	}
}

// Returns true when the caller should close this browser.
func (m *inventoryPlanBrowser) handleKey(key string, width, height int) bool {
	if m.searching {
		switch key {
		case "esc":
			m.searching = false
		case "enter":
			m.query, m.searching = m.input, false
			m.filterAssignments()
		case "backspace":
			_, size := utf8.DecodeLastRuneInString(m.input)
			m.input = m.input[:len(m.input)-size]
		case "ctrl+u":
			m.input = ""
		case "space":
			m.input += " "
		default:
			if utf8.RuneCountInString(key) == 1 {
				m.input += key
			}
		}
		return false
	}
	if m.page != nil {
		switch key {
		case "esc", "q", "left", "h", "backspace":
			m.page, m.scroll = nil, 0
		case "down", "j":
			m.scroll++
		case "up", "k":
			m.scroll--
		case "pgdown", "ctrl+d":
			m.scroll += max(1, height-5)
		case "pgup", "ctrl+u":
			m.scroll -= max(1, height-5)
		case "home":
			m.scroll = 0
		case "end":
			m.scroll = len(inventoryPlanWrap(m.page, width))
		}
		m.scroll = min(max(0, len(inventoryPlanWrap(m.page, width))-max(1, height-5)), max(0, m.scroll))
		return false
	}
	switch key {
	case "q":
		return true
	case "esc":
		if m.query == "" {
			return true
		}
		m.query = ""
		m.filterAssignments()
	case "/":
		m.searching, m.input = true, m.query
	case "tab", "shift+tab":
		m.detailFocus = !m.detailFocus
	case "enter", "right", "l":
		if !m.detailFocus {
			m.detailFocus = true
		} else if len(m.items[m.tab]) > 0 {
			m.page, m.scroll = m.items[m.tab][m.item].Details, 0
		}
	case "left", "h", "backspace":
		m.detailFocus = false
	case "t", "1", "s", "2", "c", "3", "d", "4":
		m.tab = map[string]int{"t": 0, "1": 0, "s": 1, "2": 1, "c": 2, "3": 2, "d": 3, "4": 3}[key]
		m.item, m.detailFocus = 0, true
	case "?":
		m.page, m.scroll = m.planDetails(), 0
	default:
		cursor, count := &m.cursor, len(m.visible)
		if m.detailFocus {
			cursor, count = &m.item, len(m.items[m.tab])
		}
		old := *cursor
		switch key {
		case "down", "j":
			*cursor++
		case "up", "k":
			*cursor--
		case "pgdown", "ctrl+d":
			*cursor += max(1, (height-8)/2)
		case "pgup", "ctrl+u":
			*cursor -= max(1, (height-8)/2)
		case "home":
			*cursor = 0
		case "end":
			*cursor = count - 1
		}
		*cursor = min(max(0, count-1), max(0, *cursor))
		if !m.detailFocus && old != *cursor {
			m.refreshItems()
		}
	}
	return false
}

func (m *inventoryPlanBrowser) planDetails() []string {
	lines := []string{"Plan: " + inventoryDisplay(m.plan.ID), "Planner: " + inventoryDisplay(m.plan.Planner), "Inventory: " + inventoryDisplay(m.plan.InventoryID), "Snapshot: " + inventoryDisplay(m.plan.SnapshotSHA),
		"Goal: " + inventoryDisplay(m.plan.Goal), fmt.Sprintf("Scope: path=%q group=%q tag=%q", m.plan.Selection.Path, m.plan.Selection.Group, m.plan.Selection.Tag),
		fmt.Sprintf("Limits: %d files / %d target bytes per assignment", m.plan.Limits.MaxFiles, m.plan.Limits.MaxBytes)}
	if m.plan.SemanticProfileID != "" {
		lines = append(lines, "Semantic profile: "+inventoryDisplay(m.plan.SemanticProfileID))
	} else {
		lines = append(lines, "No semantic index; targets are whole files. Run inventory index to collect symbols and includes.")
	}
	if assignment := m.assignment(); assignment != nil {
		lines = append(lines, "", "Assignment: "+inventoryDisplay(assignment.ID), "Group: "+inventoryDisplay(assignment.Group), "Directory: "+inventoryDisplay(assignment.Directory))
		for _, warning := range assignment.Warnings {
			lines = append(lines, "Warning: "+inventoryDisplay(warning))
		}
	}
	return append(lines, "", "Tab switches between assignments and items. j/k or arrows move.", "t/s/c/d (or 1/2/3/4) select targets, symbols, context, diagnostics.", "Enter opens complete, wrapped item details. Esc returns.", "/ searches assignment IDs, groups, target paths, and overlapping symbols.", "PgUp/PgDown and Home/End navigate lists and details.", "Symbols include enclosing declarations; their details identify overlap.", "Diagnostics describe file parsing, not model findings.", "This is a preview; no scan is saved or started.")
}

func inventoryPlanWrap(lines []string, width int) []string {
	return strings.Split(ansi.Hardwrap(strings.Join(lines, "\n"), max(1, width), true), "\n")
}

func (m *inventoryPlanBrowser) assignmentLines(height, width int) []string {
	header := fmt.Sprintf("Assignments %d/%d", len(m.visible), len(m.plan.Assignments))
	if !m.detailFocus {
		header = "> " + header
	}
	lines := []string{header}
	if len(m.visible) == 0 {
		return fitLines(append(lines, "No matches. / searches; Esc clears."), height, width)
	}
	start := max(0, m.cursor-max(1, height-1)+1)
	for cursor := start; cursor < min(len(m.visible), start+max(0, height-1)); cursor++ {
		index := m.visible[cursor]
		assignment := m.plan.Assignments[index]
		marker := " "
		if cursor == m.cursor {
			marker = ">"
		}
		warning := ""
		if assignment.Oversized || len(assignment.Warnings) > 0 {
			warning = " !"
		}
		lines = append(lines, fmt.Sprintf("%s %03d %s %2df %6dB%s %s", marker, index+1, shortSHA(assignment.ID), len(assignment.Files), assignment.Bytes, warning, inventoryDisplay(assignment.Directory)))
	}
	return fitLines(lines, height, width)
}

func (m *inventoryPlanBrowser) itemLines(height, width int) []string {
	assignment := m.assignment()
	if assignment == nil {
		return fitLines([]string{"No assignment selected."}, height, width)
	}
	marker := ""
	if m.detailFocus {
		marker = "> "
	}
	lines := []string{fmt.Sprintf("Assignment %03d  group: %s  %d files / %d bytes", m.visible[m.cursor]+1, inventoryDisplay(assignment.Group), len(assignment.Files), assignment.Bytes),
		fmt.Sprintf("%s%s (%d)  [t/s/c/d]", marker, inventoryPlanTabs[m.tab], len(m.items[m.tab]))}
	if len(m.items[m.tab]) == 0 {
		empty := []string{"No target ranges.", "No saved symbols overlap these targets. Check Diagnostics for index coverage.", "No context dependencies in this plan.", "No saved parse diagnostics or plan warnings."}[m.tab]
		if m.plan.SemanticProfileID == "" && (m.tab == 1 || m.tab == 2) {
			empty = "No semantic index in this plan. Run inventory index to collect symbols and includes, then reopen the plan."
		}
		return fitLines(append(lines, inventoryPlanWrap([]string{empty}, width)...), height, width)
	}
	listHeight := max(1, (height-3)/2)
	start := max(0, m.item-listHeight+1)
	list := []string{}
	for index := start; index < min(len(m.items[m.tab]), start+listHeight); index++ {
		marker := " "
		if index == m.item {
			marker = ">"
		}
		list = append(list, marker+" "+m.items[m.tab][index].Label)
	}
	lines = append(lines, fitLines(list, listHeight, width)...)
	lines = append(lines, fmt.Sprintf("--- Item %d/%d | Enter: full details ---", m.item+1, len(m.items[m.tab])))
	lines = append(lines, inventoryPlanWrap(m.items[m.tab][m.item].Details, width)...)
	return fitLines(lines, height, width)
}

func (m *inventoryPlanBrowser) view(width, height int, standalone bool) tea.View {
	width, height = max(1, width), max(1, height)
	indexLabel := "no index"
	if m.plan.SemanticProfileID != "" {
		indexLabel = "index " + shortSHA(m.plan.SemanticProfileID)
	}
	lines := []string{fmt.Sprintf("Repose plan %s | %s | %d assignments / %d files / %d bytes", shortSHA(m.plan.ID), inventoryDisplay(m.plan.Planner), len(m.plan.Assignments), m.plan.Files, m.plan.Bytes),
		fmt.Sprintf("Inventory %s | snapshot %s | %s | ?: goal, IDs, help", shortSHA(m.plan.InventoryID), shortSHA(m.plan.SnapshotSHA), indexLabel),
		"Goal: " + inventoryDisplay(m.plan.Goal)}
	bodyHeight := max(0, height-5)
	if m.page != nil {
		wrapped := inventoryPlanWrap(m.page, width)
		start := min(m.scroll, max(0, len(wrapped)-max(1, bodyHeight)))
		lines = append(lines, fitLines(wrapped[start:], bodyHeight, width)...)
		lines = append(lines, "j/k scroll | PgUp/PgDown | Home/End | Esc/q back", fmt.Sprintf("Details %d-%d/%d", start+1, min(len(wrapped), start+bodyHeight), len(wrapped)))
	} else {
		if width >= 100 {
			leftWidth := width * 2 / 5
			left := m.assignmentLines(bodyHeight, leftWidth)
			right := m.itemLines(bodyHeight, width-leftWidth-3)
			for index := range left {
				lines = append(lines, padRight(left[index], leftWidth)+" | "+right[index])
			}
		} else if m.detailFocus {
			lines = append(lines, m.itemLines(bodyHeight, width)...)
		} else {
			lines = append(lines, m.assignmentLines(bodyHeight, width)...)
		}
		if m.searching {
			lines = append(lines, "/ "+inventoryDisplay(m.input), "Search IDs, groups, target paths, symbols | Enter apply | Esc cancel | Ctrl+U clear")
		} else {
			closeLabel := "q/Esc inventory"
			if standalone {
				closeLabel = "q/Esc quit"
			}
			lines = append(lines, "Tab panes | j/k move | Enter details | t targets | s symbols | c context | d diagnostics",
				fmt.Sprintf("/ search | ? plan/help | %s | filter: %q | preview", closeLabel, m.query))
		}
	}
	view := tea.NewView(strings.Join(fitLines(lines, height, width), "\n"))
	view.AltScreen = true
	return view
}
