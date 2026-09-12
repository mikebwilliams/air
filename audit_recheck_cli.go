package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func runAuditRecheckCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("recheck", environment.Stderr)
	repo := flags.String("repo", ".", "dedicated scan checkout")
	sourceSelector := flags.String("scan", "latest", "source scan; latest means the newest completed original scan")
	path := flags.String("path", ".", "filter findings by source path prefix")
	asJSON := flags.Bool("json", false, "output recheck status as JSON")
	dryRun := flags.Bool("dry-run", false, "show selected findings without saving or invoking a model")
	createOnly := flags.Bool("create-only", false, "save the recheck without invoking a model")
	force := flags.Bool("force", false, "create a fresh pass instead of resuming a matching one")
	batchMax := flags.Int("batch-max", defaultAuditRecheckBatchMax, "maximum findings per batch within one original assignment")
	config := auditModelConfig{}
	flags.StringVar(&config.Harness, "harness", "codex", "codex, claude, or gemini")
	flags.StringVar(&config.Model, "model", "", "verification model (required)")
	flags.StringVar(&config.Effort, "effort", "high", "reasoning effort; Gemini requires default")
	flags.StringVar(&config.Binary, "binary", "", "runner executable")
	flags.DurationVar(&config.Timeout, "timeout", defaultAuditTimeout, "per-batch timeout; explicit values also override a resumed recheck")
	options := auditRunOptions{}
	flags.IntVar(&options.Jobs, "jobs", 2, "parallel recheck batches (1..32)")
	flags.IntVar(&options.Limit, "limit", 0, "maximum attempts for this invocation, including retries; 0 is unlimited")
	flags.DurationVar(&options.Duration, "duration", 0, "stop dispatching after this duration and drain active work")
	flags.BoolVar(&options.RetryFailed, "retry-failed", false, "retry failed recheck batches")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if options.Jobs < 1 || options.Jobs > 32 || options.Limit < 0 || options.Duration < 0 || config.Timeout <= 0 {
		return errors.New("jobs must be 1..32, timeout must be positive, and limit/duration must not be negative")
	}
	if *batchMax < 1 {
		return errors.New("--batch-max must be positive")
	}
	if *dryRun && *createOnly {
		return errors.New("--dry-run and --create-only cannot be combined")
	}
	if flags.Changed("timeout") {
		options.Timeout = config.Timeout
	}
	selection := inventorySelection{Path: *path, Status: "included"}
	if err := selection.validate(); err != nil {
		return err
	}
	if config.Harness != "codex" && config.Harness != "claude" && config.Harness != "gemini" {
		return errors.New("harness must be codex, claude, or gemini")
	}
	if strings.TrimSpace(config.Model) == "" || strings.TrimSpace(config.Effort) == "" {
		return errors.New("recheck requires --model and a nonempty reasoning effort")
	}
	if config.Harness == "gemini" && config.Effort != "default" {
		return errors.New("Gemini requires --effort default")
	}
	if config.Binary == "" {
		config.Binary = config.Harness
	}
	binary, err := exec.LookPath(config.Binary)
	if err != nil {
		return err
	}
	config.Binary = reposeAbsolutePath(environment.Cwd, binary)
	wanted := map[int64]bool{}
	for _, value := range flags.Args() {
		id, err := parseFindingID(value)
		if err != nil {
			return err
		}
		if wanted[id] {
			return fmt.Errorf("finding #%d was supplied more than once", id)
		}
		wanted[id] = true
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
	var source auditScan
	if *sourceSelector == "latest" {
		source, err = reader.latestAuditForRecheck(ctx)
	} else {
		source, err = reader.audit(ctx, *sourceSelector)
	}
	if err != nil {
		return err
	}
	if source.Spec.Recheck != nil || source.Status == "invalid" {
		return errors.New("--scan must identify an original scan with a valid snapshot; resume rechecks with scan resume ID")
	}
	findingStore := &auditFindingStore{reader: reader, databasePath: database, scanID: source.ID}
	findings, err := findingStore.AllFindings(ctx)
	if err != nil {
		return err
	}
	selected := []Finding{}
	matched := map[int64]bool{}
	for _, f := range findings {
		if f.DismissedAt != nil || f.File == nil || !inventoryPrefixMatches(*f.File, selection.Path) || (len(wanted) > 0 && !wanted[f.ID]) {
			continue
		}
		matched[f.ID] = true
		selected = append(selected, f)
	}
	for id := range wanted {
		if !matched[id] {
			return fmt.Errorf("finding #%d is dismissed or outside the selected source scan/path", id)
		}
	}
	if len(selected) == 0 {
		return errors.New("no open findings in the selected source scan/path")
	}
	spec, err := buildAuditRecheckSpec(source, selected, config, *batchMax)
	if err != nil {
		return err
	}
	record, err := reader.Inventory(ctx, source.Spec.Plan.InventoryID)
	if err != nil {
		return err
	}
	if err := checkInventory(ctx, repository, record.Inventory); err != nil {
		return err
	}
	existing, err := reader.auditRecheckByKey(ctx, spec.Recheck.Key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if *dryRun {
		ids := make([]int64, 0, len(selected))
		for _, f := range selected {
			ids = append(ids, f.ID)
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, struct {
				SourceScanID string              `json:"source_scan_id"`
				Model        auditModelConfig    `json:"model"`
				FindingIDs   []int64             `json:"finding_ids"`
				BatchMax     int                 `json:"batch_max"`
				Batches      []auditRecheckBatch `json:"batches"`
				ExistingID   string              `json:"existing_recheck_id,omitempty"`
				Force        bool                `json:"force"`
			}{source.ID, config, ids, *batchMax, spec.Recheck.Batches, existing.ID, *force})
		}
		fmt.Fprintf(environment.Stdout, "%d findings in %d batches (max %d findings per batch) from scan %s selected for %s/%s/%s. No work saved or started.\n", len(selected), len(spec.Recheck.Batches), *batchMax, source.ID[:12], config.Harness, config.Model, config.Effort)
		byID := map[int64]Finding{}
		for _, f := range selected {
			byID[f.ID] = f
		}
		for i, batch := range spec.Recheck.Batches {
			fmt.Fprintf(environment.Stdout, "  Batch %d, source assignment %s:\n", i+1, shortSHA(batch.SourceAssignmentID))
			for _, id := range batch.FindingIDs {
				f := byID[id]
				fmt.Fprintf(environment.Stdout, "    #%d %s: %s\n", f.ID, inventoryDisplay(*f.File), inventoryDisplay(f.Title))
			}
		}
		if existing.ID != "" && !*force {
			printAuditStatus(environment.Stdout, existing)
		}
		return nil
	}
	writer, err := openInventoryStore(ctx, database, false)
	if err != nil {
		return err
	}
	defer writer.Close()
	scan := existing
	if scan.ID == "" || *force {
		if *force {
			var nonce [16]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				return err
			}
			spec.Recheck.Key += ":" + hex.EncodeToString(nonce[:])
		}
		inputs, err := prepareAuditInputs(ctx, record, spec)
		if err != nil {
			return err
		}
		if err := checkInventory(ctx, repository, record.Inventory); err != nil {
			return err
		}
		scan, err = writer.createAudit(ctx, record, spec, inputs, environmentNow(environment))
		if err != nil {
			// A concurrent creator may have inserted this exact identity first.
			var lookupErr error
			scan, lookupErr = writer.auditRecheckByKey(ctx, spec.Recheck.Key)
			if lookupErr != nil {
				return err
			}
		}
	}
	fmt.Fprintf(environment.Stderr, "Recheck %s of source scan %s; resume with scan resume %s --repo %q.\n", scan.ID[:12], source.ID[:12], scan.ID[:12], *repo)
	if *createOnly {
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, scan)
		}
		printAuditStatus(environment.Stdout, scan)
		return nil
	}
	runErr := runAudit(ctx, writer, repository, scan, options, newAuditRunnerForScan(repository, environment, scan), environment.Stderr)
	readContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	updated, err := writer.audit(readContext, scan.ID)
	if err != nil {
		return err
	}
	if *asJSON {
		if err := writeInventoryJSON(environment.Stdout, updated); err != nil {
			return err
		}
	} else {
		printAuditStatus(environment.Stdout, updated)
	}
	return runErr
}
