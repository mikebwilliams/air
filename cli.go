package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type cliEnvironment struct {
	Cwd          string
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
	Getenv       func(string) string
	HTTPClient   *http.Client
	CodexCommand commandContextFunc
	Now          func() time.Time
}

func runCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		printUsage(environment.Stdout)
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		printUsage(environment.Stdout)
		return nil
	case "init":
		return runInit(ctx, args[1:], environment)
	case "scan":
		return runScan(ctx, args[1:], environment)
	case "retry":
		return runRetry(ctx, args[1:], environment)
	case "failures":
		return runFailures(ctx, args[1:], environment)
	case "pending":
		return runPending(ctx, args[1:], environment)
	case "clean":
		return runClean(ctx, args[1:], environment)
	case "reset":
		return runReset(ctx, args[1:], environment)
	case "config":
		return runConfig(ctx, args[1:], environment)
	case "model":
		return runModel(ctx, args[1:], environment)
	case "stats":
		return runStats(ctx, args[1:], environment)
	case "cost":
		return runCost(ctx, args[1:], environment)
	case "export":
		return runExport(ctx, args[1:], environment)
	case "status":
		return runStatus(ctx, args[1:], environment)
	case "log":
		return runLog(ctx, args[1:], environment)
	case "show":
		return runShow(ctx, args[1:], environment)
	case "finding":
		return runFinding(ctx, args[1:], environment)
	case "rescan":
		return runRescan(ctx, args[1:], environment)
	default:
		return fmt.Errorf("unknown command %q; run air help", args[0])
	}
}

func runStats(ctx context.Context, args []string, environment cliEnvironment) error {
	model, since, err := parseReportingFlags("stats", args, environment.Stderr)
	if err != nil {
		return err
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	reviews, err := store.ReviewStats(ctx, model, since)
	if err != nil {
		return err
	}
	findings, err := store.FindingStats(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(environment.Stdout, "Reviews: %d attempts across %d commits\n", reviews.Attempts, reviews.Commits)
	printTokenTotals(environment.Stdout, reviews.InputTokens, reviews.CachedInputTokens,
		reviews.CacheWriteTokens, reviews.CacheWritesUnreported, reviews.OutputTokens,
		reviews.ReasoningOutputTokens)
	printCostTotals(environment.Stdout, reviews.MinimumCostMicrousd, reviews.MaximumCostMicrousd,
		reviews.PricedAttempts, reviews.UnknownCostAttempts)
	fmt.Fprintf(environment.Stdout, "Repository findings: %d open, %d dismissed, %d resolved\n",
		findings.Open, findings.Dismissed, findings.Resolved)
	fmt.Fprintf(environment.Stdout, "Skipped commits: %d\n", findings.Skipped)
	printReviewGroups(environment.Stdout, reviews.Groups)
	return nil
}

func runCost(ctx context.Context, args []string, environment cliEnvironment) error {
	model, since, err := parseReportingFlags("cost", args, environment.Stderr)
	if err != nil {
		return err
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	reviews, err := store.ReviewStats(ctx, model, since)
	if err != nil {
		return err
	}
	printCostTotals(environment.Stdout, reviews.MinimumCostMicrousd, reviews.MaximumCostMicrousd,
		reviews.PricedAttempts, reviews.UnknownCostAttempts)
	fmt.Fprintf(environment.Stdout, "Reviews: %d attempts across %d commits\n", reviews.Attempts, reviews.Commits)
	printReviewGroups(environment.Stdout, reviews.Groups)
	return nil
}

func parseReportingFlags(command string, args []string, stderr io.Writer) (string, *time.Time, error) {
	flags := newFlagSet(command, stderr)
	model := flags.String("model", "", "include only reviews performed by this model")
	sinceValue := flags.String("since", "", "include reviews on or after YYYY-MM-DD or RFC3339")
	if err := flags.Parse(args); err != nil {
		return "", nil, err
	}
	if flags.NArg() != 0 {
		return "", nil, fmt.Errorf("usage: air %s [--model MODEL] [--since DATE]", command)
	}
	if strings.TrimSpace(*model) != *model {
		return "", nil, errors.New("model must not have leading or trailing whitespace")
	}
	if *sinceValue == "" {
		return *model, nil, nil
	}
	since, err := time.Parse(time.RFC3339, *sinceValue)
	if err != nil {
		since, err = time.Parse("2006-01-02", *sinceValue)
	}
	if err != nil {
		return "", nil, fmt.Errorf("invalid --since %q: use YYYY-MM-DD or RFC3339", *sinceValue)
	}
	return *model, &since, nil
}

func printTokenTotals(output io.Writer, input, cachedInput, cacheWrite int64,
	cacheWritesUnreported int, outputTokens, reasoningOutput int64,
) {
	fmt.Fprintf(output, "Tokens: %d input (%d cached), %d cache writes, %d output (%d reasoning)\n",
		input, cachedInput, cacheWrite, outputTokens, reasoningOutput)
	if cacheWritesUnreported != 0 {
		fmt.Fprintf(output, "Cache writes: unreported by %d attempts\n", cacheWritesUnreported)
	}
}

func printCostTotals(output io.Writer, minimum, maximum int64, priced, unknown int) {
	fmt.Fprintf(output, "Estimated cost: %s", formatCostRange(minimum, maximum))
	if unknown != 0 {
		fmt.Fprintf(output, " plus %d attempts with unknown cost", unknown)
	}
	fmt.Fprintf(output, " (%d priced attempts)\n", priced)
}

func printReviewGroups(output io.Writer, groups []ReviewStatsGroup) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintln(output, "By model and effort:")
	for _, group := range groups {
		name := group.Model
		if group.ReasoningEffort != "" {
			name += "/" + group.ReasoningEffort
		}
		cost := formatCostRange(group.MinimumCostMicrousd, group.MaximumCostMicrousd)
		if group.UnknownCostAttempts != 0 {
			cost += fmt.Sprintf(" + %d unknown", group.UnknownCostAttempts)
		}
		fmt.Fprintf(output, "  %s: %d attempts, %d input, %d output, %s\n",
			name, group.Attempts, group.InputTokens, group.OutputTokens, cost)
	}
}

func formatCostRange(minimum, maximum int64) string {
	if minimum == maximum {
		return fmt.Sprintf("$%.6f USD", float64(minimum)/1_000_000)
	}
	return fmt.Sprintf("$%.6f–$%.6f USD", float64(minimum)/1_000_000, float64(maximum)/1_000_000)
}

func runInit(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("init", environment.Stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: air init <commit-ish>")
	}
	repository, err := DiscoverGitRepository(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	if _, err := repository.MasterSHA(ctx); err != nil {
		return err
	}
	startSHA, err := repository.ResolveCommit(ctx, flags.Arg(0))
	if err != nil {
		return err
	}
	onMaster, err := repository.IsOnMasterFirstParent(ctx, startSHA)
	if err != nil {
		return err
	}
	if !onMaster {
		return fmt.Errorf("commit %s is not on the first-parent history of master", shortSHA(startSHA))
	}
	store, err := CreateStore(ctx, repository.DatabasePath(), startSHA)
	if err != nil {
		return err
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	fmt.Fprintf(environment.Stdout, "Initialized %s\nBaseline: %s\n", repository.DatabasePath(), startSHA)
	return nil
}

func runConfig(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		return errors.New("usage: air config <get|set|unset|list> ...")
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()

	switch args[0] {
	case "get":
		flags := newFlagSet("config get", environment.Stderr)
		effective := flags.Bool("effective", false, "show the effective value and its source")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: air config get [--effective] <name>")
		}
		setting, ok := settingByKey(flags.Arg(0))
		if !ok {
			return unknownSettingError(flags.Arg(0))
		}
		if *effective {
			resolved, err := effectiveSettingForDisplay(ctx, store, environment.Getenv, setting)
			if err != nil {
				return err
			}
			fmt.Fprintf(environment.Stdout, "%s\t%s\n", displaySettingValue(setting, resolved.Value), resolved.Source)
			return nil
		}
		value, found, err := store.ConfigValue(ctx, setting.Key)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("configuration %q is not set in the database", setting.Key)
		}
		fmt.Fprintln(environment.Stdout, displaySettingValue(setting, value))
		return nil

	case "set":
		flags := newFlagSet("config set", environment.Stderr)
		fromStdin := flags.Bool("stdin", false, "read the value from standard input")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		expectedArguments := 2
		if *fromStdin {
			expectedArguments = 1
		}
		if flags.NArg() != expectedArguments {
			return errors.New("usage: air config set [--stdin] <name> [<value>]")
		}
		setting, ok := settingByKey(flags.Arg(0))
		if !ok {
			return unknownSettingError(flags.Arg(0))
		}
		value := ""
		if *fromStdin {
			if environment.Stdin == nil {
				return errors.New("standard input is unavailable")
			}
			const maximumSettingBytes = 64 * 1024
			contents, err := io.ReadAll(io.LimitReader(environment.Stdin, maximumSettingBytes+1))
			if err != nil {
				return fmt.Errorf("read configuration value: %w", err)
			}
			if len(contents) > maximumSettingBytes {
				return errors.New("configuration value exceeds 64 KiB")
			}
			value = strings.TrimRight(string(contents), "\r\n")
		} else {
			value = flags.Arg(1)
		}
		value, err = validateSettingValue(setting, value)
		if err != nil {
			return fmt.Errorf("invalid %s: %w", setting.Key, err)
		}
		if err := store.SetConfig(ctx, setting.Key, value); err != nil {
			return err
		}
		fmt.Fprintf(environment.Stdout, "Set %s=%s\n", setting.Key, displaySettingValue(setting, value))
		return nil

	case "unset":
		if len(args) != 2 {
			return errors.New("usage: air config unset <name>")
		}
		setting, ok := settingByKey(args[1])
		if !ok {
			return unknownSettingError(args[1])
		}
		removed, err := store.UnsetConfig(ctx, setting.Key)
		if err != nil {
			return err
		}
		if removed {
			fmt.Fprintf(environment.Stdout, "Unset %s\n", setting.Key)
		} else {
			fmt.Fprintf(environment.Stdout, "%s was not set\n", setting.Key)
		}
		return nil

	case "list":
		flags := newFlagSet("config list", environment.Stderr)
		effective := flags.Bool("effective", false, "include overrides, defaults, and value sources")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("usage: air config list [--effective]")
		}
		printed := false
		for _, setting := range reviewerSettings {
			if *effective {
				resolved, err := effectiveSettingForDisplay(ctx, store, environment.Getenv, setting)
				if err != nil {
					return err
				}
				fmt.Fprintf(environment.Stdout, "%-16s %-24s %s\n",
					setting.Key, displaySettingValue(setting, resolved.Value), resolved.Source)
				printed = true
				continue
			}
			value, found, err := store.ConfigValue(ctx, setting.Key)
			if err != nil {
				return err
			}
			if found {
				fmt.Fprintf(environment.Stdout, "%-16s %s\n", setting.Key, displaySettingValue(setting, value))
				printed = true
			}
		}
		if !printed {
			fmt.Fprintln(environment.Stdout, "No reviewer configuration stored.")
		}
		return nil

	default:
		return fmt.Errorf("unknown config command %q; expected get, set, unset, or list", args[0])
	}
}

func effectiveSettingForDisplay(
	ctx context.Context,
	store *Store,
	getenv func(string) string,
	setting settingSpec,
) (resolvedSetting, error) {
	if setting.Key != "api-key" {
		return resolveSettingValue(ctx, store, getenv, setting, "", false)
	}
	value, source, err := configuredAPIKey(ctx, store, getenv, "", false, "", false)
	if err != nil {
		return resolvedSetting{}, err
	}
	return resolvedSetting{Value: value, Source: source}, nil
}

func unknownSettingError(key string) error {
	keys := make([]string, 0, len(reviewerSettings))
	for _, setting := range reviewerSettings {
		keys = append(keys, setting.Key)
	}
	return fmt.Errorf("unknown configuration setting %q; expected one of: %s", key, strings.Join(keys, ", "))
}

func runModel(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		return errors.New("usage: air model <list|show|set-pricing|mark-pricing-unknown> ...")
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()

	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: air model list")
		}
		models, err := availableModels(ctx, store)
		if err != nil {
			return err
		}
		for _, model := range models {
			if model.Pricing == nil {
				fmt.Fprintf(environment.Stdout, "%-24s unknown pricing\n", model.Name)
			} else {
				fmt.Fprintf(environment.Stdout, "%-24s %s pricing as of %s\n",
					model.Name, model.Pricing.ServiceTier, model.Pricing.AsOf)
			}
		}
		return nil

	case "show":
		if len(args) != 2 {
			return errors.New("usage: air model show <name>")
		}
		model, found, err := modelForDisplay(ctx, store, args[1])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("model %q is not configured", args[1])
		}
		printModel(environment.Stdout, model)
		return nil

	case "mark-pricing-unknown":
		if len(args) != 2 {
			return errors.New("usage: air model mark-pricing-unknown <name>")
		}
		name := strings.TrimSpace(args[1])
		if name == "" {
			return errors.New("model name must not be empty")
		}
		if err := store.SaveModel(ctx, Model{Name: name}); err != nil {
			return err
		}
		fmt.Fprintf(environment.Stdout, "Marked pricing unknown for %s\n", name)
		return nil

	case "set-pricing":
		return runModelSetPricing(ctx, store, args[1:], environment)

	default:
		return fmt.Errorf("unknown model command %q; expected list, show, set-pricing, or mark-pricing-unknown", args[0])
	}
}

func runModelSetPricing(ctx context.Context, store *Store, args []string, environment cliEnvironment) error {
	if len(args) > 1 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string(nil), args[1:]...), args[0])
	}
	flags := newFlagSet("model set-pricing", environment.Stderr)
	serviceTier := flags.String("service-tier", standardServiceTier, "service tier")
	source := flags.String("source", "manual", "pricing source")
	asOf := flags.String("as-of", environmentNow(environment).Format("2006-01-02"), "pricing date")
	threshold := flags.Int64("long-context-threshold", longContextInputTokens, "input-token threshold for long-context prices")
	shortInput := flags.Float64("short-input", -1, "short-context input USD per million tokens")
	shortCached := flags.Float64("short-cached-input", -1, "short-context cached-input USD per million tokens")
	shortWrite := flags.Float64("short-cache-write", -1, "short-context cache-write USD per million tokens")
	shortOutput := flags.Float64("short-output", -1, "short-context output USD per million tokens")
	longInput := flags.Float64("long-input", -1, "long-context input USD per million tokens")
	longCached := flags.Float64("long-cached-input", -1, "long-context cached-input USD per million tokens")
	longWrite := flags.Float64("long-cache-write", -1, "long-context cache-write USD per million tokens")
	longOutput := flags.Float64("long-output", -1, "long-context output USD per million tokens")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: air model set-pricing <name> [pricing flags]")
	}
	if strings.TrimSpace(*serviceTier) == "" || strings.TrimSpace(*source) == "" || strings.TrimSpace(*asOf) == "" {
		return errors.New("service tier, source, and pricing date must not be empty")
	}
	if *threshold <= 0 {
		return errors.New("--long-context-threshold must be positive")
	}
	rates := []struct {
		name  string
		value float64
	}{
		{"--short-input", *shortInput},
		{"--short-cached-input", *shortCached},
		{"--short-cache-write", *shortWrite},
		{"--short-output", *shortOutput},
		{"--long-input", *longInput},
		{"--long-cached-input", *longCached},
		{"--long-cache-write", *longWrite},
		{"--long-output", *longOutput},
	}
	for _, rate := range rates {
		if rate.value < 0 || math.IsNaN(rate.value) || math.IsInf(rate.value, 0) {
			return fmt.Errorf("%s must be a non-negative USD-per-million-token price", rate.name)
		}
	}
	name := strings.TrimSpace(flags.Arg(0))
	model := Model{
		Name: name,
		Pricing: &ModelPricing{
			ServiceTier:            strings.TrimSpace(*serviceTier),
			Source:                 strings.TrimSpace(*source),
			AsOf:                   strings.TrimSpace(*asOf),
			LongContextInputTokens: *threshold,
			ShortContext:           tokenPrices(*shortInput, *shortCached, *shortWrite, *shortOutput),
			LongContext:            tokenPrices(*longInput, *longCached, *longWrite, *longOutput),
		},
	}
	if err := store.SaveModel(ctx, model); err != nil {
		return err
	}
	fmt.Fprintf(environment.Stdout, "Stored pricing for %s\n", name)
	return nil
}

func availableModels(ctx context.Context, store *Store) ([]Model, error) {
	modelsByName := make(map[string]Model, len(builtinModels))
	for name := range builtinModels {
		modelsByName[name] = modelByName(name)
	}
	stored, err := store.Models(ctx)
	if err != nil {
		return nil, err
	}
	for _, model := range stored {
		modelsByName[model.Name] = model
	}
	names := make([]string, 0, len(modelsByName))
	for name := range modelsByName {
		names = append(names, name)
	}
	sort.Strings(names)
	models := make([]Model, 0, len(names))
	for _, name := range names {
		models = append(models, modelsByName[name])
	}
	return models, nil
}

func modelForDisplay(ctx context.Context, store *Store, name string) (Model, bool, error) {
	name = strings.TrimSpace(name)
	if model, found, err := store.Model(ctx, name); err != nil || found {
		return model, found, err
	}
	model, found := builtinModels[name]
	if !found {
		return Model{}, false, nil
	}
	return modelByName(model.Name), true, nil
}

func modelForReview(ctx context.Context, store *Store, name string) (Model, error) {
	if model, found, err := store.Model(ctx, name); err != nil {
		return Model{}, err
	} else if found {
		return model, nil
	}
	return modelByName(name), nil
}

func printModel(output io.Writer, model Model) {
	fmt.Fprintf(output, "Model: %s\n", model.Name)
	if model.Pricing == nil {
		fmt.Fprintln(output, "Pricing: unknown")
		return
	}
	pricing := model.Pricing
	fmt.Fprintf(output, "Pricing: known\nService tier: %s\nSource: %s\nAs of: %s\nLong-context threshold: %d input tokens\n",
		pricing.ServiceTier, pricing.Source, pricing.AsOf, pricing.LongContextInputTokens)
	fmt.Fprintln(output, "USD per million tokens:")
	printTokenPrices(output, "short", pricing.ShortContext)
	printTokenPrices(output, "long", pricing.LongContext)
}

func printTokenPrices(output io.Writer, contextName string, prices TokenPrices) {
	fmt.Fprintf(output, "  %-5s input %.3f, cached input %.3f, cache write %.3f, output %.3f\n",
		contextName,
		float64(prices.InputNanousdPerToken)/1_000,
		float64(prices.CachedInputNanousdPerToken)/1_000,
		float64(prices.CacheWriteNanousdPerToken)/1_000,
		float64(prices.OutputNanousdPerToken)/1_000)
}

func runScan(ctx context.Context, args []string, environment cliEnvironment) error {
	return runScanCommand(ctx, args, environment, scanCommand)
}

func runRescan(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) > 1 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string(nil), args[1:]...), args[0])
	}
	return runScanCommand(ctx, args, environment, rescanCommand)
}

func runRetry(ctx context.Context, args []string, environment cliEnvironment) error {
	return runScanCommand(ctx, args, environment, retryCommand)
}

type scanCommandMode int

const (
	scanCommand scanCommandMode = iota
	rescanCommand
	retryCommand
)

func runScanCommand(ctx context.Context, args []string, environment cliEnvironment, mode scanCommandMode) error {
	rescan := mode == rescanCommand
	retry := mode == retryCommand
	commandName := "scan"
	if rescan {
		commandName = "rescan"
	} else if retry {
		commandName = "retry"
	}
	flags := newFlagSet(commandName, environment.Stderr)
	reviewerFlag := flags.String("reviewer", "", "review backend: codex or http")
	limit := flags.Int("limit", 0, "maximum commits to process; zero means unlimited")
	dryRun := flags.Bool("dry-run", false, "show pending work without reviewing or writing")
	continueOnError := flags.Bool("continue-on-error", false, "record a failed commit and continue the batch")
	modelFlag := flags.String("model", "", "model identifier")
	effortFlag := flags.String("effort", "", "Codex reasoning effort")
	codexBinaryFlag := flags.String("codex-bin", "", "Codex CLI executable")
	codexProfileFlag := flags.String("codex-profile", "", "Codex configuration profile")
	codexTimeoutFlag := flags.String("codex-timeout", "", "per-commit Codex timeout")
	baseURLFlag := flags.String("base-url", "", "OpenAI-compatible API base URL")
	apiKeyEnvFlag := flags.String("api-key-env", "", "environment variable containing the API key")
	apiKeyFlag := flags.String("api-key", "", "HTTP reviewer API key (prefer an environment variable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if rescan {
		if flags.NArg() != 1 {
			return errors.New("usage: air rescan [flags] <commit-ish>")
		}
		if *limit != 0 {
			return errors.New("--limit is not valid with air rescan")
		}
		if *dryRun {
			return errors.New("--dry-run is not valid with air rescan")
		}
		if *continueOnError {
			return errors.New("--continue-on-error is not valid with air rescan")
		}
	} else if retry {
		if flags.NArg() != 0 {
			return errors.New("usage: air retry [flags]")
		}
		if *dryRun {
			return errors.New("--dry-run is not valid with air retry")
		}
	} else if flags.NArg() > 1 {
		return errors.New("usage: air scan [flags] [<from>..<to>]")
	}
	if *dryRun && *continueOnError {
		return errors.New("--continue-on-error is not valid with --dry-run")
	}
	if *limit < 0 {
		return errors.New("--limit must not be negative")
	}
	revisionRange := ""
	if mode == scanCommand && flags.NArg() == 1 {
		revisionRange = flags.Arg(0)
	}
	repository, err := DiscoverGitRepository(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	store, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		return err
	}
	defer store.Close()
	var explicitCommits []string
	forceCommits := make(map[string]bool)
	if rescan {
		sha, err := repository.ResolveCommit(ctx, flags.Arg(0))
		if err != nil {
			return err
		}
		onMaster, err := repository.IsOnMasterFirstParent(ctx, sha)
		if err != nil {
			return err
		}
		if !onMaster {
			return fmt.Errorf("commit %s is not on the first-parent history of master", shortSHA(sha))
		}
		record, err := store.Commit(ctx, sha)
		if err != nil {
			return err
		}
		if record.Status != "reviewed" {
			return fmt.Errorf("commit %s was skipped and has no review to rescan", shortSHA(sha))
		}
		explicitCommits = []string{sha}
	} else if retry {
		failures, err := store.ScanFailures(ctx)
		if err != nil {
			return err
		}
		if len(failures) == 0 {
			fmt.Fprintln(environment.Stdout, "No failed commits.")
			return nil
		}
		failedBySHA := make(map[string]ScanFailure, len(failures))
		for _, failure := range failures {
			failedBySHA[failure.SHA] = failure
		}
		history, err := repository.MasterHistory(ctx)
		if err != nil {
			return err
		}
		for i := len(history) - 1; i >= 0; i-- {
			failure, exists := failedBySHA[history[i]]
			if !exists {
				continue
			}
			explicitCommits = append(explicitCommits, failure.SHA)
			forceCommits[failure.SHA] = failure.Force
			delete(failedBySHA, failure.SHA)
		}
		if len(failedBySHA) != 0 {
			fmt.Fprintf(environment.Stdout, "Ignoring %d failed commits no longer on master; run air clean.\n", len(failedBySHA))
		}
		if len(explicitCommits) == 0 {
			fmt.Fprintln(environment.Stdout, "No retryable failed commits.")
			return nil
		}
	}
	setFlags := visitedFlagNames(flags)
	resolve := func(key, commandLineValue string) (resolvedSetting, error) {
		setting, _ := settingByKey(key)
		return resolveSettingValue(ctx, store, environment.Getenv, setting, commandLineValue, setFlags[key])
	}
	reviewerName, err := resolve("reviewer", *reviewerFlag)
	if err != nil {
		return err
	}
	model, err := resolve("model", *modelFlag)
	if err != nil {
		return err
	}
	effort, err := resolve("effort", *effortFlag)
	if err != nil {
		return err
	}
	codexBinary, err := resolve("codex-bin", *codexBinaryFlag)
	if err != nil {
		return err
	}
	codexProfile, err := resolve("codex-profile", *codexProfileFlag)
	if err != nil {
		return err
	}
	codexTimeoutValue, err := resolve("codex-timeout", *codexTimeoutFlag)
	if err != nil {
		return err
	}
	codexTimeout, err := time.ParseDuration(codexTimeoutValue.Value)
	if err != nil {
		return fmt.Errorf("parse codex-timeout: %w", err)
	}
	baseURL, err := resolve("base-url", *baseURLFlag)
	if err != nil {
		return err
	}

	factory := func() (Reviewer, ReviewIdentity, error) {
		switch reviewerName.Value {
		case "codex":
			configuredModel := strings.TrimSpace(model.Value)
			if configuredModel == "" {
				return nil, ReviewIdentity{}, errors.New("model is required for the Codex reviewer; configure it, set AIR_MODEL, or pass --model")
			}
			configuredEffort := strings.TrimSpace(effort.Value)
			if configuredEffort == "" {
				return nil, ReviewIdentity{}, errors.New("reasoning effort is required for the Codex reviewer; configure it, set AIR_REASONING_EFFORT, or pass --effort")
			}
			reviewModel, err := modelForReview(ctx, store, configuredModel)
			if err != nil {
				return nil, ReviewIdentity{}, err
			}
			return &CodexReviewer{
				Repository:     repository,
				Binary:         codexBinary.Value,
				Model:          configuredModel,
				Effort:         configuredEffort,
				Profile:        codexProfile.Value,
				Timeout:        codexTimeout,
				CommandContext: environment.CodexCommand,
			}, ReviewIdentity{Model: reviewModel, ReasoningEffort: configuredEffort}, nil
		case "http":
			configuredModel := strings.TrimSpace(model.Value)
			if configuredModel == "" {
				return nil, ReviewIdentity{}, errors.New("model is required for the HTTP reviewer; configure it, set AIR_MODEL, or pass --model")
			}
			apiKey, _, err := configuredAPIKey(
				ctx, store, environment.Getenv,
				*apiKeyFlag, setFlags["api-key"],
				*apiKeyEnvFlag, setFlags["api-key-env"],
			)
			if err != nil {
				return nil, ReviewIdentity{}, err
			}
			if apiKey == "" {
				return nil, ReviewIdentity{}, errors.New("API key is required for the HTTP reviewer; configure it, set AIR_API_KEY or OPENAI_API_KEY, or pass --api-key/--api-key-env")
			}
			reviewModel, err := modelForReview(ctx, store, configuredModel)
			if err != nil {
				return nil, ReviewIdentity{}, err
			}
			return &HTTPReviewer{
				Repository: repository,
				Model:      configuredModel,
				BaseURL:    baseURL.Value,
				APIKey:     apiKey,
				Client:     environment.HTTPClient,
			}, ReviewIdentity{Model: reviewModel}, nil
		default:
			return nil, ReviewIdentity{}, fmt.Errorf("unknown reviewer %q; expected codex or http", reviewerName.Value)
		}
	}
	return scanRepository(ctx, repository, store, scanOptions{
		RevisionRange:   revisionRange,
		Commits:         explicitCommits,
		Explicit:        rescan || retry,
		Force:           rescan,
		ForceCommits:    forceCommits,
		DryRun:          *dryRun,
		ContinueOnError: *continueOnError,
		Limit:           *limit,
		Output:          environment.Stdout,
		Now:             environment.Now,
		NewReviewer:     factory,
	})
}

func runFailures(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("failures", environment.Stderr)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: air failures [--json]")
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	failures, err := store.ScanFailures(ctx)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(environment.Stdout, struct {
			Failures []ScanFailure `json:"failures"`
		}{Failures: failures})
	}
	fmt.Fprintf(environment.Stdout, "%d failed commits\n", len(failures))
	for _, failure := range failures {
		identity := ""
		if failure.Model != "" {
			identity = " " + failure.Model
			if failure.ReasoningEffort != "" {
				identity += "/" + failure.ReasoningEffort
			}
		}
		kind := "scan"
		if failure.Force {
			kind = "rescan"
		}
		fmt.Fprintf(environment.Stdout, "\n%s  %s failed %d times at %s%s\n    %s\n",
			shortSHA(failure.SHA), kind, failure.AttemptCount,
			failure.FailedAt.Format(time.RFC3339), identity, failure.Error)
	}
	return nil
}

func runPending(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("pending", environment.Stderr)
	limit := flags.Int("limit", 0, "maximum pending commits to show; zero means unlimited")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: air pending [--limit N] [<from>..<to>]")
	}
	if *limit < 0 {
		return errors.New("--limit must not be negative")
	}
	revisionRange := ""
	if flags.NArg() == 1 {
		revisionRange = flags.Arg(0)
	}
	repository, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	return scanRepository(ctx, repository, store, scanOptions{
		RevisionRange: revisionRange,
		Limit:         *limit,
		DryRun:        true,
		Output:        environment.Stdout,
	})
}

func runClean(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("clean", environment.Stderr)
	dryRun := flags.Bool("dry-run", false, "show stale commits without deleting them")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: air clean [--dry-run]")
	}
	repository, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	lock, err := acquireScanLock(repository.LockPath())
	if err != nil {
		return err
	}
	defer lock.Close()
	history, err := repository.MasterHistory(ctx)
	if err != nil {
		return err
	}
	present := make(map[string]struct{}, len(history))
	for _, sha := range history {
		present[sha] = struct{}{}
	}
	stored, err := store.CommitSHAs(ctx)
	if err != nil {
		return err
	}
	failed, err := store.ScanFailureSHAs(ctx)
	if err != nil {
		return err
	}
	storedSet := make(map[string]struct{}, len(stored)+len(failed))
	for _, sha := range append(stored, failed...) {
		storedSet[sha] = struct{}{}
	}
	stored = stored[:0]
	for sha := range storedSet {
		stored = append(stored, sha)
	}
	sort.Strings(stored)
	stale := make([]string, 0)
	for _, sha := range stored {
		if _, exists := present[sha]; !exists {
			stale = append(stale, sha)
		}
	}
	if len(stale) == 0 {
		fmt.Fprintln(environment.Stdout, "No stale commits.")
		return nil
	}
	verb := "remove"
	if *dryRun {
		verb = "would remove"
	}
	for _, sha := range stale {
		fmt.Fprintf(environment.Stdout, "%s  %s\n", shortSHA(sha), verb)
	}
	if *dryRun {
		fmt.Fprintf(environment.Stdout, "Would remove %d stale commits.\n", len(stale))
		return nil
	}
	deleted, err := store.DeleteCommits(ctx, stale)
	if err != nil {
		return err
	}
	failedDeleted, err := store.DeleteScanFailures(ctx, stale)
	if err != nil {
		return err
	}
	if deleted == 0 && failedDeleted == 0 {
		return errors.New("stale AIR state changed during cleanup")
	}
	fmt.Fprintf(environment.Stdout, "Removed %d stale commits.\n", len(stale))
	return nil
}

func runReset(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("reset", environment.Stderr)
	force := flags.Bool("force", false, "delete all AIR state without prompting")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: air reset [--force]")
	}
	repository, err := DiscoverGitRepository(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	stateDirectory := repository.StateDirectory()
	if filepath.Dir(stateDirectory) != repository.CommonDir || filepath.Base(stateDirectory) != "air" {
		return errors.New("refusing to reset an unexpected AIR state path")
	}
	info, err := os.Stat(stateDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(environment.Stdout, "AIR is not initialized.")
			return nil
		}
		return fmt.Errorf("inspect AIR state directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("AIR state path is not a directory: %s", stateDirectory)
	}
	if !*force {
		if environment.Stdin == nil {
			return errors.New("standard input is unavailable; pass --force to reset")
		}
		fmt.Fprintf(environment.Stdout, "Delete all AIR state in %s? [y/N] ", stateDirectory)
		response, err := bufio.NewReader(environment.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read reset confirmation: %w", err)
		}
		response = strings.ToLower(strings.TrimSpace(response))
		if response != "y" && response != "yes" {
			fmt.Fprintln(environment.Stdout, "Reset cancelled.")
			return nil
		}
	}
	lock, err := acquireScanLock(repository.LockPath())
	if err != nil {
		return err
	}
	if err := lock.Close(); err != nil {
		return fmt.Errorf("release AIR reset lock: %w", err)
	}
	if err := os.RemoveAll(stateDirectory); err != nil {
		return fmt.Errorf("remove AIR state directory: %w", err)
	}
	fmt.Fprintf(environment.Stdout, "Removed %s\n", stateDirectory)
	return nil
}

func runStatus(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("status", environment.Stderr)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: air status [--json]")
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	findings, err := store.OpenFindings(ctx)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(environment.Stdout, struct {
			OpenFindings []Finding `json:"open_findings"`
		}{OpenFindings: findings})
	}
	fmt.Fprintf(environment.Stdout, "%d open findings\n", len(findings))
	for _, finding := range findings {
		location := "(no file)"
		if finding.File != nil {
			location = *finding.File
			if finding.Line != nil {
				location += ":" + strconv.Itoa(*finding.Line)
			}
		}
		fmt.Fprintf(environment.Stdout, "\n#%d %-7s %s\n    %s\n", finding.ID, finding.Severity, location, finding.Title)
	}
	return nil
}

func runLog(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) != 0 {
		return errors.New("usage: air log")
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	records, err := store.Log(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Status == "skipped" {
			fmt.Fprintf(environment.Stdout, "%s  skipped: %s\n", shortSHA(record.SHA), record.SkipReason)
			continue
		}
		if record.NewCount == 0 && record.ResolvedCount == 0 {
			fmt.Fprintf(environment.Stdout, "%s  clean\n", shortSHA(record.SHA))
			continue
		}
		fmt.Fprintf(environment.Stdout, "%s  %d new, %d resolved\n", shortSHA(record.SHA), record.NewCount, record.ResolvedCount)
	}
	return nil
}

func runShow(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		return errors.New("usage: air show <commit-ish> [--reviews | --review N] [--json]")
	}
	revision := args[0]
	flags := newFlagSet("show", environment.Stderr)
	listReviews := flags.Bool("reviews", false, "list all retained review attempts")
	reviewNumber := flags.Int("review", 0, "show one retained review attempt")
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*listReviews && *reviewNumber != 0) || *reviewNumber < 0 {
		return errors.New("usage: air show <commit-ish> [--reviews | --review N] [--json]")
	}
	repository, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	sha, err := repository.ResolveCommit(ctx, revision)
	if err != nil {
		return err
	}
	record, err := store.Commit(ctx, sha)
	if err != nil {
		return err
	}
	metadata, metadataErr := repository.CommitMetadata(ctx, sha)
	subject := ""
	if metadataErr == nil {
		subject = firstLine(metadata.Message)
	} else {
		metadata = CommitMetadata{SHA: sha, ParentSHA: record.ParentSHA}
	}
	if record.Status == "skipped" {
		if *listReviews || *reviewNumber != 0 {
			return fmt.Errorf("commit %s was skipped and has no review attempts", shortSHA(sha))
		}
		if *jsonOutput {
			return writeJSON(environment.Stdout, showJSONOutput{
				Commit: metadata, Record: record,
				IntroducedFindings: []Finding{}, ResolvedFindings: []Finding{},
			})
		}
		fmt.Fprintf(environment.Stdout, "%s %s\n\nSkipped: %s\n", shortSHA(sha), subject, record.SkipReason)
		return nil
	}
	if *reviewNumber != 0 {
		attempt, err := store.ReviewAttempt(ctx, sha, *reviewNumber)
		if err != nil {
			return err
		}
		introduced, err := store.FindingsIntroducedByReview(ctx, attempt.ID)
		if err != nil {
			return err
		}
		resolved, err := store.FindingsResolvedByReview(ctx, attempt.ID)
		if err != nil {
			return err
		}
		if *jsonOutput {
			return writeJSON(environment.Stdout, showJSONOutput{
				Commit: metadata, Record: record, Review: &attempt,
				IntroducedFindings: introduced, ResolvedFindings: resolved,
			})
		}
		fmt.Fprintf(environment.Stdout, "%s %s\n", shortSHA(sha), subject)
		current := ""
		if attempt.Current {
			current = " (current)"
		}
		fmt.Fprintf(environment.Stdout, "\nReview attempt #%d%s\n", attempt.Number, current)
		printReviewAccounting(environment.Stdout, attempt.Model, attempt.ReasoningEffort,
			&attempt.Usage, attempt.EstimatedCostMicrousd, attempt.EstimatedCostMaxMicrousd,
			attempt.CostContext, attempt.CostComplete)
		printReviewResult(environment.Stdout, introduced, resolved, attempt.Summary)
		return nil
	}

	introduced, err := store.FindingsIntroducedBy(ctx, sha)
	if err != nil {
		return err
	}
	resolved, err := store.FindingsResolvedBy(ctx, sha)
	if err != nil {
		return err
	}
	var attempts []ReviewAttempt
	if *listReviews {
		attempts, err = store.ReviewAttempts(ctx, sha)
		if err != nil {
			return err
		}
	}
	if *jsonOutput {
		if attempts == nil {
			attempts = []ReviewAttempt{}
		}
		return writeJSON(environment.Stdout, showJSONOutput{
			Commit: metadata, Record: record, IntroducedFindings: introduced,
			ResolvedFindings: resolved, Reviews: attempts,
		})
	}
	fmt.Fprintf(environment.Stdout, "%s %s\n\n", shortSHA(sha), subject)
	printReviewAccounting(environment.Stdout, record.Model, record.ReasoningEffort,
		record.Usage, record.EstimatedCostMicrousd, record.EstimatedCostMaxMicrousd,
		record.CostContext, record.CostComplete)
	printReviewResult(environment.Stdout, introduced, resolved, record.Summary)
	if *listReviews {
		fmt.Fprintln(environment.Stdout, "\nReview attempts:")
		for _, attempt := range attempts {
			current := ""
			if attempt.Current {
				current = " current"
			}
			effort := ""
			if attempt.ReasoningEffort != "" {
				effort = "/" + attempt.ReasoningEffort
			}
			fmt.Fprintf(environment.Stdout, "  #%d  %s  %s%s  %d new, %d resolved%s\n",
				attempt.Number, attempt.ReviewedAt.Format(time.RFC3339), attempt.Model, effort,
				attempt.NewCount, attempt.ResolvedCount, current)
		}
	}
	return nil
}

type showJSONOutput struct {
	Commit             CommitMetadata  `json:"commit"`
	Record             CommitRecord    `json:"record"`
	Review             *ReviewAttempt  `json:"review,omitempty"`
	IntroducedFindings []Finding       `json:"introduced_findings"`
	ResolvedFindings   []Finding       `json:"resolved_findings"`
	Reviews            []ReviewAttempt `json:"reviews,omitempty"`
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

func printReviewAccounting(
	output io.Writer,
	model, reasoningEffort string,
	usage *TokenUsage,
	minimumCost, maximumCost *int64,
	costContext string,
	costComplete bool,
) {
	fmt.Fprintf(output, "Model: %s\n", model)
	if reasoningEffort != "" {
		fmt.Fprintf(output, "Reasoning effort: %s\n", reasoningEffort)
	}
	if usage != nil {
		fmt.Fprintf(output, "Tokens: %d total (%d input, %d cached input, ",
			totalTokens(*usage), usage.InputTokens, usage.CachedInputTokens)
		if usage.CacheWriteTokens == nil {
			fmt.Fprint(output, "unknown cache writes, ")
		} else {
			fmt.Fprintf(output, "%d cache writes, ", *usage.CacheWriteTokens)
		}
		fmt.Fprintf(output, "%d output, %d reasoning output)\n",
			usage.OutputTokens, usage.ReasoningOutputTokens)
	}
	if minimumCost == nil {
		fmt.Fprintln(output, "Estimated cost: unavailable (model pricing unknown)")
		return
	}
	minimum := float64(*minimumCost) / 1_000_000
	maximum := minimum
	if maximumCost != nil {
		maximum = float64(*maximumCost) / 1_000_000
	}
	if minimum == maximum {
		fmt.Fprintf(output, "Estimated cost: $%.6f USD", minimum)
	} else {
		fmt.Fprintf(output, "Estimated cost: $%.6f–$%.6f USD", minimum, maximum)
	}
	if costContext != "" {
		fmt.Fprintf(output, " (%s context", costContext)
		if !costComplete {
			fmt.Fprint(output, "; cache writes unreported")
		}
		fmt.Fprint(output, ")")
	}
	fmt.Fprintln(output)
}

func printReviewResult(output io.Writer, introduced, resolved []Finding, summary string) {
	fmt.Fprintln(output, "\nNew findings:")
	printFindingList(output, introduced)
	fmt.Fprintln(output, "\nResolved:")
	printFindingList(output, resolved)
	fmt.Fprintf(output, "\nReview:\n  %s\n", summary)
}

func runExport(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("export", environment.Stderr)
	format := flags.String("format", "", "output format: json or sarif")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*format != "json" && *format != "sarif") {
		return errors.New("usage: air export --format <json|sarif>")
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	findings, err := store.OpenFindings(ctx)
	if err != nil {
		return err
	}
	if *format == "json" {
		return writeJSON(environment.Stdout, struct {
			OpenFindings []Finding `json:"open_findings"`
		}{OpenFindings: findings})
	}
	return writeJSON(environment.Stdout, buildSARIF(findings))
}

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	InformationURI string      `json:"informationUri,omitempty"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string       `json:"id"`
	ShortDescription sarifMessage `json:"shortDescription"`
}

type sarifResult struct {
	RuleID       string            `json:"ruleId"`
	Level        string            `json:"level"`
	Message      sarifMessage      `json:"message"`
	Locations    []sarifLocation   `json:"locations,omitempty"`
	Fingerprints map[string]string `json:"fingerprints"`
	Properties   sarifProperties   `json:"properties"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region,omitempty"`
}

type sarifArtifactLocation struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

type sarifProperties struct {
	FindingID    int64  `json:"finding_id"`
	IntroducedBy string `json:"introduced_sha"`
	Symbol       string `json:"symbol,omitempty"`
}

func buildSARIF(findings []Finding) sarifLog {
	results := make([]sarifResult, 0, len(findings))
	for _, finding := range findings {
		result := sarifResult{
			RuleID:  "AIR",
			Level:   sarifLevel(finding.Severity),
			Message: sarifMessage{Text: finding.Title + "\n\n" + finding.Description},
			Fingerprints: map[string]string{
				"air/finding-id": strconv.FormatInt(finding.ID, 10),
			},
			Properties: sarifProperties{
				FindingID: finding.ID, IntroducedBy: finding.IntroducedSHA,
			},
		}
		if finding.Symbol != nil {
			result.Properties.Symbol = *finding.Symbol
		}
		if finding.File != nil {
			physical := sarifPhysicalLocation{
				ArtifactLocation: sarifArtifactLocation{URI: filepath.ToSlash(*finding.File)},
			}
			if finding.Line != nil {
				physical.Region = &sarifRegion{StartLine: *finding.Line}
			}
			result.Locations = []sarifLocation{{PhysicalLocation: physical}}
		}
		results = append(results, result)
	}
	return sarifLog{
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Version: "2.1.0",
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name: "AIR", Rules: []sarifRule{{
					ID: "AIR", ShortDescription: sarifMessage{Text: "AIR semantic commit review finding"},
				}},
			}},
			Results: results,
		}},
	}
}

func sarifLevel(severity string) string {
	switch severity {
	case "error":
		return "error"
	case "warning":
		return "warning"
	default:
		return "note"
	}
}

func runFinding(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) > 0 {
		switch args[0] {
		case "dismiss":
			return runFindingDismiss(ctx, args[1:], environment)
		case "reopen":
			return runFindingReopen(ctx, args[1:], environment)
		case "note":
			return runFindingNote(ctx, args[1:], environment)
		}
	}
	if len(args) != 1 {
		return errors.New("usage: air finding <id> | air finding <dismiss|reopen|note> ...")
	}
	id, err := parseFindingID(args[0])
	if err != nil {
		return err
	}
	repository, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	finding, err := store.Finding(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(environment.Stdout, "#%d %s\n%s\n\n%s\n", finding.ID, finding.Severity, finding.Title, finding.Description)
	printCommitReference(ctx, environment.Stdout, repository, "Introduced", finding.IntroducedSHA)
	if finding.DismissedAt != nil {
		fmt.Fprintf(environment.Stdout, "\nDisposition:\n    dismissed %s\n    %s\n",
			finding.DismissedAt.Format(time.RFC3339), finding.DismissReason)
	} else if finding.ResolvedSHA == nil {
		fmt.Fprintln(environment.Stdout, "\nResolved:\n    open")
	} else {
		fmt.Fprintln(environment.Stdout)
		printCommitReference(ctx, environment.Stdout, repository, "Resolved", *finding.ResolvedSHA)
	}
	events, err := store.FindingEvents(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintln(environment.Stdout, "\nHistory:")
	for _, event := range events {
		reference := ""
		if event.SHA != nil {
			reference = " " + shortSHA(*event.SHA)
		}
		note := ""
		if event.Note != "" {
			note = " — " + event.Note
		}
		fmt.Fprintf(environment.Stdout, "    %s  %s%s%s\n",
			event.CreatedAt.Format(time.RFC3339), event.Action, reference, note)
	}
	return nil
}

func runFindingDismiss(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) < 2 || len(args) > 3 {
		return errors.New("usage: air finding dismiss <id> --reason <text>")
	}
	id, err := parseFindingID(args[0])
	if err != nil {
		return err
	}
	reason := ""
	if len(args) == 3 && args[1] == "--reason" {
		reason = args[2]
	} else if len(args) == 2 && strings.HasPrefix(args[1], "--reason=") {
		reason = strings.TrimPrefix(args[1], "--reason=")
	} else {
		return errors.New("usage: air finding dismiss <id> --reason <text>")
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	if err := store.DismissFinding(ctx, id, reason, environmentNow(environment)); err != nil {
		return err
	}
	fmt.Fprintf(environment.Stdout, "Dismissed finding #%d\n", id)
	return nil
}

func runFindingReopen(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) != 1 {
		return errors.New("usage: air finding reopen <id>")
	}
	id, err := parseFindingID(args[0])
	if err != nil {
		return err
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	if err := store.ReopenFinding(ctx, id, environmentNow(environment)); err != nil {
		return err
	}
	fmt.Fprintf(environment.Stdout, "Reopened finding #%d\n", id)
	return nil
}

func runFindingNote(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) != 2 {
		return errors.New("usage: air finding note <id> <text>")
	}
	id, err := parseFindingID(args[0])
	if err != nil {
		return err
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	if err := store.AddFindingNote(ctx, id, args[1], environmentNow(environment)); err != nil {
		return err
	}
	fmt.Fprintf(environment.Stdout, "Added note to finding #%d\n", id)
	return nil
}

func environmentNow(environment cliEnvironment) time.Time {
	if environment.Now != nil {
		return environment.Now()
	}
	return time.Now()
}

func openRepositoryStore(ctx context.Context, cwd string) (*GitRepository, *Store, func(), error) {
	repository, err := DiscoverGitRepository(ctx, cwd)
	if err != nil {
		return nil, nil, nil, err
	}
	store, err := OpenStore(ctx, repository.DatabasePath())
	if err != nil {
		return nil, nil, nil, err
	}
	return repository, store, func() { _ = store.Close() }, nil
}

func printFindingList(output io.Writer, findings []Finding) {
	if len(findings) == 0 {
		fmt.Fprintln(output, "  none")
		return
	}
	for _, finding := range findings {
		fmt.Fprintf(output, "  #%d %s\n  %s\n", finding.ID, finding.Severity, finding.Title)
	}
}

func printCommitReference(ctx context.Context, output io.Writer, repository *GitRepository, label, sha string) {
	subject := ""
	if metadata, err := repository.CommitMetadata(ctx, sha); err == nil {
		subject = firstLine(metadata.Message)
	}
	fmt.Fprintf(output, "%s:\n    %s %s\n", label, shortSHA(sha), subject)
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return value[:index]
	}
	return value
}

func newFlagSet(name string, output io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(output)
	return flags
}

func visitedFlagNames(flags *flag.FlagSet) map[string]bool {
	visited := make(map[string]bool)
	flags.Visit(func(value *flag.Flag) {
		visited[value.Name] = true
	})
	return visited
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, `AIR reviews commits on the first-parent history of master.

Usage:
  air init <commit-ish>
  air scan [flags] [<from>..<to>]
  air retry [flags]
  air failures [--json]
  air pending [--limit N] [<from>..<to>]
  air rescan <commit-ish> [flags]
  air clean [--dry-run]
  air reset [--force]
  air config <get|set|unset|list> ...
  air model <list|show|set-pricing|mark-pricing-unknown> ...
  air stats [--model MODEL] [--since DATE]
  air cost [--model MODEL] [--since DATE]
  air status [--json]
  air log
  air show <commit-ish> [--reviews | --review N] [--json]
  air export --format <json|sarif>
  air finding <id>
  air finding dismiss <id> --reason <text>
  air finding reopen <id>
  air finding note <id> <text>

Reviewer configuration precedence:
  command-line flag > environment variable > database > built-in default

Reviewer environment variables:
  AIR_REVIEWER       Review backend (default: codex)
  AIR_MODEL          Model identifier (required)
  AIR_REASONING_EFFORT
                     Codex reasoning effort (required for Codex)
  AIR_CODEX_BIN      Codex CLI executable (default: codex)
  AIR_CODEX_PROFILE  Optional Codex configuration profile
  AIR_CODEX_TIMEOUT  Per-commit timeout (default: 10m)
  AIR_BASE_URL       HTTP reviewer base URL (default: https://api.openai.com/v1)
  AIR_API_KEY        HTTP reviewer API key
  AIR_API_KEY_ENV    Name of another environment variable containing the API key
  OPENAI_API_KEY     Fallback HTTP reviewer API key

Run "air config list --effective" to show effective values and their sources.`)
}
