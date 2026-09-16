package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

const reposeModelSchemaSQL = `
CREATE TABLE models (
    name                                TEXT PRIMARY KEY,
    pricing_status                      TEXT NOT NULL CHECK(pricing_status IN ('known', 'unknown')),
    service_tier                        TEXT,
    pricing_source                      TEXT,
    pricing_as_of                       TEXT,
    long_context_input_tokens           INTEGER,
    short_input_nanousd_per_token       INTEGER,
    short_cached_nanousd_per_token      INTEGER,
    short_cache_write_nanousd_per_token INTEGER,
    short_output_nanousd_per_token      INTEGER,
    long_input_nanousd_per_token        INTEGER,
    long_cached_nanousd_per_token       INTEGER,
    long_cache_write_nanousd_per_token  INTEGER,
    long_output_nanousd_per_token       INTEGER,
    CHECK(
        (pricing_status = 'known' AND service_tier IS NOT NULL AND
         pricing_source IS NOT NULL AND pricing_as_of IS NOT NULL AND
         long_context_input_tokens IS NOT NULL AND
         short_input_nanousd_per_token IS NOT NULL AND
         short_cached_nanousd_per_token IS NOT NULL AND
         short_cache_write_nanousd_per_token IS NOT NULL AND
         short_output_nanousd_per_token IS NOT NULL AND
         long_input_nanousd_per_token IS NOT NULL AND
         long_cached_nanousd_per_token IS NOT NULL AND
         long_cache_write_nanousd_per_token IS NOT NULL AND
         long_output_nanousd_per_token IS NOT NULL) OR
        (pricing_status = 'unknown' AND service_tier IS NULL AND
         pricing_source IS NULL AND pricing_as_of IS NULL AND
         long_context_input_tokens IS NULL AND
         short_input_nanousd_per_token IS NULL AND
         short_cached_nanousd_per_token IS NULL AND
         short_cache_write_nanousd_per_token IS NULL AND
         short_output_nanousd_per_token IS NULL AND
         long_input_nanousd_per_token IS NULL AND
         long_cached_nanousd_per_token IS NULL AND
         long_cache_write_nanousd_per_token IS NULL AND
         long_output_nanousd_per_token IS NULL)
    )
);
PRAGMA user_version = 8;
`

type reposeModelInfo struct {
	Name          string        `json:"name"`
	Origin        string        `json:"origin"`
	PricingStatus string        `json:"pricing_status"`
	ObservedScans int           `json:"observed_scans"`
	Pricing       *ModelPricing `json:"pricing,omitempty"`
}

func runReposeModelCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		return errors.New("usage: repose model <list|show|set-pricing|mark-pricing-unknown> ...")
	}
	switch args[0] {
	case "list":
		return runReposeModelListCLI(ctx, args[1:], environment)
	case "show":
		return runReposeModelShowCLI(ctx, args[1:], environment)
	case "set-pricing":
		return runReposeModelSetPricingCLI(ctx, args[1:], environment)
	case "mark-pricing-unknown":
		return runReposeModelUnknownCLI(ctx, args[1:], environment)
	default:
		return fmt.Errorf("unknown model command %q; expected list, show, set-pricing, or mark-pricing-unknown", args[0])
	}
}

func runReposeModelListCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("model list", environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	asJSON := flags.Bool("json", false, "output JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: repose model list [--json] [--repo DIR]")
	}
	store, closeStore, err := openReposeModelStore(ctx, environment.Cwd, *repoPath, false)
	if err != nil {
		return err
	}
	defer closeStore()
	models, err := reposeAvailableModels(ctx, store)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeInventoryJSON(environment.Stdout, models)
	}
	for _, model := range models {
		pricing := "unknown pricing"
		if model.Pricing != nil {
			pricing = fmt.Sprintf("%s pricing as of %s", model.Pricing.ServiceTier, model.Pricing.AsOf)
		}
		observed := "not observed"
		if model.ObservedScans == 1 {
			observed = "1 scan"
		} else if model.ObservedScans > 1 {
			observed = fmt.Sprintf("%d scans", model.ObservedScans)
		}
		fmt.Fprintf(environment.Stdout, "%-24s %-36s %-10s %s\n", model.Name, pricing, model.Origin, observed)
	}
	return nil
}

func runReposeModelShowCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("model show", environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	asJSON := flags.Bool("json", false, "output JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: repose model show NAME [--json] [--repo DIR]")
	}
	name, err := cleanReposeModelName(flags.Arg(0))
	if err != nil {
		return err
	}
	store, closeStore, err := openReposeModelStore(ctx, environment.Cwd, *repoPath, false)
	if err != nil {
		return err
	}
	defer closeStore()
	models, err := reposeAvailableModels(ctx, store)
	if err != nil {
		return err
	}
	for _, model := range models {
		if model.Name != name {
			continue
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, model)
		}
		printReposeModel(environment.Stdout, model)
		return nil
	}
	return fmt.Errorf("model %q is not configured or used by a saved scan", name)
}

func runReposeModelUnknownCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("model mark-pricing-unknown", environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	asJSON := flags.Bool("json", false, "output JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: repose model mark-pricing-unknown NAME [--json] [--repo DIR]")
	}
	name, err := cleanReposeModelName(flags.Arg(0))
	if err != nil {
		return err
	}
	store, closeStore, err := openReposeModelStore(ctx, environment.Cwd, *repoPath, true)
	if err != nil {
		return err
	}
	defer closeStore()
	if err := store.SaveModel(ctx, Model{Name: name}); err != nil {
		return err
	}
	if *asJSON {
		return writeReposeSavedModelJSON(ctx, environment, store, name)
	}
	fmt.Fprintf(environment.Stdout, "Marked pricing unknown for %s\n", name)
	return nil
}

func runReposeModelSetPricingCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("model set-pricing", environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	asJSON := flags.Bool("json", false, "output JSON")
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
		return errors.New("usage: repose model set-pricing NAME [PRICING OPTIONS] [--json] [--repo DIR]")
	}
	name, err := cleanReposeModelName(flags.Arg(0))
	if err != nil {
		return err
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
		{"--short-input", *shortInput}, {"--short-cached-input", *shortCached},
		{"--short-cache-write", *shortWrite}, {"--short-output", *shortOutput},
		{"--long-input", *longInput}, {"--long-cached-input", *longCached},
		{"--long-cache-write", *longWrite}, {"--long-output", *longOutput},
	}
	for _, rate := range rates {
		if rate.value < 0 || math.IsNaN(rate.value) || math.IsInf(rate.value, 0) {
			return fmt.Errorf("%s must be a non-negative USD-per-million-token price", rate.name)
		}
	}
	model := Model{Name: name, Pricing: &ModelPricing{
		ServiceTier: strings.TrimSpace(*serviceTier), Source: strings.TrimSpace(*source), AsOf: strings.TrimSpace(*asOf),
		LongContextInputTokens: *threshold,
		ShortContext:           tokenPrices(*shortInput, *shortCached, *shortWrite, *shortOutput),
		LongContext:            tokenPrices(*longInput, *longCached, *longWrite, *longOutput),
	}}
	store, closeStore, err := openReposeModelStore(ctx, environment.Cwd, *repoPath, true)
	if err != nil {
		return err
	}
	defer closeStore()
	if err := store.SaveModel(ctx, model); err != nil {
		return err
	}
	if *asJSON {
		return writeReposeSavedModelJSON(ctx, environment, store, name)
	}
	fmt.Fprintf(environment.Stdout, "Stored pricing for %s\n", name)
	return nil
}

func openReposeModelStore(ctx context.Context, cwd, repoPath string, write bool) (*inventoryStore, func(), error) {
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(cwd, repoPath))
	if err != nil {
		return nil, nil, err
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return nil, nil, err
	}
	var store *inventoryStore
	if write {
		store, err = openInventoryStore(ctx, database, false)
	} else {
		store, err = openInventoryReadOnly(ctx, database)
	}
	if err != nil {
		return nil, nil, err
	}
	return store, func() { _ = store.Close() }, nil
}

func reposeAvailableModels(ctx context.Context, store *inventoryStore) ([]reposeModelInfo, error) {
	models := make(map[string]reposeModelInfo, len(builtinModels))
	for name := range builtinModels {
		model := modelByName(name)
		models[name] = reposeModelInfo{Name: name, Origin: "built-in", PricingStatus: "known", Pricing: model.Pricing}
	}
	if store.version >= 8 {
		stored, err := store.Models(ctx)
		if err != nil {
			return nil, err
		}
		for _, model := range stored {
			status := "unknown"
			if model.Pricing != nil {
				status = "known"
			}
			models[model.Name] = reposeModelInfo{Name: model.Name, Origin: "configured", PricingStatus: status, Pricing: model.Pricing}
		}
	}
	observed, err := reposeObservedModels(ctx, store)
	if err != nil {
		return nil, err
	}
	for name, count := range observed {
		model, found := models[name]
		if !found {
			model = reposeModelInfo{Name: name, Origin: "observed", PricingStatus: "unknown"}
		}
		model.ObservedScans = count
		models[name] = model
	}
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]reposeModelInfo, 0, len(names))
	for _, name := range names {
		result = append(result, models[name])
	}
	return result, nil
}

func reposeObservedModels(ctx context.Context, store *inventoryStore) (map[string]int, error) {
	result := map[string]int{}
	if store.version < 4 {
		return result, nil
	}
	rows, err := store.db.QueryContext(ctx, `SELECT json_extract(document,'$.model.model'),count(*)
		FROM audit_scans GROUP BY json_extract(document,'$.model.model') ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("list observed models: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, fmt.Errorf("list observed models: %w", err)
		}
		if name != "" {
			result[name] = count
		}
	}
	return result, rows.Err()
}

func reposeModelForAccounting(ctx context.Context, store *inventoryStore, name string) (Model, error) {
	if store.version >= 8 {
		model, found, err := store.Model(ctx, name)
		if err != nil {
			return Model{}, err
		}
		if found {
			return model, nil
		}
	}
	return modelByName(name), nil
}

func writeReposeSavedModelJSON(ctx context.Context, environment cliEnvironment, store *inventoryStore, name string) error {
	models, err := reposeAvailableModels(ctx, store)
	if err != nil {
		return err
	}
	for _, model := range models {
		if model.Name == name {
			return writeInventoryJSON(environment.Stdout, model)
		}
	}
	return fmt.Errorf("saved model %q disappeared", name)
}

func cleanReposeModelName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if name == "" {
		return "", errors.New("model name must not be empty")
	}
	return name, nil
}

func printReposeModel(output io.Writer, model reposeModelInfo) {
	fmt.Fprintf(output, "Model: %s\nOrigin: %s\nObserved scans: %d\n", model.Name, model.Origin, model.ObservedScans)
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
