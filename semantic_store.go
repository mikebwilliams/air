package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const reposeSemanticSchemaSQL = `
CREATE TABLE semantic_profiles (
 id TEXT PRIMARY KEY, base_id TEXT NOT NULL, document TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX semantic_profiles_base ON semantic_profiles(base_id);
CREATE TABLE semantic_results (
 id TEXT PRIMARY KEY, profile_id TEXT NOT NULL REFERENCES semantic_profiles(id),
 path TEXT NOT NULL, document TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE semantic_current (
 profile_id TEXT NOT NULL REFERENCES semantic_profiles(id), path TEXT NOT NULL,
 result_id TEXT NOT NULL REFERENCES semantic_results(id), PRIMARY KEY(profile_id,path)
);
PRAGMA user_version = 3;
`

func (store *inventoryStore) saveSemanticProfile(ctx context.Context, profile semanticProfile, now time.Time) error {
	data, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO semantic_profiles(id,base_id,document,created_at) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING`, profile.ID, profile.BaseID, string(data), formatTime(now))
	return err
}
func (store *inventoryStore) saveSemanticResult(ctx context.Context, result semanticFileResult, now time.Time) (semanticFileResult, error) {
	result.ID = ""
	data, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	result.ID = inventoryHash(data)
	data, err = json.Marshal(result)
	if err != nil {
		return result, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO semantic_results(id,profile_id,path,document,created_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, result.ID, result.ProfileID, result.Path, string(data), formatTime(now))
	if err != nil {
		return result, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO semantic_current(profile_id,path,result_id) VALUES(?,?,?) ON CONFLICT(profile_id,path) DO UPDATE SET result_id=excluded.result_id`, result.ProfileID, result.Path, result.ID)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (store *inventoryStore) semanticSnapshot(ctx context.Context, inventory Inventory, selector string) (*semanticSnapshot, error) {
	if store.version < 3 {
		if selector != "" {
			return nil, errors.New("no semantic index; run inventory index")
		}
		return nil, nil
	}
	query := `SELECT document FROM semantic_profiles WHERE base_id=?`
	args := []any{semanticBaseID(inventory)}
	if selector != "" {
		if len(selector) < 4 || len(selector) > 64 || !isLowerHex(selector) {
			return nil, errors.New("index ID must be a hexadecimal prefix of at least four characters")
		}
		query += " AND id LIKE ?"
		args = append(args, selector+"%")
	}
	query += " ORDER BY rowid DESC"
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var snapshot *semanticSnapshot
	for rows.Next() {
		if snapshot != nil {
			if selector != "" {
				rows.Close()
				return nil, errors.New("ambiguous index ID")
			}
			break
		}
		var document string
		if err := rows.Scan(&document); err != nil {
			rows.Close()
			return nil, err
		}
		snapshot = &semanticSnapshot{Files: []semanticFileResult{}}
		if err := json.Unmarshal([]byte(document), &snapshot.Profile); err != nil {
			rows.Close()
			return nil, err
		}
		id := snapshot.Profile.ID
		snapshot.Profile.ID = ""
		data, _ := json.Marshal(snapshot.Profile)
		snapshot.Profile.ID = id
		if inventoryHash(data) != id {
			rows.Close()
			return nil, errors.New("semantic profile content does not match its ID")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		if selector != "" {
			return nil, fmt.Errorf("index %q not found for this snapshot/build", selector)
		}
		return nil, nil
	}
	results, err := store.db.QueryContext(ctx, `SELECT r.document FROM semantic_current c JOIN semantic_results r ON r.id=c.result_id WHERE c.profile_id=? ORDER BY c.path`, snapshot.Profile.ID)
	if err != nil {
		return nil, err
	}
	defer results.Close()
	for results.Next() {
		var document string
		if err := results.Scan(&document); err != nil {
			return nil, err
		}
		var file semanticFileResult
		if err := json.Unmarshal([]byte(document), &file); err != nil {
			return nil, err
		}
		id := file.ID
		file.ID = ""
		data, _ := json.Marshal(file)
		file.ID = id
		if inventoryHash(data) != id {
			return nil, errors.New("semantic result content does not match its ID")
		}
		snapshot.Files = append(snapshot.Files, file)
	}
	return snapshot, results.Err()
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
