package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const reposeAuditSchemaSQL = `
CREATE TABLE audit_scans (
 id TEXT PRIMARY KEY, inventory_id TEXT NOT NULL REFERENCES inventories(id),
 created_at TEXT NOT NULL, status TEXT NOT NULL, control TEXT NOT NULL DEFAULT '', document TEXT NOT NULL
);
CREATE TABLE audit_tasks (
 scan_id TEXT NOT NULL REFERENCES audit_scans(id), id TEXT NOT NULL,
 ordinal INTEGER NOT NULL, status TEXT NOT NULL, attempt_id INTEGER,
 document TEXT NOT NULL, PRIMARY KEY(scan_id,id), UNIQUE(scan_id,ordinal)
);
CREATE TABLE audit_attempts (
 id INTEGER PRIMARY KEY, scan_id TEXT NOT NULL, task_id TEXT NOT NULL, number INTEGER NOT NULL,
 status TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT, document TEXT NOT NULL,
 FOREIGN KEY(scan_id,task_id) REFERENCES audit_tasks(scan_id,id), UNIQUE(scan_id,task_id,number)
);
CREATE INDEX audit_tasks_status ON audit_tasks(scan_id,status,ordinal);
CREATE TABLE audit_findings (
 id INTEGER PRIMARY KEY, scan_id TEXT NOT NULL REFERENCES audit_scans(id),
 task_id TEXT NOT NULL, attempt_id INTEGER NOT NULL REFERENCES audit_attempts(id),
 observed_sha TEXT NOT NULL, created_at TEXT NOT NULL, dismissed_at TEXT, dismiss_reason TEXT NOT NULL DEFAULT '',
 document TEXT NOT NULL, FOREIGN KEY(scan_id,task_id) REFERENCES audit_tasks(scan_id,id)
);
CREATE TABLE audit_finding_events (
 id INTEGER PRIMARY KEY, finding_id INTEGER NOT NULL REFERENCES audit_findings(id),
 action TEXT NOT NULL, note TEXT NOT NULL, created_at TEXT NOT NULL
);
PRAGMA user_version = 4;
`

func (s *inventoryStore) createAudit(ctx context.Context, record InventoryRecord, spec auditSpec, inputs []auditTaskInput, now time.Time) (auditScan, error) {
	if record.ReviewedAt == nil {
		return auditScan{}, fmt.Errorf("inventory %s must be approved before creating a scan", shortSHA(record.ID))
	}
	if len(inputs) == 0 || len(inputs) != len(spec.Plan.Assignments) || spec.Plan.InventoryID != record.ID {
		return auditScan{}, errors.New("invalid scan inputs")
	}
	recheckGroups, err := auditRecheckAssignmentInputs(spec)
	if err != nil {
		return auditScan{}, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return auditScan{}, err
	}
	id := hex.EncodeToString(nonce[:])
	document, err := json.Marshal(spec)
	if err != nil {
		return auditScan{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return auditScan{}, err
	}
	defer tx.Rollback()
	kind, recheckKey := "review", ""
	if spec.Recheck != nil {
		kind, recheckKey = "recheck", spec.Recheck.Key
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_scans(id,inventory_id,created_at,status,document,kind,recheck_key) VALUES(?,?,?,'pending',?,?,?)", id, record.ID, formatTime(now), string(document), kind, recheckKey); err != nil {
		return auditScan{}, err
	}
	for i, input := range inputs {
		if input.Assignment.ID != spec.Plan.Assignments[i].ID || input.Prompt == "" {
			return auditScan{}, errors.New("assignment inputs do not match frozen plan")
		}
		actual := auditRecheckInputs(input)
		if (actual != nil) != (spec.Recheck != nil) || (input.Recheck != nil && input.RecheckBatch != nil) {
			return auditScan{}, errors.New("recheck inputs do not match frozen findings")
		}
		if spec.Recheck != nil {
			if (spec.PromptVersion == auditRecheckPromptVersionV1) != (input.Recheck != nil) || len(actual) != len(recheckGroups[i]) {
				return auditScan{}, errors.New("recheck input protocol does not match frozen batch")
			}
			for j, f := range actual {
				if f.ID != recheckGroups[i][j].ID || f.TaskID != recheckGroups[i][j].TaskID {
					return auditScan{}, errors.New("recheck inputs do not match frozen batch")
				}
			}
		}
		data, err := json.Marshal(input)
		if err != nil {
			return auditScan{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_tasks(scan_id,id,ordinal,status,document) VALUES(?,?,?,'pending',?)", id, input.Assignment.ID, i+1, string(data)); err != nil {
			return auditScan{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return auditScan{}, err
	}
	return s.audit(ctx, id)
}

func (s *inventoryStore) audit(ctx context.Context, selector string) (auditScan, error) {
	if s.version < 4 {
		return auditScan{}, errors.New("no scans; use scan create")
	}
	query := "SELECT id,created_at,status,control,document FROM audit_scans"
	args := []any{}
	if selector == "latest" {
		query += " ORDER BY rowid DESC LIMIT 1"
	} else {
		if len(selector) < 4 || len(selector) > 32 || !isLowerHex(selector) {
			return auditScan{}, errors.New("scan ID must be a hexadecimal prefix of at least four characters, or latest")
		}
		query += " WHERE id LIKE ?"
		args = append(args, selector+"%")
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return auditScan{}, err
	}
	var scan auditScan
	found := false
	for rows.Next() {
		if found {
			rows.Close()
			return auditScan{}, errors.New("ambiguous scan ID")
		}
		found = true
		var created, document string
		if err = rows.Scan(&scan.ID, &created, &scan.Status, &scan.Control, &document); err != nil {
			rows.Close()
			return scan, err
		}
		scan.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err == nil {
			err = json.Unmarshal([]byte(document), &scan.Spec)
		}
		if err != nil {
			rows.Close()
			return scan, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return scan, err
	}
	if !found {
		return scan, errors.New("scan not found")
	}
	scan.Counts, err = s.auditCounts(ctx, scan.ID)
	if err == nil && scan.Status == "paused" {
		var limits []auditProviderLimit
		limits, err = s.auditPendingLimits(ctx, scan.ID)
		for i := range limits {
			if scan.Blocked == nil ||
				(scan.Blocked.Kind != auditQuotaExhausted && limits[i].Kind == auditQuotaExhausted) ||
				(scan.Blocked.Kind == auditRateLimited && limits[i].RetryAt != nil &&
					(scan.Blocked.RetryAt == nil || limits[i].RetryAt.After(*scan.Blocked.RetryAt))) {
				scan.Blocked = &limits[i]
			}
		}
	}
	if err == nil && scan.Spec.Recheck != nil {
		scan.Verdicts, err = s.auditRecheckCounts(ctx, scan.ID)
	}
	return scan, err
}

func (s *inventoryStore) audits(ctx context.Context) ([]auditScan, error) {
	if s.version < 4 {
		return []auditScan{}, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM audit_scans ORDER BY rowid DESC")
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
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
	scans := make([]auditScan, 0, len(ids))
	for _, id := range ids {
		scan, err := s.audit(ctx, id)
		if err != nil {
			return nil, err
		}
		scans = append(scans, scan)
	}
	return scans, nil
}

// Select by remaining work rather than status alone: incomplete scans can have
// only terminal unable-to-assess results, and crashed scans can still be running.
func (s *inventoryStore) resumableAuditIDs(ctx context.Context, retryFailed bool) ([]string, error) {
	ids := []string{}
	if s.version < 4 {
		return ids, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.id FROM audit_scans s
		WHERE s.status IN ('pending','paused','running','incomplete')
		AND EXISTS (SELECT 1 FROM audit_tasks t WHERE t.scan_id=s.id
			AND (t.status IN ('pending','running') OR (? AND t.status='failed')))
		ORDER BY s.rowid DESC`, retryFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *inventoryStore) auditPendingLimits(ctx context.Context, scanID string) ([]auditProviderLimit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.document FROM audit_tasks t
		JOIN audit_attempts a ON a.id=t.attempt_id
		WHERE t.scan_id=? AND t.status='pending' AND a.status IN ('quota_exhausted','rate_limited')
		ORDER BY a.id DESC`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var limits []auditProviderLimit
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var attempt auditAttempt
		if err := json.Unmarshal([]byte(data), &attempt); err != nil {
			return nil, err
		}
		if attempt.Invocation.ProviderLimit != nil {
			limits = append(limits, *attempt.Invocation.ProviderLimit)
		}
	}
	return limits, rows.Err()
}

func (s *inventoryStore) auditCounts(ctx context.Context, id string) (map[string]int, error) {
	counts := map[string]int{}
	rows, err := s.db.QueryContext(ctx, "SELECT status,count(*) FROM audit_tasks WHERE scan_id=? GROUP BY status", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

func (s *inventoryStore) auditTasks(ctx context.Context, id string) ([]auditTask, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,ordinal,status,coalesce(attempt_id,0),document FROM audit_tasks WHERE scan_id=? ORDER BY ordinal", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []auditTask{}
	for rows.Next() {
		var task auditTask
		var document string
		if err := rows.Scan(&task.ID, &task.Ordinal, &task.Status, &task.AttemptID, &document); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(document), &task.Input); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *inventoryStore) claimAuditTask(ctx context.Context, scanID string, timeout time.Duration, now time.Time) (*auditTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Acquire the writer lock before selecting a task. One coordinator owns the
	// process lock; this transaction also guards against future concurrent claims.
	if _, err = tx.ExecContext(ctx, "UPDATE audit_scans SET status=status WHERE id=?", scanID); err != nil {
		return nil, err
	}
	var task auditTask
	var document string
	err = tx.QueryRowContext(ctx, "SELECT id,ordinal,document FROM audit_tasks WHERE scan_id=? AND status='pending' ORDER BY ordinal LIMIT 1", scanID).Scan(&task.ID, &task.Ordinal, &document)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(document), &task.Input); err != nil {
		return nil, err
	}
	var number int
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(number),0)+1 FROM audit_attempts WHERE scan_id=? AND task_id=?", scanID, task.ID).Scan(&number); err != nil {
		return nil, err
	}
	attempt := auditAttempt{TaskID: task.ID, Number: number, Status: "running", StartedAt: now, Timeout: timeout}
	data, _ := json.Marshal(attempt)
	result, err := tx.ExecContext(ctx, "INSERT INTO audit_attempts(scan_id,task_id,number,status,started_at,document) VALUES(?,?,?,'running',?,?)", scanID, task.ID, number, formatTime(now), string(data))
	if err != nil {
		return nil, err
	}
	task.AttemptID, err = result.LastInsertId()
	if err != nil {
		return nil, err
	}
	task.Status = "running"
	if _, err = tx.ExecContext(ctx, "UPDATE audit_tasks SET status='running',attempt_id=? WHERE scan_id=? AND id=? AND status='pending'", task.AttemptID, scanID, task.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &task, nil
}

func (s *inventoryStore) finishAuditTask(ctx context.Context, scan auditScan, task auditTask, status string, invocation auditInvocation, output *auditOutput, runErr error, now time.Time) error {
	if status != "completed" && status != "unable_to_assess" && status != "failed" && status != "interrupted" && status != auditQuotaExhausted && status != auditRateLimited {
		return errors.New("invalid attempt status")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nextStatus := status
	if status == "interrupted" || status == auditQuotaExhausted || status == auditRateLimited {
		nextStatus = "pending"
	}
	result, err := tx.ExecContext(ctx, "UPDATE audit_tasks SET status=? WHERE scan_id=? AND id=? AND status='running' AND attempt_id=?", nextStatus, scan.ID, task.ID, task.AttemptID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errAuditStaleAttempt
	}
	var document string
	if err = tx.QueryRowContext(ctx, "SELECT document FROM audit_attempts WHERE id=?", task.AttemptID).Scan(&document); err != nil {
		return err
	}
	var attempt auditAttempt
	if err = json.Unmarshal([]byte(document), &attempt); err != nil {
		return err
	}
	attempt.ID, attempt.Status, attempt.FinishedAt = task.AttemptID, status, &now
	attempt.Invocation, attempt.Output = invocation, output
	attempt.DurationMilliseconds = max(0, now.Sub(attempt.StartedAt).Milliseconds())
	if runErr != nil {
		attempt.Error = runErr.Error()
	}
	data, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE audit_attempts SET status=?,finished_at=?,document=? WHERE id=? AND status='running'", status, formatTime(now), string(data), task.AttemptID); err != nil {
		return err
	}
	if output != nil && (output.Recheck != nil || output.RecheckBatch != nil) {
		if status != "completed" || len(auditRecheckInputs(task.Input)) == 0 || scan.Spec.Recheck == nil {
			return errors.New("recheck result does not match assignment")
		}
		results, findings := auditRecheckResults(*output), auditRecheckInputs(task.Input)
		if err := validateAuditRecheckBatch(results, findings); err != nil {
			return err
		}
		byID := map[int64]auditRecheckFinding{}
		for _, f := range findings {
			byID[f.ID] = f
		}
		for _, result := range results {
			if err := saveAuditRecheckResult(ctx, tx, scan, task, byID[result.FindingID], result, now); err != nil {
				return err
			}
		}
	} else if output != nil && (status == "completed" || status == "unable_to_assess") {
		for _, finding := range output.Findings {
			data, err := json.Marshal(finding)
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, "INSERT INTO audit_findings(scan_id,task_id,attempt_id,observed_sha,created_at,document) VALUES(?,?,?,?,?,?)", scan.ID, task.ID, task.AttemptID, scan.Spec.Plan.SnapshotSHA, formatTime(now), string(data))
			if err != nil {
				return err
			}
			id, err := result.LastInsertId()
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO audit_finding_events(finding_id,action,note,created_at) VALUES(?,'observed',?,?)", id, "Scan "+scan.ID+", assignment "+task.ID, formatTime(now)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *inventoryStore) recoverAudit(ctx context.Context, scan auditScan, retry bool, now time.Time) error {
	tasks, err := s.auditTasks(ctx, scan.ID)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.Status == "running" {
			if err = s.finishAuditTask(ctx, scan, task, "interrupted", auditInvocation{}, nil, errors.New("coordinator exited before publishing this attempt"), now); err != nil {
				return err
			}
		}
	}
	if retry {
		_, err = s.db.ExecContext(ctx, "UPDATE audit_tasks SET status='pending' WHERE scan_id=? AND status='failed'", scan.ID)
		if err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, "UPDATE audit_scans SET status='running',control='' WHERE id=?", scan.ID)
	return err
}

func (s *inventoryStore) auditAttempts(ctx context.Context, id string) ([]auditAttempt, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,document FROM audit_attempts WHERE scan_id=? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := []auditAttempt{}
	for rows.Next() {
		var id int64
		var data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var attempt auditAttempt
		if err := json.Unmarshal([]byte(data), &attempt); err != nil {
			return nil, err
		}
		attempt.ID = id
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}
