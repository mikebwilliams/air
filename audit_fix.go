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

const reposeFixSchemaSQL = `
CREATE TABLE audit_fixes (
    id INTEGER PRIMARY KEY,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    status TEXT NOT NULL,
    attempt_id INTEGER
);
CREATE TABLE audit_fix_findings (
    fix_id INTEGER NOT NULL REFERENCES audit_fixes(id) ON DELETE CASCADE,
    finding_id INTEGER NOT NULL REFERENCES audit_findings(id),
    ordinal INTEGER NOT NULL,
    PRIMARY KEY (fix_id, finding_id),
    UNIQUE (fix_id, ordinal)
);
CREATE TABLE audit_fix_attempts (
    id INTEGER PRIMARY KEY,
    fix_id INTEGER NOT NULL REFERENCES audit_fixes(id) ON DELETE CASCADE,
    number INTEGER NOT NULL,
    status TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    document TEXT NOT NULL,
    UNIQUE (fix_id, number)
);
CREATE INDEX audit_fixes_by_status ON audit_fixes(status, id);
CREATE INDEX audit_fix_findings_by_finding ON audit_fix_findings(finding_id, fix_id);
PRAGMA user_version = 11;
`

type auditFix struct {
	ID        int64             `json:"id"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Status    string            `json:"status"`
	AttemptID int64             `json:"attempt_id,omitempty"`
	Findings  []Finding         `json:"findings"`
	Attempts  []auditFixAttempt `json:"attempts,omitempty"`
}

type auditFixOutput struct {
	Status  string   `json:"status"`
	Summary string   `json:"summary"`
	Tests   []string `json:"tests"`
}

type auditFixAttempt struct {
	ID                   int64            `json:"id"`
	Number               int              `json:"number"`
	Status               string           `json:"status"`
	StartedAt            time.Time        `json:"started_at"`
	FinishedAt           *time.Time       `json:"finished_at,omitempty"`
	DurationMilliseconds int64            `json:"duration_ms"`
	WorkTree             string           `json:"worktree"`
	BaseSHA              string           `json:"base_sha"`
	Model                auditModelConfig `json:"model"`
	PromptVersion        string           `json:"prompt_version"`
	Prompt               string           `json:"prompt"`
	Error                string           `json:"error,omitempty"`
	Invocation           auditInvocation  `json:"invocation"`
	Output               *auditFixOutput  `json:"output,omitempty"`
}

func normalizeFixFindingIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, errors.New("at least one finding ID is required")
	}
	result := append([]int64(nil), ids...)
	seen := make(map[int64]bool, len(result))
	for _, id := range result {
		if id <= 0 {
			return nil, fmt.Errorf("invalid finding ID %d", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("finding #%d was supplied more than once", id)
		}
		seen[id] = true
	}
	return result, nil
}

func validateFixFindings(ctx context.Context, tx *sql.Tx, ids []int64, scanID string) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	query := "SELECT id FROM audit_findings WHERE id IN (" + placeholders + ")"
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	if scanID != "" {
		query += " AND scan_id=?"
		args = append(args, scanID)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	found := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		found[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !found[id] {
			if scanID != "" {
				return fmt.Errorf("finding #%d not found in selected scan", id)
			}
			return fmt.Errorf("finding #%d not found", id)
		}
	}
	return nil
}

func (s *inventoryStore) createAuditFix(ctx context.Context, ids []int64, scanID string, now time.Time) (auditFix, error) {
	ids, err := normalizeFixFindingIDs(ids)
	if err != nil {
		return auditFix{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return auditFix{}, err
	}
	defer tx.Rollback()
	if err := validateFixFindings(ctx, tx, ids, scanID); err != nil {
		return auditFix{}, err
	}
	stamp := formatTime(now)
	result, err := tx.ExecContext(ctx, "INSERT INTO audit_fixes(created_at,updated_at,status) VALUES(?,?,'pending')", stamp, stamp)
	if err != nil {
		return auditFix{}, err
	}
	fixID, err := result.LastInsertId()
	if err != nil {
		return auditFix{}, err
	}
	for index, findingID := range ids {
		if _, err := tx.ExecContext(ctx, "INSERT INTO audit_fix_findings(fix_id,finding_id,ordinal) VALUES(?,?,?)", fixID, findingID, index+1); err != nil {
			return auditFix{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return auditFix{}, err
	}
	return s.auditFix(ctx, fixID)
}

func (s *inventoryStore) addToLatestPendingAuditFix(ctx context.Context, findingID int64, scanID string, now time.Time) (auditFix, bool, error) {
	if findingID <= 0 {
		return auditFix{}, false, errors.New("invalid finding ID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return auditFix{}, false, err
	}
	defer tx.Rollback()
	if err := validateFixFindings(ctx, tx, []int64{findingID}, scanID); err != nil {
		return auditFix{}, false, err
	}
	var fixID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM audit_fixes WHERE status='pending' ORDER BY id DESC LIMIT 1").Scan(&fixID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auditFix{}, false, errors.New("no pending fix; press f to create one")
		}
		return auditFix{}, false, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM audit_fix_findings WHERE fix_id=? AND finding_id=?", fixID, findingID).Scan(&exists); err != nil {
		return auditFix{}, false, err
	}
	added := exists == 0
	if added {
		var ordinal int
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(ordinal),0)+1 FROM audit_fix_findings WHERE fix_id=?", fixID).Scan(&ordinal); err != nil {
			return auditFix{}, false, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO audit_fix_findings(fix_id,finding_id,ordinal) VALUES(?,?,?)", fixID, findingID, ordinal); err != nil {
			return auditFix{}, false, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE audit_fixes SET updated_at=? WHERE id=?", formatTime(now), fixID); err != nil {
			return auditFix{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return auditFix{}, false, err
	}
	fix, err := s.auditFix(ctx, fixID)
	return fix, added, err
}

func (s *inventoryStore) auditFixes(ctx context.Context) ([]auditFix, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM audit_fixes ORDER BY id")
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	fixes := make([]auditFix, 0, len(ids))
	for _, id := range ids {
		fix, err := s.auditFix(ctx, id)
		if err != nil {
			return nil, err
		}
		fixes = append(fixes, fix)
	}
	return fixes, nil
}

func (s *inventoryStore) auditFix(ctx context.Context, id int64) (auditFix, error) {
	var fix auditFix
	var created, updated string
	err := s.db.QueryRowContext(ctx, "SELECT id,created_at,updated_at,status,coalesce(attempt_id,0) FROM audit_fixes WHERE id=?", id).
		Scan(&fix.ID, &created, &updated, &fix.Status, &fix.AttemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return fix, fmt.Errorf("fix #%d not found", id)
	}
	if err != nil {
		return fix, err
	}
	if fix.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return fix, err
	}
	if fix.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return fix, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT f.id,f.scan_id,f.task_id,f.attempt_id,f.observed_sha,f.created_at,
		f.dismissed_at,f.dismiss_reason,f.document FROM audit_fix_findings q
		JOIN audit_findings f ON f.id=q.finding_id WHERE q.fix_id=? ORDER BY q.ordinal`, id)
	if err != nil {
		return fix, err
	}
	for rows.Next() {
		finding, err := scanAuditFinding(rows)
		if err != nil {
			rows.Close()
			return fix, err
		}
		fix.Findings = append(fix.Findings, finding)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fix, err
	}
	backend := &auditFindingStore{reader: s}
	if err := backend.loadVerifications(ctx, fix.Findings); err != nil {
		return fix, err
	}
	if err := backend.loadTags(ctx, fix.Findings); err != nil {
		return fix, err
	}
	if err := backend.loadAttributions(ctx, fix.Findings); err != nil {
		return fix, err
	}
	fix.Attempts, err = s.auditFixAttempts(ctx, id)
	return fix, err
}

func scanAuditFinding(row rowScanner) (Finding, error) {
	var finding Finding
	var created, document string
	var dismissed sql.NullString
	if err := row.Scan(&finding.ID, &finding.ScanID, &finding.TaskID, &finding.AttemptID, &finding.ObservedSHA,
		&created, &dismissed, &finding.DismissReason, &document); err != nil {
		return finding, err
	}
	var value NewFinding
	if err := json.Unmarshal([]byte(document), &value); err != nil {
		return finding, err
	}
	finding.Severity, finding.Title, finding.Description = value.Severity, value.Title, value.Description
	finding.File, finding.Line, finding.Symbol = value.File, value.Line, value.Symbol
	observed, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return finding, err
	}
	finding.ObservedAt = &observed
	if dismissed.Valid {
		when, err := time.Parse(time.RFC3339Nano, dismissed.String)
		if err != nil {
			return finding, err
		}
		finding.DismissedAt = &when
	}
	return finding, nil
}

func (s *inventoryStore) auditFixAttempts(ctx context.Context, fixID int64) ([]auditFixAttempt, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,number,status,started_at,finished_at,document FROM audit_fix_attempts WHERE fix_id=? ORDER BY number", fixID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []auditFixAttempt
	for rows.Next() {
		var attempt auditFixAttempt
		var id int64
		var number int
		var status string
		var started, document string
		var finished sql.NullString
		if err := rows.Scan(&id, &number, &status, &started, &finished, &document); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(document), &attempt); err != nil {
			return nil, err
		}
		attempt.ID, attempt.Number, attempt.Status = id, number, status
		attempt.StartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return nil, err
		}
		if finished.Valid {
			when, err := time.Parse(time.RFC3339Nano, finished.String)
			if err != nil {
				return nil, err
			}
			attempt.FinishedAt = &when
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *inventoryStore) deleteAuditFix(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM audit_fixes WHERE id=? AND status='pending'", id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 1 {
		return nil
	}
	var status string
	if err := s.db.QueryRowContext(ctx, "SELECT status FROM audit_fixes WHERE id=?", id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("fix #%d not found", id)
	} else if err != nil {
		return err
	}
	return fmt.Errorf("fix #%d is %s; only pending fixes can be deleted", id, status)
}

func (s *inventoryStore) recoverAuditFixes(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := formatTime(now)
	if _, err := tx.ExecContext(ctx, `UPDATE audit_fix_attempts SET status='interrupted',finished_at=?
		WHERE status='running' AND id IN (SELECT attempt_id FROM audit_fixes WHERE status='running')`, stamp); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE audit_fixes SET status='pending',updated_at=? WHERE status='running'", stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *inventoryStore) claimAuditFix(ctx context.Context, retryFailed bool, worktree, baseSHA string, config auditModelConfig, now time.Time) (*auditFix, *auditFixAttempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var fixID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM audit_fixes
		WHERE status='pending' OR (? AND status='failed') ORDER BY id LIMIT 1`, retryFailed).Scan(&fixID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var number int
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(number),0)+1 FROM audit_fix_attempts WHERE fix_id=?", fixID).Scan(&number); err != nil {
		return nil, nil, err
	}
	attempt := auditFixAttempt{Number: number, Status: "running", StartedAt: now, WorkTree: worktree, BaseSHA: baseSHA, Model: config, PromptVersion: auditFixPromptVersion}
	document, err := json.Marshal(attempt)
	if err != nil {
		return nil, nil, err
	}
	result, err := tx.ExecContext(ctx, "INSERT INTO audit_fix_attempts(fix_id,number,status,started_at,document) VALUES(?,?,'running',?,?)",
		fixID, number, formatTime(now), document)
	if err != nil {
		return nil, nil, err
	}
	attempt.ID, err = result.LastInsertId()
	if err != nil {
		return nil, nil, err
	}
	result, err = tx.ExecContext(ctx, "UPDATE audit_fixes SET status='running',attempt_id=?,updated_at=? WHERE id=? AND (status='pending' OR (? AND status='failed'))",
		attempt.ID, formatTime(now), fixID, retryFailed)
	if err != nil {
		return nil, nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		if err == nil {
			err = errors.New("fix queue changed while claiming work")
		}
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	fix, err := s.auditFix(ctx, fixID)
	if err != nil {
		return nil, nil, err
	}
	return &fix, &attempt, nil
}

func (s *inventoryStore) finishAuditFix(ctx context.Context, fix auditFix, attempt auditFixAttempt, status string, invocation auditInvocation, output *auditFixOutput, runErr error, now time.Time) error {
	switch status {
	case "completed", "unable_to_fix", "failed", "interrupted":
	default:
		return fmt.Errorf("invalid fix attempt status %q", status)
	}
	attempt.Status, attempt.Invocation, attempt.Output = status, invocation, output
	attempt.FinishedAt = &now
	attempt.DurationMilliseconds = now.Sub(attempt.StartedAt).Milliseconds()
	if runErr != nil {
		attempt.Error = runErr.Error()
	}
	document, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	fixStatus := status
	if status == "interrupted" {
		fixStatus = "pending"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE audit_fix_attempts SET status=?,finished_at=?,document=? WHERE id=? AND fix_id=? AND status='running'",
		status, formatTime(now), document, attempt.ID, fix.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		if err == nil {
			err = errors.New("fix attempt no longer owns queue entry")
		}
		return err
	}
	result, err = tx.ExecContext(ctx, "UPDATE audit_fixes SET status=?,updated_at=? WHERE id=? AND status='running' AND attempt_id=?",
		fixStatus, formatTime(now), fix.ID, attempt.ID)
	if err != nil {
		return err
	}
	changed, err = result.RowsAffected()
	if err != nil || changed != 1 {
		if err == nil {
			err = errors.New("fix attempt no longer owns queue entry")
		}
		return err
	}
	if status == "completed" && output != nil {
		note := fmt.Sprintf("Fix #%d: %s", fix.ID, output.Summary)
		for _, finding := range fix.Findings {
			if _, err := tx.ExecContext(ctx, "INSERT INTO audit_finding_events(finding_id,action,note,created_at) VALUES(?,'fix_completed',?,?)",
				finding.ID, note, formatTime(now)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *inventoryStore) saveAuditFixAttemptPrompt(ctx context.Context, fixID int64, attempt *auditFixAttempt, prompt string) error {
	attempt.Prompt = prompt
	document, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, "UPDATE audit_fix_attempts SET document=? WHERE id=? AND fix_id=? AND status='running'", document, attempt.ID, fixID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("fix attempt no longer owns queue entry")
	}
	return nil
}

func (s *auditFindingStore) CreateFix(ctx context.Context, ids []int64, now time.Time) (auditFix, error) {
	writer, err := openInventoryStore(ctx, s.databasePath, false)
	if err != nil {
		return auditFix{}, err
	}
	defer writer.Close()
	return writer.createAuditFix(ctx, ids, s.scanID, now)
}

func (s *auditFindingStore) AddFindingToLatestFix(ctx context.Context, id int64, now time.Time) (auditFix, bool, error) {
	writer, err := openInventoryStore(ctx, s.databasePath, false)
	if err != nil {
		return auditFix{}, false, err
	}
	defer writer.Close()
	return writer.addToLatestPendingAuditFix(ctx, id, s.scanID, now)
}
