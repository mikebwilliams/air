package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInventorySchemaMigrationPreservesDocumentsAndReview(t *testing.T) {
	ctx := context.Background()
	repository, _ := newInventoryFixture(t)
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	document, _ := json.Marshal(inventory)
	id := inventoryHash(document)
	filename := filepath.Join(t.TempDir(), "repose.sqlite")
	old, err := openStoreFile(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.ExecContext(ctx, reposeSchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now())
	if _, err := old.db.ExecContext(ctx, "INSERT INTO inventories(id, snapshot_sha, created_at, reviewed_at, document) VALUES(?, ?, ?, ?, ?)", id, inventory.SnapshotSHA, now, now, string(document)); err != nil {
		t.Fatal(err)
	}
	old.Close()
	reader, err := openInventoryReadOnly(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	if reader.version != 1 {
		t.Fatal("read-only inspection migrated the database")
	}
	first, err := reader.Inventory(ctx, "current")
	if err != nil || first.ID != id || first.ReviewedAt == nil {
		t.Fatalf("legacy read: %+v, %v", first, err)
	}
	reader.Close()
	store, err := openInventoryStore(ctx, filename, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.version != 7 {
		t.Fatal("writer did not upgrade policy storage")
	}
	current, err := store.Inventory(ctx, "current")
	if err != nil || current.ID != id || current.ReviewedAt == nil || !current.ReviewedAt.Equal(*first.ReviewedAt) {
		t.Fatalf("migration changed inventory: %+v, %v", current, err)
	}
	stored, _ := json.Marshal(current.Inventory)
	if string(stored) != string(document) {
		t.Fatal("migration rewrote inventory content")
	}
}

func TestInventoryReadOnlyInspectionWithReadOnlyDirectoryAndLiveWriter(t *testing.T) {
	ctx := context.Background()
	repository, _ := newInventoryFixture(t)
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	directory := t.TempDir()
	filename := filepath.Join(directory, "repose.sqlite")
	store, err := openInventoryStore(ctx, filename, true)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.SaveInventory(ctx, inventory, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filename + suffix); err != nil {
			t.Fatalf("missing persistent SQLite sidecar %s: %v", suffix, err)
		}
	}
	before, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(filename+suffix, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	defer os.Chmod(directory, 0o700)
	reader, err := openInventoryReadOnly(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reader.Inventory(ctx, "current")
	if err != nil || loaded.ID != record.ID {
		t.Fatalf("read-only inspection: %+v, %v", loaded, err)
	}
	if err := reader.MarkInventoryReviewed(ctx, loaded.ID, time.Now()); err == nil {
		t.Fatal("read-only store allowed an update")
	}
	reader.Close()
	after, err := os.ReadFile(filename)
	if err != nil || string(before) != string(after) {
		t.Fatal("inspection changed database bytes")
	}
	// Restore writes and hold a transaction open. Inspectors must not acquire a
	// writer lock or miss a committed result that still resides in the WAL.
	os.Chmod(directory, 0o700)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Chmod(filename+suffix, 0o600)
	}
	store, err = openInventoryStore(ctx, filename, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.MarkInventoryReviewed(ctx, record.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer store.db.ExecContext(ctx, "ROLLBACK")
	readContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	reader, err = openInventoryReadOnly(readContext, filename)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	loaded, err = reader.Inventory(readContext, "current")
	if err != nil || loaded.ReviewedAt == nil {
		t.Fatalf("reader blocked or ignored committed WAL data: %+v, %v", loaded, err)
	}
}

func TestStaleInventoryUpdateCannotOverwritePolicyOrLeaveVersion(t *testing.T) {
	ctx := context.Background()
	repository, _ := newInventoryFixture(t)
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	store, err := openInventoryStore(ctx, filepath.Join(t.TempDir(), "repose.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.SaveInventory(ctx, inventory, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := editInventoryScope(inventory, []string{"pcbnew"}, true, "Outside scope")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.saveCurrentInventory(ctx, updated, &first.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.saveCurrentInventory(ctx, updated, &first.ID, time.Now())
	if err != nil || duplicate.ID != second.ID {
		t.Fatalf("identical concurrent update should be idempotent: %v", err)
	}
	stale, _, _ := editInventoryScope(inventory, []string{"thirdparty"}, false, "")
	if _, err := store.saveCurrentInventory(ctx, stale, &first.ID, time.Now()); !errors.Is(err, errInventoryCurrentChanged) {
		t.Fatalf("stale edit: %v", err)
	}
	current, err := store.CurrentInventoryID(ctx)
	if err != nil || current != second.ID {
		t.Fatal("stale update replaced current policy")
	}
	list, err := store.ListInventories(ctx)
	if err != nil || len(list) != 2 {
		t.Fatal("stale update left a partially saved inventory")
	}
}
