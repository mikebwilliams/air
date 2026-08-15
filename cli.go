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
	case "status":
		return runStatus(ctx, args[1:], environment)
	case "log":
		return runLog(ctx, args[1:], environment)
	case "show":
		return runShow(ctx, args[1:], environment)
	case "finding":
		return runFinding(ctx, args[1:], environment)
	case "rescan":
		return errors.New("rescan is deferred and is not implemented in AIR 0.1")
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

func runScan(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("scan", environment.Stderr)
	reviewerDefault := environment.Getenv("AIR_REVIEWER")
	if reviewerDefault == "" {
		reviewerDefault = "codex"
	}
	reviewerName := flags.String("reviewer", reviewerDefault, "review backend: codex or http")
	limit := flags.Int("limit", 0, "maximum commits to process; zero means unlimited")
	model := flags.String("model", environment.Getenv("AIR_MODEL"), "model identifier")
	effort := flags.String("effort", environment.Getenv("AIR_REASONING_EFFORT"), "Codex reasoning effort")
	codexBinaryDefault := environment.Getenv("AIR_CODEX_BIN")
	if codexBinaryDefault == "" {
		codexBinaryDefault = "codex"
	}
	codexBinary := flags.String("codex-bin", codexBinaryDefault, "Codex CLI executable")
	codexProfile := flags.String("codex-profile", environment.Getenv("AIR_CODEX_PROFILE"), "Codex configuration profile")
	codexTimeoutDefault := defaultCodexTimeout
	if configuredTimeout := environment.Getenv("AIR_CODEX_TIMEOUT"); configuredTimeout != "" {
		parsedTimeout, err := time.ParseDuration(configuredTimeout)
		if err != nil || parsedTimeout <= 0 {
			return errors.New("AIR_CODEX_TIMEOUT must be a positive duration")
		}
		codexTimeoutDefault = parsedTimeout
	}
	codexTimeout := flags.Duration("codex-timeout", codexTimeoutDefault, "per-commit Codex timeout")
	baseURLDefault := environment.Getenv("AIR_BASE_URL")
	if baseURLDefault == "" {
		baseURLDefault = "https://api.openai.com/v1"
	}
	baseURL := flags.String("base-url", baseURLDefault, "OpenAI-compatible API base URL")
	apiKeyEnv := flags.String("api-key-env", environment.Getenv("AIR_API_KEY_ENV"), "environment variable containing the API key")
	inputPrice := flags.String("input-usd-per-million", environment.Getenv("AIR_INPUT_USD_PER_MILLION"), "estimated USD price per million uncached input tokens")
	cachedInputPrice := flags.String("cached-input-usd-per-million", environment.Getenv("AIR_CACHED_INPUT_USD_PER_MILLION"), "estimated USD price per million cached input tokens")
	outputPrice := flags.String("output-usd-per-million", environment.Getenv("AIR_OUTPUT_USD_PER_MILLION"), "estimated USD price per million output tokens")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: air scan [flags] [<from>..<to>]")
	}
	if *codexTimeout <= 0 {
		return errors.New("--codex-timeout must be positive")
	}
	if *limit < 0 {
		return errors.New("--limit must not be negative")
	}
	pricing, err := parsePricing(*inputPrice, *cachedInputPrice, *outputPrice)
	if err != nil {
		return err
	}
	revisionRange := ""
	if flags.NArg() == 1 {
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

	factory := func() (Reviewer, ReviewIdentity, error) {
		switch strings.ToLower(strings.TrimSpace(*reviewerName)) {
		case "codex":
			configuredModel := strings.TrimSpace(*model)
			if configuredModel == "" {
				return nil, ReviewIdentity{}, errors.New("model is required for the Codex reviewer; set AIR_MODEL or pass --model")
			}
			configuredEffort := strings.TrimSpace(*effort)
			if configuredEffort == "" {
				return nil, ReviewIdentity{}, errors.New("reasoning effort is required for the Codex reviewer; set AIR_REASONING_EFFORT or pass --effort")
			}
			return &CodexReviewer{
				Repository:     repository,
				Binary:         *codexBinary,
				Model:          configuredModel,
				Effort:         configuredEffort,
				Profile:        *codexProfile,
				Timeout:        *codexTimeout,
				CommandContext: environment.CodexCommand,
			}, ReviewIdentity{Model: configuredModel, ReasoningEffort: configuredEffort}, nil
		case "http":
			if strings.TrimSpace(*model) == "" {
				return nil, ReviewIdentity{}, errors.New("model is required for the HTTP reviewer; set AIR_MODEL or pass --model")
			}
			apiKey, keySource := resolveAPIKey(environment.Getenv, *apiKeyEnv)
			if apiKey == "" {
				if keySource != "" {
					return nil, ReviewIdentity{}, fmt.Errorf("API key environment variable %s is empty", keySource)
				}
				return nil, ReviewIdentity{}, errors.New("API key is required for the HTTP reviewer; set AIR_API_KEY, OPENAI_API_KEY, or --api-key-env")
			}
			return &HTTPReviewer{
				Repository: repository,
				Model:      *model,
				BaseURL:    *baseURL,
				APIKey:     apiKey,
				Client:     environment.HTTPClient,
			}, ReviewIdentity{Model: strings.TrimSpace(*model)}, nil
		default:
			return nil, ReviewIdentity{}, fmt.Errorf("unknown reviewer %q; expected codex or http", *reviewerName)
		}
	}
	return scanRepository(ctx, repository, store, scanOptions{
		RevisionRange: revisionRange,
		Limit:         *limit,
		Pricing:       pricing,
		Output:        environment.Stdout,
		Now:           environment.Now,
		NewReviewer:   factory,
	})
}

func resolveAPIKey(getenv func(string) string, configuredName string) (string, string) {
	if configuredName != "" {
		return getenv(configuredName), configuredName
	}
	if key := getenv("AIR_API_KEY"); key != "" {
		return key, "AIR_API_KEY"
	}
	return getenv("OPENAI_API_KEY"), "OPENAI_API_KEY"
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
	if len(args) != 1 {
		return errors.New("usage: air show <commit-ish>")
	}
	repository, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	sha, err := repository.ResolveCommit(ctx, args[0])
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
		fmt.Fprintf(environment.Stdout, "\nSkipped: %s\n", record.SkipReason)
		return nil
	}
	fmt.Fprintf(environment.Stdout, "\nModel: %s\n", record.Model)
	if record.ReasoningEffort != "" {
		fmt.Fprintf(environment.Stdout, "Reasoning effort: %s\n", record.ReasoningEffort)
	}
	if record.Usage != nil {
		fmt.Fprintf(
			environment.Stdout,
			"Tokens: %d total (%d input, %d cached input, %d output, %d reasoning output)\n",
			totalTokens(*record.Usage),
			record.Usage.InputTokens,
			record.Usage.CachedInputTokens,
			record.Usage.OutputTokens,
			record.Usage.ReasoningOutputTokens,
		)
	}
	if record.EstimatedCostMicrousd == nil {
		fmt.Fprintln(environment.Stdout, "Estimated cost: unavailable (no pricing configured)")
	} else {
		fmt.Fprintf(environment.Stdout, "Estimated cost: $%.6f USD\n", float64(*record.EstimatedCostMicrousd)/1_000_000)
	}
	introduced, err := store.FindingsIntroducedBy(ctx, sha)
	if err != nil {
		return err
	}
	resolved, err := store.FindingsResolvedBy(ctx, sha)
	if err != nil {
		return err
	}
	fmt.Fprintln(environment.Stdout, "\nNew findings:")
	printFindingList(environment.Stdout, introduced)
	fmt.Fprintln(environment.Stdout, "\nResolved:")
	printFindingList(environment.Stdout, resolved)
	fmt.Fprintf(environment.Stdout, "\nReview:\n  %s\n", record.Summary)
	return nil
}

func runFinding(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) != 1 {
		return errors.New("usage: air finding <id>")
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
	if finding.ResolvedSHA == nil {
		fmt.Fprintln(environment.Stdout, "\nResolved:\n    open")
	} else {
		fmt.Fprintln(environment.Stdout)
		printCommitReference(ctx, environment.Stdout, repository, "Resolved", *finding.ResolvedSHA)
	}
	return nil
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

func printUsage(output io.Writer) {
	fmt.Fprintln(output, `AIR reviews commits on the first-parent history of master.

Usage:
  air init <commit-ish>
  air scan [--limit N] [--reviewer codex|http] --model MODEL [--effort EFFORT] [<from>..<to>]
  air status
  air log
  air show <commit-ish>
  air finding <id>

Reviewer environment:
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
  AIR_INPUT_USD_PER_MILLION
  AIR_CACHED_INPUT_USD_PER_MILLION
  AIR_OUTPUT_USD_PER_MILLION
                     Optional prices used together for per-review USD estimates`)
}
