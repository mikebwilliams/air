package main

import (
	"sort"
	"strings"
)

type findingSortMode string

const (
	findingsSortNewest   findingSortMode = "newest"
	findingsSortFile     findingSortMode = "file"
	findingsSortSeverity findingSortMode = "severity"
)

var findingSortModes = []findingSortMode{
	findingsSortNewest,
	findingsSortFile,
	findingsSortSeverity,
}

func sortFindings(findings []Finding, mode findingSortMode) {
	sort.SliceStable(findings, func(leftIndex, rightIndex int) bool {
		left := findings[leftIndex]
		right := findings[rightIndex]
		switch mode {
		case findingsSortFile:
			return findingFileLess(left, right)
		case findingsSortSeverity:
			leftRank := findingSeverityRank(left.Severity)
			rightRank := findingSeverityRank(right.Severity)
			if leftRank != rightRank {
				return leftRank < rightRank
			}
		}
		return left.ID > right.ID
	})
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
