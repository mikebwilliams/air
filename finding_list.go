package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	findingListAgeWidth      = 4
	findingListBlameWidth    = 18
	findingListLocationWidth = 28
)

type findingListReview struct {
	ID              int64     `json:"id"`
	Number          int       `json:"number"`
	CommitSHA       string    `json:"commit_sha"`
	ReviewedAt      time.Time `json:"reviewed_at"`
	Harness         string    `json:"harness"`
	Model           string    `json:"model"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
}

type findingListItem struct {
	Finding
	Status     string            `json:"status"`
	Blame      string            `json:"blame"`
	CommitDate time.Time         `json:"commit_date"`
	Review     findingListReview `json:"review"`
}

type findingListJSONOutput struct {
	Status   string            `json:"status"`
	Severity string            `json:"severity"`
	Sort     string            `json:"sort"`
	Total    int               `json:"total"`
	Findings []findingListItem `json:"findings"`
}

func runFindingList(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("finding list", environment.Stderr)
	includeAll := flags.Bool("all", false, "include findings of every disposition")
	status := flags.String("status", "open", "filter by status: open, dismissed, resolved, or all")
	severity := flags.String("severity", "all", "filter by severity: error, warning, info, or all")
	sortName := flags.String("sort", string(findingsSortNewest), "sort by newest, file, or severity")
	limit := flags.Int("limit", 0, "maximum findings to return; zero means unlimited")
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: air finding list [--all] [--status STATUS] [--severity SEVERITY] [--sort SORT] [--limit N] [--json]")
	}
	visited := visitedFlagNames(flags)
	if *includeAll && visited["status"] {
		return errors.New("--all and --status cannot be used together")
	}
	if *includeAll {
		*status = "all"
	}
	*status = strings.ToLower(strings.TrimSpace(*status))
	*severity = strings.ToLower(strings.TrimSpace(*severity))
	*sortName = strings.ToLower(strings.TrimSpace(*sortName))
	if !validFindingListStatus(*status) {
		return fmt.Errorf("invalid --status %q; expected open, dismissed, resolved, or all", *status)
	}
	if !validFindingListSeverity(*severity) {
		return fmt.Errorf("invalid --severity %q; expected error, warning, info, or all", *severity)
	}
	sortMode := findingSortMode(*sortName)
	if !validFindingSortMode(sortMode) {
		return fmt.Errorf("invalid --sort %q; expected newest, file, or severity", *sortName)
	}
	if *limit < 0 {
		return errors.New("--limit must not be negative")
	}

	repository, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	allFindings, err := store.AllFindings(ctx)
	if err != nil {
		return err
	}
	findings, total := selectFindingList(allFindings, *status, *severity, sortMode, *limit)
	display, err := loadFindingDisplayMetadata(ctx, repository, findings)
	if err != nil {
		return err
	}
	if *jsonOutput {
		items, err := buildFindingListItems(ctx, store, findings, display)
		if err != nil {
			return err
		}
		return writeJSON(environment.Stdout, findingListJSONOutput{
			Status: *status, Severity: *severity, Sort: *sortName,
			Total: total, Findings: items,
		})
	}
	items := makeFindingListDisplayItems(findings, display)
	writeFindingList(environment.Stdout, items, total, *status, *severity, environmentNow(environment))
	return nil
}

func validFindingListStatus(status string) bool {
	switch status {
	case "open", "dismissed", "resolved", "all":
		return true
	default:
		return false
	}
}

func validFindingListSeverity(severity string) bool {
	switch severity {
	case "error", "warning", "info", "all":
		return true
	default:
		return false
	}
}

func validFindingSortMode(mode findingSortMode) bool {
	for _, candidate := range findingSortModes {
		if mode == candidate {
			return true
		}
	}
	return false
}

func selectFindingList(
	all []Finding,
	status, severity string,
	sortMode findingSortMode,
	limit int,
) ([]Finding, int) {
	selected := make([]Finding, 0, len(all))
	for _, finding := range all {
		if status != "all" && findingDisposition(finding) != status {
			continue
		}
		if severity != "all" && finding.Severity != severity {
			continue
		}
		selected = append(selected, finding)
	}
	sortFindings(selected, sortMode)
	total := len(selected)
	if limit > 0 && len(selected) > limit {
		selected = selected[:limit]
	}
	return selected, total
}

func buildFindingListItems(
	ctx context.Context,
	store *Store,
	findings []Finding,
	display map[int64]findingDisplayMetadata,
) ([]findingListItem, error) {
	items := make([]findingListItem, 0, len(findings))
	for _, finding := range findings {
		review, err := store.FindingReview(ctx, finding.ID)
		if err != nil {
			return nil, err
		}
		items = append(items, findingListItem{
			Finding:    finding,
			Status:     findingDisposition(finding),
			Blame:      findingBlame(display[finding.ID]),
			CommitDate: display[finding.ID].CommitDate,
			Review: findingListReview{
				ID: review.ID, Number: review.Number, CommitSHA: review.CommitSHA,
				ReviewedAt: review.ReviewedAt, Harness: review.Harness, Model: review.Model,
				ReasoningEffort: review.ReasoningEffort,
			},
		})
	}
	return items, nil
}

func makeFindingListDisplayItems(
	findings []Finding,
	display map[int64]findingDisplayMetadata,
) []findingListItem {
	items := make([]findingListItem, 0, len(findings))
	for _, finding := range findings {
		metadata := display[finding.ID]
		items = append(items, findingListItem{
			Finding:    finding,
			Status:     findingDisposition(finding),
			Blame:      findingBlame(metadata),
			CommitDate: metadata.CommitDate,
		})
	}
	return items
}

func writeFindingList(
	output io.Writer,
	findings []findingListItem,
	total int,
	status, severity string,
	now time.Time,
) {
	description := findingListDescription(status, severity, total == 1)
	if total == 0 {
		fmt.Fprintf(output, "No %s.\n", description)
		return
	}
	if len(findings) < total {
		fmt.Fprintf(output, "%d of %d %s\n\n", len(findings), total, description)
	} else {
		fmt.Fprintf(output, "%d %s\n\n", total, description)
	}
	idWidth := len("ID")
	for _, finding := range findings {
		if width := len(strconv.FormatInt(finding.ID, 10)) + 1; width > idWidth {
			idWidth = width
		}
	}
	fmt.Fprintf(output, "%-*s  %-8s  %-9s  %-*s  %-*s  %-*s  %s\n",
		idWidth, "ID", "SEVERITY", "STATUS", findingListAgeWidth, "AGE",
		findingListBlameWidth, "BLAME", findingListLocationWidth, "LOCATION", "TITLE")
	for _, finding := range findings {
		blame := padRight(truncateTerminalText(finding.Blame, findingListBlameWidth), findingListBlameWidth)
		fmt.Fprintf(output, "%-*s  %-8s  %-9s  %-*s  %s  %-*s  %s\n",
			idWidth, "#"+strconv.FormatInt(finding.ID, 10), finding.Severity,
			finding.Status, findingListAgeWidth, formatFindingAge(now, finding.CommitDate),
			blame, findingListLocationWidth, findingLocation(finding.Finding),
			singleLine(finding.Title))
	}
}

func findingListDescription(status, severity string, singular bool) string {
	parts := make([]string, 0, 3)
	if status != "all" {
		parts = append(parts, status)
	}
	if severity != "all" {
		parts = append(parts, severity)
	}
	noun := "findings"
	if singular {
		noun = "finding"
	}
	parts = append(parts, noun)
	return strings.Join(parts, " ")
}

func findingLocation(finding Finding) string {
	if finding.File == nil {
		return "(no location)"
	}
	location := *finding.File
	if finding.Line != nil {
		location += ":" + strconv.Itoa(*finding.Line)
	}
	return location
}
