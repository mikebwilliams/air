package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

const schemaVersion = 2

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

CREATE TABLE findings (
    id              INTEGER PRIMARY KEY,
    introduced_sha  TEXT NOT NULL,
    resolved_sha    TEXT,
    severity        TEXT NOT NULL CHECK(severity IN ('info', 'warning', 'error')),
    title           TEXT NOT NULL,
    description     TEXT NOT NULL,
    file            TEXT,
    line            INTEGER CHECK(line IS NULL OR line > 0),
    symbol          TEXT,

    FOREIGN KEY(introduced_sha) REFERENCES commits(sha) ON DELETE CASCADE,
    FOREIGN KEY(resolved_sha) REFERENCES commits(sha) ON DELETE SET NULL
);

CREATE TABLE finding_events (
    id          INTEGER PRIMARY KEY,
    finding_id  INTEGER NOT NULL,
    sha         TEXT NOT NULL,
    action      TEXT NOT NULL CHECK(action IN ('opened', 'resolved', 'reopened', 'updated')),
    note        TEXT,

    FOREIGN KEY(finding_id) REFERENCES findings(id) ON DELETE CASCADE,
    FOREIGN KEY(sha) REFERENCES commits(sha) ON DELETE CASCADE
);

CREATE INDEX findings_open_idx ON findings(resolved_sha);
CREATE INDEX finding_events_finding_idx ON finding_events(finding_id, id);
PRAGMA user_version = 2;
`

func CreateStore(ctx context.Context, databasePath, startSHA string) (*Store, error) {
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
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM config WHERE key = ?`, key).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("missing configuration key %q", key)
		}
		return "", fmt.Errorf("read configuration %q: %w", key, err)
	}
	return value, nil
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

func (s *Store) OpenFindings(ctx context.Context) ([]Finding, error) {
	return s.queryFindings(ctx, `
        SELECT id, introduced_sha, resolved_sha, severity, title, description, file, line, symbol
        FROM findings WHERE resolved_sha IS NULL ORDER BY id`)
}

func (s *Store) Finding(ctx context.Context, id int64) (Finding, error) {
	rows, err := s.queryFindings(ctx, `
        SELECT id, introduced_sha, resolved_sha, severity, title, description, file, line, symbol
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
        SELECT id, introduced_sha, resolved_sha, severity, title, description, file, line, symbol
        FROM findings WHERE introduced_sha = ? ORDER BY id`, sha)
}

func (s *Store) FindingsResolvedBy(ctx context.Context, sha string) ([]Finding, error) {
	return s.queryFindings(ctx, `
        SELECT id, introduced_sha, resolved_sha, severity, title, description, file, line, symbol
        FROM findings WHERE resolved_sha = ? ORDER BY id`, sha)
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
		var resolved, file, symbol sql.NullString
		var line sql.NullInt64
		if err := rows.Scan(
			&finding.ID,
			&finding.IntroducedSHA,
			&resolved,
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

	_, err = tx.ExecContext(ctx, `
        INSERT INTO commits(
            sha, parent_sha, processed_at, status, model, reasoning_effort,
            prompt_version, summary, raw_response, input_tokens,
			cached_input_tokens, cache_write_tokens, output_tokens, reasoning_output_tokens,
			estimated_cost_microusd, estimated_cost_max_microusd,
			cost_context, cost_complete
        ) VALUES(?, ?, ?, 'reviewed', ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		metadata.SHA,
		metadata.ParentSHA,
		formatTime(now),
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

	newIDs := make([]int64, 0, len(result.Output.NewFindings))
	for _, finding := range result.Output.NewFindings {
		row, err := tx.ExecContext(ctx, `
            INSERT INTO findings(
                introduced_sha, severity, title, description, file, line, symbol
            ) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			metadata.SHA,
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
            INSERT INTO finding_events(finding_id, sha, action)
            VALUES(?, ?, 'opened')`, findingID, metadata.SHA); err != nil {
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
            INSERT INTO finding_events(finding_id, sha, action, note)
            VALUES(?, ?, 'resolved', ?)`, resolution.ID, metadata.SHA, resolution.Reason); err != nil {
			return nil, fmt.Errorf("record resolution for finding #%d: %w", resolution.ID, err)
		}
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
            (SELECT COUNT(*) FROM findings f WHERE f.introduced_sha = c.sha),
            (SELECT COUNT(*) FROM findings f WHERE f.resolved_sha = c.sha)
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
            (SELECT COUNT(*) FROM findings f WHERE f.introduced_sha = c.sha),
            (SELECT COUNT(*) FROM findings f WHERE f.resolved_sha = c.sha)
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
