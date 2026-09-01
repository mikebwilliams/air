package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

func TestAllFindingsAndReviewAttribution(t *testing.T) {
	ctx := context.Background()
	store, findings, reviewedAt := testStoreWithFindings(t)
	defer store.Close()

	if err := store.DismissFinding(ctx, findings[0].ID, "expected legacy behavior", reviewedAt.Add(time.Minute)); err != nil {
		t.Fatalf("DismissFinding: %v", err)
	}
	commitB := testMetadata("b", "a")
	if _, err := store.ApplyReview(ctx, commitB,
		ReviewIdentity{Model: modelByName("resolver-model"), ReasoningEffort: "high"},
		ReviewResult{
			Output: ReviewOutput{
				ResolvedFindings: []ResolvedFinding{{ID: findings[1].ID, Reason: "fixed"}},
				Summary:          "Resolved the remaining problem.",
			},
			RawResponse: "{}",
			Usage:       &TokenUsage{InputTokens: 20, OutputTokens: 4},
		}, reviewedAt.Add(2*time.Minute)); err != nil {
		t.Fatalf("ApplyReview resolution: %v", err)
	}

	all, err := store.AllFindings(ctx)
	if err != nil {
		t.Fatalf("AllFindings: %v", err)
	}
	if len(all) != 2 || all[0].ID != findings[1].ID || all[1].ID != findings[0].ID {
		t.Fatalf("all findings = %+v", all)
	}
	if findingDisposition(all[0]) != "resolved" || findingDisposition(all[1]) != "dismissed" {
		t.Fatalf("dispositions = %q, %q", findingDisposition(all[0]), findingDisposition(all[1]))
	}
	review, err := store.FindingReview(ctx, findings[0].ID)
	if err != nil {
		t.Fatalf("FindingReview: %v", err)
	}
	if review.Number != 1 || review.Model != "browser-model" || review.ReasoningEffort != "xhigh" ||
		!review.ReviewedAt.Equal(reviewedAt) {
		t.Fatalf("review attribution = %+v", review)
	}
}

func TestFindingsModelFiltersAndRenders(t *testing.T) {
	now := time.Date(2026, 8, 16, 15, 0, 0, 0, time.UTC)
	dismissedAt := now.Add(-time.Hour)
	resolvedSHA := strings.Repeat("f", 40)
	file := "parser/state.go"
	line := 42
	model := findingsModel{
		all: []Finding{
			{ID: 3, Severity: "error", Title: "Open parser corruption", Description: "state leaks", IntroducedSHA: strings.Repeat("a", 40), File: &file, Line: &line},
			{ID: 2, Severity: "warning", Title: "Resolved retry", Description: "retry failed", IntroducedSHA: strings.Repeat("b", 40), ResolvedSHA: &resolvedSHA},
			{ID: 1, Severity: "info", Title: "Legacy fallback", Description: "expected", IntroducedSHA: strings.Repeat("c", 40), DismissedAt: &dismissedAt, DismissReason: "intentional"},
		},
		width:  120,
		height: 30,
		now:    func() time.Time { return now },
		display: map[int64]findingDisplayMetadata{
			3: {CommitDate: now.Add(-48 * time.Hour), Blame: "Ada Lovelace"},
		},
		statusFilter:   "open",
		severityFilter: "all",
		review: FindingReview{
			ID: 1, Number: 1, Model: "gpt-5.6-luna", ReasoningEffort: "xhigh", ReviewedAt: now,
		},
	}
	model.applyFilters(0)
	if len(model.visible) != 1 || model.visible[0].ID != 3 {
		t.Fatalf("open filter = %+v", model.visible)
	}
	model.statusFilter = "all"
	model.severityFilter = "warning"
	model.applyFilters(0)
	if len(model.visible) != 1 || model.visible[0].ID != 2 {
		t.Fatalf("severity filter = %+v", model.visible)
	}
	model.severityFilter = "all"
	model.query = "parser/state.go"
	model.applyFilters(0)
	if len(model.visible) != 1 || model.visible[0].ID != 3 {
		t.Fatalf("location search = %+v", model.visible)
	}
	model.query = ""
	model.applyFilters(3)
	model.review = FindingReview{ID: 1, Number: 1, Model: "gpt-5.6-luna", ReasoningEffort: "xhigh", ReviewedAt: now}
	rendered := model.render()
	for _, expected := range []string{
		"Open parser corruption", "parser/state.go:42", "2d", "Ada Lovelace", "gpt-5.6-luna/xhigh", "Description:",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("render did not contain %q:\n%s", expected, rendered)
		}
	}
	model.width = 70
	if rendered = model.render(); !strings.Contains(rendered, "Open parser corruption") {
		t.Fatalf("narrow render lost selected finding:\n%s", rendered)
	}
}

func TestFindingsModelChangesSortWithLeftAndRight(t *testing.T) {
	fileA := "pkg/alpha.go"
	fileB := "pkg/beta.go"
	line10 := 10
	line20 := 20
	model := findingsModel{
		all: []Finding{
			{ID: 5, Severity: "info", Title: "no location"},
			{ID: 4, Severity: "error", Title: "beta", File: &fileB, Line: &line10},
			{ID: 3, Severity: "warning", Title: "alpha later", File: &fileA, Line: &line20},
			{ID: 2, Severity: "error", Title: "alpha earlier", File: &fileA, Line: &line10},
			{ID: 1, Severity: "info", Title: "alpha unknown line", File: &fileA},
		},
		statusFilter:   "open",
		severityFilter: "all",
		sortMode:       findingsSortNewest,
	}
	model.applyFilters(0)
	if got := findingIDs(model.visible); got != "5,4,3,2,1" {
		t.Fatalf("newest order = %s", got)
	}

	model.cursor = 2
	updatedValue, _ := model.handleKey("right")
	model = updatedValue.(findingsModel)
	if model.sortMode != findingsSortFile {
		t.Fatalf("right sort mode = %q", model.sortMode)
	}
	if got := findingIDs(model.visible); got != "2,3,1,4,5" {
		t.Fatalf("file order = %s", got)
	}
	if model.selectedID() != 3 {
		t.Fatalf("selected finding after sort = %d", model.selectedID())
	}

	updatedValue, _ = model.handleKey("right")
	model = updatedValue.(findingsModel)
	if model.sortMode != findingsSortSeverity || findingIDs(model.visible) != "4,2,3,5,1" {
		t.Fatalf("severity sort: mode=%q order=%s", model.sortMode, findingIDs(model.visible))
	}
	if model.selectedID() != 3 {
		t.Fatalf("selected finding after severity sort = %d", model.selectedID())
	}

	updatedValue, _ = model.handleKey("right")
	model = updatedValue.(findingsModel)
	if model.sortMode != findingsSortNewest || findingIDs(model.visible) != "5,4,3,2,1" {
		t.Fatalf("wrapped right sort: mode=%q order=%s", model.sortMode, findingIDs(model.visible))
	}

	updatedValue, _ = model.handleKey("left")
	model = updatedValue.(findingsModel)
	if model.sortMode != findingsSortSeverity || findingIDs(model.visible) != "4,2,3,5,1" {
		t.Fatalf("wrapped left sort: mode=%q order=%s", model.sortMode, findingIDs(model.visible))
	}
}

func TestFindingsModelAlignsFindingIDs(t *testing.T) {
	model := findingsModel{
		all: []Finding{
			{ID: 123, Severity: "warning", Title: "wide ID"},
			{ID: 7, Severity: "warning", Title: "narrow ID"},
		},
		statusFilter:   "open",
		severityFilter: "all",
	}
	model.applyFilters(0)
	lines := model.listLines(2, 120)
	if len(lines) != 2 || !strings.Contains(lines[0], "#123 WARN") ||
		!strings.Contains(lines[1], "#  7 WARN") {
		t.Fatalf("finding ID columns are not aligned: %q", lines)
	}
}

func TestFindingsModelUsesTerminalAwareColor(t *testing.T) {
	model := findingsModel{
		all:            []Finding{{ID: 1, Severity: "error", Title: "colored finding"}},
		statusFilter:   "open",
		severityFilter: "all",
		width:          100,
		height:         20,
	}
	model.applyFilters(0)
	plain := model.render()
	if strings.Contains(plain, "\x1b[") {
		t.Fatalf("plain render contains ANSI styling: %q", plain)
	}

	updatedValue, _ := model.Update(tea.ColorProfileMsg{Profile: colorprofile.ANSI})
	coloredModel := updatedValue.(findingsModel)
	colored := coloredModel.render()
	if !strings.Contains(colored, "\x1b[") || ansi.Strip(colored) != plain {
		t.Fatalf("colored render did not preserve plain content:\nplain=%q\ncolored=%q", plain, colored)
	}
	if severity := coloredModel.styleSeverity("ERR ", "error"); severity != "\x1b[1;31mERR \x1b[0m" {
		t.Fatalf("error severity style = %q", severity)
	}
	styled := "\x1b[31mabcdef\x1b[0m"
	truncated := truncateTerminalText(styled, 4)
	if ansi.StringWidth(truncated) != 4 || ansi.Strip(truncated) != "abc…" {
		t.Fatalf("ANSI-aware truncation = %q", truncated)
	}
}

func findingIDs(findings []Finding) string {
	ids := make([]string, 0, len(findings))
	for _, finding := range findings {
		ids = append(ids, strconv.FormatInt(finding.ID, 10))
	}
	return strings.Join(ids, ",")
}

func TestFindingsModelActionsAreAudited(t *testing.T) {
	ctx := context.Background()
	store, findings, reviewedAt := testStoreWithFindings(t)
	defer store.Close()
	now := reviewedAt.Add(time.Hour)
	model := newFindingsModel(ctx, findingExternalCommands{}, store, findings, nil, true, func() time.Time {
		now = now.Add(time.Second)
		return now
	})

	model.mode = findingsDismiss
	model.input = "not actionable"
	model.dismissSelected()
	finding, err := store.Finding(ctx, model.selectedID())
	if err != nil || findingDisposition(finding) != "dismissed" {
		t.Fatalf("dismissed finding = %+v, %v", finding, err)
	}
	model.mode = findingsConfirmReopen
	model.reopenSelected()
	finding, err = store.Finding(ctx, model.selectedID())
	if err != nil || findingDisposition(finding) != "open" {
		t.Fatalf("reopened finding = %+v, %v", finding, err)
	}
	model.mode = findingsNote
	model.input = "double-check this path"
	model.noteSelected()
	events, err := store.FindingEvents(ctx, model.selectedID())
	if err != nil {
		t.Fatalf("FindingEvents: %v", err)
	}
	actions := make([]string, 0, len(events))
	for _, event := range events {
		actions = append(actions, event.Action)
	}
	if got := strings.Join(actions, ","); got != "opened,dismissed,reopened,noted" {
		t.Fatalf("event actions = %q", got)
	}
	if events[len(events)-1].Note != "double-check this path" {
		t.Fatalf("last event = %+v", events[len(events)-1])
	}
}

func TestFindingsModelLaunchesExternalActions(t *testing.T) {
	ctx := context.Background()
	finding := Finding{ID: 9, Severity: "warning", Title: "open me", IntroducedSHA: strings.Repeat("a", 40)}
	var diffID int64
	model := findingsModel{
		ctx: ctx,
		external: findingExternalCommands{
			diff: func(_ context.Context, selected Finding) (*exec.Cmd, error) {
				diffID = selected.ID
				return exec.Command("true"), nil
			},
			open: func(_ context.Context, selected Finding) (*exec.Cmd, error) {
				return nil, errors.New("file is unavailable")
			},
		},
		all:            []Finding{finding},
		statusFilter:   "open",
		severityFilter: "all",
	}
	model.applyFilters(0)

	updatedValue, command := model.handleKey("d")
	updated := updatedValue.(findingsModel)
	if command == nil || diffID != 9 || !strings.Contains(updated.message, "Opening diff") {
		t.Fatalf("diff launch: command=%v, id=%d, message=%q", command, diffID, updated.message)
	}
	finishedValue, _ := updated.Update(findingExternalFinishedMsg{action: "Diff", findingID: 9})
	finished := finishedValue.(findingsModel)
	if finished.message != "Diff closed for finding #9." {
		t.Fatalf("completion message = %q", finished.message)
	}
	dismissValue, command := model.handleKey("D")
	dismiss := dismissValue.(findingsModel)
	if command != nil || dismiss.mode != findingsDismiss {
		t.Fatalf("dismiss key: command=%v, mode=%v", command, dismiss.mode)
	}

	updatedValue, command = model.handleKey("o")
	updated = updatedValue.(findingsModel)
	if command != nil || !strings.Contains(updated.message, "file is unavailable") {
		t.Fatalf("open failure: command=%v, message=%q", command, updated.message)
	}
}

func TestRunFindingsHandsAllRowsToUI(t *testing.T) {
	ctx := context.Background()
	repository, directory := newTestGitRepository(t)
	startSHA := testCommitFile(t, directory, "start.txt", []byte("start\n"), "start")
	commitSHA := testCommitFile(t, directory, "change.txt", []byte("change\n"), "change")
	store, err := CreateStore(ctx, repository.DatabasePath(), startSHA)
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	metadata, err := repository.CommitMetadata(ctx, commitSHA)
	if err != nil {
		t.Fatalf("CommitMetadata: %v", err)
	}
	if _, err := store.ApplyReview(ctx, metadata,
		ReviewIdentity{Model: modelByName("cli-model"), ReasoningEffort: "low"},
		ReviewResult{
			Output: ReviewOutput{NewFindings: []NewFinding{{
				Severity: "warning", Title: "CLI finding", Description: "visible in browser",
			}}, Summary: "found one"},
			RawResponse: "{}",
			Usage:       &TokenUsage{InputTokens: 3, OutputTokens: 1},
		}, time.Now()); err != nil {
		t.Fatalf("ApplyReview: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	called := false
	environment := cliEnvironment{
		Cwd:    directory,
		Stdin:  bytes.NewBuffer(nil),
		Stdout: io.Discard,
		Stderr: io.Discard,
		FindingsUI: func(_ context.Context, _ io.Reader, _ io.Writer, _ findingExternalCommands, _ *Store,
			findings []Finding, display map[int64]findingDisplayMetadata, includeAll bool, _ func() time.Time,
		) error {
			called = true
			if !includeAll || len(findings) != 1 || findings[0].Title != "CLI finding" {
				t.Fatalf("UI input: includeAll=%t, findings=%+v", includeAll, findings)
			}
			metadata := display[findings[0].ID]
			if metadata.Blame != "AIR Test" || metadata.CommitDate.IsZero() {
				t.Fatalf("UI display metadata = %+v", metadata)
			}
			return nil
		},
	}
	if err := runCLI(ctx, []string{"findings", "--all"}, environment); err != nil {
		t.Fatalf("runCLI: %v", err)
	}
	if !called {
		t.Fatal("findings UI was not called")
	}
}

func TestTerminalFindingsUIRejectsPipes(t *testing.T) {
	err := runTerminalFindingsUI(context.Background(), bytes.NewBuffer(nil), io.Discard,
		findingExternalCommands{}, nil, nil, nil, false, time.Now)
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("error = %v", err)
	}
}

func testStoreWithFindings(t *testing.T) (*Store, []Finding, time.Time) {
	t.Helper()
	ctx := context.Background()
	store, err := CreateStore(ctx, filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	reviewedAt := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	ids, err := store.ApplyReview(ctx, testMetadata("a", "0"),
		ReviewIdentity{Model: modelByName("browser-model"), ReasoningEffort: "xhigh"},
		ReviewResult{
			Output: ReviewOutput{NewFindings: []NewFinding{
				{Severity: "warning", Title: "first", Description: "first description"},
				{Severity: "error", Title: "second", Description: "second description"},
			}, Summary: "Found two problems."},
			RawResponse: "{}",
			Usage:       &TokenUsage{InputTokens: 10, OutputTokens: 2},
		}, reviewedAt)
	if err != nil {
		store.Close()
		t.Fatalf("ApplyReview: %v", err)
	}
	findings := make([]Finding, 0, len(ids))
	for _, id := range ids {
		finding, err := store.Finding(ctx, id)
		if err != nil {
			store.Close()
			t.Fatalf("Finding: %v", err)
		}
		findings = append(findings, finding)
	}
	return store, findings, reviewedAt
}
