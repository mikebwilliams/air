package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

type findingsUIRunner func(
	context.Context,
	io.Reader,
	io.Writer,
	findingExternalCommands,
	*Store,
	[]Finding,
	map[int64]findingDisplayMetadata,
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
	display, err := loadFindingDisplayMetadata(ctx, repository, findings)
	if err != nil {
		return err
	}
	runner := environment.FindingsUI
	if runner == nil {
		runner = runTerminalFindingsUI
	}
	commands := newFindingExternalCommands(repository, environment.ExternalCommand)
	return runner(ctx, environment.Stdin, environment.Stdout, commands, store, findings, display, *includeAll,
		func() time.Time { return environmentNow(environment) })
}

func runTerminalFindingsUI(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
	external findingExternalCommands,
	store *Store,
	findings []Finding,
	display map[int64]findingDisplayMetadata,
	includeAll bool,
	now func() time.Time,
) error {
	model := newFindingsModel(ctx, external, store, findings, display, includeAll, now)
	return runTerminalFindingsModel(ctx, input, output, model)
}

func runTerminalFindingsModel(ctx context.Context, input io.Reader, output io.Writer, model findingsModel) error {
	inputFile, inputOK := input.(*os.File)
	outputFile, outputOK := output.(*os.File)
	if !inputOK || !outputOK || !term.IsTerminal(inputFile.Fd()) || !term.IsTerminal(outputFile.Fd()) {
		return errors.New("findings requires an interactive terminal; use --json for non-interactive output")
	}
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
	findingsTag
	findingsUntag
	findingsTagFilter
)

type findingPreviewLoadedMsg struct {
	findingID int64
	preview   findingDiffPreview
	err       error
}

type findingPreviewCacheEntry struct {
	preview findingDiffPreview
	err     string
}

type findingUIStore interface {
	AllFindings(context.Context) ([]Finding, error)
	DismissFinding(context.Context, int64, string, time.Time) error
	AddFindingNote(context.Context, int64, string, time.Time) error
	ReopenFinding(context.Context, int64, time.Time) error
	FindingEvents(context.Context, int64) ([]FindingEvent, error)
	FindingReview(context.Context, int64) (FindingReview, error)
}

type findingTagUIStore interface {
	TagFindings(context.Context, []int64, []string, time.Time) (int, error)
	UntagFindings(context.Context, []int64, []string, time.Time) (int, error)
}

type findingsModel struct {
	ctx                context.Context
	external           findingExternalCommands
	store              findingUIStore
	now                func() time.Time
	all                []Finding
	visible            []Finding
	display            map[int64]findingDisplayMetadata
	cursor             int
	width              int
	height             int
	detailOffset       int
	statusFilter       string
	severityFilter     string
	verificationFilter string
	tagFilters         []string
	sortMode           findingSortMode
	query              string
	queryBeforeEdit    string
	tagsBeforeEdit     []string
	mode               findingsInputMode
	input              string
	message            string
	help               bool
	events             []FindingEvent
	review             FindingReview
	colorProfile       colorprofile.Profile
	preview            findingDiffPreview
	previewError       string
	previewFinding     int64
	previewLoading     bool
	previewCache       map[int64]findingPreviewCacheEntry
}

func newFindingsModel(
	ctx context.Context,
	external findingExternalCommands,
	store findingUIStore,
	findings []Finding,
	display map[int64]findingDisplayMetadata,
	includeAll bool,
	now func() time.Time,
) findingsModel {
	status := "open"
	if includeAll {
		status = "all"
	}
	model := findingsModel{
		ctx:                ctx,
		external:           external,
		store:              store,
		now:                now,
		all:                findings,
		display:            display,
		width:              100,
		height:             30,
		statusFilter:       status,
		severityFilter:     "all",
		verificationFilter: "all",
		sortMode:           findingsSortID,
		previewCache:       make(map[int64]findingPreviewCacheEntry),
	}
	model.applyFilters(0)
	model.loadDetail()
	model.prepareInitialPreview()
	return model
}

func (m findingsModel) Init() tea.Cmd {
	if !m.previewLoading {
		return nil
	}
	finding, ok := m.selectedFinding()
	if !ok {
		return nil
	}
	return m.previewCommand(finding)
}

func (m findingsModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case findingPreviewLoadedMsg:
		if m.previewCache == nil {
			m.previewCache = make(map[int64]findingPreviewCacheEntry)
		}
		entry := findingPreviewCacheEntry{preview: message.preview}
		if message.err != nil {
			entry.err = message.err.Error()
		}
		m.previewCache[message.findingID] = entry
		if m.selectedID() == message.findingID {
			m.applyPreviewEntry(message.findingID, entry)
		}
		return m, nil
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
	case tea.ColorProfileMsg:
		m.colorProfile = message.Profile
		return m, nil
	case tea.PasteMsg:
		if m.mode == findingsSearch || m.mode == findingsDismiss || m.mode == findingsNote || m.mode == findingsTag || m.mode == findingsUntag || m.mode == findingsTagFilter {
			m.input += message.Content
			if m.mode == findingsSearch {
				m.query = m.input
				m.applyFilters(0)
				return m, m.refreshSelection()
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
		return m, m.moveCursor(-1)
	case "down", "j":
		return m, m.moveCursor(1)
	case "left":
		return m, m.changeSort(-1)
	case "right":
		return m, m.changeSort(1)
	case "pgup":
		return m, m.moveCursor(-m.pageSize())
	case "pgdown":
		return m, m.moveCursor(m.pageSize())
	case "home", "g":
		return m, m.moveCursor(-len(m.visible))
	case "end", "G":
		return m, m.moveCursor(len(m.visible))
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
		return m, m.refreshSelection()
	case "v":
		m.severityFilter = nextValue(m.severityFilter, []string{"all", "error", "warning", "info"})
		m.applyFilters(m.selectedID())
		return m, m.refreshSelection()
	case "c":
		m.query = ""
		m.statusFilter = "open"
		m.severityFilter = "all"
		m.verificationFilter = "all"
		m.tagFilters = nil
		m.applyFilters(m.selectedID())
		return m, m.refreshSelection()
	case "V":
		if m.external.snapshot {
			m.verificationFilter = nextValue(m.verificationFilter, []string{"all", "unchecked", "confirmed", "false_positive", "uncertain"})
			m.applyFilters(m.selectedID())
			return m, m.refreshSelection()
		}
	case "T":
		if m.external.snapshot {
			m.mode = findingsTagFilter
			m.input = strings.Join(m.tagFilters, ",")
			m.tagsBeforeEdit = append([]string(nil), m.tagFilters...)
		}
	case "t":
		if _, ok := m.store.(findingTagUIStore); ok {
			if _, selected := m.selectedFinding(); selected {
				m.mode = findingsTag
				m.input = ""
			}
		}
	case "u":
		if _, ok := m.store.(findingTagUIStore); ok {
			if _, selected := m.selectedFinding(); selected {
				m.mode = findingsUntag
				m.input = ""
			}
		}
	case "D":
		if finding, ok := m.selectedFinding(); ok {
			if findingDisposition(finding) != "open" {
				m.message = "Only an open finding can be dismissed."
			} else {
				m.mode = findingsDismiss
				m.input = ""
			}
		}
	case "R":
		return m, m.reload(m.selectedID())
	case "d":
		if m.external.snapshot {
			m.message = "Snapshot source is shown in the preview."
			return m, nil
		}
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
			command := m.refreshSelection()
			m.mode = findingsBrowse
			m.input = ""
			return m, command
		}
		if m.mode == findingsTagFilter {
			m.tagFilters = append([]string(nil), m.tagsBeforeEdit...)
			m.applyFilters(0)
			command := m.refreshSelection()
			m.mode = findingsBrowse
			m.input = ""
			return m, command
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
			return m, m.refreshSelection()
		case findingsDismiss:
			return m, m.dismissSelected()
		case findingsNote:
			return m, m.noteSelected()
		case findingsTag:
			return m, m.tagSelected(true)
		case findingsUntag:
			return m, m.tagSelected(false)
		case findingsTagFilter:
			tags, err := normalizeFindingTags(splitFindingTags(m.input))
			if err != nil {
				m.message = err.Error()
				return m, nil
			}
			m.tagFilters = tags
			m.mode = findingsBrowse
			m.input = ""
			m.applyFilters(0)
			return m, m.refreshSelection()
		case findingsConfirmReopen:
			m.message = "Press y to reopen or n to cancel."
		}
		return m, nil
	case "backspace", "ctrl+h":
		m.input = removeLastRune(m.input)
		if m.mode == findingsSearch {
			m.query = m.input
			m.applyFilters(0)
			return m, m.refreshSelection()
		}
		return m, nil
	}
	if m.mode == findingsConfirmReopen {
		switch key {
		case "y", "Y":
			return m, m.reopenSelected()
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
		return m, m.refreshSelection()
	}
	return m, nil
}

func (m *findingsModel) dismissSelected() tea.Cmd {
	finding, ok := m.selectedFinding()
	if !ok {
		m.mode = findingsBrowse
		return nil
	}
	reason := strings.TrimSpace(m.input)
	if reason == "" {
		m.message = "A dismissal reason is required."
		return nil
	}
	if err := m.store.DismissFinding(m.ctx, finding.ID, reason, m.now()); err != nil {
		m.message = "Dismiss failed: " + err.Error()
		return nil
	}
	m.mode = findingsBrowse
	m.input = ""
	m.message = fmt.Sprintf("Dismissed finding #%d.", finding.ID)
	return m.reload(finding.ID)
}

func (m *findingsModel) noteSelected() tea.Cmd {
	finding, ok := m.selectedFinding()
	if !ok {
		m.mode = findingsBrowse
		return nil
	}
	note := strings.TrimSpace(m.input)
	if note == "" {
		m.message = "A note is required."
		return nil
	}
	if err := m.store.AddFindingNote(m.ctx, finding.ID, note, m.now()); err != nil {
		m.message = "Adding note failed: " + err.Error()
		return nil
	}
	m.mode = findingsBrowse
	m.input = ""
	m.message = fmt.Sprintf("Added note to finding #%d.", finding.ID)
	return m.reload(finding.ID)
}

func splitFindingTags(value string) []string {
	return strings.Fields(strings.ReplaceAll(value, ",", " "))
}

func (m *findingsModel) tagSelected(add bool) tea.Cmd {
	finding, ok := m.selectedFinding()
	if !ok {
		m.mode = findingsBrowse
		return nil
	}
	tags, err := normalizeFindingTags(splitFindingTags(m.input))
	if err != nil {
		m.message = err.Error()
		return nil
	}
	if len(tags) == 0 {
		m.message = "At least one tag is required."
		return nil
	}
	store, ok := m.store.(findingTagUIStore)
	if !ok {
		m.message = "Finding tags are unavailable."
		m.mode = findingsBrowse
		return nil
	}
	changes := 0
	if add {
		changes, err = store.TagFindings(m.ctx, []int64{finding.ID}, tags, m.now())
	} else {
		changes, err = store.UntagFindings(m.ctx, []int64{finding.ID}, tags, m.now())
	}
	if err != nil {
		m.message = "Tag edit failed: " + err.Error()
		return nil
	}
	m.mode = findingsBrowse
	m.input = ""
	verb := "Added"
	if !add {
		verb = "Removed"
	}
	m.message = fmt.Sprintf("%s %d tag assignments for finding #%d.", verb, changes, finding.ID)
	return m.reload(finding.ID)
}

func (m *findingsModel) reopenSelected() tea.Cmd {
	finding, ok := m.selectedFinding()
	if !ok {
		m.mode = findingsBrowse
		return nil
	}
	if err := m.store.ReopenFinding(m.ctx, finding.ID, m.now()); err != nil {
		m.message = "Reopen failed: " + err.Error()
		return nil
	}
	m.mode = findingsBrowse
	m.message = fmt.Sprintf("Reopened finding #%d.", finding.ID)
	return m.reload(finding.ID)
}

func (m *findingsModel) reload(preferredID int64) tea.Cmd {
	findings, err := m.store.AllFindings(m.ctx)
	if err != nil {
		m.message = "Reload failed: " + err.Error()
		return nil
	}
	m.all = findings
	if m.external.snapshot {
		m.display = auditFindingDisplay(findings)
	}
	m.applyFilters(preferredID)
	return m.refreshSelection()
}

func (m *findingsModel) applyFilters(preferredID int64) {
	if m.sortMode == "" {
		m.sortMode = findingsSortID
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
		if m.verificationFilter != "" && m.verificationFilter != "all" && auditVerificationOutcome(finding) != m.verificationFilter {
			continue
		}
		if !findingHasTags(finding, m.tagFilters) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(findingSearchText(finding)), query) {
			continue
		}
		m.visible = append(m.visible, finding)
	}
	sortFindings(m.visible, m.sortMode, m.display)
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

func (m *findingsModel) changeSort(delta int) tea.Cmd {
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
	return m.refreshSelection()
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

func (m *findingsModel) refreshSelection() tea.Cmd {
	m.loadDetail()
	return m.preparePreview()
}

func (m *findingsModel) prepareInitialPreview() {
	_, _ = m.setPreviewState()
}

func (m *findingsModel) preparePreview() tea.Cmd {
	finding, load := m.setPreviewState()
	if !load {
		return nil
	}
	return m.previewCommand(finding)
}

func (m *findingsModel) setPreviewState() (Finding, bool) {
	finding, ok := m.selectedFinding()
	if !ok {
		m.preview = findingDiffPreview{}
		m.previewError = ""
		m.previewFinding = 0
		m.previewLoading = false
		return Finding{}, false
	}
	if m.previewFinding == finding.ID && m.previewLoading {
		return finding, false
	}
	m.preview = findingDiffPreview{}
	m.previewError = ""
	m.previewFinding = finding.ID
	m.previewLoading = false
	if entry, exists := m.previewCache[finding.ID]; exists {
		m.applyPreviewEntry(finding.ID, entry)
		return finding, false
	}
	if finding.File == nil {
		m.preview.Message = "No file location was recorded for this finding."
		return finding, false
	}
	if m.external.preview == nil {
		m.preview.Message = "Diff preview is unavailable."
		return finding, false
	}
	m.previewLoading = true
	return finding, true
}

func (m findingsModel) previewCommand(finding Finding) tea.Cmd {
	loader := m.external.preview
	ctx := m.ctx
	return func() tea.Msg {
		preview, err := loader(ctx, finding)
		return findingPreviewLoadedMsg{findingID: finding.ID, preview: preview, err: err}
	}
}

func (m *findingsModel) applyPreviewEntry(findingID int64, entry findingPreviewCacheEntry) {
	m.previewFinding = findingID
	m.preview = entry.preview
	m.previewError = entry.err
	m.previewLoading = false
}

func (m *findingsModel) moveCursor(delta int) tea.Cmd {
	if len(m.visible) == 0 {
		return nil
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.visible) {
		m.cursor = len(m.visible) - 1
	}
	m.detailOffset = 0
	return m.refreshSelection()
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
	content := m.render()
	if m.external.snapshot {
		content = strings.Join(fitLines(strings.Split(content, "\n"), max(1, m.height), max(1, m.width)), "\n")
	}
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

func (m findingsModel) sortLabel() string {
	if m.external.snapshot && m.sortMode == findingsSortAuthor {
		return "scan"
	}
	return string(m.sortMode)
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
	name := "AIR"
	if m.external.snapshot {
		name = "Repose"
	}
	header := fmt.Sprintf(name+" findings  %d/%d  status:%s  severity:%s  sort:%s",
		m.position(), len(m.visible), m.statusFilter, m.severityFilter, m.sortLabel())
	if m.external.snapshot && m.verificationFilter != "" && m.verificationFilter != "all" {
		header += "  verification:" + m.verificationFilter
	}
	if len(m.tagFilters) > 0 {
		header += "  tags:" + strings.Join(m.tagFilters, ",")
	}
	if m.query != "" {
		header += "  search:" + strconv.Quote(m.query)
	}
	lines := []string{
		truncateTerminalText(m.style(header, "1", "36"), width),
		m.style(strings.Repeat("─", width), "2"),
	}
	bodyHeight := height - 5
	if m.help {
		lines = append(lines, fitLines(m.helpLines(width), bodyHeight, width)...)
	} else if width >= 96 {
		leftWidth := width * 52 / 100
		rightWidth := width - leftWidth - 3
		left := fitLines(m.listLines(bodyHeight, leftWidth), bodyHeight, leftWidth)
		right := fitLines(m.rightPaneLines(bodyHeight, rightWidth), bodyHeight, rightWidth)
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
		lines = append(lines, m.style(strings.Repeat("─", width), "2"))
		lines = append(lines, fitLines(m.detailLines(width), detailHeight, width)...)
	}
	lines = append(lines,
		truncateTerminalText(m.style(m.footer(), "2"), width),
		truncateTerminalText(m.style(m.message, "1", "33"), width),
	)
	return strings.Join(lines, "\n")
}

func (m findingsModel) rightPaneLines(height, width int) []string {
	if height < 18 || width < 40 {
		return fitLines(m.detailLines(width), height, width)
	}
	previewHeight := height * 2 / 5
	if previewHeight < 7 {
		previewHeight = 7
	}
	detailHeight := height - previewHeight - 1
	lines := fitLines(m.detailLines(width), detailHeight, width)
	lines = append(lines, m.style(strings.Repeat("─", width), "2"))
	lines = append(lines, fitLines(m.previewLines(previewHeight, width), previewHeight, width)...)
	return lines
}

func (m findingsModel) previewLines(height, width int) []string {
	if height <= 0 {
		return nil
	}
	finding, ok := m.selectedFinding()
	heading := "Introducing diff"
	if m.external.snapshot {
		heading = "Observed source"
	}
	if !ok {
		return []string{m.style(heading, "1", "36"), "No finding is selected."}
	}
	if finding.File != nil {
		heading += " — " + *finding.File
	}
	lines := []string{m.style(heading, "1", "36")}
	if finding.File == nil {
		return append(lines, "No file location was recorded for this finding.")
	}
	if m.previewFinding != finding.ID || m.previewLoading {
		return append(lines, m.style("Loading relevant hunk…", "2"))
	}
	if m.previewError != "" {
		return append(lines, wrapText("Preview unavailable: "+m.previewError, width)...)
	}
	if m.preview.Message != "" {
		return append(lines, wrapText(m.preview.Message, width)...)
	}
	if m.preview.HunkHeader != "" && len(lines) < height {
		lines = append(lines, m.style("  "+m.preview.HunkHeader, "36"))
	}
	available := height - len(lines)
	if available <= 0 || len(m.preview.Lines) == 0 {
		return lines
	}
	start := m.preview.Target - available/2
	if start < 0 {
		start = 0
	}
	if maximum := len(m.preview.Lines) - available; start > maximum && maximum >= 0 {
		start = maximum
	}
	end := start + available
	if end > len(m.preview.Lines) {
		end = len(m.preview.Lines)
	}
	for index := start; index < end; index++ {
		line := "  " + m.preview.Lines[index]
		target := index == m.preview.Target
		if target {
			line = "> " + m.preview.Lines[index]
		}
		lines = append(lines, m.styleDiffLine(line, target))
	}
	return lines
}

func (m findingsModel) styleDiffLine(line string, target bool) string {
	if target {
		return m.style(line, "1", "7")
	}
	trimmed := strings.TrimLeft(line, " >")
	switch {
	case strings.HasPrefix(trimmed, "+"):
		return m.style(line, "32")
	case strings.HasPrefix(trimmed, "-"):
		return m.style(line, "31")
	case strings.HasPrefix(trimmed, "@@"):
		return m.style(line, "36")
	default:
		return m.style(line, "2")
	}
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
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		finding := m.visible[index]
		location := ""
		if finding.File != nil {
			location = " " + *finding.File
			if finding.Line != nil {
				location += ":" + strconv.Itoa(*finding.Line)
			}
		}
		line := m.findingListLine(finding, idWidth, location, index == m.cursor, now)
		lines = append(lines, truncateTerminalText(line, width))
	}
	return lines
}

func (m findingsModel) findingListLine(
	finding Finding,
	idWidth int,
	location string,
	selected bool,
	now time.Time,
) string {
	id := fmt.Sprintf("%*d", idWidth, finding.ID)
	severity := fmt.Sprintf("%-4s", severityLabel(finding.Severity))
	dispositionName := findingDisposition(finding)
	disposition := fmt.Sprintf("%-9s", dispositionName)
	display := m.display[finding.ID]
	age := fmt.Sprintf("%4s", formatFindingAge(now, display.CommitDate))
	blame := padRight(truncateTerminalText(findingBlame(display), 14), 14)
	title := singleLine(finding.Title)
	if len(finding.Verifications) > 0 {
		title = "[" + auditVerificationOutcome(finding) + "] " + title
	}
	if selected {
		return m.style(fmt.Sprintf("> #%s %s %s %s %s%s  %s",
			id, severity, disposition, age, blame, location, title), "1", "7")
	}
	if dispositionName != "open" {
		title = m.style(title, "2")
	}
	return fmt.Sprintf("  #%s %s %s %s %s%s  %s",
		m.style(id, "2"),
		m.styleSeverity(severity, finding.Severity),
		m.styleDisposition(disposition, dispositionName),
		m.style(age, "36"),
		m.style(blame, "2"),
		m.style(location, "2"),
		title,
	)
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
	lines := []string{fmt.Sprintf("%s  %s  %s",
		m.style(fmt.Sprintf("#%d", finding.ID), "1"),
		m.styleSeverity(strings.ToUpper(finding.Severity), finding.Severity),
		m.styleDisposition(findingDisposition(finding), findingDisposition(finding)),
	)}
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
	if finding.ObservedSHA != "" {
		lines = append(lines, "Observed: "+shortSHA(finding.ObservedSHA), "Scan: "+finding.ScanID, "Assignment: "+shortSHA(finding.TaskID), fmt.Sprintf("Attempt: %d", finding.AttemptID))
	} else {
		lines = append(lines, "Introduced: "+shortSHA(finding.IntroducedSHA))
	}
	if len(finding.Tags) > 0 {
		lines = append(lines, "Tags: "+strings.Join(finding.Tags, ", "))
	}
	display := m.display[finding.ID]
	if !display.CommitDate.IsZero() {
		now := time.Now()
		if m.now != nil {
			now = m.now()
		}
		label := "Commit age: "
		if m.external.snapshot {
			label = "Finding age: "
		}
		lines = append(lines, label+formatFindingAge(now, display.CommitDate))
	}
	if !m.external.snapshot {
		lines = append(lines, "Blame: "+findingBlame(display))
	}
	if m.review.ID != 0 {
		identity := m.review.Model
		if m.review.Harness != "" && m.review.Harness != codexReviewerName {
			identity = m.review.Harness + ":" + identity
		}
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
	if len(finding.Verifications) > 0 {
		v := finding.Verifications[len(finding.Verifications)-1]
		lines = append(lines, "", m.style("Latest verification: "+v.Outcome, "1", "36"))
		lines = append(lines, wrapText(fmt.Sprintf("%s/%s/%s at %s; recheck %s", v.Harness, v.Model, v.Effort, v.CheckedAt.Format(time.RFC3339), shortSHA(v.ScanID)), width)...)
		lines = append(lines, wrapText(v.Reason, width)...)
	}
	lines = append(lines, "", m.style("Description:", "1", "36"))
	lines = append(lines, wrapText(finding.Description, width)...)
	lines = append(lines, "", m.style("History:", "1", "36"))
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
	case findingsTag:
		return fmt.Sprintf("Tag #%d (comma or space separated): %s_  (Enter save, Esc cancel)", m.selectedID(), m.input)
	case findingsUntag:
		return fmt.Sprintf("Untag #%d (comma or space separated): %s_  (Enter save, Esc cancel)", m.selectedID(), m.input)
	case findingsTagFilter:
		return "Exact tag filters (comma or space separated): " + m.input + "_  (Enter apply, Esc cancel)"
	case findingsConfirmReopen:
		return fmt.Sprintf("Reopen #%d?  y yes, n no", m.selectedID())
	default:
		if m.external.snapshot {
			return "j/k move | / search | t/u tag | T filter | D dismiss | r reopen | n note | R reload | ? | q"
		}
		return "↑/↓ j/k move  ←/→ sort  / search  d diff  o open  D dismiss  r reopen  n note  ? help  q quit"
	}
}

func (m findingsModel) helpLines(width int) []string {
	lines := wrapText(`Keyboard

↑/↓ or j/k    select finding
←/→           change sort: id, age, file, author, severity, status, title
PgUp/PgDn     move one page
g/G           first/last finding
Ctrl+U/Ctrl+D scroll detail
/             search title, text, location, tags, SHA, or ID
s             cycle status: open, all, dismissed, resolved
v             cycle severity: all, error, warning, info
c             clear search and restore default filters
t/u           add/remove tags on the selected finding (Repose)
T             set exact tag filters (Repose; multiple filters use AND)
d             open the introducing commit in git difftool
o             open the finding's file and line in the configured Git editor
D             dismiss the selected open finding (reason required)
r             reopen a dismissed or resolved finding (confirmation required)
n             append an audited note
?             close this help
q             quit

All changes use the same audited finding lifecycle as the singular finding command.`, width)
	if m.external.snapshot {
		lines = wrapText("j/k or arrows: select finding; left/right: sort\n/: search; s: status; v: severity; V: verification; T: exact tag filters; c: clear filters\nCtrl+U/Ctrl+D: scroll details\nt/u: add/remove tags; D: dismiss with reason; r: reopen; n: add note\nR: reload saved findings; ?: help; q: quit\nPreview shows the recorded snapshot source.\nTag changes and verification history preserve original findings and manual dispositions.", width)
	}
	if len(lines) != 0 {
		lines[0] = m.style(lines[0], "1", "36")
	}
	return lines
}

func (m findingsModel) styleSeverity(value, severity string) string {
	switch severity {
	case "error":
		return m.style(value, "1", "31")
	case "warning":
		return m.style(value, "33")
	default:
		return m.style(value, "36")
	}
}

func (m findingsModel) styleDisposition(value, disposition string) string {
	switch disposition {
	case "open":
		return m.style(value, "32")
	case "dismissed":
		return m.style(value, "35")
	default:
		return m.style(value, "36")
	}
}

func (m findingsModel) style(value string, codes ...string) string {
	if value == "" || m.colorProfile <= colorprofile.ASCII {
		return value
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + value + "\x1b[0m"
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
		finding.Title, finding.Description, finding.IntroducedSHA, finding.ObservedSHA, finding.ScanID, finding.TaskID, finding.DismissReason, strings.Join(finding.Tags, " "),
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
	if len(finding.Verifications) > 0 {
		v := finding.Verifications[len(finding.Verifications)-1]
		parts = append(parts, v.Outcome, v.Model, v.Reason)
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
	if ansi.StringWidth(value) <= width {
		return value
	}
	return ansi.Truncate(value, width, "…")
}

func padRight(value string, width int) string {
	value = truncateTerminalText(value, width)
	padding := width - ansi.StringWidth(value)
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
