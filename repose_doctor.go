package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func runReposeDoctorCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("doctor", environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	asJSON := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: repose doctor [--json] [--repo DIR]")
	}
	report := inspectReposeDoctor(ctx, reposeAbsolutePath(environment.Cwd, *repoPath))
	report.OK = report.Failed == 0
	if *asJSON {
		if err := writeInventoryJSON(environment.Stdout, report); err != nil {
			return err
		}
	} else {
		for _, check := range report.Checks {
			fmt.Fprintf(environment.Stdout, "%-4s %-22s %s\n", strings.ToUpper(check.Status), check.Name, check.Detail)
		}
		fmt.Fprintf(environment.Stdout, "Doctor: %d passed, %d warnings, %d failed\n", report.Passed, report.Warnings, report.Failed)
	}
	if report.Failed != 0 {
		return fmt.Errorf("doctor found %d failed checks", report.Failed)
	}
	return nil
}

func inspectReposeDoctor(ctx context.Context, repositoryPath string) doctorReport {
	report := doctorReport{Checks: []doctorCheck{}}
	repository, err := DiscoverGitRepository(ctx, repositoryPath)
	if err != nil {
		report.add("repository", "fail", err.Error())
		return report
	}
	report.add("repository", "pass", repository.WorkTree)
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		report.add("database path", "fail", err.Error())
		return report
	}
	report.DatabasePath = database
	stateDirectory := filepath.Dir(database)
	inspectReposeDoctorPermissions(&report, "state directory", stateDirectory, true)
	inspectReposeDoctorPermissions(&report, "database file", database, false)

	store, err := openInventoryReadOnly(ctx, database)
	if err != nil {
		report.add("database", "fail", err.Error())
		return report
	}
	defer store.Close()
	if store.version == reposeCurrentSchemaVersion {
		report.add("database", "pass", fmt.Sprintf("schema %d at %s", store.version, database))
	} else {
		report.add("database", "warn", fmt.Sprintf("schema %d at %s; the next write upgrades to schema %d", store.version, database, reposeCurrentSchemaVersion))
	}
	if err := store.IntegrityCheck(ctx); err != nil {
		report.add("database integrity", "fail", err.Error())
	} else {
		report.add("database integrity", "pass", "SQLite quick_check returned ok")
	}
	if err := validateBackupForeignKeys(ctx, store.Store); err != nil {
		report.add("foreign keys", "fail", strings.ReplaceAll(err.Error(), "backup ", "database "))
	} else {
		report.add("foreign keys", "pass", "all references are valid")
	}

	record, inventoryOK := inspectReposeDoctorInventories(ctx, &report, repository, store)
	if inventoryOK {
		inspectReposeDoctorCheckout(ctx, &report, repository, record)
		inspectReposeDoctorSemantic(ctx, &report, record, store)
	}
	inspectReposeDoctorScans(ctx, &report, store)
	return report
}

func inspectReposeDoctorPermissions(report *doctorReport, name, filename string, directory bool) {
	info, err := os.Stat(filename)
	if err != nil {
		report.add(name, "fail", err.Error())
		return
	}
	if directory && !info.IsDir() {
		report.add(name, "fail", "not a directory: "+filename)
		return
	}
	if !directory && !info.Mode().IsRegular() {
		report.add(name, "fail", "not a regular file: "+filename)
		return
	}
	permissions := info.Mode().Perm()
	want := os.FileMode(0o600)
	if directory {
		want = 0o700
	}
	if permissions&want != want {
		report.add(name, "fail", fmt.Sprintf("%s has permissions %04o; owner requires %04o", filename, permissions, want))
		return
	}
	if permissions&0o077 != 0 {
		report.add(name, "warn", fmt.Sprintf("%s has permissions %04o; expected %04o", filename, permissions, want))
		return
	}
	report.add(name, "pass", filename)
}

func inspectReposeDoctorInventories(
	ctx context.Context,
	report *doctorReport,
	repository *GitRepository,
	store *inventoryStore,
) (InventoryRecord, bool) {
	list, err := store.ListInventories(ctx)
	if err != nil {
		report.add("inventory history", "fail", err.Error())
		return InventoryRecord{}, false
	}
	if len(list) == 0 {
		report.add("inventory history", "fail", "database contains no inventories")
		return InventoryRecord{}, false
	}
	for _, item := range list {
		record, err := store.Inventory(ctx, item.ID)
		if err != nil {
			report.add("inventory history", "fail", fmt.Sprintf("inventory %s: %v", shortSHA(item.ID), err))
			return InventoryRecord{}, false
		}
		if record.Inventory.SnapshotSHA != item.SnapshotSHA {
			report.add("inventory history", "fail", fmt.Sprintf("inventory %s snapshot column does not match its document", shortSHA(item.ID)))
			return InventoryRecord{}, false
		}
		resolved, err := repository.ResolveCommit(ctx, item.SnapshotSHA)
		if err != nil || resolved != item.SnapshotSHA {
			report.add("inventory history", "fail", fmt.Sprintf("inventory %s snapshot %s is unavailable: %v", shortSHA(item.ID), shortSHA(item.SnapshotSHA), err))
			return InventoryRecord{}, false
		}
	}
	report.add("inventory history", "pass", fmt.Sprintf("%d saved %s with available Git snapshots", len(list), statusPlural(len(list), "inventory", "inventories")))
	current, err := store.Inventory(ctx, "current")
	if err != nil {
		report.add("current inventory", "fail", err.Error())
		return InventoryRecord{}, false
	}
	report.add("current inventory", "pass", fmt.Sprintf("%s at snapshot %s", shortSHA(current.ID), shortSHA(current.Inventory.SnapshotSHA)))
	if current.ReviewedAt == nil {
		report.add("inventory approval", "warn", fmt.Sprintf("inventory %s is awaiting review", shortSHA(current.ID)))
	} else {
		report.add("inventory approval", "pass", "approved at "+current.ReviewedAt.Format("2006-01-02 15:04:05Z07:00"))
	}
	return current, true
}

func inspectReposeDoctorCheckout(ctx context.Context, report *doctorReport, repository *GitRepository, record InventoryRecord) {
	if err := checkInventory(ctx, repository, record.Inventory); err != nil {
		report.add("checkout", "fail", err.Error())
		return
	}
	report.add("checkout", "pass", fmt.Sprintf("HEAD and %d build %s match inventory %s", len(record.Inventory.BuildInputs), statusPlural(len(record.Inventory.BuildInputs), "input", "inputs"), shortSHA(record.ID)))
}

func inspectReposeDoctorSemantic(ctx context.Context, report *doctorReport, record InventoryRecord, store *inventoryStore) {
	snapshot, err := store.semanticSnapshot(ctx, record.Inventory, "")
	if err != nil {
		report.add("semantic index", "fail", err.Error())
		return
	}
	if snapshot == nil {
		report.add("semantic index", "warn", "no index for the current snapshot/build; run repose inventory index")
		if binary, err := exec.LookPath("clangd"); err != nil {
			report.add("clangd", "warn", "clangd is not available on PATH")
		} else {
			report.add("clangd", "pass", binary+" is available; no indexed version is recorded")
		}
		return
	}
	selected := map[string]bool{}
	for _, file := range (inventorySelection{Path: ".", Status: "included"}).files(record.Inventory) {
		if file.Kind == "source" || file.Kind == "header" {
			selected[file.Path] = true
		}
	}
	counts := map[string]int{}
	seen := map[string]bool{}
	invalid := []string{}
	for _, file := range snapshot.Files {
		if file.ProfileID != snapshot.Profile.ID {
			invalid = append(invalid, fmt.Sprintf("%s references profile %s", file.Path, shortSHA(file.ProfileID)))
		}
		switch file.Status {
		case "indexed", "partial", "unavailable", "failed":
		default:
			invalid = append(invalid, fmt.Sprintf("%s has status %q", file.Path, file.Status))
		}
		if selected[file.Path] {
			counts[file.Status]++
			seen[file.Path] = true
		}
	}
	pending := len(selected) - len(seen)
	detail := fmt.Sprintf("profile %s: %d indexed, %d partial, %d unavailable, %d failed, %d pending of %d files",
		shortSHA(snapshot.Profile.ID), counts["indexed"], counts["partial"], counts["unavailable"], counts["failed"], pending, len(selected))
	if len(invalid) > 0 {
		report.add("semantic index", "fail", detail+"; invalid results: "+strings.Join(invalid, "; "))
	} else if counts["partial"]+counts["unavailable"]+counts["failed"]+pending > 0 {
		report.add("semantic index", "warn", detail)
	} else {
		report.add("semantic index", "pass", detail)
	}
	inspectReposeDoctorClangd(report, snapshot.Profile)
}

func inspectReposeDoctorClangd(report *doctorReport, profile semanticProfile) {
	info, err := os.Stat(profile.Clangd)
	if err != nil {
		report.add("clangd", "warn", fmt.Sprintf("indexed with %s, which is no longer available: %v", profile.Clangd, err))
		return
	}
	if !info.Mode().IsRegular() {
		report.add("clangd", "warn", profile.Clangd+" is not a regular file")
		return
	}
	digest, err := reposeDoctorFileSHA256(profile.Clangd)
	if err != nil {
		report.add("clangd", "warn", err.Error())
		return
	}
	version := strings.Split(profile.ClangdVersion, "\n")[0]
	issues := []string{}
	if version == "" {
		issues = append(issues, "the indexed version is empty")
	} else if major, found := reposeDoctorClangdMajor(version); found && major < 21 {
		issues = append(issues, fmt.Sprintf("recorded version %d is older than required version 21", major))
	}
	if digest != profile.ClangdSHA256 {
		issues = append(issues, "the binary changed since indexing")
	}
	if len(issues) > 0 {
		report.add("clangd", "warn", fmt.Sprintf("%s; recorded version: %s; %s", profile.Clangd, version, strings.Join(issues, "; ")))
		return
	}
	report.add("clangd", "pass", fmt.Sprintf("%s; %s; binary matches indexed SHA-256", profile.Clangd, version))
}

func reposeDoctorClangdMajor(version string) (int, bool) {
	lower := strings.ToLower(version)
	marker := "clangd version "
	index := strings.Index(lower, marker)
	if index < 0 {
		return 0, false
	}
	value := strings.Fields(version[index+len(marker):])
	if len(value) == 0 {
		return 0, false
	}
	majorText := value[0]
	if separator := strings.IndexAny(majorText, ".-"); separator >= 0 {
		majorText = majorText[:separator]
	}
	major, err := strconv.Atoi(majorText)
	return major, err == nil
}

func reposeDoctorFileSHA256(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filename, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", filename, err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

type reposeDoctorRunner struct {
	Harness       string
	Binary        string
	Scans         int
	RequiredScans int
}

func inspectReposeDoctorScans(ctx context.Context, report *doctorReport, store *inventoryStore) {
	scans, err := store.audits(ctx)
	if err != nil {
		report.add("scan state", "fail", err.Error())
		return
	}
	if len(scans) == 0 {
		report.add("scan state", "warn", "no saved scans")
		report.add("model pricing", "warn", "no saved scan models to inspect")
		return
	}
	scanStatuses, taskStatuses := map[string]int{}, map[string]int{}
	runners := map[string]*reposeDoctorRunner{}
	models := map[string]bool{}
	configurationErrors := []string{}
	stateErrors := []string{}
	for _, scan := range scans {
		scanStatuses[scan.Status]++
		if !validReposeDoctorScanStatus(scan.Status) {
			stateErrors = append(stateErrors, fmt.Sprintf("scan %s has status %q", shortSHA(scan.ID), scan.Status))
		}
		for status, count := range scan.Counts {
			taskStatuses[status] += count
			if !validReposeDoctorTaskStatus(status) {
				stateErrors = append(stateErrors, fmt.Sprintf("scan %s has %d assignments with status %q", shortSHA(scan.ID), count, status))
			}
		}
		config := scan.Spec.Model
		if config.Harness != "codex" && config.Harness != "claude" && config.Harness != "gemini" {
			configurationErrors = append(configurationErrors, fmt.Sprintf("scan %s has unknown harness %q", shortSHA(scan.ID), config.Harness))
		}
		if config.Model == "" || config.Effort == "" || config.Binary == "" || config.Timeout <= 0 {
			configurationErrors = append(configurationErrors, fmt.Sprintf("scan %s has incomplete model configuration", shortSHA(scan.ID)))
		}
		if config.Harness == "gemini" && config.Effort != "default" {
			configurationErrors = append(configurationErrors, fmt.Sprintf("scan %s uses unsupported Gemini effort %q", shortSHA(scan.ID), config.Effort))
		}
		key := config.Harness + "\x00" + config.Binary
		if runners[key] == nil {
			runners[key] = &reposeDoctorRunner{Harness: config.Harness, Binary: config.Binary}
		}
		runners[key].Scans++
		if scan.Status != "invalid" && scan.Counts["pending"]+scan.Counts["running"]+scan.Counts["failed"] > 0 {
			runners[key].RequiredScans++
		}
		models[config.Model] = true
	}
	if len(configurationErrors) > 0 {
		report.add("scan configuration", "fail", strings.Join(configurationErrors, "; "))
	} else {
		report.add("scan configuration", "pass", fmt.Sprintf("%d saved scan specifications are valid", len(scans)))
	}
	detail := fmt.Sprintf("%d scans (%s); assignments: %s", len(scans), reposeDoctorCounts(scanStatuses), reposeDoctorCounts(taskStatuses))
	if len(stateErrors) > 0 {
		report.add("scan state", "fail", detail+"; "+strings.Join(stateErrors, "; "))
	} else if taskStatuses["running"] > 0 {
		report.add("scan state", "warn", detail+"; running assignments may be active or abandoned")
	} else if taskStatuses["failed"] > 0 {
		report.add("scan state", "warn", detail+"; failed assignments require --retry-failed")
	} else {
		report.add("scan state", "pass", detail)
	}
	inspectReposeDoctorRunners(report, runners)
	inspectReposeDoctorPricing(report, models)
}

func validReposeDoctorScanStatus(status string) bool {
	switch status {
	case "pending", "running", "paused", "completed", "incomplete", "invalid":
		return true
	default:
		return false
	}
}

func validReposeDoctorTaskStatus(status string) bool {
	switch status {
	case "pending", "running", "completed", "unable_to_assess", "failed":
		return true
	default:
		return false
	}
}

func inspectReposeDoctorRunners(report *doctorReport, runners map[string]*reposeDoctorRunner) {
	keys := make([]string, 0, len(runners))
	for key := range runners {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		runner := runners[key]
		name := "runner " + runner.Harness
		filename := runner.Binary
		if !filepath.IsAbs(filename) {
			resolved, err := exec.LookPath(filename)
			if err != nil {
				reposeDoctorMissingRunner(report, name, filename, runner, err)
				continue
			}
			filename = resolved
		}
		info, err := os.Stat(filename)
		if err != nil {
			reposeDoctorMissingRunner(report, name, filename, runner, err)
			continue
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			reposeDoctorMissingRunner(report, name, filename, runner, errors.New("not an executable regular file"))
			continue
		}
		report.add(name, "pass", fmt.Sprintf("%s; used by %d %s", filename, runner.Scans, statusPlural(runner.Scans, "scan", "scans")))
	}
}

func reposeDoctorMissingRunner(report *doctorReport, name, filename string, runner *reposeDoctorRunner, problem error) {
	status := "warn"
	detail := fmt.Sprintf("%s is unavailable but is referenced only by %d historical scans: %v", filename, runner.Scans, problem)
	if runner.RequiredScans > 0 {
		status = "fail"
		detail = fmt.Sprintf("%s is unavailable and is required by %d unfinished scans: %v", filename, runner.RequiredScans, problem)
	}
	report.add(name, status, detail)
}

func inspectReposeDoctorPricing(report *doctorReport, models map[string]bool) {
	names := make([]string, 0, len(models))
	unknown := []string{}
	for name := range models {
		names = append(names, name)
		if modelByName(name).Pricing == nil {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(names)
	sort.Strings(unknown)
	if len(unknown) > 0 {
		report.add("model pricing", "warn", fmt.Sprintf("unknown for %s; stats remain available but token-price cost estimates are incomplete", strings.Join(unknown, ", ")))
		return
	}
	report.add("model pricing", "pass", fmt.Sprintf("built-in pricing is available for %s (snapshot %s)", strings.Join(names, ", "), openAIPricingSnapshotDay))
}

func reposeDoctorCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key, count := range counts {
		if count > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[key], strings.ReplaceAll(key, "_", " ")))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
