package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseUnifiedDiffSelectsFindingHunkAndLine(t *testing.T) {
	diff := `diff --git a/example.go b/example.go
--- a/example.go
+++ b/example.go
@@ -1,2 +1,2 @@
-old one
+new one
 context
@@ -10,2 +10,2 @@
-old ten
+new ten
 context ten
`
	hunks := parseUnifiedDiffHunks(diff)
	if len(hunks) != 2 {
		t.Fatalf("hunks = %+v", hunks)
	}
	line := 10
	selected := selectDiffHunk(hunks, &line)
	if selected.newStart != 10 || !strings.Contains(strings.Join(selected.lines, "\n"), "+new ten") {
		t.Fatalf("selected hunk = %+v", selected)
	}
	if target := diffTargetIndex(selected, &line); target != 1 {
		t.Fatalf("target index = %d, want 1", target)
	}
}

func TestLoadFindingDiffPreviewUsesIntroducingCommit(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	base := strings.Join([]string{
		"package sample", "", "var first = 1", "", "var filler1 = 1", "var filler2 = 2",
		"var filler3 = 3", "var filler4 = 4", "var filler5 = 5", "var filler6 = 6",
		"var filler7 = 7", "var filler8 = 8", "var second = 1", "",
	}, "\n")
	testCommitFile(t, directory, "sample.go", []byte(base), "base")
	changed := strings.Replace(base, "var first = 1", "var first = 2", 1)
	changed = strings.Replace(changed, "var second = 1", "var second = 2", 1)
	sha := testCommitFile(t, directory, "sample.go", []byte(changed), "introduce findings")
	file := "sample.go"
	line := 13
	preview, err := loadFindingDiffPreview(context.Background(), repository, Finding{
		ID: 4, IntroducedSHA: sha, File: &file, Line: &line,
	})
	if err != nil {
		t.Fatalf("load preview: %v", err)
	}
	if preview.File != file || preview.CommitSHA != sha ||
		!strings.Contains(strings.Join(preview.Lines, "\n"), "+var second = 2") ||
		strings.Contains(strings.Join(preview.Lines, "\n"), "+var first = 2") {
		t.Fatalf("preview = %+v", preview)
	}
	if preview.Target < 0 || preview.Target >= len(preview.Lines) || preview.Lines[preview.Target] != "+var second = 2" {
		t.Fatalf("preview target = %d in %q", preview.Target, preview.Lines)
	}
}

func TestFindingsModelLoadsAndRendersDiffPreview(t *testing.T) {
	file := "sample.go"
	line := 8
	finding := Finding{ID: 7, Severity: "warning", Title: "preview me", Description: "issue details",
		IntroducedSHA: strings.Repeat("a", 40), File: &file, Line: &line}
	loads := 0
	model := newFindingsModel(context.Background(), findingExternalCommands{
		preview: func(_ context.Context, selected Finding) (findingDiffPreview, error) {
			loads++
			return findingDiffPreview{
				File: file, CommitSHA: selected.IntroducedSHA, HunkHeader: "@@ -7,2 +7,2 @@",
				Lines: []string{"-broken()", "+fixed()"}, Target: 1,
			}, nil
		},
	}, nil, []Finding{finding}, nil, false, time.Now)
	model.width = 120
	model.height = 30
	command := model.Init()
	if command == nil {
		t.Fatal("initial preview command is nil")
	}
	message := command()
	updatedValue, _ := model.Update(message)
	updated := updatedValue.(findingsModel)
	if loads != 1 || updated.previewLoading || updated.preview.Target != 1 {
		t.Fatalf("loaded preview: loads=%d state=%+v", loads, updated.preview)
	}
	rendered := updated.render()
	for _, expected := range []string{"Introducing diff", "@@ -7,2 +7,2 @@", "> +fixed()"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("render omitted %q:\n%s", expected, rendered)
		}
	}
	updated.width = 80
	if rendered := updated.render(); strings.Contains(rendered, "Introducing diff") {
		t.Fatalf("narrow render unexpectedly included preview:\n%s", rendered)
	}
}

func TestFindingsModelCachesPreviewAndIgnoresStaleResult(t *testing.T) {
	file := "sample.go"
	findings := []Finding{
		{ID: 2, Severity: "warning", Title: "newer", IntroducedSHA: strings.Repeat("b", 40), File: &file},
		{ID: 1, Severity: "warning", Title: "older", IntroducedSHA: strings.Repeat("a", 40), File: &file},
	}
	loads := 0
	model := newFindingsModel(context.Background(), findingExternalCommands{
		preview: func(_ context.Context, finding Finding) (findingDiffPreview, error) {
			loads++
			return findingDiffPreview{File: file, Lines: []string{fmt.Sprintf("+finding %d", finding.ID)}}, nil
		},
	}, nil, findings, nil, false, time.Now)
	firstCommand := model.Init()
	secondCommand := model.moveCursor(1)
	if firstCommand == nil || secondCommand == nil || model.selectedID() != 1 {
		t.Fatalf("preview commands: first=%v second=%v selected=%d", firstCommand, secondCommand, model.selectedID())
	}

	updatedValue, _ := model.Update(firstCommand())
	model = updatedValue.(findingsModel)
	if model.selectedID() != 1 || model.previewFinding != 1 || !model.previewLoading {
		t.Fatalf("stale result changed current preview: selected=%d preview=%d loading=%t",
			model.selectedID(), model.previewFinding, model.previewLoading)
	}
	updatedValue, _ = model.Update(secondCommand())
	model = updatedValue.(findingsModel)
	if model.previewLoading || len(model.preview.Lines) != 1 || model.preview.Lines[0] != "+finding 1" {
		t.Fatalf("second preview = %+v, loading=%t", model.preview, model.previewLoading)
	}
	if command := model.moveCursor(-1); command != nil || model.preview.Lines[0] != "+finding 2" || loads != 2 {
		t.Fatalf("cached preview: command=%v preview=%+v loads=%d", command, model.preview, loads)
	}
}
