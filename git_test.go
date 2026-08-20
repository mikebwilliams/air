package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRepositoryStateLivesUnderPrivateAirDirectory(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	stateDirectory := filepath.Join(repository.CommonDir, "air")
	if got, want := repository.DatabasePath(), filepath.Join(stateDirectory, "reviews.sqlite"); got != want {
		t.Fatalf("database path = %q, want %q", got, want)
	}
	if got, want := repository.LockPath(), filepath.Join(stateDirectory, "scan.lock"); got != want {
		t.Fatalf("lock path = %q, want %q", got, want)
	}
	store, err := CreateStore(context.Background(), repository.DatabasePath(), base)
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	lock, err := acquireScanLock(repository.LockPath())
	if err != nil {
		t.Fatalf("acquireScanLock: %v", err)
	}
	defer lock.Close()

	if runtime.GOOS != "windows" {
		for name, expectedMode := range map[string]os.FileMode{
			stateDirectory:            0o700,
			repository.DatabasePath(): 0o600,
			repository.LockPath():     0o600,
		} {
			info, err := os.Stat(name)
			if err != nil {
				t.Fatalf("stat %s: %v", name, err)
			}
			if mode := info.Mode().Perm(); mode != expectedMode {
				t.Errorf("mode of %s = %04o, want %04o", name, mode, expectedMode)
			}
		}
	}
}

func TestEnumerateDefaultAndExplicitRange(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "app.txt", []byte("base\n"), "base")
	second := testCommitFile(t, directory, "app.txt", []byte("base\nsecond\n"), "second")
	third := testCommitFile(t, directory, "app.txt", []byte("base\nsecond\nthird\n"), "third")

	commits, err := repository.EnumerateDefault(context.Background(), base)
	if err != nil {
		t.Fatalf("EnumerateDefault: %v", err)
	}
	assertStrings(t, commits, []string{second, third})

	commits, err = repository.EnumerateRange(context.Background(), shortSHA(base)+".."+shortSHA(second))
	if err != nil {
		t.Fatalf("EnumerateRange: %v", err)
	}
	assertStrings(t, commits, []string{second})

	if _, err := repository.EnumerateRange(context.Background(), third+".."+base); err == nil {
		t.Fatal("reverse range unexpectedly succeeded")
	}
}

func TestFirstParentEnumerationExcludesMergedBranchCommits(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "base.txt", []byte("base\n"), "base")
	testGit(t, directory, "switch", "-c", "feature")
	feature := testCommitFile(t, directory, "feature.txt", []byte("feature\n"), "feature")
	testGit(t, directory, "switch", "master")
	mainline := testCommitFile(t, directory, "main.txt", []byte("main\n"), "mainline")
	testGit(t, directory, "merge", "--no-ff", "feature", "-m", "merge feature")
	merge := strings.TrimSpace(testGit(t, directory, "rev-parse", "HEAD"))

	commits, err := repository.EnumerateDefault(context.Background(), base)
	if err != nil {
		t.Fatalf("EnumerateDefault: %v", err)
	}
	assertStrings(t, commits, []string{mainline, merge})
	for _, sha := range commits {
		if sha == feature {
			t.Fatal("feature commit appeared in first-parent enumeration")
		}
	}
	metadata, err := repository.CommitMetadata(context.Background(), merge)
	if err != nil {
		t.Fatalf("CommitMetadata: %v", err)
	}
	if metadata.ParentSHA != mainline {
		t.Fatalf("merge parent = %s, want %s", metadata.ParentSHA, mainline)
	}
	diff, err := repository.CommitDiff(context.Background(), metadata.ParentSHA, merge)
	if err != nil {
		t.Fatalf("CommitDiff: %v", err)
	}
	if !strings.Contains(diff.Text, "feature.txt") {
		t.Fatalf("merge diff does not contain merged change:\n%s", diff.Text)
	}
}

func TestCommitDiffFiltersBinaryAndCapsLargeText(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "base.txt", []byte("base\n"), "base")

	if err := writeTestFile(directory, "base.txt", []byte("base\ntext change\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(directory, "image.bin", []byte{0, 1, 2, 3, 0, 4}); err != nil {
		t.Fatal(err)
	}
	testGit(t, directory, "add", "--", "base.txt", "image.bin")
	testGit(t, directory, "commit", "-m", "mixed")
	mixed := strings.TrimSpace(testGit(t, directory, "rev-parse", "HEAD"))
	diff, err := repository.CommitDiff(context.Background(), base, mixed)
	if err != nil {
		t.Fatalf("CommitDiff mixed: %v", err)
	}
	if !strings.Contains(diff.Text, "text change") {
		t.Fatalf("textual change missing:\n%s", diff.Text)
	}
	if strings.Contains(diff.Text, "image.bin") {
		t.Fatalf("binary path leaked into textual diff:\n%s", diff.Text)
	}
	assertStrings(t, diff.TextFiles, []string{"base.txt"})
	assertStrings(t, diff.BinaryFiles, []string{"image.bin"})

	large := bytes.Repeat([]byte("0123456789abcdef\n"), 20_000)
	largeSHA := testCommitFile(t, directory, "large.txt", large, "large")
	diff, err = repository.CommitDiff(context.Background(), mixed, largeSHA)
	if err != nil {
		t.Fatalf("CommitDiff large: %v", err)
	}
	if !diff.Oversized {
		t.Fatal("large textual diff was not marked oversized")
	}
	if len(diff.Text) != maxDiffBytes {
		t.Fatalf("captured diff length = %d, want %d", len(diff.Text), maxDiffBytes)
	}
	assertStrings(t, diff.TextFiles, []string{"large.txt"})
}

func TestCommitDiffDoesNotRunTextconv(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	script := filepath.Join(directory, "textconv.sh")
	marker := filepath.Join(directory, "textconv-ran")
	scriptContents := []byte("#!/bin/sh\ntouch \"" + marker + "\"\ncat \"$1\"\n")
	if err := os.WriteFile(script, scriptContents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(directory, ".gitattributes", []byte("*.special diff=evil\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(directory, "value.special", []byte("before\n")); err != nil {
		t.Fatal(err)
	}
	testGit(t, directory, "config", "diff.evil.textconv", script)
	testGit(t, directory, "add", "--", ".gitattributes", "value.special")
	testGit(t, directory, "commit", "-m", "base")
	base := strings.TrimSpace(testGit(t, directory, "rev-parse", "HEAD"))
	head := testCommitFile(t, directory, "value.special", []byte("after\n"), "change")

	diff, err := repository.CommitDiff(context.Background(), base, head)
	if err != nil {
		t.Fatalf("CommitDiff: %v", err)
	}
	if !strings.Contains(diff.Text, "after") {
		t.Fatalf("missing ordinary diff:\n%s", diff.Text)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository textconv command was executed; stat error = %v", err)
	}
}

func TestSplitTwoDotRange(t *testing.T) {
	from, to, err := splitTwoDotRange("a..b")
	if err != nil || from != "a" || to != "b" {
		t.Fatalf("splitTwoDotRange = %q, %q, %v", from, to, err)
	}
	for _, invalid := range []string{"a", "a...b", "..b", "a..", "a..b..c"} {
		if _, _, err := splitTwoDotRange(invalid); err == nil {
			t.Errorf("splitTwoDotRange(%q) unexpectedly succeeded", invalid)
		}
	}
}

func assertStrings(t *testing.T, actual, expected []string) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("got %v, want %v", actual, expected)
	}
	for i := range actual {
		if actual[i] != expected[i] {
			t.Fatalf("got %v, want %v", actual, expected)
		}
	}
}

func writeTestFile(directory, name string, data []byte) error {
	return os.WriteFile(filepath.Join(directory, name), data, 0o644)
}
