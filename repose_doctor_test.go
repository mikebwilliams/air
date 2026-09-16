package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestReposeDoctorHealthyStateAndJSON(t *testing.T) {
	repository, _, _, _ := auditFixture(t)
	ctx := context.Background()
	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: repository.WorkTree, Stdout: &stdout, Stderr: io.Discard}
	if err := runReposeCLI(ctx, []string{"doctor", "--json"}, environment); err != nil {
		t.Fatalf("doctor: %v\n%s", err, stdout.String())
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor report: %v\n%s", err, stdout.String())
	}
	if !report.OK || report.Failed != 0 || report.Passed == 0 || report.Warnings == 0 || report.DatabasePath == "" {
		t.Fatalf("doctor report = %+v", report)
	}
	want := map[string]string{
		"repository":         "pass",
		"database":           "pass",
		"database integrity": "pass",
		"foreign keys":       "pass",
		"inventory history":  "pass",
		"current inventory":  "pass",
		"inventory approval": "pass",
		"checkout":           "pass",
		"semantic index":     "warn",
		"scan configuration": "pass",
		"scan state":         "pass",
		"runner codex":       "pass",
		"model pricing":      "warn",
	}
	for name, status := range want {
		check, found := reposeDoctorCheck(report, name)
		if !found || check.Status != status {
			t.Errorf("check %q = %+v, found=%t; want %s", name, check, found, status)
		}
	}

	stdout.Reset()
	if err := runReposeCLI(ctx, []string{"doctor"}, environment); err != nil {
		t.Fatal(err)
	}
	if output := stdout.String(); !strings.Contains(output, "PASS database integrity") ||
		!strings.Contains(output, "WARN semantic index") || !strings.Contains(output, "Doctor:") {
		t.Fatalf("doctor text output:\n%s", output)
	}
}

func TestReposeDoctorReportsCheckoutAndRunnerFailures(t *testing.T) {
	repository, store, scan, _ := auditFixture(t)
	ctx := context.Background()
	scan.Spec.Model.Binary = filepath.Join(repository.WorkTree, "missing-runner")
	document, err := json.Marshal(scan.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE audit_scans SET document=? WHERE id=?", string(document), scan.ID); err != nil {
		t.Fatal(err)
	}
	writeInventoryFixtureFile(t, filepath.Join(repository.WorkTree, "pcbnew", "main.cpp"), []byte("int changed() { return 1; }\n"))

	var stdout bytes.Buffer
	environment := cliEnvironment{Cwd: repository.WorkTree, Stdout: &stdout, Stderr: io.Discard}
	err = runReposeCLI(ctx, []string{"doctor", "--json"}, environment)
	if err == nil || !strings.Contains(err.Error(), "doctor found 2 failed checks") {
		t.Fatalf("doctor error = %v\n%s", err, stdout.String())
	}
	var report doctorReport
	if decodeErr := json.Unmarshal(stdout.Bytes(), &report); decodeErr != nil {
		t.Fatalf("decode failed doctor report: %v\n%s", decodeErr, stdout.String())
	}
	if report.OK || report.Failed != 2 {
		t.Fatalf("failed doctor report = %+v", report)
	}
	for _, name := range []string{"checkout", "runner codex"} {
		check, found := reposeDoctorCheck(report, name)
		if !found || check.Status != "fail" {
			t.Errorf("check %q = %+v, found=%t", name, check, found)
		}
	}
}

func reposeDoctorCheck(report doctorReport, name string) (doctorCheck, bool) {
	for _, check := range report.Checks {
		if check.Name == name {
			return check, true
		}
	}
	return doctorCheck{}, false
}

func TestReposeDoctorParsesClangdMajorVersion(t *testing.T) {
	for _, test := range []struct {
		version string
		major   int
		found   bool
	}{
		{"Ubuntu clangd version 21.1.8 (6ubuntu1)", 21, true},
		{"clangd version 18.1.3", 18, true},
		{"Apple clangd version 16.0.0", 16, true},
		{"custom language server", 0, false},
	} {
		major, found := reposeDoctorClangdMajor(test.version)
		if major != test.major || found != test.found {
			t.Errorf("reposeDoctorClangdMajor(%q) = %d, %t; want %d, %t", test.version, major, found, test.major, test.found)
		}
	}
}

func TestReposeDoctorMissingRunnerFailsOnlyUnfinishedScans(t *testing.T) {
	for _, test := range []struct {
		required int
		status   string
	}{
		{0, "warn"},
		{1, "fail"},
	} {
		report := doctorReport{}
		reposeDoctorMissingRunner(&report, "runner codex", "/missing", &reposeDoctorRunner{Scans: 2, RequiredScans: test.required}, filepath.ErrBadPattern)
		if len(report.Checks) != 1 || report.Checks[0].Status != test.status {
			t.Fatalf("required=%d report=%+v", test.required, report)
		}
	}
}
