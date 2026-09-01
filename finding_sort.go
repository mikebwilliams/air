package main

import (
	"sort"
	"strings"
)

type findingSortMode string

const (
	findingsSortID       findingSortMode = "id"
	findingsSortAge      findingSortMode = "age"
	findingsSortFile     findingSortMode = "file"
	findingsSortAuthor   findingSortMode = "author"
	findingsSortSeverity findingSortMode = "severity"
	findingsSortStatus   findingSortMode = "status"
	findingsSortTitle    findingSortMode = "title"
)

var findingSortModes = []findingSortMode{
	findingsSortID,
	findingsSortAge,
	findingsSortFile,
	findingsSortAuthor,
	findingsSortSeverity,
	findingsSortStatus,
	findingsSortTitle,
}

func sortFindings(
	findings []Finding,
	mode findingSortMode,
	display map[int64]findingDisplayMetadata,
) {
	sort.SliceStable(findings, func(leftIndex, rightIndex int) bool {
		left := findings[leftIndex]
		right := findings[rightIndex]
		switch mode {
		case findingsSortAge:
			return findingAgeLess(left, right, display)
		case findingsSortFile:
			return findingFileLess(left, right)
		case findingsSortAuthor:
			return findingAuthorLess(left, right, display)
		case findingsSortSeverity:
			leftRank := findingSeverityRank(left.Severity)
			rightRank := findingSeverityRank(right.Severity)
			if leftRank != rightRank {
				return leftRank < rightRank
			}
		case findingsSortStatus:
			leftRank := findingStatusRank(findingDisposition(left))
			rightRank := findingStatusRank(findingDisposition(right))
			if leftRank != rightRank {
				return leftRank < rightRank
			}
		case findingsSortTitle:
			return findingTextLess(left.Title, right.Title, left.ID, right.ID)
		}
		return left.ID > right.ID
	})
}

func findingAgeLess(
	left, right Finding,
	display map[int64]findingDisplayMetadata,
) bool {
	leftDate := display[left.ID].CommitDate
	rightDate := display[right.ID].CommitDate
	if leftDate.IsZero() || rightDate.IsZero() {
		if leftDate.IsZero() && rightDate.IsZero() {
			return left.ID > right.ID
		}
		return !leftDate.IsZero()
	}
	if !leftDate.Equal(rightDate) {
		return leftDate.Before(rightDate)
	}
	return left.ID > right.ID
}

func findingAuthorLess(
	left, right Finding,
	display map[int64]findingDisplayMetadata,
) bool {
	leftAuthor := display[left.ID].Blame
	rightAuthor := display[right.ID].Blame
	if leftAuthor == "" || rightAuthor == "" {
		if leftAuthor == "" && rightAuthor == "" {
			return left.ID > right.ID
		}
		return leftAuthor != ""
	}
	return findingTextLess(leftAuthor, rightAuthor, left.ID, right.ID)
}

func findingTextLess(left, right string, leftID, rightID int64) bool {
	leftFolded := strings.ToLower(left)
	rightFolded := strings.ToLower(right)
	if leftFolded != rightFolded {
		return leftFolded < rightFolded
	}
	if left != right {
		return left < right
	}
	return leftID > rightID
}

func findingFileLess(left, right Finding) bool {
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
}

func findingSeverityRank(severity string) int {
	switch severity {
	case "error":
		return 0
	case "warning":
		return 1
	case "info":
		return 2
	default:
		return 3
	}
}

func findingStatusRank(status string) int {
	switch status {
	case "open":
		return 0
	case "dismissed":
		return 1
	case "resolved":
		return 2
	default:
		return 3
	}
}
