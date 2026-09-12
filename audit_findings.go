package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

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
	return findings, nil
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
	start, end := max(0, target-20), min(len(all), target+21)
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
			return errors.New("usage: repose finding <show|dismiss|reopen|note> ID")
		}
		command = args[1]
		offset = 2
	}
	flags := newFlagSet("findings", environment.Stderr)
	repo := flags.String("repo", ".", "scan checkout")
	asJSON := flags.Bool("json", false, "output findings JSON")
	scanSelector := flags.String("scan", "", "filter to a scan")
	includeAll := flags.Bool("all", false, "include dismissed findings")
	verification := flags.String("verification", "all", "filter latest verdict: all, unchecked, confirmed, false_positive, uncertain")
	reason := flags.String("reason", "", "dismissal reason or note text")
	if err := flags.Parse(args[offset:]); err != nil {
		return err
	}
	if !validAuditVerificationFilter(*verification) {
		return errors.New("verification must be all, unchecked, confirmed, false_positive, or uncertain")
	}
	if !singular && flags.NArg() != 0 || singular && flags.NArg() != 1 {
		return errors.New("findings takes no positional arguments; finding actions require one ID")
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
	}
	findings, err := store.AllFindings(ctx)
	if err != nil {
		return err
	}
	if singular {
		id, err := parseFindingID(flags.Arg(0))
		if err != nil {
			return err
		}
		var found *Finding
		for i := range findings {
			if findings[i].ID == id {
				found = &findings[i]
				break
			}
		}
		if found == nil {
			return errors.New("finding not found in selected scope")
		}
		switch command {
		case "show":
			return writeInventoryJSON(environment.Stdout, found)
		case "dismiss":
			err = store.DismissFinding(ctx, id, *reason, environmentNow(environment))
		case "reopen":
			err = store.ReopenFinding(ctx, id, environmentNow(environment))
		case "note":
			err = store.AddFindingNote(ctx, id, *reason, environmentNow(environment))
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
			if (*includeAll || f.DismissedAt == nil) && (*verification == "all" || auditVerificationOutcome(f) == *verification) {
				filtered = append(filtered, f)
			}
		}
		return writeInventoryJSON(environment.Stdout, filtered)
	}
	external := findingExternalCommands{snapshot: true, preview: func(ctx context.Context, f Finding) (findingDiffPreview, error) {
		return loadAuditFindingPreview(ctx, repository, f)
	}}
	model := newFindingsModel(ctx, external, store, findings, auditFindingDisplay(findings), *includeAll, func() time.Time { return environmentNow(environment) })
	model.verificationFilter = *verification
	model.applyFilters(model.selectedID())
	model.loadDetail()
	model.prepareInitialPreview()
	return runTerminalFindingsModel(ctx, environment.Stdin, environment.Stdout, model)
}

func auditFindingDisplay(findings []Finding) map[int64]findingDisplayMetadata {
	display := map[int64]findingDisplayMetadata{}
	for _, f := range findings {
		metadata := findingDisplayMetadata{Blame: shortSHA(f.ScanID)}
		if f.ObservedAt != nil {
			metadata.CommitDate = *f.ObservedAt
		}
		display[f.ID] = metadata
	}
	return display
}
