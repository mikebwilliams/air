package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const reposeFindingTagsSchemaSQL = `
CREATE TABLE audit_finding_tags (
    finding_id INTEGER NOT NULL REFERENCES audit_findings(id) ON DELETE CASCADE,
    tag TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (finding_id, tag)
);
CREATE INDEX audit_finding_tags_by_tag ON audit_finding_tags(tag, finding_id);
PRAGMA user_version = 7;
`

var findingTagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,63}$`)

type auditFindingStore struct {
	reader               *inventoryStore
	databasePath, scanID string
}

func (s *auditFindingStore) AllFindings(ctx context.Context) ([]Finding, error) {
	findings := []Finding{}
	if s.reader.version < 4 {
		return findings, nil
	}
	query := "SELECT id,scan_id,task_id,attempt_id,observed_sha,created_at,dismissed_at,dismiss_reason,document FROM audit_findings"
	args := []any{}
	if s.scanID != "" {
		query += " WHERE scan_id=?"
		args = append(args, s.scanID)
	}
	query += " ORDER BY id"
	rows, err := s.reader.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f Finding
		var created, data string
		var dismissed sql.NullString
		if err := rows.Scan(&f.ID, &f.ScanID, &f.TaskID, &f.AttemptID, &f.ObservedSHA, &created, &dismissed, &f.DismissReason, &data); err != nil {
			return nil, err
		}
		var value NewFinding
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			return nil, err
		}
		f.Severity, f.Title, f.Description, f.File, f.Line, f.Symbol = value.Severity, value.Title, value.Description, value.File, value.Line, value.Symbol
		observed, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		f.ObservedAt = &observed
		if dismissed.Valid {
			when, err := time.Parse(time.RFC3339Nano, dismissed.String)
			if err != nil {
				return nil, err
			}
			f.DismissedAt = &when
		}
		findings = append(findings, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := s.loadVerifications(ctx, findings); err != nil {
		return nil, err
	}
	if err := s.loadTags(ctx, findings); err != nil {
		return nil, err
	}
	if err := s.loadAttributions(ctx, findings); err != nil {
		return nil, err
	}
	return findings, nil
}

func (s *auditFindingStore) loadTags(ctx context.Context, findings []Finding) error {
	if len(findings) == 0 {
		return nil
	}
	var version int
	if err := s.reader.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 7 {
		return nil
	}
	query := `SELECT t.finding_id,t.tag FROM audit_finding_tags t JOIN audit_findings f ON f.id=t.finding_id`
	args := []any{}
	if s.scanID != "" {
		query += " WHERE f.scan_id=?"
		args = append(args, s.scanID)
	}
	query += " ORDER BY t.finding_id,t.tag"
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
		var tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return err
		}
		if finding := byID[id]; finding != nil {
			finding.Tags = append(finding.Tags, tag)
		}
	}
	return rows.Err()
}

func normalizeFindingTag(value string) (string, error) {
	tag := strings.ToLower(strings.TrimSpace(value))
	if !findingTagPattern.MatchString(tag) {
		return "", fmt.Errorf("invalid tag %q: use 1-64 lowercase letters, digits, '.', '_', ':', '/', or '-' and start with a letter or digit", value)
	}
	return tag, nil
}

func normalizeFindingTags(values []string) ([]string, error) {
	tags := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		tag, err := normalizeFindingTag(value)
		if err != nil {
			return nil, err
		}
		if seen[tag] {
			return nil, fmt.Errorf("tag %q was supplied more than once", tag)
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags, nil
}

func findingHasTags(finding Finding, tags []string) bool {
	for _, wanted := range tags {
		found := false
		for _, tag := range finding.Tags {
			if tag == wanted {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *auditFindingStore) editFindingTags(ctx context.Context, ids []int64, tags []string, add bool, now time.Time) (int, error) {
	if len(ids) == 0 || len(tags) == 0 {
		return 0, errors.New("at least one finding ID and tag are required")
	}
	normalized, err := normalizeFindingTags(tags)
	if err != nil {
		return 0, err
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id < 1 {
			return 0, errors.New("finding IDs must be positive")
		}
		if seen[id] {
			return 0, fmt.Errorf("finding #%d was supplied more than once", id)
		}
		seen[id] = true
	}
	writer, err := openInventoryStore(ctx, s.databasePath, false)
	if err != nil {
		return 0, err
	}
	defer writer.Close()
	tx, err := writer.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	check, err := tx.PrepareContext(ctx, "SELECT scan_id FROM audit_findings WHERE id=?")
	if err != nil {
		return 0, err
	}
	defer check.Close()
	for _, id := range ids {
		var scanID string
		if err := check.QueryRowContext(ctx, id).Scan(&scanID); errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("finding #%d not found", id)
		} else if err != nil {
			return 0, err
		}
		if s.scanID != "" && scanID != s.scanID {
			return 0, fmt.Errorf("finding #%d is outside the selected scan", id)
		}
	}
	statement := "INSERT OR IGNORE INTO audit_finding_tags(finding_id,tag,created_at) VALUES(?,?,?)"
	action := "tagged"
	if !add {
		statement = "DELETE FROM audit_finding_tags WHERE finding_id=? AND tag=?"
		action = "untagged"
	}
	edit, err := tx.PrepareContext(ctx, statement)
	if err != nil {
		return 0, err
	}
	defer edit.Close()
	event, err := tx.PrepareContext(ctx, "INSERT INTO audit_finding_events(finding_id,action,note,created_at) VALUES(?,?,?,?)")
	if err != nil {
		return 0, err
	}
	defer event.Close()
	createdAt := formatTime(now)
	changes := 0
	for _, id := range ids {
		for _, tag := range normalized {
			var result sql.Result
			if add {
				result, err = edit.ExecContext(ctx, id, tag, createdAt)
			} else {
				result, err = edit.ExecContext(ctx, id, tag)
			}
			if err != nil {
				return 0, err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}
			if changed == 0 {
				continue
			}
			if _, err := event.ExecContext(ctx, id, action, tag, createdAt); err != nil {
				return 0, err
			}
			changes++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changes, nil
}

func (s *auditFindingStore) TagFindings(ctx context.Context, ids []int64, tags []string, now time.Time) (int, error) {
	return s.editFindingTags(ctx, ids, tags, true, now)
}

func (s *auditFindingStore) UntagFindings(ctx context.Context, ids []int64, tags []string, now time.Time) (int, error) {
	return s.editFindingTags(ctx, ids, tags, false, now)
}

func (s *auditFindingStore) edit(ctx context.Context, id int64, action, note string, now time.Time) error {
	if (action == "dismissed" || action == "note") && strings.TrimSpace(note) == "" {
		return errors.New("a reason or note is required")
	}
	writer, err := openInventoryStore(ctx, s.databasePath, false)
	if err != nil {
		return err
	}
	defer writer.Close()
	tx, err := writer.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE audit_findings SET dismiss_reason=dismiss_reason WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("finding not found")
	}
	if action == "dismissed" {
		result, err = tx.ExecContext(ctx, "UPDATE audit_findings SET dismissed_at=?,dismiss_reason=? WHERE id=? AND dismissed_at IS NULL", formatTime(now), note, id)
	} else if action == "reopened" {
		result, err = tx.ExecContext(ctx, "UPDATE audit_findings SET dismissed_at=NULL,dismiss_reason='' WHERE id=? AND dismissed_at IS NOT NULL", id)
	}
	if err != nil {
		return err
	}
	n, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("finding disposition changed; reload before retrying")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_finding_events(finding_id,action,note,created_at) VALUES(?,?,?,?)", id, action, note, formatTime(now)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *auditFindingStore) DismissFinding(ctx context.Context, id int64, note string, now time.Time) error {
	return s.edit(ctx, id, "dismissed", note, now)
}
func (s *auditFindingStore) ReopenFinding(ctx context.Context, id int64, now time.Time) error {
	return s.edit(ctx, id, "reopened", "", now)
}
func (s *auditFindingStore) AddFindingNote(ctx context.Context, id int64, note string, now time.Time) error {
	return s.edit(ctx, id, "note", note, now)
}

func (s *auditFindingStore) FindingEvents(ctx context.Context, id int64) ([]FindingEvent, error) {
	rows, err := s.reader.db.QueryContext(ctx, "SELECT id,action,note,created_at FROM audit_finding_events WHERE finding_id=? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []FindingEvent{}
	for rows.Next() {
		var e FindingEvent
		var created string
		e.FindingID = id
		if err := rows.Scan(&e.ID, &e.Action, &e.Note, &created); err != nil {
			return nil, err
		}
		e.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *auditFindingStore) FindingReview(ctx context.Context, id int64) (FindingReview, error) {
	var review FindingReview
	var created, data string
	err := s.reader.db.QueryRowContext(ctx, `SELECT a.id,a.number,f.observed_sha,f.created_at,s.document FROM audit_findings f JOIN audit_attempts a ON a.id=f.attempt_id JOIN audit_scans s ON s.id=f.scan_id WHERE f.id=?`, id).Scan(&review.ID, &review.Number, &review.CommitSHA, &created, &data)
	if err != nil {
		return review, err
	}
	review.ReviewedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return review, err
	}
	var spec auditSpec
	if err = json.Unmarshal([]byte(data), &spec); err != nil {
		return review, err
	}
	review.Harness, review.Model, review.ReasoningEffort = spec.Model.Harness, spec.Model.Model, spec.Model.Effort
	return review, nil
}

func loadAuditFindingPreview(ctx context.Context, repository *GitRepository, f Finding) (findingDiffPreview, error) {
	return loadAuditFindingSource(ctx, repository, f, 20)
}

func loadAuditFindingSource(ctx context.Context, repository *GitRepository, f Finding, contextLines int) (findingDiffPreview, error) {
	if contextLines < 0 {
		return findingDiffPreview{}, errors.New("source context must not be negative")
	}
	if f.File == nil {
		return findingDiffPreview{Message: "No source location recorded."}, nil
	}
	if err := validateRepositoryPath(*f.File); err != nil {
		return findingDiffPreview{}, err
	}
	if !isHexObjectID(f.ObservedSHA) {
		return findingDiffPreview{}, errors.New("invalid observed snapshot")
	}
	output, err := repository.runBytes(ctx, 8*1024*1024, "show", f.ObservedSHA+":"+*f.File)
	if err != nil {
		return findingDiffPreview{}, err
	}
	if output.exceeded {
		return findingDiffPreview{}, errors.New("source exceeds preview limit")
	}
	all := strings.Split(string(output.data), "\n")
	target := 0
	if f.Line != nil {
		target = *f.Line - 1
	}
	if target < 0 || target >= len(all) {
		return findingDiffPreview{}, errors.New("finding line outside snapshot source")
	}
	start, end := max(0, target-contextLines), min(len(all), target+contextLines+1)
	lines := []string{}
	for i := start; i < end; i++ {
		lines = append(lines, fmt.Sprintf("%6d  %s", i+1, inventoryDisplay(all[i])))
	}
	return findingDiffPreview{File: *f.File, CommitSHA: f.ObservedSHA, HunkHeader: "Snapshot " + shortSHA(f.ObservedSHA), Lines: lines, Target: target - start}, nil
}

func runAuditFindingsCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	singular := args[0] == "finding"
	command := ""
	offset := 1
	if singular {
		if len(args) < 2 {
			return errors.New("usage: repose finding <list|show|source|open|dismiss|reopen|note|tag|untag|backfill-authors> ...")
		}
		command = args[1]
		offset = 2
		if command != "" && strings.Trim(command, "0123456789") == "" {
			command = "show"
			offset = 1
		}
		switch command {
		case "list":
			return runAuditFindingListCLI(ctx, args[offset:], environment)
		case "source":
			return runAuditFindingSourceCLI(ctx, args[offset:], environment)
		case "open":
			return runAuditFindingOpenCLI(ctx, args[offset:], environment)
		case "backfill-authors":
			return runAuditFindingBackfillAuthorsCLI(ctx, args[offset:], environment)
		}
	}
	flags := newFlagSet("findings", environment.Stderr)
	repo := flags.String("repo", ".", "scan checkout")
	asJSON := flags.Bool("json", false, "output findings JSON")
	scanSelector := flags.String("scan", "", "filter to a scan")
	includeAll := flags.Bool("all", false, "include dismissed findings")
	verification := flags.String("verification", "all", "filter latest verdict: all, unchecked, confirmed, false_positive, uncertain")
	reason := flags.String("reason", "", "dismissal reason or note text")
	tagValues := flags.StringArray("tag", nil, "require a tag, or add/remove it for tag actions (repeatable)")
	if err := flags.Parse(args[offset:]); err != nil {
		return err
	}
	if !validAuditVerificationFilter(*verification) {
		return errors.New("verification must be all, unchecked, confirmed, false_positive, or uncertain")
	}
	tags, err := normalizeFindingTags(*tagValues)
	if err != nil {
		return err
	}
	bulkTagAction := singular && (command == "tag" || command == "untag")
	if !singular && flags.NArg() != 0 || singular && !bulkTagAction && flags.NArg() != 1 || bulkTagAction && flags.NArg() == 0 {
		return errors.New("findings takes no positional arguments; show/dismiss/reopen/note require one ID; tag/untag require one or more IDs")
	}
	if bulkTagAction && len(tags) == 0 {
		return errors.New("tag and untag require at least one --tag TAG")
	}
	if singular && !bulkTagAction && len(tags) > 0 {
		return errors.New("--tag filters findings lists and supplies tags to tag/untag actions")
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
		scan, err := reader.audit(ctx, *scanSelector)
		if err != nil {
			return err
		}
		store.scanID = scan.ID
		if scan.Spec.Recheck != nil {
			store.scanID = scan.Spec.Recheck.SourceScanID
		}
	}
	findings, err := store.AllFindings(ctx)
	if err != nil {
		return err
	}
	if singular {
		ids := make([]int64, 0, flags.NArg())
		seen := make(map[int64]bool, flags.NArg())
		available := make(map[int64]bool, len(findings))
		for _, finding := range findings {
			available[finding.ID] = true
		}
		for _, value := range flags.Args() {
			id, err := parseFindingID(value)
			if err != nil {
				return err
			}
			if seen[id] {
				return fmt.Errorf("finding #%d was supplied more than once", id)
			}
			if !available[id] {
				return fmt.Errorf("finding #%d not found in selected scope", id)
			}
			seen[id] = true
			ids = append(ids, id)
		}
		id := ids[0]
		switch command {
		case "show":
			for i := range findings {
				if findings[i].ID == id {
					if *asJSON {
						return writeInventoryJSON(environment.Stdout, &findings[i])
					}
					return writeAuditFindingDetail(ctx, environment.Stdout, store, findings[i])
				}
			}
			return errors.New("finding not found in selected scope")
		case "dismiss":
			err = store.DismissFinding(ctx, id, *reason, environmentNow(environment))
		case "reopen":
			err = store.ReopenFinding(ctx, id, environmentNow(environment))
		case "note":
			err = store.AddFindingNote(ctx, id, *reason, environmentNow(environment))
		case "tag", "untag":
			changes := 0
			if command == "tag" {
				changes, err = store.TagFindings(ctx, ids, tags, environmentNow(environment))
			} else {
				changes, err = store.UntagFindings(ctx, ids, tags, environmentNow(environment))
			}
			if err != nil {
				return err
			}
			if *asJSON {
				updated, err := store.AllFindings(ctx)
				if err != nil {
					return err
				}
				selected := make([]Finding, 0, len(ids))
				for _, finding := range updated {
					if seen[finding.ID] {
						selected = append(selected, finding)
					}
				}
				return writeInventoryJSON(environment.Stdout, selected)
			}
			verb := "Added"
			if command == "untag" {
				verb = "Removed"
			}
			fmt.Fprintf(environment.Stdout, "%s %d tag assignments across %d findings.\n", verb, changes, len(ids))
			return nil
		default:
			return fmt.Errorf("unknown finding action %q", command)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(environment.Stdout, "Finding #%d: %s saved.\n", id, command)
		return nil
	}
	if *asJSON {
		filtered := []Finding{}
		for _, f := range findings {
			if (*includeAll || f.DismissedAt == nil) && (*verification == "all" || auditVerificationOutcome(f) == *verification) && findingHasTags(f, tags) {
				filtered = append(filtered, f)
			}
		}
		return writeInventoryJSON(environment.Stdout, filtered)
	}
	commandContext := environment.ExternalCommand
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	external := findingExternalCommands{
		snapshot: true,
		open: func(ctx context.Context, f Finding) (*exec.Cmd, error) {
			return buildAuditFindingOpenCommand(ctx, repository, f, commandContext)
		},
		preview: func(ctx context.Context, f Finding) (findingDiffPreview, error) {
			return loadAuditFindingPreview(ctx, repository, f)
		},
	}
	model := newFindingsModel(ctx, external, store, findings, auditFindingDisplay(findings), *includeAll, func() time.Time { return environmentNow(environment) })
	model.verificationFilter = *verification
	model.tagFilters = tags
	model.applyFilters(model.selectedID())
	model.loadDetail()
	model.prepareInitialPreview()
	return runTerminalFindingsModel(ctx, environment.Stdin, environment.Stdout, model)
}

func auditFindingDisplay(findings []Finding) map[int64]findingDisplayMetadata {
	display := map[int64]findingDisplayMetadata{}
	for _, f := range findings {
		metadata := findingDisplayMetadata{}
		if f.Attribution != nil && f.Attribution.Status == attributionStatusAttributed {
			metadata.Blame = f.Attribution.Author
			if f.Attribution.AuthoredAt != nil {
				metadata.CommitDate = *f.Attribution.AuthoredAt
			}
		}
		display[f.ID] = metadata
	}
	return display
}

type findingTagCount struct {
	Tag      string `json:"tag"`
	Findings int    `json:"findings"`
}

func runAuditTagsCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("tags", environment.Stderr)
	repo := flags.String("repo", ".", "scan checkout")
	scanSelector := flags.String("scan", "", "filter to a source scan; latest selects the newest completed original scan")
	includeAll := flags.Bool("all", false, "include dismissed findings")
	asJSON := flags.Bool("json", false, "output tag counts as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: repose tags [--scan ID|latest] [--all] [--json] [--repo DIR]")
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
		var scan auditScan
		if *scanSelector == "latest" {
			scan, err = reader.latestAuditForRecheck(ctx)
		} else {
			scan, err = reader.audit(ctx, *scanSelector)
		}
		if err != nil {
			return err
		}
		store.scanID = scan.ID
		if scan.Spec.Recheck != nil {
			store.scanID = scan.Spec.Recheck.SourceScanID
		}
	}
	findings, err := store.AllFindings(ctx)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, finding := range findings {
		if !*includeAll && finding.DismissedAt != nil {
			continue
		}
		for _, tag := range finding.Tags {
			counts[tag]++
		}
	}
	tags := make([]string, 0, len(counts))
	for tag := range counts {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	result := make([]findingTagCount, 0, len(tags))
	for _, tag := range tags {
		result = append(result, findingTagCount{Tag: tag, Findings: counts[tag]})
	}
	if *asJSON {
		return writeInventoryJSON(environment.Stdout, result)
	}
	if len(result) == 0 {
		fmt.Fprintln(environment.Stdout, "No finding tags in the selected scope.")
		return nil
	}
	for _, entry := range result {
		fmt.Fprintf(environment.Stdout, "%6d  %s\n", entry.Findings, entry.Tag)
	}
	return nil
}
