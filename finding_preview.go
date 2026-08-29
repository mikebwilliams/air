package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type findingDiffPreview struct {
	File       string
	CommitSHA  string
	HunkHeader string
	Lines      []string
	Target     int
	Message    string
}

type parsedDiffHunk struct {
	header   string
	lines    []string
	newStart int
	newCount int
}

var unifiedHunkHeader = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@`)

func loadFindingDiffPreview(ctx context.Context, repository *GitRepository, finding Finding) (findingDiffPreview, error) {
	if repository == nil {
		return findingDiffPreview{}, errors.New("no Git repository is available")
	}
	if finding.File == nil {
		return findingDiffPreview{Message: "No file location was recorded for this finding."}, nil
	}
	if err := validateRepositoryPath(*finding.File); err != nil {
		return findingDiffPreview{}, fmt.Errorf("invalid finding location: %w", err)
	}
	metadata, err := repository.CommitMetadata(ctx, finding.IntroducedSHA)
	if err != nil {
		return findingDiffPreview{}, fmt.Errorf("locate introducing commit: %w", err)
	}
	diff, err := repository.DiffFile(ctx, metadata.ParentSHA, metadata.SHA, *finding.File)
	if err != nil {
		return findingDiffPreview{}, fmt.Errorf("read introducing diff: %w", err)
	}
	hunks := parseUnifiedDiffHunks(diff)
	if len(hunks) == 0 {
		return findingDiffPreview{
			File:      *finding.File,
			CommitSHA: metadata.SHA,
			Message:   "No textual diff hunk is available for this file.",
		}, nil
	}
	hunk := selectDiffHunk(hunks, finding.Line)
	return findingDiffPreview{
		File:       *finding.File,
		CommitSHA:  metadata.SHA,
		HunkHeader: hunk.header,
		Lines:      append([]string(nil), hunk.lines...),
		Target:     diffTargetIndex(hunk, finding.Line),
	}, nil
}

func parseUnifiedDiffHunks(diff string) []parsedDiffHunk {
	var hunks []parsedDiffHunk
	var current *parsedDiffHunk
	for _, line := range strings.Split(strings.TrimSuffix(diff, "\n"), "\n") {
		matches := unifiedHunkHeader.FindStringSubmatch(line)
		if matches != nil {
			newStart, _ := strconv.Atoi(matches[1])
			newCount := 1
			if matches[2] != "" {
				newCount, _ = strconv.Atoi(matches[2])
			}
			hunks = append(hunks, parsedDiffHunk{header: line, newStart: newStart, newCount: newCount})
			current = &hunks[len(hunks)-1]
			continue
		}
		if current != nil {
			current.lines = append(current.lines, line)
		}
	}
	return hunks
}

func selectDiffHunk(hunks []parsedDiffHunk, line *int) parsedDiffHunk {
	if line == nil || len(hunks) == 1 {
		return hunks[0]
	}
	selected := hunks[0]
	bestDistance := diffHunkDistance(selected, *line)
	for _, hunk := range hunks[1:] {
		if distance := diffHunkDistance(hunk, *line); distance < bestDistance {
			selected = hunk
			bestDistance = distance
		}
	}
	return selected
}

func diffHunkDistance(hunk parsedDiffHunk, line int) int {
	end := hunk.newStart + hunk.newCount - 1
	if hunk.newCount == 0 {
		end = hunk.newStart
	}
	if line < hunk.newStart {
		return hunk.newStart - line
	}
	if line > end {
		return line - end
	}
	return 0
}

func diffTargetIndex(hunk parsedDiffHunk, target *int) int {
	if len(hunk.lines) == 0 {
		return -1
	}
	if target == nil {
		return 0
	}
	newLine := hunk.newStart
	bestIndex := 0
	bestDistance := int(^uint(0) >> 1)
	for index, text := range hunk.lines {
		candidate := newLine
		usesNewLine := true
		switch {
		case strings.HasPrefix(text, "-"):
			usesNewLine = false
		case strings.HasPrefix(text, "\\"):
			usesNewLine = false
		default:
			newLine++
		}
		if !usesNewLine {
			continue
		}
		distance := candidate - *target
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance {
			bestIndex = index
			bestDistance = distance
		}
		if distance == 0 {
			return index
		}
	}
	return bestIndex
}
