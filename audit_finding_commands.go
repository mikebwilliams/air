package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	auditFindingAgeWidth      = 4
	auditFindingScanWidth     = 12
	auditFindingLocationWidth = 34
)

type auditFindingListItem struct {
	Finding
	Status       string `json:"status"`
	Verification string `json:"verification"`
}

type auditFindingListOutput struct {
	Version      int                    `json:"version"`
	ScanID       string                 `json:"scan_id,omitempty"`
	Status       string                 `json:"status"`
	Severity     string                 `json:"severity"`
	Verification string                 `json:"verification"`
	Path         string                 `json:"path"`
	Tags         []string               `json:"tags,omitempty"`
	Search       string                 `json:"search,omitempty"`
	Sort         string                 `json:"sort"`
	Total        int                    `json:"total"`
	Findings     []auditFindingListItem `json:"findings"`
}

type auditFindingSourceOutput struct {
	Version     int      `json:"version"`
	FindingID   int64    `json:"finding_id"`
	File        string   `json:"file,omitempty"`
	Line        *int     `json:"line,omitempty"`
	ObservedSHA string   `json:"observed_sha"`
	Header      string   `json:"header,omitempty"`
	Lines       []string `json:"lines,omitempty"`
	Target      int      `json:"target"`
	Message     string   `json:"message,omitempty"`
}

func writeAuditFindingDetail(ctx context.Context, output io.Writer, store *auditFindingStore, finding Finding) error {
	review, err := store.FindingReview(ctx, finding.ID)
	if err != nil {
		return err
	}
	events, err := store.FindingEvents(ctx, finding.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "#%d %s  %s\n%s\n\n", finding.ID, strings.ToUpper(finding.Severity), findingDisposition(finding), finding.Title)
	fmt.Fprintln(output, "Location: "+findingLocation(finding))
	fmt.Fprintln(output, "Observed: "+finding.ObservedSHA)
	fmt.Fprintf(output, "Scan: %s\nAssignment: %s\nAttempt: %d\n", finding.ScanID, finding.TaskID, finding.AttemptID)
	identity := review.Model
	if review.Harness != "" {
		identity = review.Harness + "/" + identity
	}
	if review.ReasoningEffort != "" {
		identity += "/" + review.ReasoningEffort
	}
	fmt.Fprintf(output, "Model: %s\nObserved at: %s\n", identity, review.ReviewedAt.Format(time.RFC3339))
	if len(finding.Tags) > 0 {
		fmt.Fprintln(output, "Tags: "+strings.Join(finding.Tags, ", "))
	}
	if finding.DismissedAt != nil {
		fmt.Fprintf(output, "Dismissed at: %s\nReason: %s\n", finding.DismissedAt.Format(time.RFC3339), finding.DismissReason)
	}
	fmt.Fprintln(output, "\nDescription:")
	fmt.Fprintln(output, finding.Description)
	fmt.Fprintln(output, "\nVerification:")
	if len(finding.Verifications) == 0 {
		fmt.Fprintln(output, "    unchecked")
	}
	for _, verification := range finding.Verifications {
		identity := verification.Model
		if verification.Harness != "" {
			identity = verification.Harness + "/" + identity
		}
		if verification.Effort != "" {
			identity += "/" + verification.Effort
		}
		fmt.Fprintf(output, "    %s  %s  %s  recheck %s\n", verification.CheckedAt.Format(time.RFC3339), verification.Outcome, identity, verification.ScanID)
		fmt.Fprintf(output, "        %s\n", verification.Reason)
	}
	fmt.Fprintln(output, "\nHistory:")
	if len(events) == 0 {
		fmt.Fprintln(output, "    none")
	}
	for _, event := range events {
		note := ""
		if event.Note != "" {
			note = " — " + event.Note
		}
		fmt.Fprintf(output, "    %s  %s%s\n", event.CreatedAt.Format(time.RFC3339), event.Action, note)
	}
	return nil
}

func runAuditFindingListCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("finding list", environment.Stderr)
	repo := flags.String("repo", ".", "scan checkout")
	scanSelector := flags.String("scan", "", "source scan; latest selects the newest completed original scan")
	includeAll := flags.Bool("all", false, "include findings of every disposition")
	status := flags.String("status", "open", "filter by status: open, dismissed, or all")
	severity := flags.String("severity", "all", "filter by severity: error, warning, info, or all")
	verification := flags.String("verification", "all", "filter latest verdict: all, unchecked, confirmed, false_positive, uncertain")
	pathPrefix := flags.String("path", ".", "filter by repository path prefix")
	tagValues := flags.StringArray("tag", nil, "require a finding tag (repeatable; multiple tags use AND)")
	search := flags.String("search", "", "search finding text, location, tags, provenance, or ID")
	sortName := flags.String("sort", "id", "sort by id, age, file, scan, severity, status, title, or verification")
	limit := flags.Int("limit", 0, "maximum findings to return; zero means unlimited")
	asJSON := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: repose finding list [--scan ID|latest] [--status STATUS] [--severity SEVERITY] [--verification VERDICT] [--path PREFIX] [--tag TAG] [--search TEXT] [--sort SORT] [--limit N] [--all] [--json] [--repo DIR]")
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
	*verification = strings.ToLower(strings.TrimSpace(*verification))
	*search = strings.TrimSpace(*search)
	*sortName = strings.ToLower(strings.TrimSpace(*sortName))
	if *status != "open" && *status != "dismissed" && *status != "all" {
		return fmt.Errorf("invalid --status %q; expected open, dismissed, or all", *status)
	}
	if !validFindingListSeverity(*severity) {
		return fmt.Errorf("invalid --severity %q; expected error, warning, info, or all", *severity)
	}
	if !validAuditVerificationFilter(*verification) {
		return errors.New("verification must be all, unchecked, confirmed, false_positive, or uncertain")
	}
	if !validAuditFindingSort(*sortName) {
		return fmt.Errorf("invalid --sort %q; expected id, age, file, scan, severity, status, title, or verification", *sortName)
	}
	if *limit < 0 {
		return errors.New("--limit must not be negative")
	}
	selection := inventorySelection{Path: *pathPrefix, Status: "included"}
	if err := selection.validate(); err != nil {
		return err
	}
	tags, err := normalizeFindingTags(*tagValues)
	if err != nil {
		return err
	}

	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repo))
	if err != nil {
		return err
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	reader, err := openInventoryReadOnly(ctx, database)
	if err != nil {
		return err
	}
	defer reader.Close()
	store := &auditFindingStore{reader: reader, databasePath: database}
	if *scanSelector != "" {
		store.scanID, err = resolveAuditFindingScan(ctx, reader, *scanSelector)
		if err != nil {
			return err
		}
	}
	all, err := store.AllFindings(ctx)
	if err != nil {
		return err
	}
	findings, total := selectAuditFindingList(all, *status, *severity, *verification, selection.Path, tags, *search, *sortName, *limit)
	items := make([]auditFindingListItem, 0, len(findings))
	for _, finding := range findings {
		items = append(items, auditFindingListItem{
			Finding: finding, Status: findingDisposition(finding), Verification: auditVerificationOutcome(finding),
		})
	}
	if *asJSON {
		return writeInventoryJSON(environment.Stdout, auditFindingListOutput{
			Version: 1, ScanID: store.scanID, Status: *status, Severity: *severity,
			Verification: *verification, Path: selection.Path, Tags: tags, Search: *search, Sort: *sortName,
			Total: total, Findings: items,
		})
	}
	writeAuditFindingList(environment.Stdout, items, total, *status, *severity, environmentNow(environment))
	return nil
}

func resolveAuditFindingScan(ctx context.Context, reader *inventoryStore, selector string) (string, error) {
	var scan auditScan
	var err error
	if selector == "latest" {
		scan, err = reader.latestAuditForRecheck(ctx)
	} else {
		scan, err = reader.audit(ctx, selector)
	}
	if err != nil {
		return "", err
	}
	if scan.Spec.Recheck != nil {
		return scan.Spec.Recheck.SourceScanID, nil
	}
	return scan.ID, nil
}

func validAuditFindingSort(value string) bool {
	switch value {
	case "id", "age", "file", "scan", "severity", "status", "title", "verification":
		return true
	default:
		return false
	}
}

func selectAuditFindingList(
	all []Finding,
	status, severity, verification, pathPrefix string,
	tags []string,
	search string,
	sortName string,
	limit int,
) ([]Finding, int) {
	query := strings.ToLower(strings.TrimSpace(search))
	selected := make([]Finding, 0, len(all))
	for _, finding := range all {
		if status != "all" && findingDisposition(finding) != status {
			continue
		}
		if severity != "all" && finding.Severity != severity {
			continue
		}
		if verification != "all" && auditVerificationOutcome(finding) != verification {
			continue
		}
		if pathPrefix != "." && (finding.File == nil || !inventoryPrefixMatches(*finding.File, pathPrefix)) {
			continue
		}
		if !findingHasTags(finding, tags) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(findingSearchText(finding)), query) {
			continue
		}
		selected = append(selected, finding)
	}
	sortAuditFindingList(selected, sortName)
	total := len(selected)
	if limit > 0 && len(selected) > limit {
		selected = selected[:limit]
	}
	return selected, total
}

func sortAuditFindingList(findings []Finding, sortName string) {
	if sortName == "scan" {
		sort.SliceStable(findings, func(i, j int) bool {
			if findings[i].ScanID != findings[j].ScanID {
				return findings[i].ScanID < findings[j].ScanID
			}
			return findings[i].ID > findings[j].ID
		})
		return
	}
	if sortName == "verification" {
		sort.SliceStable(findings, func(i, j int) bool {
			left, right := auditVerificationOutcome(findings[i]), auditVerificationOutcome(findings[j])
			if left != right {
				return left < right
			}
			return findings[i].ID > findings[j].ID
		})
		return
	}
	sortFindings(findings, findingSortMode(sortName), auditFindingDisplay(findings))
}

func writeAuditFindingList(output io.Writer, findings []auditFindingListItem, total int, status, severity string, now time.Time) {
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
		idWidth = max(idWidth, len(strconv.FormatInt(finding.ID, 10))+1)
	}
	fmt.Fprintf(output, "%-*s  %-8s  %-9s  %-14s  %-*s  %-*s  %-*s  %s\n",
		idWidth, "ID", "SEVERITY", "STATUS", "VERIFICATION", auditFindingAgeWidth, "AGE",
		auditFindingScanWidth, "SCAN", auditFindingLocationWidth, "LOCATION", "TITLE / TAGS")
	for _, finding := range findings {
		age := time.Time{}
		if finding.ObservedAt != nil {
			age = *finding.ObservedAt
		}
		title := singleLine(finding.Title)
		if len(finding.Tags) > 0 {
			title += "  [" + strings.Join(finding.Tags, ", ") + "]"
		}
		fmt.Fprintf(output, "%-*s  %-8s  %-9s  %-14s  %-*s  %-*s  %-*s  %s\n",
			idWidth, "#"+strconv.FormatInt(finding.ID, 10), finding.Severity, finding.Status,
			finding.Verification, auditFindingAgeWidth, formatFindingAge(now, age),
			auditFindingScanWidth, truncateTerminalText(finding.ScanID, auditFindingScanWidth),
			auditFindingLocationWidth, findingLocation(finding.Finding), title)
	}
}

func runAuditFindingSourceCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("finding source", environment.Stderr)
	repo := flags.String("repo", ".", "scan checkout")
	contextLines := flags.Int("context", 20, "lines to show before and after the finding")
	asJSON := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: repose finding source ID [--context N] [--json] [--repo DIR]")
	}
	if *contextLines < 0 || *contextLines > 1000 {
		return errors.New("--context must be between 0 and 1000")
	}
	id, err := parseFindingID(flags.Arg(0))
	if err != nil {
		return err
	}
	repository, reader, store, err := openAuditFindingStore(ctx, environment, *repo)
	if err != nil {
		return err
	}
	defer reader.Close()
	finding, err := findAuditFinding(ctx, store, id)
	if err != nil {
		return err
	}
	preview, err := loadAuditFindingSource(ctx, repository, finding, *contextLines)
	if err != nil {
		return fmt.Errorf("read source for finding #%d: %w", id, err)
	}
	if *asJSON {
		return writeInventoryJSON(environment.Stdout, auditFindingSourceOutput{
			Version: 1, FindingID: finding.ID, File: preview.File, Line: finding.Line,
			ObservedSHA: finding.ObservedSHA, Header: preview.HunkHeader, Lines: preview.Lines,
			Target: preview.Target, Message: preview.Message,
		})
	}
	if preview.Message != "" {
		fmt.Fprintf(environment.Stdout, "Finding #%d: %s\n", finding.ID, preview.Message)
		return nil
	}
	fmt.Fprintf(environment.Stdout, "Finding #%d — %s\n%s\n\n", finding.ID, findingLocation(finding), preview.HunkHeader)
	for index, line := range preview.Lines {
		marker := "  "
		if index == preview.Target {
			marker = "> "
		}
		fmt.Fprintln(environment.Stdout, marker+line)
	}
	return nil
}

func runAuditFindingOpenCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("finding open", environment.Stderr)
	repo := flags.String("repo", ".", "scan checkout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: repose finding open ID [--repo DIR]")
	}
	id, err := parseFindingID(flags.Arg(0))
	if err != nil {
		return err
	}
	repository, reader, store, err := openAuditFindingStore(ctx, environment, *repo)
	if err != nil {
		return err
	}
	defer reader.Close()
	finding, err := findAuditFinding(ctx, store, id)
	if err != nil {
		return err
	}
	commandContext := environment.ExternalCommand
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	command, err := buildAuditFindingOpenCommand(ctx, repository, finding, commandContext)
	if err != nil {
		return err
	}
	attachCommandIO(command, environment.Stdin, environment.Stdout, environment.Stderr)
	if err := command.Run(); err != nil {
		return fmt.Errorf("open finding #%d: %w", id, err)
	}
	return nil
}

func openAuditFindingStore(ctx context.Context, environment cliEnvironment, repo string) (*GitRepository, *inventoryStore, *auditFindingStore, error) {
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, repo))
	if err != nil {
		return nil, nil, nil, err
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return nil, nil, nil, err
	}
	reader, err := openInventoryReadOnly(ctx, database)
	if err != nil {
		return nil, nil, nil, err
	}
	return repository, reader, &auditFindingStore{reader: reader, databasePath: database}, nil
}

func findAuditFinding(ctx context.Context, store *auditFindingStore, id int64) (Finding, error) {
	findings, err := store.AllFindings(ctx)
	if err != nil {
		return Finding{}, err
	}
	for _, finding := range findings {
		if finding.ID == id {
			return finding, nil
		}
	}
	return Finding{}, fmt.Errorf("finding #%d not found", id)
}

func buildAuditFindingOpenCommand(
	ctx context.Context,
	repository *GitRepository,
	finding Finding,
	commandContext commandContextFunc,
) (*exec.Cmd, error) {
	if repository == nil {
		return nil, errors.New("no Git repository is available")
	}
	if finding.File == nil {
		return nil, fmt.Errorf("finding #%d has no file location", finding.ID)
	}
	if err := validateRepositoryPath(*finding.File); err != nil {
		return nil, fmt.Errorf("finding #%d has an invalid file location: %w", finding.ID, err)
	}
	if !isHexObjectID(finding.ObservedSHA) {
		return nil, fmt.Errorf("finding #%d has an invalid observed snapshot", finding.ID)
	}
	head, err := repository.ResolveCommit(ctx, "HEAD")
	if err != nil {
		return nil, err
	}
	if head != finding.ObservedSHA {
		return nil, fmt.Errorf("finding #%d was observed at %s, but the checkout is at %s; use 'repose finding source %d' to inspect the recorded snapshot", finding.ID, shortSHA(finding.ObservedSHA), shortSHA(head), finding.ID)
	}
	filename := filepath.Join(repository.WorkTree, filepath.FromSlash(*finding.File))
	workingSource, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", *finding.File, err)
	}
	snapshotSource, err := repository.runBytes(ctx, 64*1024*1024, "show", finding.ObservedSHA+":"+*finding.File)
	if err != nil {
		return nil, fmt.Errorf("read %s at the recorded snapshot: %w", *finding.File, err)
	}
	if snapshotSource.exceeded {
		return nil, fmt.Errorf("%s exceeds the 64 MiB editor safety check", *finding.File)
	}
	if !bytes.Equal(workingSource, snapshotSource.data) {
		return nil, fmt.Errorf("%s differs from the recorded snapshot; use 'repose finding source %d' to inspect the exact source", *finding.File, finding.ID)
	}
	return buildFindingOpenCommand(ctx, repository, finding, commandContext)
}
