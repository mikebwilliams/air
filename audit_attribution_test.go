package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestAuditFindingDisplayUsesBlamedLineAge(t *testing.T) {
	observed := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	introduced := observed.Add(-3 * 365 * 24 * time.Hour)
	findings := []Finding{
		{ID: 1, ObservedAt: &observed, Attribution: &FindingAttribution{Status: attributionStatusAttributed, Author: "Ada", AuthoredAt: &introduced}},
		{ID: 2, ObservedAt: &observed},
	}
	display := auditFindingDisplay(findings)
	if display[1].Blame != "Ada" || !display[1].CommitDate.Equal(introduced) {
		t.Fatalf("attributed display = %+v", display[1])
	}
	if display[2].Blame != "" || !display[2].CommitDate.IsZero() {
		t.Fatalf("unattributed display uses finding observation time: %+v", display[2])
	}
}

func TestParseLinePorcelainBlame(t *testing.T) {
	data := `0123456789abcdef0123456789abcdef01234567 4 10 1
author Ada Lovelace
author-mail <ada@example.invalid>
author-time 1700000000
author-tz +0000
summary first
filename source.cpp
	first line
^89abcdef0123456789abcdef0123456789abcdef 8 11 1
author Grace Hopper
author-mail <grace@example.invalid>
author-time 1700000100
author-tz +0000
summary second
filename old.cpp
	second line
`
	got, err := parseLinePorcelainBlame([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[10].Author != "Ada Lovelace" || got[10].AuthorEmail != "ada@example.invalid" ||
		got[10].OriginalLine != 4 || got[10].AuthoredAt == nil ||
		got[11].CommitSHA != "89abcdef0123456789abcdef0123456789abcdef" || got[11].Author != "Grace Hopper" {
		t.Fatalf("parsed blame = %+v", got)
	}
}

func TestFindingAuthorBackfillMigratesAndResumes(t *testing.T) {
	repository, store, scan, _, findings := auditTagFixture(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `
		DROP INDEX audit_finding_attributions_author;
		DROP TABLE audit_finding_attributions;
		PRAGMA user_version=9;`); err != nil {
		t.Fatal(err)
	}
	environment := cliEnvironment{Cwd: repository.WorkTree, Stdout: &bytes.Buffer{}, Stderr: io.Discard}
	if err := runReposeCLI(ctx, []string{"finding", "backfill-authors", "--scan", scan.ID, "--jobs", "2", "--limit", "1", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var first findingAttributionBackfillReport
	if err := json.Unmarshal(environment.Stdout.(*bytes.Buffer).Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Selected != 1 || first.Attributed != 1 || first.Unavailable != 0 || first.Remaining != len(findings)-1 {
		t.Fatalf("first backfill = %+v, findings = %d", first, len(findings))
	}

	environment.Stdout = &bytes.Buffer{}
	if err := runReposeCLI(ctx, []string{"finding", "backfill-authors", "--scan", scan.ID, "--jobs", "2", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var second findingAttributionBackfillReport
	if err := json.Unmarshal(environment.Stdout.(*bytes.Buffer).Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.Selected != len(findings)-1 || second.Attributed != len(findings)-1 || second.Unavailable != 0 || second.Remaining != 0 {
		t.Fatalf("second backfill = %+v, findings = %d", second, len(findings))
	}
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != reposeCurrentSchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	loaded, err := (&auditFindingStore{reader: store, scanID: scan.ID}).AllFindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range loaded {
		if finding.Attribution == nil || finding.Attribution.Status != attributionStatusAttributed ||
			finding.Attribution.Author != "AIR Test" || finding.Attribution.CommitSHA == "" {
			t.Fatalf("finding attribution = %+v", finding.Attribution)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE audit_finding_attributions SET
		status='unavailable',commit_sha='',author='',author_email='',authored_at=NULL,
		original_line=NULL,error='temporary fixture failure' WHERE finding_id=?`, loaded[0].ID); err != nil {
		t.Fatal(err)
	}
	environment.Stdout = &bytes.Buffer{}
	if err := runReposeCLI(ctx, []string{"finding", "backfill-authors", "--scan", scan.ID, "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var skipped findingAttributionBackfillReport
	if err := json.Unmarshal(environment.Stdout.(*bytes.Buffer).Bytes(), &skipped); err != nil || skipped.Selected != 0 || skipped.Retryable != 1 {
		t.Fatalf("default retry selection = %+v, %v", skipped, err)
	}
	environment.Stdout = &bytes.Buffer{}
	if err := runReposeCLI(ctx, []string{"finding", "backfill-authors", "--scan", scan.ID, "--retry-errors", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	var retried findingAttributionBackfillReport
	if err := json.Unmarshal(environment.Stdout.(*bytes.Buffer).Bytes(), &retried); err != nil || retried.Selected != 1 || retried.Attributed != 1 || retried.Retryable != 0 {
		t.Fatalf("retried backfill = %+v, %v", retried, err)
	}

	environment.Stdout = &bytes.Buffer{}
	if err := runReposeCLI(ctx, []string{"finding", "list", "--scan", scan.ID, "--search", "air test", "--json"}, environment); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(environment.Stdout.(*bytes.Buffer).String(), `"author": "AIR Test"`) {
		t.Fatalf("finding search did not include author attribution:\n%s", environment.Stdout.(*bytes.Buffer).String())
	}
}
