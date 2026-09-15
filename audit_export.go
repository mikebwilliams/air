package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"time"
)

type auditExportFinding struct {
	Finding
	Review *htmlExportReview `json:"review,omitempty"`
	Events []htmlExportEvent `json:"events"`
}

type auditExportReport struct {
	Version     int                  `json:"version"`
	Repository  string               `json:"repository"`
	GeneratedAt string               `json:"generated_at"`
	ScanID      string               `json:"scan_id,omitempty"`
	Findings    []auditExportFinding `json:"findings"`
}

func runAuditExportCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("export", environment.Stderr)
	format := flags.String("format", "", "required output format: json, sarif, or html")
	outputPath := flags.StringP("output", "o", "", "output file relative to the current directory; - means stdout")
	repo := flags.String("repo", ".", "scan checkout")
	scanSelector := flags.String("scan", "", "source scan; latest selects the newest completed original scan")
	path := flags.String("path", ".", "filter findings by source path prefix")
	tagValues := flags.StringArray("tag", nil, "require a finding tag (repeatable; multiple tags use AND)")
	verification := flags.String("verification", "all", "filter latest verdict: all, unchecked, confirmed, false_positive, uncertain")
	includeAll := flags.Bool("all", false, "include dismissed findings; HTML includes them and uses this as its initial display")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*format != "json" && *format != "sarif" && *format != "html") {
		return errors.New("usage: repose export --format <json|sarif|html> [-o FILE] [--scan ID|latest] [--repo DIR]")
	}
	if !validAuditVerificationFilter(*verification) {
		return errors.New("verification must be all, unchecked, confirmed, false_positive, or uncertain")
	}
	tags, err := normalizeFindingTags(*tagValues)
	if err != nil {
		return err
	}
	selection := inventorySelection{Path: *path, Status: "included"}
	if err := selection.validate(); err != nil {
		return err
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repo))
	if err != nil {
		return err
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	reader, err := openInventoryReadOnly(ctx, database)
	if err != nil {
		return err
	}
	defer reader.Close()
	// The reader has a single connection. Keep findings, dispositions, and
	// verification histories consistent while another process publishes work.
	if _, err := reader.db.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer reader.db.ExecContext(context.Background(), "ROLLBACK")
	store := &auditFindingStore{reader: reader, databasePath: database}
	if *scanSelector != "" {
		var scan auditScan
		if *scanSelector == "latest" {
			scan, err = reader.latestAuditForRecheck(ctx)
		} else {
			scan, err = reader.audit(ctx, *scanSelector)
		}
		if err != nil {
			return err
		}
		store.scanID = scan.ID
		if scan.Spec.Recheck != nil {
			store.scanID = scan.Spec.Recheck.SourceScanID
		}
	}
	findings, err := store.AllFindings(ctx)
	if err != nil {
		return err
	}
	report := auditExportReport{Version: 1, Repository: filepath.Base(repository.WorkTree),
		GeneratedAt: environmentNow(environment).Format(time.RFC3339), ScanID: store.scanID, Findings: []auditExportFinding{}}
	selected := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if *format != "html" && !*includeAll && f.DismissedAt != nil {
			continue
		}
		if selection.Path != "." && (f.File == nil || !inventoryPrefixMatches(*f.File, selection.Path)) {
			continue
		}
		if *verification != "all" && auditVerificationOutcome(f) != *verification {
			continue
		}
		if !findingHasTags(f, tags) {
			continue
		}
		selected = append(selected, f)
	}
	reviews, err := loadAuditExportReviews(ctx, reader, selected)
	if err != nil {
		return err
	}
	events, err := loadAuditExportEvents(ctx, reader, selected, store.scanID)
	if err != nil {
		return err
	}
	for _, f := range selected {
		report.Findings = append(report.Findings, auditExportFinding{Finding: f, Review: reviews[f.ID], Events: events[f.ID]})
	}
	if _, err := reader.db.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	output := *outputPath
	if output != "" && output != "-" {
		output = reposeAbsolutePath(environment.Cwd, output)
	}
	if *format == "html" {
		html := htmlExportReport{Version: 2, Title: "Repose Findings Report", Snapshot: true,
			Repository: report.Repository, GeneratedAt: report.GeneratedAt, ScanID: report.ScanID,
			InitialStatus: "open", Verification: *verification, Findings: []htmlExportFinding{}}
		if *includeAll {
			html.InitialStatus = "all"
		}
		for _, f := range report.Findings {
			if err := ctx.Err(); err != nil {
				return err
			}
			preview, previewErr := loadAuditFindingPreview(ctx, repository, f.Finding)
			exported := makeHTMLExportFinding(f.Finding, findingDisplayMetadata{}, FindingReview{}, nil, preview, previewErr)
			exported.Review, exported.Events = f.Review, f.Events
			html.Findings = append(html.Findings, exported)
		}
		return writeCommandOutput(environment.Stdout, output, func(w io.Writer) error { return writeHTMLExportReport(w, html) })
	}
	return writeCommandOutput(environment.Stdout, output, func(w io.Writer) error {
		if *format == "sarif" {
			return writeJSON(w, buildAuditSARIF(report.Findings))
		}
		return writeJSON(w, report)
	})
}

func loadAuditExportReviews(ctx context.Context, reader *inventoryStore, findings []Finding) (map[int64]*htmlExportReview, error) {
	reviews := make(map[int64]*htmlExportReview, len(findings))
	if len(findings) == 0 {
		return reviews, nil
	}
	wantedAttempts := make(map[int64]bool, len(findings))
	wantedScans := make(map[string]bool)
	for _, finding := range findings {
		wantedAttempts[finding.AttemptID] = true
		wantedScans[finding.ScanID] = true
	}
	attemptNumbers := make(map[int64]int, len(wantedAttempts))
	rows, err := reader.db.QueryContext(ctx, "SELECT id,number FROM audit_attempts")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var number int
		if err := rows.Scan(&id, &number); err != nil {
			rows.Close()
			return nil, err
		}
		if wantedAttempts[id] {
			attemptNumbers[id] = number
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	models := make(map[string]auditModelConfig, len(wantedScans))
	for scanID := range wantedScans {
		var model auditModelConfig
		err := reader.db.QueryRowContext(ctx, `SELECT
			COALESCE(json_extract(document,'$.model.harness'),''),
			COALESCE(json_extract(document,'$.model.model'),''),
			COALESCE(json_extract(document,'$.model.effort'),'')
			FROM audit_scans WHERE id=?`, scanID).Scan(&model.Harness, &model.Model, &model.Effort)
		if err != nil {
			return nil, err
		}
		models[scanID] = model
	}
	for _, finding := range findings {
		number, ok := attemptNumbers[finding.AttemptID]
		if !ok {
			return nil, fmt.Errorf("finding %d references unavailable attempt %d", finding.ID, finding.AttemptID)
		}
		if finding.ObservedAt == nil {
			return nil, fmt.Errorf("finding %d has no observation time", finding.ID)
		}
		model := models[finding.ScanID]
		reviews[finding.ID] = &htmlExportReview{Number: number, Harness: model.Harness, Model: model.Model,
			ReasoningEffort: model.Effort, ReviewedAt: finding.ObservedAt.Format(time.RFC3339)}
	}
	return reviews, nil
}

func loadAuditExportEvents(ctx context.Context, reader *inventoryStore, findings []Finding, scanID string) (map[int64][]htmlExportEvent, error) {
	events := make(map[int64][]htmlExportEvent, len(findings))
	wanted := make(map[int64]bool, len(findings))
	for _, finding := range findings {
		wanted[finding.ID] = true
		events[finding.ID] = []htmlExportEvent{}
	}
	if len(findings) == 0 {
		return events, nil
	}
	query := `SELECT e.finding_id,e.action,e.note,e.created_at
		FROM audit_finding_events e JOIN audit_findings f ON f.id=e.finding_id`
	args := []any{}
	if scanID != "" {
		query += " WHERE f.scan_id=?"
		args = append(args, scanID)
	}
	query += " ORDER BY e.id"
	rows, err := reader.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var findingID int64
		var action, note, created string
		if err := rows.Scan(&findingID, &action, &note, &created); err != nil {
			return nil, err
		}
		if !wanted[findingID] {
			continue
		}
		when, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		events[findingID] = append(events[findingID], htmlExportEvent{Action: action, Note: note, CreatedAt: when.Format(time.RFC3339)})
	}
	return events, rows.Err()
}

func buildAuditSARIF(findings []auditExportFinding) sarifLog {
	plain := make([]Finding, 0, len(findings))
	for _, f := range findings {
		plain = append(plain, f.Finding)
	}
	log := buildSARIF(plain)
	run := &log.Runs[0]
	run.Tool.Driver = sarifDriver{Name: "Repose", Rules: []sarifRule{{ID: "REPOSE", ShortDescription: sarifMessage{Text: "Repose repository audit finding"}}}}
	for i, f := range findings {
		result := &run.Results[i]
		result.RuleID = "REPOSE"
		result.Fingerprints = map[string]string{"repose/finding-id": fmt.Sprintf("%s/%s/%s", f.ObservedSHA, f.ScanID, strconv.FormatInt(f.ID, 10))}
		result.Properties = sarifProperties{FindingID: f.ID, ObservedSHA: f.ObservedSHA, ObservedAt: f.ObservedAt,
			ScanID: f.ScanID, TaskID: f.TaskID, AttemptID: f.AttemptID, Disposition: findingDisposition(f.Finding),
			DismissedAt: f.DismissedAt, DismissReason: f.DismissReason, Tags: f.Tags, Review: f.Review, Events: f.Events, Verifications: f.Verifications}
		if f.Symbol != nil {
			result.Properties.Symbol = *f.Symbol
		}
		if f.File != nil {
			result.Locations[0].PhysicalLocation.ArtifactLocation.URI = (&url.URL{Path: filepath.ToSlash(*f.File)}).String()
		}
	}
	return log
}
