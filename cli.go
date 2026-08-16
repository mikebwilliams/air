package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
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
	case "pending":
		return runPending(ctx, args[1:], environment)
	case "config":
		return runConfig(ctx, args[1:], environment)
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

func runScan(ctx context.Context, args []string, environment cliEnvironment) error {
	return runScanCommand(ctx, args, environment, false)
}

func runRescan(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) > 1 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string(nil), args[1:]...), args[0])
	}
	return runScanCommand(ctx, args, environment, true)
}

func runScanCommand(ctx context.Context, args []string, environment cliEnvironment, rescan bool) error {
	commandName := "scan"
	if rescan {
		commandName = "rescan"
	}
	flags := newFlagSet(commandName, environment.Stderr)
	reviewerFlag := flags.String("reviewer", "", "review backend: codex or http")
	limit := flags.Int("limit", 0, "maximum commits to process; zero means unlimited")
	dryRun := flags.Bool("dry-run", false, "show pending work without reviewing or writing")
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
	} else if flags.NArg() > 1 {
		return errors.New("usage: air scan [flags] [<from>..<to>]")
	}
	if *limit < 0 {
		return errors.New("--limit must not be negative")
	}
	revisionRange := ""
	if !rescan && flags.NArg() == 1 {
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
			return &CodexReviewer{
				Repository:     repository,
				Binary:         codexBinary.Value,
				Model:          configuredModel,
				Effort:         configuredEffort,
				Profile:        codexProfile.Value,
				Timeout:        codexTimeout,
				CommandContext: environment.CodexCommand,
			}, ReviewIdentity{Model: modelByName(configuredModel), ReasoningEffort: configuredEffort}, nil
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
			return &HTTPReviewer{
				Repository: repository,
				Model:      configuredModel,
				BaseURL:    baseURL.Value,
				APIKey:     apiKey,
				Client:     environment.HTTPClient,
			}, ReviewIdentity{Model: modelByName(configuredModel)}, nil
		default:
			return nil, ReviewIdentity{}, fmt.Errorf("unknown reviewer %q; expected codex or http", reviewerName.Value)
		}
	}
	return scanRepository(ctx, repository, store, scanOptions{
		RevisionRange: revisionRange,
		Commits:       explicitCommits,
		Force:         rescan,
		DryRun:        *dryRun,
		Limit:         *limit,
		Output:        environment.Stdout,
		Now:           environment.Now,
		NewReviewer:   factory,
	})
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

func runStatus(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) != 0 {
		return errors.New("usage: air status")
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
		return errors.New("usage: air show <commit-ish> [--reviews | --review N]")
	}
	revision := args[0]
	flags := newFlagSet("show", environment.Stderr)
	listReviews := flags.Bool("reviews", false, "list all retained review attempts")
	reviewNumber := flags.Int("review", 0, "show one retained review attempt")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*listReviews && *reviewNumber != 0) || *reviewNumber < 0 {
		return errors.New("usage: air show <commit-ish> [--reviews | --review N]")
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
	}
	fmt.Fprintf(environment.Stdout, "%s %s\n", shortSHA(sha), subject)
	if record.Status == "skipped" {
		if *listReviews || *reviewNumber != 0 {
			return fmt.Errorf("commit %s was skipped and has no review attempts", shortSHA(sha))
		}
		fmt.Fprintf(environment.Stdout, "\nSkipped: %s\n", record.SkipReason)
		return nil
	}
	if *reviewNumber != 0 {
		attempt, err := store.ReviewAttempt(ctx, sha, *reviewNumber)
		if err != nil {
			return err
		}
		current := ""
		if attempt.Current {
			current = " (current)"
		}
		fmt.Fprintf(environment.Stdout, "\nReview attempt #%d%s\n", attempt.Number, current)
		printReviewAccounting(environment.Stdout, attempt.Model, attempt.ReasoningEffort,
			&attempt.Usage, attempt.EstimatedCostMicrousd, attempt.EstimatedCostMaxMicrousd,
			attempt.CostContext, attempt.CostComplete)
		introduced, err := store.FindingsIntroducedByReview(ctx, attempt.ID)
		if err != nil {
			return err
		}
		resolved, err := store.FindingsResolvedByReview(ctx, attempt.ID)
		if err != nil {
			return err
		}
		printReviewResult(environment.Stdout, introduced, resolved, attempt.Summary)
		return nil
	}

	fmt.Fprintln(environment.Stdout)
	printReviewAccounting(environment.Stdout, record.Model, record.ReasoningEffort,
		record.Usage, record.EstimatedCostMicrousd, record.EstimatedCostMaxMicrousd,
		record.CostContext, record.CostComplete)
	introduced, err := store.FindingsIntroducedBy(ctx, sha)
	if err != nil {
		return err
	}
	resolved, err := store.FindingsResolvedBy(ctx, sha)
	if err != nil {
		return err
	}
	printReviewResult(environment.Stdout, introduced, resolved, record.Summary)
	if *listReviews {
		attempts, err := store.ReviewAttempts(ctx, sha)
		if err != nil {
			return err
		}
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
  air pending [--limit N] [<from>..<to>]
  air rescan <commit-ish> [flags]
  air config <get|set|unset|list> ...
  air status
  air log
  air show <commit-ish> [--reviews | --review N]
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
