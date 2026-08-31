package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const doctorAuthTimeout = 15 * time.Second

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	OK           bool          `json:"ok"`
	DatabasePath string        `json:"database_path,omitempty"`
	Checks       []doctorCheck `json:"checks"`
	Passed       int           `json:"passed"`
	Warnings     int           `json:"warnings"`
	Failed       int           `json:"failed"`
}

func (report *doctorReport) add(name, status, detail string) {
	report.Checks = append(report.Checks, doctorCheck{Name: name, Status: status, Detail: detail})
	switch status {
	case "pass":
		report.Passed++
	case "warn":
		report.Warnings++
	case "fail":
		report.Failed++
	}
}

func runDB(ctx context.Context, args []string, environment cliEnvironment) error {
	positionals, err := parsePositionals("db", args, environment.Stderr)
	if err != nil {
		return err
	}
	if len(positionals) != 1 || positionals[0] != "path" {
		return errors.New("usage: air db path")
	}
	repository, err := DiscoverGitRepository(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	fmt.Fprintln(environment.Stdout, repository.DatabasePath())
	return nil
}

func runDoctor(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("doctor", environment.Stderr)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: air doctor [--json]")
	}
	report := inspectDoctor(ctx, environment)
	report.OK = report.Failed == 0
	if *jsonOutput {
		if err := writeJSON(environment.Stdout, report); err != nil {
			return err
		}
	} else {
		for _, check := range report.Checks {
			fmt.Fprintf(environment.Stdout, "%-4s %-20s %s\n",
				strings.ToUpper(check.Status), check.Name, check.Detail)
		}
		fmt.Fprintf(environment.Stdout, "Doctor: %d passed, %d warnings, %d failed\n",
			report.Passed, report.Warnings, report.Failed)
	}
	if report.Failed != 0 {
		return fmt.Errorf("doctor found %d failed checks", report.Failed)
	}
	return nil
}

func inspectDoctor(ctx context.Context, environment cliEnvironment) doctorReport {
	report := doctorReport{Checks: make([]doctorCheck, 0)}
	repository, err := DiscoverGitRepository(ctx, environment.Cwd)
	if err != nil {
		report.add("repository", "fail", err.Error())
		return report
	}
	report.DatabasePath = repository.DatabasePath()
	report.add("repository", "pass", repository.WorkTree)
	masterSHA, err := repository.MasterSHA(ctx)
	if err != nil {
		report.add("master", "fail", err.Error())
	} else {
		report.add("master", "pass", masterRef+" at "+shortSHA(masterSHA))
	}

	if info, err := os.Stat(repository.StateDirectory()); err != nil {
		report.add("state directory", "fail", err.Error())
	} else if !info.IsDir() {
		report.add("state directory", "fail", "not a directory: "+repository.StateDirectory())
	} else if info.Mode().Perm()&0o077 != 0 {
		report.add("state directory", "warn", fmt.Sprintf("permissions are %04o; expected 0700", info.Mode().Perm()))
	} else {
		report.add("state directory", "pass", repository.StateDirectory())
	}

	store, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		report.add("database", "fail", err.Error())
		return report
	}
	defer store.Close()
	if info, err := os.Stat(repository.DatabasePath()); err != nil {
		report.add("database", "fail", err.Error())
	} else if info.Mode().Perm()&0o077 != 0 {
		report.add("database", "warn", fmt.Sprintf("schema %d; permissions are %04o; expected 0600", schemaVersion, info.Mode().Perm()))
	} else {
		report.add("database", "pass", fmt.Sprintf("schema %d at %s", schemaVersion, repository.DatabasePath()))
	}
	if err := store.IntegrityCheck(ctx); err != nil {
		report.add("database integrity", "fail", err.Error())
	} else {
		report.add("database integrity", "pass", "SQLite quick_check returned ok")
	}

	resolve := func(key string) (resolvedSetting, error) {
		setting, _ := settingByKey(key)
		return resolveSettingValue(ctx, store, environment.Getenv, setting, "", false)
	}
	model, modelOK := doctorRequiredSetting(&report, resolve, "model", "model")
	if modelOK {
		configuredModel, found, err := modelForDisplay(ctx, store, model.Value)
		switch {
		case err != nil:
			report.add("model pricing", "fail", err.Error())
		case !found || configuredModel.Pricing == nil:
			report.add("model pricing", "warn", model.Value+" has unknown pricing; reviews remain usable but cost is unavailable")
		default:
			report.add("model pricing", "pass", fmt.Sprintf("%s prices from %s as of %s",
				model.Value, configuredModel.Pricing.Source, configuredModel.Pricing.AsOf))
		}
	}

	doctorRequiredSetting(&report, resolve, "effort", "reasoning effort")
	if timeout, ok := doctorRequiredSetting(&report, resolve, "codex-timeout", "Codex timeout"); ok {
		report.Checks[len(report.Checks)-1].Detail = timeout.Value + " from " + timeout.Source
	}
	binary, ok := doctorRequiredSetting(&report, resolve, "codex-bin", "Codex executable setting")
	if !ok {
		return report
	}
	lookup := binary.Value
	if strings.ContainsRune(lookup, filepath.Separator) && !filepath.IsAbs(lookup) {
		lookup = filepath.Join(environment.Cwd, lookup)
	}
	resolvedBinary, err := exec.LookPath(lookup)
	if err != nil {
		report.add("Codex executable", "fail", fmt.Sprintf("%s: %v", binary.Value, err))
		return report
	}
	report.add("Codex executable", "pass", resolvedBinary+" from "+binary.Source)
	doctorCodexAuth(ctx, &report, environment, resolvedBinary)
	return report
}

func doctorRequiredSetting(report *doctorReport,
	resolve func(string) (resolvedSetting, error), key, label string,
) (resolvedSetting, bool) {
	value, err := resolve(key)
	if err != nil {
		report.add(label, "fail", err.Error())
		return resolvedSetting{}, false
	}
	if strings.TrimSpace(value.Value) == "" {
		report.add(label, "fail", key+" is unset")
		return value, false
	}
	report.add(label, "pass", value.Value+" from "+value.Source)
	return value, true
}

func doctorCodexAuth(ctx context.Context, report *doctorReport,
	environment cliEnvironment, binary string,
) {
	authContext, cancel := context.WithTimeout(ctx, doctorAuthTimeout)
	defer cancel()
	commandContext := environment.CodexCommand
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	command := commandContext(authContext, binary, "login", "status")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	detail := strings.TrimSpace(output.String())
	if len(detail) > 500 {
		detail = detail[:500] + "…"
	}
	if err != nil {
		if errors.Is(authContext.Err(), context.DeadlineExceeded) {
			detail = "timed out after " + doctorAuthTimeout.String()
		} else if detail == "" {
			detail = err.Error()
		}
		report.add("Codex authentication", "fail", detail)
		return
	}
	if detail == "" {
		detail = "codex login status succeeded"
	}
	report.add("Codex authentication", "pass", detail)
}
