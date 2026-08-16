package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

const schemaVersion = 3

const schemaSQL = `
CREATE TABLE config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE models (
    name                              TEXT PRIMARY KEY,
    pricing_status                    TEXT NOT NULL CHECK(pricing_status IN ('known', 'unknown')),
    service_tier                      TEXT,
    pricing_source                    TEXT,
    pricing_as_of                     TEXT,
    long_context_input_tokens         INTEGER,
    short_input_nanousd_per_token     INTEGER,
    short_cached_nanousd_per_token    INTEGER,
    short_cache_write_nanousd_per_token INTEGER,
    short_output_nanousd_per_token    INTEGER,
    long_input_nanousd_per_token      INTEGER,
    long_cached_nanousd_per_token     INTEGER,
    long_cache_write_nanousd_per_token INTEGER,
    long_output_nanousd_per_token     INTEGER,
    CHECK(
        (pricing_status = 'known' AND service_tier IS NOT NULL AND
         pricing_source IS NOT NULL AND pricing_as_of IS NOT NULL AND
         long_context_input_tokens IS NOT NULL AND
         short_input_nanousd_per_token IS NOT NULL AND
         short_cached_nanousd_per_token IS NOT NULL AND
         short_cache_write_nanousd_per_token IS NOT NULL AND
         short_output_nanousd_per_token IS NOT NULL AND
         long_input_nanousd_per_token IS NOT NULL AND
         long_cached_nanousd_per_token IS NOT NULL AND
         long_cache_write_nanousd_per_token IS NOT NULL AND
         long_output_nanousd_per_token IS NOT NULL) OR
        (pricing_status = 'unknown' AND service_tier IS NULL AND
         pricing_source IS NULL AND pricing_as_of IS NULL AND
         long_context_input_tokens IS NULL AND
         short_input_nanousd_per_token IS NULL AND
         short_cached_nanousd_per_token IS NULL AND
         short_cache_write_nanousd_per_token IS NULL AND
         short_output_nanousd_per_token IS NULL AND
         long_input_nanousd_per_token IS NULL AND
         long_cached_nanousd_per_token IS NULL AND
         long_cache_write_nanousd_per_token IS NULL AND
         long_output_nanousd_per_token IS NULL)
    )
);

CREATE TABLE commits (
    sha             TEXT PRIMARY KEY,
    parent_sha      TEXT,
    processed_at    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK(status IN ('reviewed', 'skipped')),
    skip_reason     TEXT,
    model           TEXT,
    reasoning_effort TEXT,
    prompt_version  TEXT,
    summary         TEXT,
    raw_response    TEXT,
	input_tokens            INTEGER CHECK(input_tokens IS NULL OR input_tokens >= 0),
	cached_input_tokens     INTEGER CHECK(cached_input_tokens IS NULL OR cached_input_tokens >= 0),
	cache_write_tokens      INTEGER CHECK(cache_write_tokens IS NULL OR cache_write_tokens >= 0),
	output_tokens           INTEGER CHECK(output_tokens IS NULL OR output_tokens >= 0),
	reasoning_output_tokens INTEGER CHECK(reasoning_output_tokens IS NULL OR reasoning_output_tokens >= 0),
	estimated_cost_microusd INTEGER CHECK(estimated_cost_microusd IS NULL OR estimated_cost_microusd >= 0),
	estimated_cost_max_microusd INTEGER CHECK(estimated_cost_max_microusd IS NULL OR estimated_cost_max_microusd >= 0),
	cost_context            TEXT CHECK(cost_context IS NULL OR cost_context IN ('short', 'long')),
	cost_complete           INTEGER CHECK(cost_complete IS NULL OR cost_complete IN (0, 1)),
	FOREIGN KEY(model) REFERENCES models(name),
    CHECK(
        (status = 'reviewed' AND skip_reason IS NULL) OR
        (status = 'skipped' AND skip_reason IS NOT NULL)
	),
	CHECK(
		(status = 'reviewed' AND input_tokens IS NOT NULL AND
		 cached_input_tokens IS NOT NULL AND output_tokens IS NOT NULL AND
		 reasoning_output_tokens IS NOT NULL) OR
		(status = 'skipped' AND input_tokens IS NULL AND
		 cached_input_tokens IS NULL AND cache_write_tokens IS NULL AND output_tokens IS NULL AND
		 reasoning_output_tokens IS NULL AND estimated_cost_microusd IS NULL)
	),
	CHECK(cached_input_tokens IS NULL OR cached_input_tokens <= input_tokens),
	CHECK(cache_write_tokens IS NULL OR cached_input_tokens + cache_write_tokens <= input_tokens),
	CHECK(reasoning_output_tokens IS NULL OR reasoning_output_tokens <= output_tokens),
	CHECK(
		(estimated_cost_microusd IS NULL AND estimated_cost_max_microusd IS NULL AND
		 cost_context IS NULL AND cost_complete IS NULL) OR
		(estimated_cost_microusd IS NOT NULL AND estimated_cost_max_microusd IS NOT NULL AND
		 estimated_cost_microusd <= estimated_cost_max_microusd AND
		 cost_context IS NOT NULL AND cost_complete IS NOT NULL)
	)
);

CREATE TABLE review_attempts (
	id                          INTEGER PRIMARY KEY,
	commit_sha                  TEXT NOT NULL,
	reviewed_at                 TEXT NOT NULL,
	model                       TEXT NOT NULL,
	reasoning_effort            TEXT,
	prompt_version              TEXT NOT NULL,
	summary                     TEXT NOT NULL,
	raw_response                TEXT NOT NULL,
	input_tokens                INTEGER NOT NULL CHECK(input_tokens >= 0),
	cached_input_tokens         INTEGER NOT NULL CHECK(cached_input_tokens >= 0),
	cache_write_tokens          INTEGER CHECK(cache_write_tokens IS NULL OR cache_write_tokens >= 0),
	output_tokens               INTEGER NOT NULL CHECK(output_tokens >= 0),
	reasoning_output_tokens     INTEGER NOT NULL CHECK(reasoning_output_tokens >= 0),
	estimated_cost_microusd     INTEGER CHECK(estimated_cost_microusd IS NULL OR estimated_cost_microusd >= 0),
	estimated_cost_max_microusd INTEGER CHECK(estimated_cost_max_microusd IS NULL OR estimated_cost_max_microusd >= 0),
	cost_context                TEXT CHECK(cost_context IS NULL OR cost_context IN ('short', 'long')),
	cost_complete               INTEGER CHECK(cost_complete IS NULL OR cost_complete IN (0, 1)),
	new_count                   INTEGER NOT NULL DEFAULT 0 CHECK(new_count >= 0),
	resolved_count              INTEGER NOT NULL DEFAULT 0 CHECK(resolved_count >= 0),

	FOREIGN KEY(commit_sha) REFERENCES commits(sha) ON DELETE CASCADE,
	FOREIGN KEY(model) REFERENCES models(name),
	CHECK(cached_input_tokens <= input_tokens),
	CHECK(cache_write_tokens IS NULL OR cached_input_tokens + cache_write_tokens <= input_tokens),
	CHECK(reasoning_output_tokens <= output_tokens),
	CHECK(
		(estimated_cost_microusd IS NULL AND estimated_cost_max_microusd IS NULL AND
		 cost_context IS NULL AND cost_complete IS NULL) OR
		(estimated_cost_microusd IS NOT NULL AND estimated_cost_max_microusd IS NOT NULL AND
		 estimated_cost_microusd <= estimated_cost_max_microusd AND
		 cost_context IS NOT NULL AND cost_complete IS NOT NULL)
	)
);

CREATE TABLE findings (
    id              INTEGER PRIMARY KEY,
    introduced_sha  TEXT NOT NULL,
	introduced_review_id INTEGER NOT NULL,
    resolved_sha    TEXT,
	dismissed_at      TEXT,
	dismiss_reason    TEXT,
    severity        TEXT NOT NULL CHECK(severity IN ('info', 'warning', 'error')),
    title           TEXT NOT NULL,
    description     TEXT NOT NULL,
    file            TEXT,
    line            INTEGER CHECK(line IS NULL OR line > 0),
    symbol          TEXT,

    FOREIGN KEY(introduced_sha) REFERENCES commits(sha) ON DELETE CASCADE,
	FOREIGN KEY(introduced_review_id) REFERENCES review_attempts(id) ON DELETE CASCADE,
	FOREIGN KEY(resolved_sha) REFERENCES commits(sha) ON DELETE SET NULL,
	CHECK((dismissed_at IS NULL AND dismiss_reason IS NULL) OR
	      (dismissed_at IS NOT NULL AND dismiss_reason IS NOT NULL))
);

CREATE TABLE finding_events (
    id          INTEGER PRIMARY KEY,
    finding_id  INTEGER NOT NULL,
	review_id   INTEGER,
	sha         TEXT,
	action      TEXT NOT NULL CHECK(action IN ('opened', 'resolved', 'reopened', 'updated', 'dismissed', 'noted')),
    note        TEXT,
	created_at  TEXT NOT NULL,

    FOREIGN KEY(finding_id) REFERENCES findings(id) ON DELETE CASCADE,
	FOREIGN KEY(review_id) REFERENCES review_attempts(id) ON DELETE CASCADE,
    FOREIGN KEY(sha) REFERENCES commits(sha) ON DELETE CASCADE
);

CREATE INDEX findings_open_idx ON findings(resolved_sha);
CREATE INDEX finding_events_finding_idx ON finding_events(finding_id, id);
CREATE INDEX review_attempts_commit_idx ON review_attempts(commit_sha, id);
CREATE INDEX finding_events_review_idx ON finding_events(review_id, id);
PRAGMA user_version = 3;
`

func CreateStore(ctx context.Context, databasePath, startSHA string) (*Store, error) {
	databaseDirectory := filepath.Dir(databasePath)
	if err := os.Mkdir(databaseDirectory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create AIR state directory: %w", err)
	}
	file, err := os.OpenFile(databasePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("AIR is already initialized at %s", databasePath)
		}
		return nil, fmt.Errorf("create database: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(databasePath)
		return nil, fmt.Errorf("create database: %w", err)
	}

	store, err := openStoreFile(ctx, databasePath)
	if err != nil {
		_ = os.Remove(databasePath)
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = store.Close()
			_ = os.Remove(databasePath)
		}
	}()

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("initialize schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO config(key, value) VALUES
        ('start_sha', ?), ('prompt_version', ?)`, startSHA, promptVersion); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("initialize configuration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	cleanup = false
	return store, nil
}

func OpenStore(ctx context.Context, databasePath string) (*Store, error) {
	info, err := os.Stat(databasePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("AIR is not initialized; run air init <commit-ish>")
		}
		return nil, fmt.Errorf("open database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database path is not a regular file: %s", databasePath)
	}
	store, err := openStoreFile(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	var version int
	if err := store.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	if version != schemaVersion {
		_ = store.Close()
		return nil, fmt.Errorf("unsupported AIR schema version %d", version)
	}
	return store, nil
}

func openStoreFile(ctx context.Context, databasePath string) (*Store, error) {
	u := &url.URL{Scheme: "file", Path: databasePath}
	query := u.Query()
	query.Set("_foreign_keys", "on")
	query.Set("_busy_timeout", "5000")
	query.Set("_journal_mode", "WAL")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable SQLite foreign keys: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Config(ctx context.Context, key string) (string, error) {
	value, found, err := s.ConfigValue(ctx, key)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("missing configuration key %q", key)
	}
	return value, nil
}

func (s *Store) ConfigValue(ctx context.Context, key string) (string, bool, error) {
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM config WHERE key = ?`, key).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read configuration %q: %w", key, err)
	}
	return value, true, nil
}

func (s *Store) SetConfig(ctx context.Context, key, value string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("configuration key must not be empty")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO config(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("write configuration %q: %w", key, err)
	}
	return nil
}

func (s *Store) UnsetConfig(ctx context.Context, key string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM config WHERE key = ?`, key)
	if err != nil {
		return false, fmt.Errorf("remove configuration %q: %w", key, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count removed configuration %q: %w", key, err)
	}
	return count != 0, nil
}

func (s *Store) ProcessedSHAs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sha FROM commits`)
	if err != nil {
		return nil, fmt.Errorf("list processed commits: %w", err)
	}
	defer rows.Close()
	result := make(map[string]struct{})
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, fmt.Errorf("list processed commits: %w", err)
		}
		result[sha] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list processed commits: %w", err)
	}
	return result, nil
}

func (s *Store) CommitSHAs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sha FROM commits ORDER BY sha`)
	if err != nil {
		return nil, fmt.Errorf("list stored commits: %w", err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, fmt.Errorf("list stored commits: %w", err)
		}
		result = append(result, sha)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stored commits: %w", err)
	}
	return result, nil
}

func (s *Store) DeleteCommits(ctx context.Context, shas []string) (int, error) {
	if len(shas) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("delete stale commits: %w", err)
	}
	defer tx.Rollback()
	deleted := 0
	for _, sha := range shas {
		result, err := tx.ExecContext(ctx, `DELETE FROM commits WHERE sha = ?`, sha)
		if err != nil {
			return 0, fmt.Errorf("delete stale commit %s: %w", shortSHA(sha), err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count deleted commit %s: %w", shortSHA(sha), err)
		}
		deleted += int(count)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("delete stale commits: %w", err)
	}
	return deleted, nil
}

func (s *Store) OpenFindings(ctx context.Context) ([]Finding, error) {
	return s.queryFindings(ctx, `
		SELECT id, introduced_sha, resolved_sha, dismissed_at, dismiss_reason,
		       severity, title, description, file, line, symbol
		FROM findings WHERE resolved_sha IS NULL AND dismissed_at IS NULL ORDER BY id`)
}

func (s *Store) OpenFindingsExcludingCommit(ctx context.Context, sha string) ([]Finding, error) {
	return s.queryFindings(ctx, `
		SELECT id, introduced_sha, resolved_sha, dismissed_at, dismiss_reason,
		       severity, title, description, file, line, symbol
		FROM findings
		WHERE resolved_sha IS NULL AND dismissed_at IS NULL AND introduced_sha <> ?
		ORDER BY id`, sha)
}

func (s *Store) Finding(ctx context.Context, id int64) (Finding, error) {
	rows, err := s.queryFindings(ctx, `
		SELECT id, introduced_sha, resolved_sha, dismissed_at, dismiss_reason,
		       severity, title, description, file, line, symbol
        FROM findings WHERE id = ?`, id)
	if err != nil {
		return Finding{}, err
	}
	if len(rows) == 0 {
		return Finding{}, fmt.Errorf("finding #%d does not exist", id)
	}
	return rows[0], nil
}

func (s *Store) FindingsIntroducedBy(ctx context.Context, sha string) ([]Finding, error) {
	return s.queryFindings(ctx, `
		SELECT id, introduced_sha, resolved_sha, dismissed_at, dismiss_reason,
		       severity, title, description, file, line, symbol
		FROM findings
		WHERE introduced_review_id = (
			SELECT id FROM review_attempts WHERE commit_sha = ? ORDER BY id DESC LIMIT 1
		)
		ORDER BY id`, sha)
}

func (s *Store) FindingsResolvedBy(ctx context.Context, sha string) ([]Finding, error) {
	return s.queryFindings(ctx, `
		SELECT f.id, f.introduced_sha, f.resolved_sha, f.dismissed_at, f.dismiss_reason,
		       f.severity, f.title,
		       f.description, f.file, f.line, f.symbol
		FROM findings f
		JOIN finding_events e ON e.finding_id = f.id
		WHERE e.action = 'resolved' AND e.review_id = (
			SELECT id FROM review_attempts WHERE commit_sha = ? ORDER BY id DESC LIMIT 1
		)
		ORDER BY f.id`, sha)
}

func (s *Store) FindingsIntroducedByReview(ctx context.Context, reviewID int64) ([]Finding, error) {
	return s.queryFindings(ctx, `
		SELECT id, introduced_sha, resolved_sha, dismissed_at, dismiss_reason,
		       severity, title, description, file, line, symbol
		FROM findings WHERE introduced_review_id = ? ORDER BY id`, reviewID)
}

func (s *Store) FindingsResolvedByReview(ctx context.Context, reviewID int64) ([]Finding, error) {
	return s.queryFindings(ctx, `
		SELECT f.id, f.introduced_sha, f.resolved_sha, f.dismissed_at, f.dismiss_reason,
		       f.severity, f.title,
		       f.description, f.file, f.line, f.symbol
		FROM findings f
		JOIN finding_events e ON e.finding_id = f.id
		WHERE e.action = 'resolved' AND e.review_id = ?
		ORDER BY f.id`, reviewID)
}

func (s *Store) queryFindings(ctx context.Context, query string, args ...any) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query findings: %w", err)
	}
	defer rows.Close()
	var findings []Finding
	for rows.Next() {
		var finding Finding
		var resolved, dismissedAt, dismissReason, file, symbol sql.NullString
		var line sql.NullInt64
		if err := rows.Scan(
			&finding.ID,
			&finding.IntroducedSHA,
			&resolved,
			&dismissedAt,
			&dismissReason,
			&finding.Severity,
			&finding.Title,
			&finding.Description,
			&file,
			&line,
			&symbol,
		); err != nil {
			return nil, fmt.Errorf("query findings: %w", err)
		}
		if resolved.Valid {
			finding.ResolvedSHA = stringPointer(resolved.String)
		}
		if dismissedAt.Valid {
			parsed, err := time.Parse(time.RFC3339Nano, dismissedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse finding dismissal timestamp: %w", err)
			}
			finding.DismissedAt = &parsed
			finding.DismissReason = dismissReason.String
		}
		if file.Valid {
			finding.File = stringPointer(file.String)
		}
		if line.Valid {
			value := int(line.Int64)
			finding.Line = &value
		}
		if symbol.Valid {
			finding.Symbol = stringPointer(symbol.String)
		}
		findings = append(findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query findings: %w", err)
	}
	return findings, nil
}

func (s *Store) DismissFinding(ctx context.Context, id int64, reason string, now time.Time) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("dismissal reason must not be empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dismiss finding #%d: %w", id, err)
	}
	defer tx.Rollback()
	var resolved, dismissed sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT resolved_sha, dismissed_at FROM findings WHERE id = ?`, id).Scan(&resolved, &dismissed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("finding #%d does not exist", id)
		}
		return fmt.Errorf("dismiss finding #%d: %w", id, err)
	}
	if resolved.Valid {
		return fmt.Errorf("finding #%d is resolved; reopen it before dismissing it", id)
	}
	if dismissed.Valid {
		return fmt.Errorf("finding #%d is already dismissed", id)
	}
	timestamp := formatTime(now)
	if _, err := tx.ExecContext(ctx, `
		UPDATE findings SET dismissed_at = ?, dismiss_reason = ? WHERE id = ?`, timestamp, reason, id); err != nil {
		return fmt.Errorf("dismiss finding #%d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO finding_events(finding_id, action, note, created_at)
		VALUES(?, 'dismissed', ?, ?)`, id, reason, timestamp); err != nil {
		return fmt.Errorf("record dismissal for finding #%d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dismiss finding #%d: %w", id, err)
	}
	return nil
}

func (s *Store) ReopenFinding(ctx context.Context, id int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reopen finding #%d: %w", id, err)
	}
	defer tx.Rollback()
	var resolved, dismissed sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT resolved_sha, dismissed_at FROM findings WHERE id = ?`, id).Scan(&resolved, &dismissed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("finding #%d does not exist", id)
		}
		return fmt.Errorf("reopen finding #%d: %w", id, err)
	}
	if !resolved.Valid && !dismissed.Valid {
		return fmt.Errorf("finding #%d is already open", id)
	}
	note := "reopened dismissed finding"
	if resolved.Valid {
		note = "reopened resolved finding"
	}
	timestamp := formatTime(now)
	if _, err := tx.ExecContext(ctx, `
		UPDATE findings
		SET resolved_sha = NULL, dismissed_at = NULL, dismiss_reason = NULL
		WHERE id = ?`, id); err != nil {
		return fmt.Errorf("reopen finding #%d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO finding_events(finding_id, action, note, created_at)
		VALUES(?, 'reopened', ?, ?)`, id, note, timestamp); err != nil {
		return fmt.Errorf("record reopening for finding #%d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reopen finding #%d: %w", id, err)
	}
	return nil
}

func (s *Store) AddFindingNote(ctx context.Context, id int64, note string, now time.Time) error {
	note = strings.TrimSpace(note)
	if note == "" {
		return errors.New("finding note must not be empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("note finding #%d: %w", id, err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM findings WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("finding #%d does not exist", id)
		}
		return fmt.Errorf("note finding #%d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO finding_events(finding_id, action, note, created_at)
		VALUES(?, 'noted', ?, ?)`, id, note, formatTime(now)); err != nil {
		return fmt.Errorf("note finding #%d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("note finding #%d: %w", id, err)
	}
	return nil
}

func (s *Store) FindingEvents(ctx context.Context, id int64) ([]FindingEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, finding_id, review_id, sha, action, note, created_at
		FROM finding_events WHERE finding_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("read history for finding #%d: %w", id, err)
	}
	defer rows.Close()
	var events []FindingEvent
	for rows.Next() {
		var event FindingEvent
		var reviewID sql.NullInt64
		var sha, note sql.NullString
		var createdAt string
		if err := rows.Scan(&event.ID, &event.FindingID, &reviewID, &sha,
			&event.Action, &note, &createdAt); err != nil {
			return nil, fmt.Errorf("read history for finding #%d: %w", id, err)
		}
		if reviewID.Valid {
			event.ReviewID = int64Pointer(reviewID.Int64)
		}
		if sha.Valid {
			event.SHA = stringPointer(sha.String)
		}
		event.Note = note.String
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse finding event timestamp: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read history for finding #%d: %w", id, err)
	}
	return events, nil
}

func (s *Store) InsertSkipped(ctx context.Context, metadata CommitMetadata, reason string, now time.Time) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("skip reason must not be empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record skipped commit: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
        INSERT INTO commits(sha, parent_sha, processed_at, status, skip_reason)
        VALUES(?, ?, ?, 'skipped', ?)`,
		metadata.SHA, metadata.ParentSHA, formatTime(now), reason)
	if err != nil {
		return fmt.Errorf("record skipped commit %s: %w", shortSHA(metadata.SHA), err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record skipped commit %s: %w", shortSHA(metadata.SHA), err)
	}
	return nil
}

func recordModel(ctx context.Context, tx *sql.Tx, model Model) error {
	status := "unknown"
	values := make([]any, 12)
	if model.Pricing != nil {
		pricing := model.Pricing
		if strings.TrimSpace(pricing.ServiceTier) == "" || strings.TrimSpace(pricing.Source) == "" ||
			strings.TrimSpace(pricing.AsOf) == "" || pricing.LongContextInputTokens <= 0 {
			return fmt.Errorf("model %s has incomplete pricing metadata", model.Name)
		}
		rates := []int64{
			pricing.ShortContext.InputNanousdPerToken,
			pricing.ShortContext.CachedInputNanousdPerToken,
			pricing.ShortContext.CacheWriteNanousdPerToken,
			pricing.ShortContext.OutputNanousdPerToken,
			pricing.LongContext.InputNanousdPerToken,
			pricing.LongContext.CachedInputNanousdPerToken,
			pricing.LongContext.CacheWriteNanousdPerToken,
			pricing.LongContext.OutputNanousdPerToken,
		}
		for _, rate := range rates {
			if rate < 0 {
				return fmt.Errorf("model %s has a negative token price", model.Name)
			}
		}
		status = "known"
		values = []any{
			pricing.ServiceTier,
			pricing.Source,
			pricing.AsOf,
			pricing.LongContextInputTokens,
			pricing.ShortContext.InputNanousdPerToken,
			pricing.ShortContext.CachedInputNanousdPerToken,
			pricing.ShortContext.CacheWriteNanousdPerToken,
			pricing.ShortContext.OutputNanousdPerToken,
			pricing.LongContext.InputNanousdPerToken,
			pricing.LongContext.CachedInputNanousdPerToken,
			pricing.LongContext.CacheWriteNanousdPerToken,
			pricing.LongContext.OutputNanousdPerToken,
		}
	}
	arguments := []any{model.Name, status}
	arguments = append(arguments, values...)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO models(
			name, pricing_status, service_tier, pricing_source, pricing_as_of,
			long_context_input_tokens,
			short_input_nanousd_per_token, short_cached_nanousd_per_token,
			short_cache_write_nanousd_per_token, short_output_nanousd_per_token,
			long_input_nanousd_per_token, long_cached_nanousd_per_token,
			long_cache_write_nanousd_per_token, long_output_nanousd_per_token
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			pricing_status = excluded.pricing_status,
			service_tier = excluded.service_tier,
			pricing_source = excluded.pricing_source,
			pricing_as_of = excluded.pricing_as_of,
			long_context_input_tokens = excluded.long_context_input_tokens,
			short_input_nanousd_per_token = excluded.short_input_nanousd_per_token,
			short_cached_nanousd_per_token = excluded.short_cached_nanousd_per_token,
			short_cache_write_nanousd_per_token = excluded.short_cache_write_nanousd_per_token,
			short_output_nanousd_per_token = excluded.short_output_nanousd_per_token,
			long_input_nanousd_per_token = excluded.long_input_nanousd_per_token,
			long_cached_nanousd_per_token = excluded.long_cached_nanousd_per_token,
			long_cache_write_nanousd_per_token = excluded.long_cache_write_nanousd_per_token,
			long_output_nanousd_per_token = excluded.long_output_nanousd_per_token`,
		arguments...)
	if err != nil {
		return fmt.Errorf("record model %s: %w", model.Name, err)
	}
	return nil
}

func costMinimum(estimate *CostEstimate) any {
	if estimate == nil {
		return nil
	}
	return estimate.MinimumMicrousd
}

func costMaximum(estimate *CostEstimate) any {
	if estimate == nil {
		return nil
	}
	return estimate.MaximumMicrousd
}

func costContext(estimate *CostEstimate) any {
	if estimate == nil {
		return nil
	}
	return estimate.Context
}

func costComplete(estimate *CostEstimate) any {
	if estimate == nil {
		return nil
	}
	return estimate.Complete
}

func (s *Store) ApplyReview(
	ctx context.Context,
	metadata CommitMetadata,
	identity ReviewIdentity,
	result ReviewResult,
	now time.Time,
) ([]int64, error) {
	if strings.TrimSpace(identity.Model.Name) == "" {
		return nil, errors.New("review model must not be empty")
	}
	if result.Usage == nil {
		return nil, errors.New("review token usage must be present")
	}
	if err := validateTokenUsage(*result.Usage); err != nil {
		return nil, fmt.Errorf("invalid review token usage: %w", err)
	}
	estimate, err := identity.Model.EstimateCost(*result.Usage)
	if err != nil {
		return nil, fmt.Errorf("estimate review cost: %w", err)
	}
	if err := validateReviewOutput(result.Output, nil); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("record review: %w", err)
	}
	defer tx.Rollback()
	if err := recordModel(ctx, tx, identity.Model); err != nil {
		return nil, err
	}

	processedAt := formatTime(now)
	_, err = tx.ExecContext(ctx, `
        INSERT INTO commits(
            sha, parent_sha, processed_at, status, model, reasoning_effort,
            prompt_version, summary, raw_response, input_tokens,
			cached_input_tokens, cache_write_tokens, output_tokens, reasoning_output_tokens,
			estimated_cost_microusd, estimated_cost_max_microusd,
			cost_context, cost_complete
		) VALUES(?, ?, ?, 'reviewed', ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sha) DO UPDATE SET
			parent_sha = excluded.parent_sha,
			processed_at = excluded.processed_at,
			status = 'reviewed',
			skip_reason = NULL,
			model = excluded.model,
			reasoning_effort = excluded.reasoning_effort,
			prompt_version = excluded.prompt_version,
			summary = excluded.summary,
			raw_response = excluded.raw_response,
			input_tokens = excluded.input_tokens,
			cached_input_tokens = excluded.cached_input_tokens,
			cache_write_tokens = excluded.cache_write_tokens,
			output_tokens = excluded.output_tokens,
			reasoning_output_tokens = excluded.reasoning_output_tokens,
			estimated_cost_microusd = excluded.estimated_cost_microusd,
			estimated_cost_max_microusd = excluded.estimated_cost_max_microusd,
			cost_context = excluded.cost_context,
			cost_complete = excluded.cost_complete`,
		metadata.SHA,
		metadata.ParentSHA,
		processedAt,
		identity.Model.Name,
		identity.ReasoningEffort,
		promptVersion,
		result.Output.Summary,
		result.RawResponse,
		result.Usage.InputTokens,
		result.Usage.CachedInputTokens,
		nullableInt64(result.Usage.CacheWriteTokens),
		result.Usage.OutputTokens,
		result.Usage.ReasoningOutputTokens,
		costMinimum(estimate),
		costMaximum(estimate),
		costContext(estimate),
		costComplete(estimate),
	)
	if err != nil {
		return nil, fmt.Errorf("record review for %s: %w", shortSHA(metadata.SHA), err)
	}
	attemptRow, err := tx.ExecContext(ctx, `
		INSERT INTO review_attempts(
			commit_sha, reviewed_at, model, reasoning_effort, prompt_version,
			summary, raw_response, input_tokens, cached_input_tokens,
			cache_write_tokens, output_tokens, reasoning_output_tokens,
			estimated_cost_microusd, estimated_cost_max_microusd,
			cost_context, cost_complete
		) VALUES(?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		metadata.SHA,
		processedAt,
		identity.Model.Name,
		identity.ReasoningEffort,
		promptVersion,
		result.Output.Summary,
		result.RawResponse,
		result.Usage.InputTokens,
		result.Usage.CachedInputTokens,
		nullableInt64(result.Usage.CacheWriteTokens),
		result.Usage.OutputTokens,
		result.Usage.ReasoningOutputTokens,
		costMinimum(estimate),
		costMaximum(estimate),
		costContext(estimate),
		costComplete(estimate),
	)
	if err != nil {
		return nil, fmt.Errorf("record review attempt for %s: %w", shortSHA(metadata.SHA), err)
	}
	attemptID, err := attemptRow.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read review attempt ID: %w", err)
	}

	newIDs := make([]int64, 0, len(result.Output.NewFindings))
	for _, finding := range result.Output.NewFindings {
		row, err := tx.ExecContext(ctx, `
            INSERT INTO findings(
				introduced_sha, introduced_review_id, severity, title, description, file, line, symbol
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			metadata.SHA,
			attemptID,
			finding.Severity,
			finding.Title,
			finding.Description,
			nullableString(finding.File),
			nullableInt(finding.Line),
			nullableString(finding.Symbol),
		)
		if err != nil {
			return nil, fmt.Errorf("record finding for %s: %w", shortSHA(metadata.SHA), err)
		}
		findingID, err := row.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read new finding ID: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO finding_events(finding_id, review_id, sha, action, created_at)
			VALUES(?, ?, ?, 'opened', ?)`, findingID, attemptID, metadata.SHA, processedAt); err != nil {
			return nil, fmt.Errorf("record finding event: %w", err)
		}
		newIDs = append(newIDs, findingID)
	}

	seenResolved := make(map[int64]struct{}, len(result.Output.ResolvedFindings))
	for _, resolution := range result.Output.ResolvedFindings {
		if _, duplicate := seenResolved[resolution.ID]; duplicate {
			return nil, fmt.Errorf("model resolved finding #%d more than once", resolution.ID)
		}
		seenResolved[resolution.ID] = struct{}{}
		row, err := tx.ExecContext(ctx, `
            UPDATE findings SET resolved_sha = ?
            WHERE id = ? AND resolved_sha IS NULL`, metadata.SHA, resolution.ID)
		if err != nil {
			return nil, fmt.Errorf("resolve finding #%d: %w", resolution.ID, err)
		}
		updated, err := row.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("resolve finding #%d: %w", resolution.ID, err)
		}
		if updated != 1 {
			return nil, fmt.Errorf("finding #%d is missing or is no longer open", resolution.ID)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO finding_events(finding_id, review_id, sha, action, note, created_at)
			VALUES(?, ?, ?, 'resolved', ?, ?)`,
			resolution.ID, attemptID, metadata.SHA, resolution.Reason, processedAt); err != nil {
			return nil, fmt.Errorf("record resolution for finding #%d: %w", resolution.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE review_attempts SET new_count = ?, resolved_count = ? WHERE id = ?`,
		len(newIDs), len(result.Output.ResolvedFindings), attemptID); err != nil {
		return nil, fmt.Errorf("record review attempt counts: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("record review for %s: %w", shortSHA(metadata.SHA), err)
	}
	return newIDs, nil
}

func (s *Store) Commit(ctx context.Context, sha string) (CommitRecord, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT
            c.sha, c.parent_sha, c.processed_at, c.status, c.skip_reason,
            c.model, c.reasoning_effort, c.prompt_version, c.summary, c.raw_response,
			c.input_tokens, c.cached_input_tokens, c.cache_write_tokens, c.output_tokens,
			c.reasoning_output_tokens, c.estimated_cost_microusd,
			c.estimated_cost_max_microusd, c.cost_context, c.cost_complete,
			COALESCE((SELECT a.new_count FROM review_attempts a
			          WHERE a.commit_sha = c.sha ORDER BY a.id DESC LIMIT 1), 0),
			COALESCE((SELECT a.resolved_count FROM review_attempts a
			          WHERE a.commit_sha = c.sha ORDER BY a.id DESC LIMIT 1), 0)
        FROM commits c WHERE c.sha = ?`, sha)
	record, err := scanCommitRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CommitRecord{}, fmt.Errorf("commit %s has not been processed", shortSHA(sha))
	}
	if err != nil {
		return CommitRecord{}, fmt.Errorf("read commit review: %w", err)
	}
	return record, nil
}

func (s *Store) Log(ctx context.Context) ([]CommitRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT
            c.sha, c.parent_sha, c.processed_at, c.status, c.skip_reason,
            c.model, c.reasoning_effort, c.prompt_version, c.summary, c.raw_response,
			c.input_tokens, c.cached_input_tokens, c.cache_write_tokens, c.output_tokens,
			c.reasoning_output_tokens, c.estimated_cost_microusd,
			c.estimated_cost_max_microusd, c.cost_context, c.cost_complete,
			COALESCE((SELECT a.new_count FROM review_attempts a
			          WHERE a.commit_sha = c.sha ORDER BY a.id DESC LIMIT 1), 0),
			COALESCE((SELECT a.resolved_count FROM review_attempts a
			          WHERE a.commit_sha = c.sha ORDER BY a.id DESC LIMIT 1), 0)
        FROM commits c ORDER BY c.processed_at DESC, c.sha DESC`)
	if err != nil {
		return nil, fmt.Errorf("read review log: %w", err)
	}
	defer rows.Close()
	var records []CommitRecord
	for rows.Next() {
		record, err := scanCommitRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("read review log: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read review log: %w", err)
	}
	return records, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCommitRecord(row rowScanner) (CommitRecord, error) {
	var record CommitRecord
	var processedAt string
	var parent, skipReason, model, effort, version, summary, raw sql.NullString
	var inputTokens, cachedInputTokens, cacheWriteTokens, outputTokens, reasoningOutputTokens sql.NullInt64
	var estimatedCost, estimatedCostMaximum sql.NullInt64
	var costContextValue sql.NullString
	var costCompleteValue sql.NullBool
	err := row.Scan(
		&record.SHA,
		&parent,
		&processedAt,
		&record.Status,
		&skipReason,
		&model,
		&effort,
		&version,
		&summary,
		&raw,
		&inputTokens,
		&cachedInputTokens,
		&cacheWriteTokens,
		&outputTokens,
		&reasoningOutputTokens,
		&estimatedCost,
		&estimatedCostMaximum,
		&costContextValue,
		&costCompleteValue,
		&record.NewCount,
		&record.ResolvedCount,
	)
	if err != nil {
		return CommitRecord{}, err
	}
	record.ParentSHA = parent.String
	record.SkipReason = skipReason.String
	record.Model = model.String
	record.ReasoningEffort = effort.String
	record.PromptVersion = version.String
	record.Summary = summary.String
	record.RawResponse = raw.String
	if inputTokens.Valid && cachedInputTokens.Valid && outputTokens.Valid && reasoningOutputTokens.Valid {
		record.Usage = &TokenUsage{
			InputTokens:           inputTokens.Int64,
			CachedInputTokens:     cachedInputTokens.Int64,
			OutputTokens:          outputTokens.Int64,
			ReasoningOutputTokens: reasoningOutputTokens.Int64,
		}
		if cacheWriteTokens.Valid {
			record.Usage.CacheWriteTokens = int64Pointer(cacheWriteTokens.Int64)
		}
	}
	if estimatedCost.Valid {
		record.EstimatedCostMicrousd = int64Pointer(estimatedCost.Int64)
	}
	if estimatedCostMaximum.Valid {
		record.EstimatedCostMaxMicrousd = int64Pointer(estimatedCostMaximum.Int64)
	}
	record.CostContext = costContextValue.String
	record.CostComplete = costCompleteValue.Bool
	record.ProcessedAt, err = time.Parse(time.RFC3339Nano, processedAt)
	if err != nil {
		return CommitRecord{}, fmt.Errorf("parse processed timestamp: %w", err)
	}
	return record, nil
}

func (s *Store) ReviewAttempts(ctx context.Context, sha string) ([]ReviewAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, commit_sha, reviewed_at, model, reasoning_effort, prompt_version,
		       summary, raw_response, input_tokens, cached_input_tokens,
		       cache_write_tokens, output_tokens, reasoning_output_tokens,
		       estimated_cost_microusd, estimated_cost_max_microusd,
		       cost_context, cost_complete, new_count, resolved_count
		FROM review_attempts WHERE commit_sha = ? ORDER BY id`, sha)
	if err != nil {
		return nil, fmt.Errorf("list review attempts: %w", err)
	}
	defer rows.Close()
	var attempts []ReviewAttempt
	for rows.Next() {
		attempt, err := scanReviewAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("list review attempts: %w", err)
		}
		attempt.Number = len(attempts) + 1
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list review attempts: %w", err)
	}
	if len(attempts) > 0 {
		attempts[len(attempts)-1].Current = true
	}
	return attempts, nil
}

func (s *Store) ReviewAttempt(ctx context.Context, sha string, number int) (ReviewAttempt, error) {
	if number <= 0 {
		return ReviewAttempt{}, errors.New("review number must be positive")
	}
	attempts, err := s.ReviewAttempts(ctx, sha)
	if err != nil {
		return ReviewAttempt{}, err
	}
	if number > len(attempts) {
		return ReviewAttempt{}, fmt.Errorf("commit %s has no review attempt #%d", shortSHA(sha), number)
	}
	return attempts[number-1], nil
}

func scanReviewAttempt(row rowScanner) (ReviewAttempt, error) {
	var attempt ReviewAttempt
	var reviewedAt string
	var effort sql.NullString
	var cacheWriteTokens, estimatedCost, estimatedCostMaximum sql.NullInt64
	var costContextValue sql.NullString
	var costCompleteValue sql.NullBool
	if err := row.Scan(
		&attempt.ID,
		&attempt.CommitSHA,
		&reviewedAt,
		&attempt.Model,
		&effort,
		&attempt.PromptVersion,
		&attempt.Summary,
		&attempt.RawResponse,
		&attempt.Usage.InputTokens,
		&attempt.Usage.CachedInputTokens,
		&cacheWriteTokens,
		&attempt.Usage.OutputTokens,
		&attempt.Usage.ReasoningOutputTokens,
		&estimatedCost,
		&estimatedCostMaximum,
		&costContextValue,
		&costCompleteValue,
		&attempt.NewCount,
		&attempt.ResolvedCount,
	); err != nil {
		return ReviewAttempt{}, err
	}
	attempt.ReasoningEffort = effort.String
	if cacheWriteTokens.Valid {
		attempt.Usage.CacheWriteTokens = int64Pointer(cacheWriteTokens.Int64)
	}
	if estimatedCost.Valid {
		attempt.EstimatedCostMicrousd = int64Pointer(estimatedCost.Int64)
	}
	if estimatedCostMaximum.Valid {
		attempt.EstimatedCostMaxMicrousd = int64Pointer(estimatedCostMaximum.Int64)
	}
	attempt.CostContext = costContextValue.String
	attempt.CostComplete = costCompleteValue.Bool
	parsed, err := time.Parse(time.RFC3339Nano, reviewedAt)
	if err != nil {
		return ReviewAttempt{}, fmt.Errorf("parse review timestamp: %w", err)
	}
	attempt.ReviewedAt = parsed
	return attempt, nil
}

func validateReviewOutput(output ReviewOutput, allowedResolutions map[int64]struct{}) error {
	if strings.TrimSpace(output.Summary) == "" {
		return errors.New("model response summary must not be empty")
	}
	for i, finding := range output.NewFindings {
		if finding.Severity != "info" && finding.Severity != "warning" && finding.Severity != "error" {
			return fmt.Errorf("new_findings[%d] has invalid severity %q", i, finding.Severity)
		}
		if strings.TrimSpace(finding.Title) == "" || strings.TrimSpace(finding.Description) == "" {
			return fmt.Errorf("new_findings[%d] requires a title and description", i)
		}
		if finding.Line != nil && *finding.Line <= 0 {
			return fmt.Errorf("new_findings[%d] has an invalid line number", i)
		}
	}
	seen := make(map[int64]struct{}, len(output.ResolvedFindings))
	for i, resolution := range output.ResolvedFindings {
		if resolution.ID <= 0 || strings.TrimSpace(resolution.Reason) == "" {
			return fmt.Errorf("resolved_findings[%d] requires a positive ID and reason", i)
		}
		if _, exists := seen[resolution.ID]; exists {
			return fmt.Errorf("resolved_findings contains duplicate ID #%d", resolution.ID)
		}
		seen[resolution.ID] = struct{}{}
		if allowedResolutions != nil {
			if _, allowed := allowedResolutions[resolution.ID]; !allowed {
				return fmt.Errorf("model resolved finding #%d, which was not supplied as open", resolution.ID)
			}
		}
	}
	return nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func int64Pointer(value int64) *int64 {
	return &value
}

func stringPointer(value string) *string {
	return &value
}

func parseFindingID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("finding ID must be a positive integer")
	}
	return id, nil
}
