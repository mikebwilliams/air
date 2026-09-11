package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConcurrentInventoryBuildersSaveOneVersion(t *testing.T) {
	ctx := context.Background()
	repository, _ := newInventoryFixture(t)
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	filename, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	results := make(chan error, 4)
	for range 4 {
		workers.Go(func() {
			store, err := openInventoryStore(ctx, filename, true)
			if err != nil {
				results <- err
				return
			}
			defer store.Close()
			_, err = store.SaveInventory(ctx, inventory, time.Now())
			results <- err
		})
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := openInventoryStore(ctx, filename, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	list, err := store.ListInventories(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("concurrent save produced %d versions: %v", len(list), err)
	}
}

func TestInventoryVersionsSurviveReopenAndApproval(t *testing.T) {
	ctx := context.Background()
	repository, _ := newInventoryFixture(t)
	inventory := mustBuildInventory(t, repository, InventoryPolicy{})
	filename, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openInventoryStore(ctx, filename, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	first, err := store.SaveInventory(ctx, inventory, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkInventoryReviewed(ctx, first.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	again, err := store.SaveInventory(ctx, inventory, now.Add(time.Hour))
	if err != nil || again.ID != first.ID || !again.CreatedAt.Equal(now) || again.ReviewedAt == nil {
		t.Fatalf("idempotent build = %+v, %v", again, err)
	}
	policy := InventoryPolicy{Rules: []InventoryRule{{Prefix: "pcbnew", Note: "Preserve undo invariants."}}}
	updated := mustBuildInventory(t, repository, policy)
	second, err := store.SaveInventory(ctx, updated, now.Add(time.Hour))
	if err != nil || second.ID == first.ID || second.ReviewedAt != nil || second.Inventory.SnapshotSHA != first.Inventory.SnapshotSHA {
		t.Fatalf("new policy version = %+v, %v", second, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openInventoryStore(ctx, filename, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	latest, err := store.Inventory(ctx, "latest")
	if err != nil || latest.ID != second.ID {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	old, err := store.Inventory(ctx, first.ID[:12])
	if err != nil || len(old.Inventory.Policy.Rules) != 0 || old.ReviewedAt == nil {
		t.Fatalf("original changed = %+v, %v", old, err)
	}
	list, err := store.ListInventories(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	for _, selector := range []string{"%", "___", "not-an-id", "f00d"} {
		if _, err := store.Inventory(ctx, selector); err == nil {
			t.Errorf("accepted nonexistent/invalid selector %q", selector)
		}
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE inventories SET document = '{}' WHERE id = ?", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Inventory(ctx, first.ID); err == nil {
		t.Fatal("accepted corrupted inventory")
	}
}

func TestInventoryStateIsSeparateFromAIRAndOtherWorktrees(t *testing.T) {
	ctx := context.Background()
	repository, directory := newInventoryFixture(t)
	primary, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if primary == repository.DatabasePath() {
		t.Fatal("Repose shares AIR database")
	}
	checkout := filepath.Join(t.TempDir(), "scan")
	testGit(t, directory, "worktree", "add", "--detach", checkout)
	scan, err := DiscoverGitRepository(ctx, checkout)
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := reposeDatabasePath(ctx, scan)
	if err != nil || secondary == primary {
		t.Fatalf("worktree state path = %s, %v", secondary, err)
	}
	if _, err := openInventoryStore(ctx, primary, false); err == nil {
		t.Fatal("reading absent state should not initialize it")
	}
	legacy, err := CreateStore(ctx, repository.DatabasePath(), mustBuildInventory(t, repository, InventoryPolicy{}).SnapshotSHA)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Close()
	if unexpected, err := openInventoryStore(ctx, repository.DatabasePath(), true); err == nil {
		unexpected.Close()
		t.Fatal("accepted AIR schema as Repose state")
	}
	legacy, err = OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		t.Fatalf("legacy database damaged: %v", err)
	}
	legacy.Close()
}
