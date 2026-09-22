package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const reposeFindingAttributionSchemaSQL = `
CREATE TABLE audit_finding_attributions (
    finding_id INTEGER PRIMARY KEY REFERENCES audit_findings(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK(status IN ('attributed','unavailable')),
    commit_sha TEXT NOT NULL DEFAULT '',
    author TEXT NOT NULL DEFAULT '',
    author_email TEXT NOT NULL DEFAULT '',
    authored_at TEXT,
    original_line INTEGER,
    error TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);
CREATE INDEX audit_finding_attributions_author ON audit_finding_attributions(author, finding_id);
PRAGMA user_version = 10;
`

const (
	attributionStatusAttributed  = "attributed"
	attributionStatusUnavailable = "unavailable"
	maximumBlameOutputBytes      = 64 * 1024 * 1024
	maximumBlameSpanLines        = 8192
)

type findingAttributionTarget struct {
	ID          int64
	SnapshotSHA string
	Path        string
	Line        int
}

type findingAttributionResult struct {
	Target      findingAttributionTarget
	Attribution FindingAttribution
}

type findingAttributionGroup struct {
	SnapshotSHA string
	Path        string
	Targets     []findingAttributionTarget
}

type findingAttributionBackfillReport struct {
	Version     int `json:"version"`
	Selected    int `json:"selected"`
	Attributed  int `json:"attributed"`
	Unavailable int `json:"unavailable"`
	Remaining   int `json:"remaining"`
	Retryable   int `json:"retryable_errors"`
}

func findingAttributionTime(finding Finding) time.Time {
	if finding.Attribution == nil || finding.Attribution.Status != attributionStatusAttributed || finding.Attribution.AuthoredAt == nil {
		return time.Time{}
	}
	return *finding.Attribution.AuthoredAt
}

func (s *auditFindingStore) loadAttributions(ctx context.Context, findings []Finding) error {
	if len(findings) == 0 || s.reader.version < 10 {
		return nil
	}
	query := `SELECT a.finding_id,a.status,a.commit_sha,a.author,a.author_email,
		a.authored_at,a.original_line,a.error
		FROM audit_finding_attributions a JOIN audit_findings f ON f.id=a.finding_id`
	args := []any{}
	if s.scanID != "" {
		query += " WHERE f.scan_id=?"
		args = append(args, s.scanID)
	}
	rows, err := s.reader.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[int64]*Finding, len(findings))
	for i := range findings {
		byID[findings[i].ID] = &findings[i]
	}
	for rows.Next() {
		var id int64
		var attribution FindingAttribution
		var authored sql.NullString
		var originalLine sql.NullInt64
		if err := rows.Scan(&id, &attribution.Status, &attribution.CommitSHA, &attribution.Author,
			&attribution.AuthorEmail, &authored, &originalLine, &attribution.Error); err != nil {
			return err
		}
		if authored.Valid {
			parsed, err := time.Parse(time.RFC3339Nano, authored.String)
			if err != nil {
				return fmt.Errorf("parse attribution time for finding %d: %w", id, err)
			}
			attribution.AuthoredAt = &parsed
		}
		if originalLine.Valid {
			attribution.OriginalLine = int(originalLine.Int64)
		}
		if finding := byID[id]; finding != nil {
			finding.Attribution = &attribution
		}
	}
	return rows.Err()
}

func saveFindingAttributions(ctx context.Context, store *inventoryStore, results []findingAttributionResult, now time.Time) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO audit_finding_attributions(
		finding_id,status,commit_sha,author,author_email,authored_at,original_line,error,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(finding_id) DO UPDATE SET
		status=excluded.status,commit_sha=excluded.commit_sha,author=excluded.author,
		author_email=excluded.author_email,authored_at=excluded.authored_at,
		original_line=excluded.original_line,error=excluded.error,updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, result := range results {
		attribution := result.Attribution
		var authored any
		if attribution.AuthoredAt != nil {
			authored = formatTime(*attribution.AuthoredAt)
		}
		var originalLine any
		if attribution.OriginalLine > 0 {
			originalLine = attribution.OriginalLine
		}
		if _, err := statement.ExecContext(ctx, result.Target.ID, attribution.Status,
			attribution.CommitSHA, attribution.Author, attribution.AuthorEmail, authored,
			originalLine, attribution.Error, formatTime(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertFindingAttribution(ctx context.Context, tx *sql.Tx, findingID int64, attribution FindingAttribution, now time.Time) error {
	var authored any
	if attribution.AuthoredAt != nil {
		authored = formatTime(*attribution.AuthoredAt)
	}
	var originalLine any
	if attribution.OriginalLine > 0 {
		originalLine = attribution.OriginalLine
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_finding_attributions(
		finding_id,status,commit_sha,author,author_email,authored_at,original_line,error,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, findingID, attribution.Status, attribution.CommitSHA,
		attribution.Author, attribution.AuthorEmail, authored, originalLine,
		attribution.Error, formatTime(now))
	return err
}

func findingAttributionTargets(snapshotSHA string, findings []NewFinding) []findingAttributionTarget {
	targets := make([]findingAttributionTarget, 0, len(findings))
	for i, finding := range findings {
		if finding.File == nil || finding.Line == nil {
			continue
		}
		targets = append(targets, findingAttributionTarget{
			ID: int64(i + 1), SnapshotSHA: snapshotSHA, Path: *finding.File, Line: *finding.Line,
		})
	}
	return targets
}

func attributeAuditOutput(repository *GitRepository, snapshotSHA string, output *auditOutput) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	targets := findingAttributionTargets(snapshotSHA, output.Findings)
	attributed := attributeFindingTargets(ctx, repository, targets, 4)
	byIndex := make(map[int64]FindingAttribution, len(attributed))
	for _, result := range attributed {
		byIndex[result.Target.ID] = result.Attribution
	}
	output.Attributions = make([]FindingAttribution, len(output.Findings))
	for index := range output.Findings {
		attribution, ok := byIndex[int64(index+1)]
		if !ok {
			attribution = unavailableFindingAttribution("author attribution did not complete")
		}
		output.Attributions[index] = attribution
	}
}

func attributeFindingTargets(ctx context.Context, repository *GitRepository, targets []findingAttributionTarget, jobs int) []findingAttributionResult {
	if len(targets) == 0 {
		return nil
	}
	groups := groupFindingAttributionTargets(targets)
	results := startFindingAttributionWorkers(ctx, repository, groups, jobs)
	all := make([]findingAttributionResult, 0, len(targets))
	for groupResults := range results {
		all = append(all, groupResults...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Target.ID < all[j].Target.ID })
	return all
}

func startFindingAttributionWorkers(
	ctx context.Context,
	repository *GitRepository,
	groups []findingAttributionGroup,
	jobs int,
) <-chan []findingAttributionResult {
	if jobs < 1 {
		jobs = 1
	}
	jobs = min(jobs, len(groups))
	tasks := make(chan findingAttributionGroup)
	results := make(chan []findingAttributionResult, len(groups))
	var workers sync.WaitGroup
	for range jobs {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for group := range tasks {
				results <- attributeFindingGroup(ctx, repository, group)
			}
		}()
	}
	go func() {
		defer close(tasks)
		for _, group := range groups {
			select {
			case tasks <- group:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()
	return results
}

func groupFindingAttributionTargets(targets []findingAttributionTarget) []findingAttributionGroup {
	groups := map[string]*findingAttributionGroup{}
	order := []string{}
	for _, target := range targets {
		key := target.SnapshotSHA + "\x00" + target.Path
		group := groups[key]
		if group == nil {
			group = &findingAttributionGroup{SnapshotSHA: target.SnapshotSHA, Path: target.Path}
			groups[key] = group
			order = append(order, key)
		}
		group.Targets = append(group.Targets, target)
	}
	result := make([]findingAttributionGroup, 0, len(order))
	for _, key := range order {
		group := groups[key]
		sort.Slice(group.Targets, func(i, j int) bool {
			if group.Targets[i].Line != group.Targets[j].Line {
				return group.Targets[i].Line < group.Targets[j].Line
			}
			return group.Targets[i].ID < group.Targets[j].ID
		})
		result = append(result, *group)
	}
	return result
}

func attributeFindingGroup(ctx context.Context, repository *GitRepository, group findingAttributionGroup) []findingAttributionResult {
	results := make([]findingAttributionResult, len(group.Targets))
	for i, target := range group.Targets {
		results[i].Target = target
	}
	if !isHexObjectID(group.SnapshotSHA) {
		return unavailableAttributionResults(results, "invalid observed snapshot")
	}
	if err := validateRepositoryPath(group.Path); err != nil {
		return unavailableAttributionResults(results, "invalid finding path: "+err.Error())
	}
	for start := 0; start < len(group.Targets); {
		end := start + 1
		for end < len(group.Targets) && group.Targets[end].Line-group.Targets[start].Line < maximumBlameSpanLines {
			end++
		}
		span := group.Targets[start:end]
		attributions, err := repository.blameLines(ctx, group.SnapshotSHA, group.Path, span[0].Line, span[len(span)-1].Line)
		if err != nil {
			for i := start; i < end; i++ {
				results[i].Attribution = unavailableFindingAttribution(err.Error())
			}
		} else {
			for i := start; i < end; i++ {
				attribution, ok := attributions[group.Targets[i].Line]
				if !ok {
					attribution = unavailableFindingAttribution(fmt.Sprintf("git blame returned no attribution for line %d", group.Targets[i].Line))
				}
				results[i].Attribution = attribution
			}
		}
		start = end
	}
	return results
}

func unavailableAttributionResults(results []findingAttributionResult, message string) []findingAttributionResult {
	for i := range results {
		results[i].Attribution = unavailableFindingAttribution(message)
	}
	return results
}

func unavailableFindingAttribution(message string) FindingAttribution {
	return FindingAttribution{Status: attributionStatusUnavailable, Error: inventoryDisplay(message)}
}

func (r *GitRepository) blameLines(ctx context.Context, snapshotSHA, repositoryPath string, startLine, endLine int) (map[int]FindingAttribution, error) {
	if startLine < 1 || endLine < startLine {
		return nil, errors.New("invalid blame line range")
	}
	out, err := r.runBytes(ctx, maximumBlameOutputBytes, "blame", "--line-porcelain",
		"-L", fmt.Sprintf("%d,%d", startLine, endLine), snapshotSHA, "--", repositoryPath)
	if err != nil {
		return nil, err
	}
	if out.exceeded {
		return nil, fmt.Errorf("git blame output exceeded %d MiB", maximumBlameOutputBytes/(1024*1024))
	}
	return parseLinePorcelainBlame(out.data)
}

func parseLinePorcelainBlame(data []byte) (map[int]FindingAttribution, error) {
	result := map[int]FindingAttribution{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var current FindingAttribution
	var finalLine int
	var authorTime int64
	inRecord := false
	for scanner.Scan() {
		line := scanner.Text()
		if !inRecord {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				return nil, fmt.Errorf("unexpected git blame header %q", line)
			}
			sha := strings.TrimPrefix(fields[0], "^")
			if !isHexObjectID(sha) {
				return nil, fmt.Errorf("invalid blamed commit %q", fields[0])
			}
			originalLine, err := strconv.Atoi(fields[1])
			if err != nil || originalLine < 1 {
				return nil, fmt.Errorf("invalid original blame line %q", fields[1])
			}
			finalLine, err = strconv.Atoi(fields[2])
			if err != nil || finalLine < 1 {
				return nil, fmt.Errorf("invalid final blame line %q", fields[2])
			}
			current = FindingAttribution{Status: attributionStatusAttributed, CommitSHA: sha, OriginalLine: originalLine}
			authorTime = 0
			inRecord = true
			continue
		}
		if strings.HasPrefix(line, "author ") {
			current.Author = strings.TrimPrefix(line, "author ")
		} else if strings.HasPrefix(line, "author-mail ") {
			current.AuthorEmail = strings.Trim(strings.TrimPrefix(line, "author-mail "), "<>")
		} else if strings.HasPrefix(line, "author-time ") {
			parsed, err := strconv.ParseInt(strings.TrimPrefix(line, "author-time "), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid git blame author time for line %d", finalLine)
			}
			authorTime = parsed
		} else if strings.HasPrefix(line, "\t") {
			if current.Author == "" {
				return nil, fmt.Errorf("git blame omitted author for line %d", finalLine)
			}
			if authorTime != 0 {
				when := time.Unix(authorTime, 0).UTC()
				current.AuthoredAt = &when
			}
			result[finalLine] = current
			inRecord = false
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if inRecord {
		return nil, errors.New("truncated git blame output")
	}
	return result, nil
}

func runAuditFindingBackfillAuthorsCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("finding backfill-authors", environment.Stderr)
	repo := flags.String("repo", ".", "scan checkout")
	scanSelector := flags.String("scan", "", "restrict to one original source scan")
	jobs := flags.Int("jobs", 8, "concurrent git blame workers")
	limit := flags.Int("limit", 0, "maximum findings to process; zero means unlimited")
	retryErrors := flags.Bool("retry-errors", false, "retry previously unavailable attributions")
	asJSON := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: repose finding backfill-authors [--scan ID|latest] [--jobs N] [--limit N] [--retry-errors] [--json] [--repo DIR]")
	}
	if *jobs < 1 || *jobs > 32 {
		return errors.New("--jobs must be between 1 and 32")
	}
	if *limit < 0 {
		return errors.New("--limit must not be negative")
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repo))
	if err != nil {
		return err
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	lock, err := acquireScanLock(filepath.Join(filepath.Dir(database), "scan.lock"))
	if err != nil {
		return fmt.Errorf("cannot backfill authors: %w", err)
	}
	defer lock.Close()
	store, err := openInventoryStore(ctx, database, false)
	if err != nil {
		return err
	}
	defer store.Close()
	scanID := ""
	if *scanSelector != "" {
		scanID, err = resolveAuditFindingScan(ctx, store, *scanSelector)
		if err != nil {
			return err
		}
	}
	targets, err := missingFindingAttributionTargets(ctx, store, scanID, *retryErrors, *limit)
	if err != nil {
		return err
	}
	report := findingAttributionBackfillReport{Version: 1, Selected: len(targets)}
	if len(targets) > 0 {
		groups := groupFindingAttributionTargets(targets)
		for results := range startFindingAttributionWorkers(ctx, repository, groups, *jobs) {
			if ctx.Err() != nil {
				successful := results[:0]
				for _, result := range results {
					if result.Attribution.Status == attributionStatusAttributed {
						successful = append(successful, result)
					}
				}
				results = successful
			}
			if len(results) == 0 {
				continue
			}
			writeContext, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			err := saveFindingAttributions(writeContext, store, results, environmentNow(environment))
			cancelWrite()
			if err != nil {
				return err
			}
			for _, result := range results {
				if result.Attribution.Status == attributionStatusAttributed {
					report.Attributed++
				} else {
					report.Unavailable++
				}
			}
		}
	}
	readContext, cancelRead := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	report.Remaining, err = countMissingFindingAttributions(readContext, store, scanID, false)
	if err == nil {
		report.Retryable, err = countUnavailableFindingAttributions(readContext, store, scanID)
	}
	cancelRead()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if *asJSON {
		return writeInventoryJSON(environment.Stdout, report)
	}
	fmt.Fprintf(environment.Stdout, "Line attribution: %d selected, %d attributed, %d unavailable, %d remaining, %d retryable errors.\n",
		report.Selected, report.Attributed, report.Unavailable, report.Remaining, report.Retryable)
	return nil
}

func missingFindingAttributionTargets(ctx context.Context, store *inventoryStore, scanID string, retryErrors bool, limit int) ([]findingAttributionTarget, error) {
	query := `SELECT f.id,f.observed_sha,f.document FROM audit_findings f
		LEFT JOIN audit_finding_attributions a ON a.finding_id=f.id WHERE `
	if retryErrors {
		query += "(a.finding_id IS NULL OR a.status='unavailable')"
	} else {
		query += "a.finding_id IS NULL"
	}
	args := []any{}
	if scanID != "" {
		query += " AND f.scan_id=?"
		args = append(args, scanID)
	}
	query += " ORDER BY f.id"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := []findingAttributionTarget{}
	for rows.Next() {
		var target findingAttributionTarget
		var document string
		if err := rows.Scan(&target.ID, &target.SnapshotSHA, &document); err != nil {
			return nil, err
		}
		var finding NewFinding
		if err := json.Unmarshal([]byte(document), &finding); err != nil {
			return nil, err
		}
		if finding.File != nil {
			target.Path = *finding.File
		}
		if finding.Line != nil {
			target.Line = *finding.Line
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func countMissingFindingAttributions(ctx context.Context, store *inventoryStore, scanID string, includeErrors bool) (int, error) {
	query := `SELECT count(*) FROM audit_findings f LEFT JOIN audit_finding_attributions a ON a.finding_id=f.id WHERE `
	if includeErrors {
		query += "(a.finding_id IS NULL OR a.status='unavailable')"
	} else {
		query += "a.finding_id IS NULL"
	}
	args := []any{}
	if scanID != "" {
		query += " AND f.scan_id=?"
		args = append(args, scanID)
	}
	var count int
	err := store.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

func countUnavailableFindingAttributions(ctx context.Context, store *inventoryStore, scanID string) (int, error) {
	query := `SELECT count(*) FROM audit_finding_attributions a
		JOIN audit_findings f ON f.id=a.finding_id WHERE a.status='unavailable'`
	args := []any{}
	if scanID != "" {
		query += " AND f.scan_id=?"
		args = append(args, scanID)
	}
	var count int
	err := store.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}
