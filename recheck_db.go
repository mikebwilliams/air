package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) PreviouslyRecheckedFindingIDs(
	ctx context.Context,
	headSHA, model, effort, version string,
) (map[int64]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rr.finding_id
		FROM recheck_results rr
		JOIN recheck_attempts a ON a.id = rr.recheck_id
		WHERE a.head_sha = ? AND a.reviewer = ? AND a.model = ?
		  AND COALESCE(a.reasoning_effort, '') = ? AND a.prompt_version = ?`,
		headSHA, codexReviewerName, model, effort, version)
	if err != nil {
		return nil, fmt.Errorf("read prior HEAD rechecks: %w", err)
	}
	defer rows.Close()
	ids := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read prior HEAD rechecks: %w", err)
		}
		ids[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read prior HEAD rechecks: %w", err)
	}
	return ids, nil
}

func (s *Store) ApplyRecheck(
	ctx context.Context,
	headSHA string,
	identity ReviewIdentity,
	findings []Finding,
	result RecheckResult,
	now time.Time,
) (int64, error) {
	if !isHexObjectID(headSHA) {
		return 0, errors.New("recheck HEAD must be a full Git object ID")
	}
	if strings.TrimSpace(identity.Model.Name) == "" {
		return 0, errors.New("recheck model must not be empty")
	}
	if len(findings) == 0 {
		return 0, errors.New("recheck must contain at least one finding")
	}
	if result.Usage == nil {
		return 0, errors.New("recheck token usage must be present")
	}
	if result.Duration < 0 {
		return 0, errors.New("recheck duration must not be negative")
	}
	if err := validateTokenUsage(*result.Usage); err != nil {
		return 0, fmt.Errorf("invalid recheck token usage: %w", err)
	}
	allowed := make(map[int64]struct{}, len(findings))
	for _, finding := range findings {
		if finding.ID <= 0 {
			return 0, errors.New("recheck finding has an invalid ID")
		}
		if _, duplicate := allowed[finding.ID]; duplicate {
			return 0, fmt.Errorf("recheck supplied finding #%d more than once", finding.ID)
		}
		allowed[finding.ID] = struct{}{}
	}
	if err := validateRecheckOutput(result.Output, allowed); err != nil {
		return 0, err
	}
	estimate, err := identity.Model.EstimateCost(*result.Usage)
	if err != nil {
		return 0, fmt.Errorf("estimate recheck cost: %w", err)
	}
	attemptPromptVersion := promptVersionOrDefault(identity.PromptVersion, recheckPromptVersion)

	resolved, stillPresent, uncertain := countRecheckOutcomes(result.Output)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("record HEAD recheck: %w", err)
	}
	defer tx.Rollback()
	if err := recordModel(ctx, tx, identity.Model); err != nil {
		return 0, err
	}
	for _, finding := range findings {
		var resolvedSHA, dismissedAt sql.NullString
		var resolvedRecheck sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT resolved_sha, resolved_recheck_id, dismissed_at
			FROM findings WHERE id = ?`, finding.ID).
			Scan(&resolvedSHA, &resolvedRecheck, &dismissedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("finding #%d no longer exists", finding.ID)
			}
			return 0, fmt.Errorf("verify finding #%d before recheck: %w", finding.ID, err)
		}
		if resolvedSHA.Valid || resolvedRecheck.Valid || dismissedAt.Valid {
			return 0, fmt.Errorf("finding #%d is no longer open", finding.ID)
		}
	}

	timestamp := formatTime(now)
	attemptRow, err := tx.ExecContext(ctx, `
		INSERT INTO recheck_attempts(
			head_sha, checked_at, reviewer, model, reasoning_effort, prompt_version,
			summary, raw_response, input_tokens, cached_input_tokens,
			cache_write_tokens, output_tokens, reasoning_output_tokens,
			estimated_cost_microusd, estimated_cost_max_microusd,
			cost_context, cost_complete, duration_ms, finding_count,
			resolved_count, still_present_count, uncertain_count
		) VALUES(?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		headSHA, timestamp, codexReviewerName, identity.Model.Name, identity.ReasoningEffort,
		attemptPromptVersion, result.Output.Summary, result.RawResponse,
		result.Usage.InputTokens, result.Usage.CachedInputTokens,
		nullableInt64(result.Usage.CacheWriteTokens), result.Usage.OutputTokens,
		result.Usage.ReasoningOutputTokens, costMinimum(estimate), costMaximum(estimate),
		costContext(estimate), costComplete(estimate), result.Duration.Milliseconds(),
		len(findings), resolved, stillPresent, uncertain)
	if err != nil {
		return 0, fmt.Errorf("record HEAD recheck attempt: %w", err)
	}
	attemptID, err := attemptRow.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read HEAD recheck attempt ID: %w", err)
	}
	for _, outcome := range result.Output.Findings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO recheck_results(recheck_id, finding_id, outcome, reason)
			VALUES(?, ?, ?, ?)`, attemptID, outcome.ID, outcome.Outcome, outcome.Reason); err != nil {
			return 0, fmt.Errorf("record recheck result for finding #%d: %w", outcome.ID, err)
		}
		if outcome.Outcome != "resolved" {
			continue
		}
		row, err := tx.ExecContext(ctx, `
			UPDATE findings SET resolved_recheck_id = ?
			WHERE id = ? AND resolved_sha IS NULL AND resolved_recheck_id IS NULL
			  AND dismissed_at IS NULL`, attemptID, outcome.ID)
		if err != nil {
			return 0, fmt.Errorf("resolve finding #%d by HEAD recheck: %w", outcome.ID, err)
		}
		updated, err := row.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("resolve finding #%d by HEAD recheck: %w", outcome.ID, err)
		}
		if updated != 1 {
			return 0, fmt.Errorf("finding #%d is no longer open", outcome.ID)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("record HEAD recheck: %w", err)
	}
	return attemptID, nil
}

func countRecheckOutcomes(output RecheckOutput) (resolved, stillPresent, uncertain int) {
	for _, finding := range output.Findings {
		switch finding.Outcome {
		case "resolved":
			resolved++
		case "still_present":
			stillPresent++
		case "uncertain":
			uncertain++
		}
	}
	return resolved, stillPresent, uncertain
}
