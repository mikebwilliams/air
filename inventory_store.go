package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

// Separate storage avoids inheriting AIR's introducing-commit foreign keys.
// Queue/attempt tables can be added here without changing inventory documents.
const reposeSchemaSQL = `
CREATE TABLE inventories (
    id TEXT PRIMARY KEY,
    snapshot_sha TEXT NOT NULL,
    created_at TEXT NOT NULL,
    reviewed_at TEXT,
    document TEXT NOT NULL
);
PRAGMA application_id = 1380994899;
PRAGMA user_version = 1;
`

const reposePolicySchemaSQL = `
CREATE TABLE inventory_state (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    current_inventory_id TEXT NOT NULL REFERENCES inventories(id)
);
INSERT INTO inventory_state(singleton, current_inventory_id)
    SELECT 1, id FROM inventories ORDER BY rowid DESC LIMIT 1;
PRAGMA user_version = 2;
`

func init() {
	sql.Register("sqlite3_repose", &sqlite3.SQLiteDriver{ConnectHook: func(connection *sqlite3.SQLiteConn) error {
		// Keep checkpointed WAL/SHM files available after writers close, so SQLite
		// readers can open the database even with a read-only state directory.
		return connection.SetFileControlInt("main", sqlite3.SQLITE_FCNTL_PERSIST_WAL, 1)
	}})
}

type inventoryStore struct {
	*Store
	version int
}

type InventoryRecord struct {
	ID         string     `json:"id"`
	CreatedAt  time.Time  `json:"created_at"`
	ReviewedAt *time.Time `json:"reviewed_at,omitempty"`
	Inventory  Inventory  `json:"inventory"`
}

type InventoryListing struct {
	ID          string     `json:"id"`
	SnapshotSHA string     `json:"snapshot_sha"`
	CreatedAt   time.Time  `json:"created_at"`
	ReviewedAt  *time.Time `json:"reviewed_at,omitempty"`
	Current     bool       `json:"current"`
}

func reposeDatabasePath(ctx context.Context, repository *GitRepository) (string, error) {
	// Worktree-local state keeps a linked scan checkout separate from development.
	gitDir, err := repository.run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSpace(gitDir), "repose", "repose.sqlite"), nil
}

func openInventoryStore(ctx context.Context, filename string, create bool) (*inventoryStore, error) {
	if create {
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			return nil, fmt.Errorf("create Repose state directory: %w", err)
		}
		file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	} else if _, err := os.Stat(filename); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("no Repose inventories; run repose inventory build")
		}
		return nil, err
	}
	base, err := openInventoryDatabase(ctx, filename)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = base.Close()
		}
	}()
	// Serialize first-time initialization across concurrent inventory builders.
	if _, err := base.db.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	defer base.db.ExecContext(context.Background(), "ROLLBACK") // harmless after COMMIT
	var version, application int
	if err := base.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if err := base.db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&application); err != nil {
		return nil, err
	}
	if version == 0 && application == 0 && create {
		var tables int
		if err := base.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tables); err != nil {
			return nil, err
		}
		if tables != 0 {
			return nil, errors.New("refusing to initialize an existing non-Repose database")
		}
		if _, err := base.db.ExecContext(ctx, reposeSchemaSQL); err != nil {
			return nil, fmt.Errorf("initialize Repose database: %w", err)
		}
		version, application = 1, 1380994899
	}
	if (version != 1 && version != 2) || application != 1380994899 {
		return nil, fmt.Errorf("unsupported Repose database (application %d, version %d)", application, version)
	}
	if version == 1 {
		if _, err := base.db.ExecContext(ctx, reposePolicySchemaSQL); err != nil {
			return nil, fmt.Errorf("upgrade Repose policy storage: %w", err)
		}
		version = 2
	}
	if _, err := base.db.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	success = true
	return &inventoryStore{Store: base, version: version}, nil
}

func openInventoryReadOnly(ctx context.Context, filename string) (*inventoryStore, error) {
	if _, err := os.Stat(filename); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("no Repose inventories; run repose inventory build")
		}
		return nil, err
	}
	base, err := openInventoryConnection(ctx, filename, true)
	if err != nil {
		return nil, err
	}
	var version, application int
	if err = base.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err == nil {
		err = base.db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&application)
	}
	if err == nil && ((version != 1 && version != 2) || application != 1380994899) {
		err = fmt.Errorf("unsupported Repose database (application %d, version %d)", application, version)
	}
	if err != nil {
		base.Close()
		return nil, err
	}
	return &inventoryStore{Store: base, version: version}, nil
}

func openInventoryConnection(ctx context.Context, filename string, readOnly bool) (*Store, error) {
	u := &url.URL{Scheme: "file", Path: filename}
	query := u.Query()
	query.Set("_foreign_keys", "on")
	query.Set("_busy_timeout", "5000")
	if readOnly {
		query.Set("mode", "ro")
		query.Set("_query_only", "on")
	} else {
		query.Set("_journal_mode", "WAL")
	}
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3_repose", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open Repose database: %w", err)
	}
	return &Store{db: db}, nil
}

func openInventoryDatabase(ctx context.Context, filename string) (*Store, error) {
	// Changing journal_mode during connection setup can return SQLITE_BUSY even
	// with a busy timeout. Retry only contention, before beginning schema work.
	openContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for delay := 10 * time.Millisecond; ; delay = min(2*delay, 200*time.Millisecond) {
		store, err := openInventoryConnection(openContext, filename, false)
		if err == nil {
			return store, nil
		}
		var sqliteError sqlite3.Error
		if !errors.As(err, &sqliteError) || (sqliteError.Code != sqlite3.ErrBusy && sqliteError.Code != sqlite3.ErrLocked) {
			return nil, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-openContext.Done():
			timer.Stop()
			return nil, fmt.Errorf("open Repose database after contention: %w", openContext.Err())
		case <-timer.C:
		}
	}
}

func (store *inventoryStore) SaveInventory(ctx context.Context, inventory Inventory, now time.Time) (InventoryRecord, error) {
	return store.saveCurrentInventory(ctx, inventory, nil, now)
}

var errInventoryCurrentChanged = errors.New("current inventory changed while preparing this update; retry the command")

func (store *inventoryStore) saveCurrentInventory(ctx context.Context, inventory Inventory, expectedID *string, now time.Time) (InventoryRecord, error) {
	if inventory.FormatVersion != inventoryFormatVersion || !isHexObjectID(inventory.SnapshotSHA) ||
		!filepath.IsAbs(inventory.WorkTree) || len(inventory.BuildInputs) == 0 || len(inventory.Files) == 0 {
		return InventoryRecord{}, errors.New("invalid inventory document")
	}
	document, err := json.Marshal(inventory)
	if err != nil {
		return InventoryRecord{}, err
	}
	id := inventoryHash(document)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return InventoryRecord{}, err
	}
	defer tx.Rollback()
	// Acquire the writer lock before reading current state to avoid lost edits.
	if _, err := tx.ExecContext(ctx, "UPDATE inventory_state SET current_inventory_id = current_inventory_id WHERE singleton = 1"); err != nil {
		return InventoryRecord{}, err
	}
	if expectedID != nil {
		var current string
		err := tx.QueryRowContext(ctx, "SELECT current_inventory_id FROM inventory_state WHERE singleton = 1").Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return InventoryRecord{}, err
		}
		if current != *expectedID && current != id {
			return InventoryRecord{}, errInventoryCurrentChanged
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inventories(id, snapshot_sha, created_at, document)
        VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, id, inventory.SnapshotSHA, formatTime(now), string(document)); err != nil {
		return InventoryRecord{}, fmt.Errorf("save inventory: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO inventory_state(singleton, current_inventory_id) VALUES(1, ?)
        ON CONFLICT(singleton) DO UPDATE SET current_inventory_id = excluded.current_inventory_id`, id); err != nil {
		return InventoryRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return InventoryRecord{}, err
	}
	return store.Inventory(ctx, id)
}

func (store *inventoryStore) CurrentInventoryID(ctx context.Context) (string, error) {
	query := "SELECT current_inventory_id FROM inventory_state WHERE singleton = 1"
	if store.version == 1 {
		query = "SELECT id FROM inventories ORDER BY rowid DESC LIMIT 1"
	}
	var id string
	err := store.db.QueryRowContext(ctx, query).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (store *inventoryStore) Inventory(ctx context.Context, selector string) (InventoryRecord, error) {
	query := "SELECT id, created_at, reviewed_at, document FROM inventories"
	var args []any
	if selector == "" || selector == "current" {
		if store.version == 1 {
			query += " ORDER BY rowid DESC LIMIT 1"
		} else {
			query += " WHERE id = (SELECT current_inventory_id FROM inventory_state WHERE singleton = 1)"
		}
	} else if selector == "latest" {
		query += " ORDER BY rowid DESC LIMIT 1"
	} else {
		if len(selector) < 4 || len(selector) > 64 || strings.Trim(selector, "0123456789abcdef") != "" {
			return InventoryRecord{}, errors.New("inventory ID must be current, latest, or a hexadecimal prefix of at least four characters")
		}
		query += " WHERE id LIKE ? ORDER BY rowid DESC LIMIT 2"
		args = append(args, selector+"%")
	}
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return InventoryRecord{}, err
	}
	defer rows.Close()
	var record InventoryRecord
	count := 0
	for rows.Next() {
		count++
		if count > 1 {
			return InventoryRecord{}, fmt.Errorf("inventory prefix %q is ambiguous", selector)
		}
		var created, document string
		var reviewed sql.NullString
		if err := rows.Scan(&record.ID, &created, &reviewed, &document); err != nil {
			return InventoryRecord{}, err
		}
		if inventoryHash([]byte(document)) != record.ID {
			return InventoryRecord{}, errors.New("stored inventory content does not match its ID")
		}
		if err := json.Unmarshal([]byte(document), &record.Inventory); err != nil {
			return InventoryRecord{}, err
		}
		if record.Inventory.FormatVersion != inventoryFormatVersion {
			return InventoryRecord{}, fmt.Errorf("unsupported inventory format %d", record.Inventory.FormatVersion)
		}
		if record.CreatedAt, record.ReviewedAt, err = inventoryTimes(created, reviewed); err != nil {
			return InventoryRecord{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return InventoryRecord{}, err
	}
	if count == 0 {
		return InventoryRecord{}, fmt.Errorf("inventory %q not found", selector)
	}
	return record, nil
}

func (store *inventoryStore) ListInventories(ctx context.Context) ([]InventoryListing, error) {
	query := "SELECT id, snapshot_sha, created_at, reviewed_at, id = (SELECT current_inventory_id FROM inventory_state WHERE singleton = 1) FROM inventories ORDER BY rowid DESC"
	if store.version == 1 {
		query = "SELECT id, snapshot_sha, created_at, reviewed_at, id = (SELECT id FROM inventories ORDER BY rowid DESC LIMIT 1) FROM inventories ORDER BY rowid DESC"
	}
	rows, err := store.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []InventoryListing{}
	for rows.Next() {
		var item InventoryListing
		var created string
		var reviewed sql.NullString
		if err := rows.Scan(&item.ID, &item.SnapshotSHA, &created, &reviewed, &item.Current); err != nil {
			return nil, err
		}
		if item.CreatedAt, item.ReviewedAt, err = inventoryTimes(created, reviewed); err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	return list, rows.Err()
}

func (store *inventoryStore) MarkInventoryReviewed(ctx context.Context, id string, now time.Time) error {
	result, err := store.db.ExecContext(ctx, "UPDATE inventories SET reviewed_at = COALESCE(reviewed_at, ?) WHERE id = ?", formatTime(now), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return fmt.Errorf("inventory %q not found", id)
	}
	return err
}

func inventoryTimes(created string, reviewed sql.NullString) (time.Time, *time.Time, error) {
	createdAt, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return time.Time{}, nil, err
	}
	if !reviewed.Valid {
		return createdAt, nil, nil
	}
	reviewedAt, err := time.Parse(time.RFC3339Nano, reviewed.String)
	return createdAt, &reviewedAt, err
}
