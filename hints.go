package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

const (
	hintsConfigKey     = "hints"
	hintSnapshotPrefix = "hint.snapshot."
	maximumHintBytes   = 16 * 1024
)

// A zero ID denotes a one-off command-line hint; saved hints have stable IDs.
type ReviewHint struct {
	ID   int64  `json:"id,omitempty"`
	Text string `json:"text"`
}

type hintState struct {
	LastID int64        `json:"last_id"`
	Hints  []ReviewHint `json:"hints"`
}

func validateHint(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", errors.New("hint must be valid UTF-8")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("hint must not be empty")
	}
	if len(text) > maximumHintBytes {
		return "", fmt.Errorf("hint exceeds %d KiB", maximumHintBytes/1024)
	}
	return text, nil
}

func validateHints(hints []ReviewHint) error {
	size := 0
	for _, hint := range hints {
		if hint.ID < 0 {
			return errors.New("hint ID must not be negative")
		}
		if _, err := validateHint(hint.Text); err != nil {
			return err
		}
		size += len(hint.Text)
	}
	if size > maximumPromptBytes {
		return fmt.Errorf("combined hints exceed %d KiB", maximumPromptBytes/1024)
	}
	return nil
}

func decodeHintState(value string) (hintState, error) {
	var state hintState
	if err := json.Unmarshal([]byte(value), &state); err != nil {
		return state, fmt.Errorf("invalid stored hints: %w", err)
	}
	if state.LastID < 0 {
		return state, errors.New("invalid stored hint counter")
	}
	var previousID int64
	for _, hint := range state.Hints {
		if hint.ID <= previousID || hint.ID > state.LastID {
			return state, errors.New("invalid stored hint IDs")
		}
		previousID = hint.ID
	}
	return state, validateHints(state.Hints)
}

func (s *Store) Hints(ctx context.Context) ([]ReviewHint, error) {
	value, found, err := s.ConfigValue(ctx, hintsConfigKey)
	if err != nil || !found {
		return nil, err
	}
	state, err := decodeHintState(value)
	return state.Hints, err
}

// Take the SQLite write lock before reading so concurrent edits cannot lose
// each other's changes or reuse an ID. No schema changes are needed.
func (s *Store) updateHints(ctx context.Context, change func(*hintState) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO config(key, value) VALUES (?, '{"last_id":0,"hints":[]}')
		ON CONFLICT(key) DO NOTHING`, hintsConfigKey); err != nil {
		return fmt.Errorf("lock hints for editing: %w", err)
	}
	var value string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM config WHERE key = ?`, hintsConfigKey).Scan(&value); err != nil {
		return err
	}
	state, err := decodeHintState(value)
	if err != nil {
		return err
	}
	if err := change(&state); err != nil {
		return err
	}
	if err := validateHints(state.Hints); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE config SET value = ? WHERE key = ?`, string(data), hintsConfigKey); err != nil {
		return fmt.Errorf("save hints: %w", err)
	}
	return tx.Commit()
}

func (s *Store) AddHint(ctx context.Context, text string) (int64, error) {
	text, err := validateHint(text)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.updateHints(ctx, func(state *hintState) error {
		if state.LastID == math.MaxInt64 {
			return errors.New("hint IDs exhausted")
		}
		state.LastID++
		id = state.LastID
		state.Hints = append(state.Hints, ReviewHint{ID: id, Text: text})
		return nil
	})
	return id, err
}

func (s *Store) RemoveHint(ctx context.Context, id int64) error {
	return s.updateHints(ctx, func(state *hintState) error {
		for index, hint := range state.Hints {
			if hint.ID == id {
				state.Hints = append(state.Hints[:index], state.Hints[index+1:]...)
				return nil
			}
		}
		return fmt.Errorf("hint #%d does not exist", id)
	})
}

// Snapshots are retained independently of active hints and written atomically
// with successful attempts. Prompt identities already link both review kinds
// to their snapshot, including superseded attempts.
func recordHintSnapshot(ctx context.Context, tx *sql.Tx, identity ReviewIdentity) error {
	if len(identity.Hints) == 0 {
		return nil
	}
	if !strings.HasPrefix(identity.PromptVersion, "hints:sha256:") {
		return errors.New("hint snapshot requires a hint-aware prompt identity")
	}
	if err := validateHints(identity.Hints); err != nil {
		return err
	}
	data, err := json.Marshal(identity.Hints)
	if err != nil {
		return err
	}
	key := hintSnapshotPrefix + identity.PromptVersion
	if _, err := tx.ExecContext(ctx, `INSERT INTO config(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO NOTHING`, key, string(data)); err != nil {
		return fmt.Errorf("record hint snapshot: %w", err)
	}
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM config WHERE key = ?`, key).Scan(&existing); err != nil {
		return err
	}
	if existing != string(data) {
		return errors.New("hint snapshot conflicts with recorded prompt identity")
	}
	return nil
}

func (s *Store) HintsForPrompt(ctx context.Context, version string) ([]ReviewHint, error) {
	if !strings.HasPrefix(version, "hints:sha256:") {
		return nil, nil
	}
	value, found, err := s.ConfigValue(ctx, hintSnapshotPrefix+version)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no hint snapshot for prompt %s", version)
	}
	var hints []ReviewHint
	if err := json.Unmarshal([]byte(value), &hints); err != nil {
		return nil, fmt.Errorf("decode hint snapshot: %w", err)
	}
	return hints, validateHints(hints)
}
