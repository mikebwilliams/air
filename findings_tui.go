package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
)

type findingsUIRunner func(
	context.Context,
	io.Reader,
	io.Writer,
	findingExternalCommands,
	*Store,
	[]Finding,
	bool,
	func() time.Time,
) error

func runFindings(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("findings", environment.Stderr)
	includeAll := flags.Bool("all", false, "show findings of every disposition initially")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: air findings [--all]")
	}
	repository, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	findings, err := store.AllFindings(ctx)
	if err != nil {
		return err
	}
	runner := environment.FindingsUI
	if runner == nil {
		runner = runTerminalFindingsUI
	}
	commands := newFindingExternalCommands(repository, environment.ExternalCommand)
	return runner(ctx, environment.Stdin, environment.Stdout, commands, store, findings, *includeAll,
		func() time.Time { return environmentNow(environment) })
}

func runTerminalFindingsUI(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
	external findingExternalCommands,
	store *Store,
	findings []Finding,
	includeAll bool,
	now func() time.Time,
) error {
	inputFile, inputOK := input.(*os.File)
	outputFile, outputOK := output.(*os.File)
	if !inputOK || !outputOK || !term.IsTerminal(inputFile.Fd()) || !term.IsTerminal(outputFile.Fd()) {
		return errors.New("findings requires an interactive terminal; use air status --json for non-interactive output")
	}
	model := newFindingsModel(ctx, external, store, findings, includeAll, now)
	program := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithInput(input),
		tea.WithOutput(output),
	)
	if _, err := program.Run(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("run findings browser: %w", err)
	}
	return nil
}

type findingsInputMode int

const (
	findingsBrowse findingsInputMode = iota
	findingsSearch
	findingsDismiss
	findingsNote
	findingsConfirmReopen
)

type findingSortMode string

const (
	findingsSortNewest findingSortMode = "newest"
	findingsSortFile   findingSortMode = "file"
)

var findingSortModes = []findingSortMode{
	findingsSortNewest,
	findingsSortFile,
}

type findingsModel struct {
	ctx             context.Context
	external        findingExternalCommands
	store           *Store
	now             func() time.Time
	all             []Finding
	visible         []Finding
	cursor          int
	width           int
	height          int
	detailOffset    int
	statusFilter    string
	severityFilter  string
	sortMode        findingSortMode
	query           string
	queryBeforeEdit string
	mode            findingsInputMode
	input           string
	message         string
	help            bool
	events          []FindingEvent
	review          FindingReview
}

func newFindingsModel(
	ctx context.Context,
	external findingExternalCommands,
	store *Store,
	findings []Finding,
	includeAll bool,
	now func() time.Time,
) findingsModel {
	status := "open"
	if includeAll {
		status = "all"
	}
	model := findingsModel{
		ctx:            ctx,
		external:       external,
		store:          store,
		now:            now,
		all:            findings,
		width:          100,
		height:         30,
		statusFilter:   status,
		severityFilter: "all",
		sortMode:       findingsSortNewest,
	}
	model.applyFilters(0)
	model.loadDetail()
	return model
}

func (m findingsModel) Init() tea.Cmd {
	return nil
}

func (m findingsModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case findingExternalFinishedMsg:
		if message.err != nil {
			m.message = fmt.Sprintf("%s failed for finding #%d: %v", message.action, message.findingID, message.err)
		} else {
			m.message = fmt.Sprintf("%s closed for finding #%d.", message.action, message.findingID)
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
		return m, nil
	case tea.PasteMsg:
		if m.mode == findingsSearch || m.mode == findingsDismiss || m.mode == findingsNote {
			m.input += message.Content
			if m.mode == findingsSearch {
				m.query = m.input
				m.applyFilters(0)
				m.loadDetail()
			}
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(message.String())
	default:
		return m, nil
	}
}

func (m findingsModel) handleKey(key string) (tea.Model, tea.Cmd) {
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if m.help {
		if key == "?" || key == "esc" || key == "q" {
			m.help = false
		}
		return m, nil
	}
	if m.mode != findingsBrowse {
		return m.handleInputKey(key)
	}
	m.message = ""
	switch key {
	case "q", "esc":
		return m, tea.Quit
	case "?":
		m.help = true
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "left":
		m.changeSort(-1)
	case "right":
		m.changeSort(1)
	case "pgup":
		m.moveCursor(-m.pageSize())
	case "pgdown":
		m.moveCursor(m.pageSize())
	case "home", "g":
		m.moveCursor(-len(m.visible))
	case "end", "G":
		m.moveCursor(len(m.visible))
	case "ctrl+u":
		m.detailOffset -= 5
		if m.detailOffset < 0 {
			m.detailOffset = 0
		}
	case "ctrl+d":
		m.detailOffset += 5
	case "/":
		m.mode = findingsSearch
		m.input = m.query
		m.queryBeforeEdit = m.query
	case "s":
		m.statusFilter = nextValue(m.statusFilter, []string{"open", "all", "dismissed", "resolved"})
		m.applyFilters(m.selectedID())
		m.loadDetail()
	case "v":
		m.severityFilter = nextValue(m.severityFilter, []string{"all", "error", "warning", "info"})
		m.applyFilters(m.selectedID())
		m.loadDetail()
	case "c":
		m.query = ""
		m.statusFilter = "open"
		m.severityFilter = "all"
		m.applyFilters(m.selectedID())
		m.loadDetail()
	case "d":
		if finding, ok := m.selectedFinding(); ok {
			if findingDisposition(finding) != "open" {
				m.message = "Only an open finding can be dismissed."
			} else {
				m.mode = findingsDismiss
				m.input = ""
			}
		}
	case "D":
		return m, m.launchExternal("Diff", m.external.diff)
	case "o":
		return m, m.launchExternal("Editor", m.external.open)
	case "r":
		if finding, ok := m.selectedFinding(); ok {
			if findingDisposition(finding) == "open" {
				m.message = "Finding is already open."
			} else {
				m.mode = findingsConfirmReopen
			}
		}
	case "n":
		if _, ok := m.selectedFinding(); ok {
			m.mode = findingsNote
			m.input = ""
		}
	}
	return m, nil
}

type findingExternalFinishedMsg struct {
	action    string
	findingID int64
	err       error
}

func (m *findingsModel) launchExternal(action string, builder findingCommandBuilder) tea.Cmd {
	finding, ok := m.selectedFinding()
	if !ok {
		return nil
	}
	if builder == nil {
		m.message = action + " command is unavailable."
		return nil
	}
	command, err := builder(m.ctx, finding)
	if err != nil {
		m.message = action + " failed: " + err.Error()
		return nil
	}
	m.message = fmt.Sprintf("Opening %s for finding #%d…", strings.ToLower(action), finding.ID)
	return tea.ExecProcess(command, func(err error) tea.Msg {
		return findingExternalFinishedMsg{action: action, findingID: finding.ID, err: err}
	})
}

func (m findingsModel) handleInputKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		if m.mode == findingsSearch {
			m.query = m.queryBeforeEdit
			m.applyFilters(0)
			m.loadDetail()
		}
		m.mode = findingsBrowse
		m.input = ""
		return m, nil
	case "enter":
		switch m.mode {
		case findingsSearch:
			m.query = strings.TrimSpace(m.input)
			m.mode = findingsBrowse
			m.input = ""
			m.applyFilters(0)
			m.loadDetail()
		case findingsDismiss:
			m.dismissSelected()
		case findingsNote:
			m.noteSelected()
		case findingsConfirmReopen:
			m.message = "Press y to reopen or n to cancel."
		}
		return m, nil
	case "backspace", "ctrl+h":
		m.input = removeLastRune(m.input)
		if m.mode == findingsSearch {
			m.query = m.input
			m.applyFilters(0)
			m.loadDetail()
		}
		return m, nil
	}
	if m.mode == findingsConfirmReopen {
		switch key {
		case "y", "Y":
			m.reopenSelected()
		case "n", "N", "q":
			m.mode = findingsBrowse
			m.message = "Reopen cancelled."
		}
		return m, nil
	}
	if key == "space" {
		m.input += " "
	} else if utf8.RuneCountInString(key) == 1 {
		m.input += key
	}
	if m.mode == findingsSearch {
		m.query = m.input
		m.applyFilters(0)
		m.loadDetail()
	}
	return m, nil
}

func (m *findingsModel) dismissSelected() {
	finding, ok := m.selectedFinding()
	if !ok {
		m.mode = findingsBrowse
		return
	}
	reason := strings.TrimSpace(m.input)
	if reason == "" {
		m.message = "A dismissal reason is required."
		return
	}
	if err := m.store.DismissFinding(m.ctx, finding.ID, reason, m.now()); err != nil {
		m.message = "Dismiss failed: " + err.Error()
		return
	}
	m.mode = findingsBrowse
	m.input = ""
	m.message = fmt.Sprintf("Dismissed finding #%d.", finding.ID)
	m.reload(finding.ID)
}

func (m *findingsModel) noteSelected() {
	finding, ok := m.selectedFinding()
	if !ok {
		m.mode = findingsBrowse
		return
	}
	note := strings.TrimSpace(m.input)
	if note == "" {
		m.message = "A note is required."
		return
	}
	if err := m.store.AddFindingNote(m.ctx, finding.ID, note, m.now()); err != nil {
		m.message = "Adding note failed: " + err.Error()
		return
	}
	m.mode = findingsBrowse
	m.input = ""
	m.message = fmt.Sprintf("Added note to finding #%d.", finding.ID)
	m.reload(finding.ID)
}

func (m *findingsModel) reopenSelected() {
	finding, ok := m.selectedFinding()
	if !ok {
		m.mode = findingsBrowse
		return
	}
	if err := m.store.ReopenFinding(m.ctx, finding.ID, m.now()); err != nil {
		m.message = "Reopen failed: " + err.Error()
		return
	}
	m.mode = findingsBrowse
	m.message = fmt.Sprintf("Reopened finding #%d.", finding.ID)
	m.reload(finding.ID)
}

func (m *findingsModel) reload(preferredID int64) {
	findings, err := m.store.AllFindings(m.ctx)
	if err != nil {
		m.message = "Reload failed: " + err.Error()
		return
	}
	m.all = findings
	m.applyFilters(preferredID)
	m.loadDetail()
}

func (m *findingsModel) applyFilters(preferredID int64) {
	if m.sortMode == "" {
		m.sortMode = findingsSortNewest
	}
	query := strings.ToLower(strings.TrimSpace(m.query))
	m.visible = m.visible[:0]
	for _, finding := range m.all {
		disposition := findingDisposition(finding)
		if m.statusFilter != "all" && m.statusFilter != disposition {
			continue
		}
		if m.severityFilter != "all" && m.severityFilter != finding.Severity {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(findingSearchText(finding)), query) {
			continue
		}
		m.visible = append(m.visible, finding)
	}
	sortFindings(m.visible, m.sortMode)
	if len(m.visible) == 0 {
		m.cursor = 0
		m.events = nil
		m.review = FindingReview{}
		m.detailOffset = 0
		return
	}
	if preferredID != 0 {
		for index := range m.visible {
			if m.visible[index].ID == preferredID {
				m.cursor = index
				m.detailOffset = 0
				return
			}
		}
	}
	if m.cursor >= len(m.visible) {
		m.cursor = len(m.visible) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.detailOffset = 0
}

func (m *findingsModel) changeSort(delta int) {
	selectedID := m.selectedID()
	current := 0
	for index, mode := range findingSortModes {
		if mode == m.sortMode {
			current = index
			break
		}
	}
	current = (current + delta) % len(findingSortModes)
	if current < 0 {
		current += len(findingSortModes)
	}
	m.sortMode = findingSortModes[current]
	m.applyFilters(selectedID)
	m.loadDetail()
}

func sortFindings(findings []Finding, mode findingSortMode) {
	sort.SliceStable(findings, func(leftIndex, rightIndex int) bool {
		left := findings[leftIndex]
		right := findings[rightIndex]
		if mode != findingsSortFile {
			return left.ID > right.ID
		}
		if left.File == nil || right.File == nil {
			if left.File == nil && right.File == nil {
				return left.ID > right.ID
			}
			return left.File != nil
		}
		leftFolded := strings.ToLower(*left.File)
		rightFolded := strings.ToLower(*right.File)
		if leftFolded != rightFolded {
			return leftFolded < rightFolded
		}
		if *left.File != *right.File {
			return *left.File < *right.File
		}
		if left.Line == nil || right.Line == nil {
			if left.Line == nil && right.Line == nil {
				return left.ID > right.ID
			}
			return left.Line != nil
		}
		if *left.Line != *right.Line {
			return *left.Line < *right.Line
		}
		return left.ID > right.ID
	})
}

func (m *findingsModel) loadDetail() {
	finding, ok := m.selectedFinding()
	if !ok || m.store == nil {
		m.events = nil
		m.review = FindingReview{}
		return
	}
	events, err := m.store.FindingEvents(m.ctx, finding.ID)
	if err != nil {
		m.message = "Loading history failed: " + err.Error()
		return
	}
	review, err := m.store.FindingReview(m.ctx, finding.ID)
	if err != nil {
		m.message = "Loading review attribution failed: " + err.Error()
		return
	}
	m.events = events
	m.review = review
}

func (m *findingsModel) moveCursor(delta int) {
	if len(m.visible) == 0 {
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.visible) {
		m.cursor = len(m.visible) - 1
	}
	m.detailOffset = 0
	m.loadDetail()
}

func (m findingsModel) selectedFinding() (Finding, bool) {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return Finding{}, false
	}
	return m.visible[m.cursor], true
}

func (m findingsModel) selectedID() int64 {
	finding, ok := m.selectedFinding()
	if !ok {
		return 0
	}
	return finding.ID
}

func (m findingsModel) pageSize() int {
	size := m.height - 6
	if m.width < 96 {
		size /= 3
	}
	if size < 1 {
		return 1
	}
	return size
}

func (m findingsModel) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = true
	return view
}

func (m findingsModel) render() string {
	width := m.width
	height := m.height
	if width < 40 {
		width = 40
	}
	if height < 10 {
		height = 10
	}
	header := fmt.Sprintf("AIR findings  %d/%d  status:%s  severity:%s  sort:%s",
		m.position(), len(m.visible), m.statusFilter, m.severityFilter, m.sortMode)
	if m.query != "" {
		header += "  search:" + strconv.Quote(m.query)
	}
	lines := []string{truncateTerminalText(header, width), strings.Repeat("─", width)}
	bodyHeight := height - 5
	if m.help {
		lines = append(lines, fitLines(m.helpLines(width), bodyHeight, width)...)
	} else if width >= 96 {
		leftWidth := width * 42 / 100
		rightWidth := width - leftWidth - 3
		left := fitLines(m.listLines(bodyHeight, leftWidth), bodyHeight, leftWidth)
		right := fitLines(m.detailLines(rightWidth), bodyHeight, rightWidth)
		for index := 0; index < bodyHeight; index++ {
			lines = append(lines, padRight(left[index], leftWidth)+" │ "+right[index])
		}
	} else {
		listHeight := bodyHeight / 3
		if listHeight < 2 {
			listHeight = 2
		}
		detailHeight := bodyHeight - listHeight - 1
		lines = append(lines, fitLines(m.listLines(listHeight, width), listHeight, width)...)
		lines = append(lines, strings.Repeat("─", width))
		lines = append(lines, fitLines(m.detailLines(width), detailHeight, width)...)
	}
	lines = append(lines, truncateTerminalText(m.footer(), width), truncateTerminalText(m.message, width))
	return strings.Join(lines, "\n")
}

func (m findingsModel) position() int {
	if len(m.visible) == 0 {
		return 0
	}
	return m.cursor + 1
}

func (m findingsModel) listLines(height, width int) []string {
	if len(m.visible) == 0 {
		return []string{"No findings match the current filters."}
	}
	start := m.cursor - height/2
	if start < 0 {
		start = 0
	}
	if maximum := len(m.visible) - height; start > maximum && maximum >= 0 {
		start = maximum
	}
	end := start + height
	if end > len(m.visible) {
		end = len(m.visible)
	}
	idWidth := m.findingIDWidth()
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		finding := m.visible[index]
		marker := " "
		if index == m.cursor {
			marker = ">"
		}
		location := ""
		if finding.File != nil {
			location = " " + *finding.File
			if finding.Line != nil {
				location += ":" + strconv.Itoa(*finding.Line)
			}
		}
		line := fmt.Sprintf("%s #%*d %-4s %-9s%s  %s", marker, idWidth, finding.ID,
			severityLabel(finding.Severity), findingDisposition(finding), location,
			singleLine(finding.Title))
		lines = append(lines, truncateTerminalText(line, width))
	}
	return lines
}

func (m findingsModel) findingIDWidth() int {
	width := 1
	for _, finding := range m.all {
		if candidate := len(strconv.FormatInt(finding.ID, 10)); candidate > width {
			width = candidate
		}
	}
	return width
}

func (m findingsModel) detailLines(width int) []string {
	finding, ok := m.selectedFinding()
	if !ok {
		return []string{"Select a broader filter to browse findings."}
	}
	lines := []string{
		fmt.Sprintf("#%d  %s  %s", finding.ID, strings.ToUpper(finding.Severity), findingDisposition(finding)),
	}
	lines = append(lines, wrapText(finding.Title, width)...)
	lines = append(lines, "")
	if finding.File != nil {
		location := *finding.File
		if finding.Line != nil {
			location += ":" + strconv.Itoa(*finding.Line)
		}
		if finding.Symbol != nil {
			location += "  " + *finding.Symbol
		}
		lines = append(lines, "Location: "+location)
	}
	lines = append(lines, "Introduced: "+shortSHA(finding.IntroducedSHA))
	if m.review.ID != 0 {
		identity := m.review.Model
		if m.review.ReasoningEffort != "" {
			identity += "/" + m.review.ReasoningEffort
		}
		lines = append(lines,
			fmt.Sprintf("Review: #%d (%s)", m.review.Number, identity),
			"Reviewed: "+m.review.ReviewedAt.Format(time.RFC3339),
		)
	}
	switch findingDisposition(finding) {
	case "dismissed":
		lines = append(lines, "Dismissed: "+finding.DismissedAt.Format(time.RFC3339))
		lines = append(lines, wrapPrefixed("Reason: ", finding.DismissReason, width)...)
	case "resolved":
		lines = append(lines, "Resolved: "+shortSHA(*finding.ResolvedSHA))
	}
	lines = append(lines, "", "Description:")
	lines = append(lines, wrapText(finding.Description, width)...)
	lines = append(lines, "", "History:")
	if len(m.events) == 0 {
		lines = append(lines, "  none")
	}
	for _, event := range m.events {
		eventLine := "  " + event.CreatedAt.Format("2006-01-02 15:04") + "  " + event.Action
		if event.SHA != nil {
			eventLine += " " + shortSHA(*event.SHA)
		}
		lines = append(lines, wrapPrefixed(eventLine, event.Note, width)...)
	}
	if m.detailOffset >= len(lines) {
		return []string{"(end of detail; Ctrl+U scrolls up)"}
	}
	return lines[m.detailOffset:]
}

func (m findingsModel) footer() string {
	switch m.mode {
	case findingsSearch:
		return "Search: " + m.input + "_  (Enter apply, Esc cancel)"
	case findingsDismiss:
		return fmt.Sprintf("Dismiss #%d — reason: %s_  (Enter save, Esc cancel)", m.selectedID(), m.input)
	case findingsNote:
		return fmt.Sprintf("Note #%d: %s_  (Enter save, Esc cancel)", m.selectedID(), m.input)
	case findingsConfirmReopen:
		return fmt.Sprintf("Reopen #%d?  y yes, n no", m.selectedID())
	default:
		return "↑/↓ j/k move  ←/→ sort  / search  D diff  o open  d dismiss  r reopen  n note  ? help  q quit"
	}
}

func (m findingsModel) helpLines(width int) []string {
	return wrapText(`Keyboard

↑/↓ or j/k    select finding
←/→           change sort: newest, file
PgUp/PgDn     move one page
g/G           first/last finding
Ctrl+U/Ctrl+D scroll detail
/             search title, text, location, SHA, or ID
s             cycle status: open, all, dismissed, resolved
v             cycle severity: all, error, warning, info
c             clear search and restore default filters
D             open the introducing commit in git difftool
o             open the finding's file and line in the configured Git editor
d             dismiss the selected open finding (reason required)
r             reopen a dismissed or resolved finding (confirmation required)
n             append an audited note
?             close this help
q             quit

All changes use the same audited finding lifecycle as the singular finding command.`, width)
}

func findingDisposition(finding Finding) string {
	if finding.DismissedAt != nil {
		return "dismissed"
	}
	if finding.ResolvedSHA != nil {
		return "resolved"
	}
	return "open"
}

func findingSearchText(finding Finding) string {
	parts := []string{
		strconv.FormatInt(finding.ID, 10), finding.Severity, findingDisposition(finding),
		finding.Title, finding.Description, finding.IntroducedSHA, finding.DismissReason,
	}
	if finding.ResolvedSHA != nil {
		parts = append(parts, *finding.ResolvedSHA)
	}
	if finding.File != nil {
		parts = append(parts, *finding.File)
	}
	if finding.Symbol != nil {
		parts = append(parts, *finding.Symbol)
	}
	return strings.Join(parts, " ")
}

func severityLabel(severity string) string {
	switch severity {
	case "error":
		return "ERR"
	case "warning":
		return "WARN"
	default:
		return "INFO"
	}
}

func nextValue(current string, values []string) string {
	for index, value := range values {
		if value == current {
			return values[(index+1)%len(values)]
		}
	}
	return values[0]
}

func removeLastRune(value string) string {
	if value == "" {
		return ""
	}
	_, size := utf8.DecodeLastRuneInString(value)
	return value[:len(value)-size]
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateTerminalText(value string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func padRight(value string, width int) string {
	value = truncateTerminalText(value, width)
	padding := width - utf8.RuneCountInString(value)
	if padding <= 0 {
		return value
	}
	return value + strings.Repeat(" ", padding)
}

func fitLines(lines []string, height, width int) []string {
	if height < 0 {
		height = 0
	}
	result := make([]string, height)
	for index := 0; index < height && index < len(lines); index++ {
		result[index] = truncateTerminalText(lines[index], width)
	}
	return result
}

func wrapPrefixed(prefix, value string, width int) []string {
	if value == "" {
		return []string{truncateTerminalText(prefix, width)}
	}
	return wrapText(prefix+" — "+value, width)
}

func wrapText(value string, width int) []string {
	if width < 1 {
		return nil
	}
	paragraphs := strings.Split(value, "\n")
	var lines []string
	for _, paragraph := range paragraphs {
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		line := ""
		for _, word := range words {
			for utf8.RuneCountInString(word) > width {
				if line != "" {
					lines = append(lines, line)
					line = ""
				}
				runes := []rune(word)
				lines = append(lines, string(runes[:width]))
				word = string(runes[width:])
			}
			if line == "" {
				line = word
			} else if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= width {
				line += " " + word
			} else {
				lines = append(lines, line)
				line = word
			}
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
