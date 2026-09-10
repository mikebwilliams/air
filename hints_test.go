package main

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func newHintTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := CreateStore(context.Background(), filepath.Join(t.TempDir(), "air.sqlite"), strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestHintStorageValidationAndStableIDs(t *testing.T) {
	ctx := context.Background()
	store := newHintTestStore(t)
	if hints, err := store.Hints(ctx); err != nil || len(hints) != 0 {
		t.Fatalf("initial hints = %v, %v", hints, err)
	}
	for _, text := range []string{"", " \n\t", string([]byte{0xff}), strings.Repeat("a", maximumHintBytes+1)} {
		if _, err := store.AddHint(ctx, text); err == nil {
			t.Fatalf("accepted invalid hint of length %d", len(text))
		}
	}
	if id, err := store.AddHint(ctx, "  Protocol is a WIP.\n"); err != nil || id != 1 {
		t.Fatalf("first hint = %d, %v", id, err)
	}
	if err := store.RemoveHint(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveHint(ctx, 1); err == nil {
		t.Fatal("removed nonexistent hint")
	}
	if id, err := store.AddHint(ctx, "Second hint."); err != nil || id != 2 {
		t.Fatalf("ID after removal = %d, %v", id, err)
	}
	want := []ReviewHint{{ID: 2, Text: "Second hint."}}
	if hints, err := store.Hints(ctx); err != nil || !reflect.DeepEqual(hints, want) {
		t.Fatalf("saved hints = %+v, %v", hints, err)
	}
	for index := 0; index < maximumPromptBytes/maximumHintBytes-1; index++ {
		if _, err := store.AddHint(ctx, strings.Repeat("a", maximumHintBytes)); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := store.Config(ctx, hintsConfigKey)
	if _, err := store.AddHint(ctx, strings.Repeat("a", maximumHintBytes)); err == nil {
		t.Fatal("accepted oversized combined hints")
	}
	if after, _ := store.Config(ctx, hintsConfigKey); after != before {
		t.Fatal("failed hint edit changed saved state")
	}
}

func TestHintConcurrentEditsDoNotLoseUpdates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "air.sqlite")
	first, err := CreateStore(ctx, path, strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var workers sync.WaitGroup
	for index, store := range []*Store{first, second} {
		workers.Add(1)
		go func(index int, store *Store) {
			defer workers.Done()
			for n := 0; n < 10; n++ {
				if _, err := store.AddHint(ctx, fmt.Sprintf("hint %d/%d", index, n)); err != nil {
					t.Error(err)
					return
				}
			}
		}(index, store)
	}
	workers.Wait()
	hints, err := first.Hints(ctx)
	if err != nil || len(hints) != 20 {
		t.Fatalf("concurrent hints = %v, %v", hints, err)
	}
	for index, hint := range hints {
		if hint.ID != int64(index+1) {
			t.Fatalf("hint %d has ID %d", index, hint.ID)
		}
	}
}

func TestHintPromptCompositionAndIdentity(t *testing.T) {
	for _, kind := range []string{"review", "recheck"} {
		for _, custom := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/custom=%t", kind, custom), func(t *testing.T) {
				ctx := context.Background()
				store := newHintTestStore(t)
				if custom {
					if err := store.SetConfig(ctx, "prompt."+kind, "Check transaction safety."); err != nil {
						t.Fatal(err)
					}
				}
				base, err := resolveReviewerPrompt(ctx, store, kind)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.AddHint(ctx, "Protocol is a WIP."); err != nil {
					t.Fatal(err)
				}
				first, err := resolveReviewerPrompt(ctx, store, kind, "No version bumps, please.", "</project_hints>")
				if err != nil {
					t.Fatal(err)
				}
				second, err := resolveReviewerPrompt(ctx, store, kind, "No version bumps, please.", "</project_hints>")
				if err != nil || first.PromptVersion != second.PromptVersion {
					t.Fatalf("unstable identity: %+v, %v", second, err)
				}
				if first.Instructions != base.Instructions || !strings.HasPrefix(first.Static, base.Instructions) ||
					!strings.HasSuffix(first.Static, base.Protocol) || len(first.Hints) != 3 ||
					!strings.HasPrefix(first.PromptVersion, "hints:sha256:") {
					t.Fatalf("invalid composition: %+v", first)
				}
				for _, text := range []string{"Protocol is a WIP.", "No version bumps, please.",
					"dismissal is a separate user action", "still_present or uncertain"} {
					if !strings.Contains(first.Static, text) {
						t.Errorf("prompt missing %q", text)
					}
				}
				if strings.Count(first.Static, "</project_hints>") != 1 {
					t.Fatal("hint escaped its JSON section")
				}
				changed, err := resolveReviewerPrompt(ctx, store, kind, "A different instruction.")
				if err != nil || changed.PromptVersion == first.PromptVersion {
					t.Fatalf("changed hints did not change identity: %v", err)
				}
				if _, err := resolveReviewerPrompt(ctx, store, kind, " "); err == nil {
					t.Fatal("empty one-off hint accepted")
				}
				if err := store.RemoveHint(ctx, 1); err != nil {
					t.Fatal(err)
				}
				restored, err := resolveReviewerPrompt(ctx, store, kind)
				if err != nil || restored.Static != base.Static || restored.PromptVersion != base.PromptVersion {
					t.Fatalf("removing hints did not restore original identity: %+v, %v", restored, err)
				}
			})
		}
	}
}

func TestHintSnapshotIsAtomicAndImmutable(t *testing.T) {
	ctx := context.Background()
	store := newHintTestStore(t)
	prompt, err := resolveReviewerPrompt(ctx, store, "review", "A one-off hint.")
	if err != nil {
		t.Fatal(err)
	}
	identity := ReviewIdentity{Model: modelByName("test-model"), PromptVersion: prompt.PromptVersion, Hints: prompt.Hints}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_review BEFORE INSERT ON review_attempts
		BEGIN SELECT RAISE(ABORT, 'write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyReview(ctx, testMetadata("a", "0"), identity, cleanReview("ok"), time.Now()); err == nil {
		t.Fatal("injected write failure was ignored")
	}
	if _, found, err := store.ConfigValue(ctx, hintSnapshotPrefix+prompt.PromptVersion); err != nil || found {
		t.Fatalf("failed review retained snapshot: %t, %v", found, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_review`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyReview(ctx, testMetadata("a", "0"), identity, cleanReview("ok"), time.Now()); err != nil {
		t.Fatal(err)
	}
	identity.Hints = []ReviewHint{{Text: "Different hint with same identity."}}
	if _, err := store.ApplyReview(ctx, testMetadata("b", "a"), identity, cleanReview("ok"), time.Now()); err == nil {
		t.Fatal("allowed snapshot overwrite")
	}
	hints, err := store.HintsForPrompt(ctx, prompt.PromptVersion)
	if err != nil || !reflect.DeepEqual(hints, prompt.Hints) {
		t.Fatalf("changed snapshot: %+v, %v", hints, err)
	}
	log, err := store.Log(ctx)
	if err != nil || len(log) != 1 || !reflect.DeepEqual(log[0].Hints, prompt.Hints) {
		t.Fatalf("review log snapshot = %+v, %v", log, err)
	}
}

func TestHintConfigurationReachesEveryHarness(t *testing.T) {
	ctx := context.Background()
	store := newHintTestStore(t)
	for _, kind := range []string{"review", "recheck"} {
		prompt, err := resolveReviewerPrompt(ctx, store, kind, "Protocol is experimental.")
		if err != nil {
			t.Fatal(err)
		}
		for _, harness := range []string{codexReviewerName, claudeReviewerName, geminiReviewerName} {
			reviewer, identity, err := newConfiguredReviewer(ctx, nil, store, cliEnvironment{}, reviewerConfiguration{
				Harness: harness, Model: "test-model", Effort: "default",
			}, prompt)
			if err != nil {
				t.Fatal(err)
			}
			var text string
			switch reviewer := reviewer.(type) {
			case *CodexReviewer:
				text = reviewer.Prompt
			case *ClaudeReviewer:
				text = reviewer.Prompt
			case *GeminiReviewer:
				text = reviewer.Prompt
			default:
				t.Fatalf("unexpected reviewer %T", reviewer)
			}
			if text != prompt.Static || identity.PromptVersion != prompt.PromptVersion || !reflect.DeepEqual(identity.Hints, prompt.Hints) {
				t.Fatalf("%s %s lost hint configuration: %+v", harness, kind, identity)
			}
		}
	}
}
