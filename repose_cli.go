package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
)

const reposeVersion = "0.5-dev"

const reposeHelp = `Repose — sustained C/C++ repository audits

Usage:
  repose doctor [--json] [--repo DIR]
  repose status [--scan ID|latest] [--jobs N] [--json] [--repo DIR]
  repose stats [--scan ID|latest] [--model MODEL] [--since DATE] [--json] [--repo DIR]
  repose cost [--scan ID|latest] [--model MODEL] [--since DATE] [--json] [--repo DIR]
  repose model list [--json] [--repo DIR]
  repose model show NAME [--json] [--repo DIR]
  repose model set-pricing NAME [PRICING OPTIONS] [--json] [--repo DIR]
  repose model mark-pricing-unknown NAME [--json] [--repo DIR]
  repose prompt list [--json] [--repo DIR]
  repose prompt show [--full] scan|recheck [--json] [--repo DIR]
  repose prompt set (--file FILE|--stdin) scan|recheck [--json] [--repo DIR]
  repose prompt reset scan|recheck [--json] [--repo DIR]
  repose hint add TEXT [--json] [--repo DIR]
  repose hint list [--scan ID|latest] [--json] [--repo DIR]
  repose hint remove ID [--json] [--repo DIR]
  repose scan create --model MODEL [--harness codex|claude|gemini] [--effort LEVEL]
                     [--inventory ID] [--path PREFIX] [--goal TEXT] [--hint TEXT]
                     [--timeout DURATION] [--repo DIR]
  repose scan run ID [--jobs N] [--limit N] [--duration DURATION]
                     [--retry-failed] [--timeout DURATION] [--repo DIR]
  repose scan resume [ID] [--jobs N] [--limit N] [--duration DURATION]
                          [--retry-failed] [--timeout DURATION] [--repo DIR]
  repose scan list|show|tasks|attempts [ID] [--json] [--repo DIR]
  repose scan prompt ID TASK [--repo DIR]
  repose scan pause|interrupt ID [--repo DIR]
  repose recheck [FINDING_ID...] --model MODEL [--scan ID|latest] [--path PREFIX] [--tag TAG]
                 [--hint TEXT] [--harness codex|claude|gemini] [--effort LEVEL] [--timeout DURATION]
                 [--jobs N] [--limit N] [--duration DURATION] [--retry-failed]
                 [--dry-run|--create-only] [--force] [--json] [--repo DIR]
  repose findings [--scan ID] [--verification VERDICT] [--tag TAG] [--all] [--json] [--repo DIR]
  repose finding list [--scan ID|latest] [--status STATUS] [--severity SEVERITY]
                       [--verification VERDICT] [--path PREFIX] [--tag TAG]
                       [--sort SORT] [--limit N] [--all] [--json] [--repo DIR]
  repose finding [show] ID [--json] [--repo DIR]
  repose finding dismiss|reopen|note ID [--reason TEXT] [--repo DIR]
  repose finding source ID [--context N] [--json] [--repo DIR]
  repose finding open ID [--repo DIR]
  repose finding tag|untag ID... --tag TAG [--tag TAG...] [--repo DIR]
  repose tags [--scan ID|latest] [--all] [--json] [--repo DIR]
  repose backup [PATH] [--repo DIR]
  repose backup import [--force] PATH [--repo DIR]
  repose export --format json|sarif|html [-o FILE] [--scan ID|latest]
                [--path PREFIX] [--verification VERDICT] [--tag TAG] [--all] [--repo DIR]
  repose inventory build [--repo DIR] [--compile-commands FILE] [--policy FILE]
  repose inventory list [--repo DIR]
  repose inventory show [ID] [--repo DIR]
  repose inventory files [ID] [--repo DIR] [--path PREFIX] [--group NAME]
                         [--tag TAG] [--status STATUS]
  repose inventory tree [ID] [--path PREFIX] [--depth N] [--repo DIR]
  repose inventory groups [ID] [--repo DIR]
  repose inventory inspect [ID] [--path PREFIX] [--group NAME] [--repo DIR]
  repose inventory group PREFIX... --name NAME [--repo DIR]
  repose inventory annotate PREFIX... [--tag TAG] [--note TEXT] [--repo DIR]
  repose inventory plan [ID] [--path PREFIX] [--group NAME] [--tag TAG]
                        [--goal TEXT] [--max-files N] [--max-bytes N] [--repo DIR]
                        [--semantic] [--index ID] [--tui]
  repose inventory browse [ID] [--path PREFIX] [--repo DIR]
  repose inventory index [ID] [--path PREFIX] [--jobs N] [--max-files N]
                         [--timeout DURATION] [--retry-errors] [--refresh] [--repo DIR]
  repose inventory index-status [ID] [--path PREFIX] [--index ID] [--repo DIR]
  repose inventory symbols [ID] [--path PREFIX] [--index ID] [--repo DIR]
  repose inventory includes [ID] [--path PREFIX] [--index ID] [--repo DIR]
  repose inventory check [ID] [--repo DIR]
  repose inventory approve ID [--repo DIR]
  repose inventory exclude PREFIX... --reason TEXT [--repo DIR]
  repose inventory include PREFIX... [--reason TEXT] [--repo DIR]
  repose inventory exclusions [ID] [--repo DIR]
  repose inventory policy export [ID] [--repo DIR]
  repose inventory policy import FILE [--repo DIR]
  repose version

Inventory commands except browse support --json. IDs may be hexadecimal prefixes, current,
or latest (most recently created). Inspection defaults to current. Approval
requires an explicit ID.

Build uses a clean, dedicated checkout. It inherits the current policy and build
path; the first build defaults to build/compile_commands.json. --policy replaces
the saved policy after a successful build.
--compile-commands is relative to the checkout; --repo and --policy are relative
to the current directory. Inventory build executes no compiler or model.

Files default to in-scope code. Status: included, excluded, all, missing-command,
or header-unmapped. Path prefixes match exact files or directory descendants.
JSON show exports the complete saved inventory, commands, and policy.

Tree, groups, inspect, and browse accept the same selection filters as files.
Tree defaults to one directory level; counts include all selected descendants.
Group assigns a name to prefixes; annotate adds tags (repeatable --tag) and notes.
Edits version the current policy; policy export/import supports bulk replacement
and removing annotations. Excluded paths can still carry annotations.

Plan previews deterministic file assignments within each group and directory.
Defaults: 8 files / 65536 target bytes, goal: Find correctness issues.
Each selected file appears once; oversized files remain flagged singletons.
The preview uses saved facts, without validating the live checkout, and neither
saves a scan nor invokes a model. Byte limits are not model context limits.
Plan --semantic uses saved symbol boundaries and resolved includes; --index ID
selects a semantic profile and implies --semantic. Missing/partial files remain
whole and flagged. Byte ranges cover all selected contents, including gaps.
Plan --tui opens an assignment browser, using a saved index when available.
Tab switches panes; t/s/c/d select targets, symbols, context, or diagnostics.
Enter opens full details; / searches assignments and symbols; ? shows plan IDs
and help. --tui accepts the same goal, scope, and limits; it conflicts with --json.

Index runs clangd 21+ (override with --clangd FILE), using one shared process and
two file workers by default. It validates the snapshot/build, saves each result,
and resumes completed work on rerun. --max-files limits further attempts; Ctrl-C
leaves completed results saved. --retry-errors retries gaps; --refresh reparses
the selection. One source command variant is indexed; headers borrow commands
from observed includers. Index-status, symbols, and includes read saved facts.
They accept the same selection filters as files. Indexing is independent of
policy edits; transitive/environment changes require --refresh. No models run.

Browse opens the terminal inventory browser; ? shows keys. Current is editable,
while explicit IDs and latest open read-only. Edits affect all paths under the
selected prefix, including files hidden by display filters. The browser opens
a write connection only when saving an edit. The p key opens an assignment
browser for the selected scope; q returns to the inventory.

Exclude/include derive a new current inventory using the saved map. Old versions
remain unchanged. Excluded files remain available as context. Changes are shown
in a short preview; --json includes the full change list. Exclusions lists default
rules and explicit overrides with matched/effective C/C++ file counts.
Policy export writes editable JSON; policy import replaces the current policy.

Scan create freezes an approved inventory, subset, goal, model settings, source
prompts, and semantic plan. --instructions FILE freezes additional project guidance.
Prompt set customizes the editable scan or recheck reviewer instructions while
leaving Repose's safety, scope, and response protocol fixed. Active saved hints
supplement both kinds. Repeated --hint values add one-off guidance. Prompt text,
hint IDs and text, source, and a content identity are frozen in every new scan;
later edits do not change queued work. Prompt show --full displays the current
effective instructions, hints, and fixed protocol. Hint list --scan ID inspects a
saved snapshot.
It requires --model; runner defaults to codex, effort to high, timeout to 30m.
Gemini requires --effort default. --binary selects a runner executable.
Creation makes no model calls. Run/resume share one coordinator lock per checkout.
Run/resume --timeout DURATION overrides the saved per-assignment timeout for that
invocation only; omitting it uses the saved limit. Durations must be positive.
New attempts record their effective timeout before dispatch; scan attempts shows it.
Resume without an ID selects the only scan with pending or abandoned work;
--retry-failed also includes failed work. Completed and invalidated scans are
excluded. Multiple candidates are listed for an explicit choice. With no eligible
scans, resume reports an error.
--limit caps total attempts across workers, including throttle retries, per run.
--jobs 32 --limit 32 runs one batch of at most 32 attempts. --duration drains
active work when time expires. Pause drains; interrupt and Ctrl-C cancel active
workers. Resume recovers unfinished attempts and preserves completed assignments.
Failed work is retried only with --retry-failed. Raw outputs, errors, usage and
findings are retained per attempt. Findings refer to the observed snapshot.
Codex quota exhaustion pauses dispatch and drains active work; blocked tasks remain
pending for resume. Temporary throttling uses shared cooldowns and single probes
(30s, 1m, 2m, or a longer provider delay), then pauses after three failed probes.
Cooldowns survive resume. Scan show/attempts retain provider-limit details.
Findings opens the triage TUI; --json exports instead. R reloads, D dismisses,
r reopens, n adds a note, t/u adds/removes tags, and T filters by exact tags.
The preview shows source at the observed snapshot. Finding ID displays its model,
snapshot provenance, verification results, and audited history; show is an alias.
Finding list provides a noninteractive filtered table or versioned JSON. Finding
source reads the exact recorded snapshot even when the checkout has moved. Finding
open launches the configured Git editor only when HEAD and the target file still
match that snapshot.
Finding tag/untag accepts any
number of IDs and repeatable --tag values in one atomic edit. Tags are lowercase
labels; category:value namespacing is supported but optional. Repeated --tag
filters use AND semantics. The tags command lists counts for open findings;
--all includes dismissed findings.
Recheck verifies existing findings at their observed snapshot with an explicit
model. --scan defaults to the newest completed original scan. Optional finding IDs,
--path, and repeatable exact --tag filters restrict scope; dismissed findings are
excluded. Findings are grouped
by original assignment; --batch-max caps each batch (default 5). Smaller batches
stay small. Each finding receives an independent verdict and reasoning.
--dry-run only previews; --create-only saves prompts and a queue without model calls.
Repeating the same selection/model/batch maximum resumes its saved pass; --force
creates a fresh pass (resume it by ID). Rechecks share scan pause/interrupt/resume,
quota handling, and timeouts. --jobs counts parallel batches, --limit counts batch
attempts, and --timeout applies per batch. Verdicts are confirmed, false_positive, or
uncertain, with reasoning and full history. Original findings and manual decisions
are preserved. Findings V filters latest verdicts; --verification also filters CLI
output (all, unchecked, confirmed, false_positive, uncertain).
Export restores JSON, SARIF, and a self-contained offline HTML viewer. --format
is required; -o/--output writes to a file (relative to the current directory),
and - or no output file writes to stdout. JSON/SARIF contain open findings by
default; --all includes dismissed findings. HTML includes all dispositions and
initially shows open findings; --all initially shows every disposition.
--scan selects source findings; latest means the newest completed original scan,
and a recheck ID selects its source scan. --path, --verification, and repeatable
exact --tag filters narrow scope.
Exports retain observed snapshots, assignment/attempt provenance, model identity,
manual history, and every recheck verdict. HTML shows snapshot source excerpts
and supports full-text search, sorting, verdict filters, and exact multi-tag filters.
Finding URL fragments such as #1234 provide shareable direct links. Export reads
saved state without upgrading the database or invoking models; source previews use
the observed commit.
Backup creates a consistent, integrity-checked SQLite snapshot without stopping
active scans and never overwrites an existing file. Its default name is
repose-backup-YYYYMMDD-HHMMSS.sqlite in the current directory. Backup import
validates the schema, foreign keys, inventory documents, and repository snapshots,
then replaces the worktree-local database while scan and index locks are held.
Replacing existing state prompts unless --force is supplied.
Status summarizes the current inventory, findings, scans, assignment progress,
token usage, and model worker time. --scan restricts every total to one scan;
a recheck selects findings from its source scan. Remaining-time estimates use
successful assignment durations. --jobs converts remaining worker time into an
approximate wall-clock duration without changing or starting a scan.
Failed assignments remain separate from the estimate until resumed with
--retry-failed.
Stats aggregates retained review and recheck attempts, outcomes, worker time,
token usage, estimated cost, and current finding counts. Cost presents the same
attempt accounting with cost first. --model and --since filter attempts; --scan
selects one frozen scan. Provider-reported cost takes precedence over token-price
estimates. Unknown pricing and missing accounting data remain explicit. Model list
combines built-in prices, repository overrides, and model names observed in saved
scans. Model set-pricing records short- and long-context USD-per-million-token
rates; mark-pricing-unknown explicitly overrides any built-in price. Repository
prices are used immediately by stats, cost, and doctor. Token-price estimates do
not represent Codex subscription use.
Cross-scan deduplication and token/cost budgets remain future work.
Doctor performs a read-only health audit of the repository, Repose state,
SQLite integrity and foreign keys, saved inventory snapshots, checkout/build
identity, semantic coverage and clangd identity, saved scan state, runner
executables, reviewer guidance, hints, and model pricing coverage. Warnings describe optional or incomplete
capabilities; failed checks produce a nonzero exit status. --json emits the full
report before the command returns a failed-check error.
`

func runReposeCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(environment.Stdout, reposeHelp)
		return nil
	}
	if args[0] == "version" || args[0] == "--version" {
		if len(args) != 1 {
			return errors.New("usage: repose version")
		}
		fmt.Fprintf(environment.Stdout, "repose %s\n", reposeVersion)
		return nil
	}
	if args[0] == "doctor" {
		if containsHelpFlag(args[1:]) {
			fmt.Fprint(environment.Stdout, reposeHelp)
			return nil
		}
		return runReposeDoctorCLI(ctx, args[1:], environment)
	}
	if args[0] == "status" {
		if containsHelpFlag(args[1:]) {
			fmt.Fprint(environment.Stdout, reposeHelp)
			return nil
		}
		return runReposeStatusCLI(ctx, args[1:], environment)
	}
	if args[0] == "stats" || args[0] == "cost" {
		if containsHelpFlag(args[1:]) {
			fmt.Fprint(environment.Stdout, reposeHelp)
			return nil
		}
		return runReposeStatsCLI(ctx, args[0], args[1:], environment)
	}
	if args[0] == "model" {
		if containsHelpFlag(args[1:]) {
			fmt.Fprint(environment.Stdout, reposeHelp)
			return nil
		}
		return runReposeModelCLI(ctx, args[1:], environment)
	}
	if args[0] == "prompt" || args[0] == "hint" {
		if containsHelpFlag(args[1:]) {
			fmt.Fprint(environment.Stdout, reposeHelp)
			return nil
		}
		if args[0] == "prompt" {
			return runReposePromptCLI(ctx, args[1:], environment)
		}
		return runReposeHintCLI(ctx, args[1:], environment)
	}
	if args[0] == "scan" {
		if containsHelpFlag(args[1:]) {
			fmt.Fprint(environment.Stdout, reposeHelp)
			return nil
		}
		return runAuditCLI(ctx, args[1:], environment)
	}
	if args[0] == "recheck" {
		if containsHelpFlag(args[1:]) {
			fmt.Fprint(environment.Stdout, reposeHelp)
			return nil
		}
		return runAuditRecheckCLI(ctx, args[1:], environment)
	}
	if args[0] == "export" {
		if containsHelpFlag(args[1:]) {
			fmt.Fprint(environment.Stdout, reposeHelp)
			return nil
		}
		return runAuditExportCLI(ctx, args[1:], environment)
	}
	if args[0] == "findings" || args[0] == "finding" {
		if containsHelpFlag(args[1:]) {
			fmt.Fprint(environment.Stdout, reposeHelp)
			return nil
		}
		return runAuditFindingsCLI(ctx, args, environment)
	}
	if args[0] == "tags" {
		return runAuditTagsCLI(ctx, args[1:], environment)
	}
	if args[0] == "backup" {
		if containsHelpFlag(args[1:]) {
			fmt.Fprint(environment.Stdout, reposeHelp)
			return nil
		}
		return runReposeBackupCLI(ctx, args[1:], environment)
	}
	if args[0] != "inventory" {
		return fmt.Errorf("unknown command %q; run repose help", args[0])
	}
	if len(args) == 1 || containsHelpFlag(args[1:]) {
		fmt.Fprint(environment.Stdout, reposeHelp)
		return nil
	}
	return runInventoryCLI(ctx, args[1:], environment)
}

func runInventoryCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	command := args[0]
	switch command {
	case "index", "index-status", "symbols", "includes":
		return runSemanticCLI(ctx, args, environment)
	case "tree", "groups", "inspect", "group", "annotate", "plan", "browse":
		return runInventoryWorkspaceCLI(ctx, args, environment)
	case "exclude", "include", "exclusions", "policy":
		return runInventoryPolicyCLI(ctx, args, environment)
	case "build", "list", "show", "files", "check", "approve":
	default:
		return fmt.Errorf("unknown inventory command %q; run repose help", command)
	}
	flags := newFlagSet("inventory "+command, environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	asJSON := flags.Bool("json", false, "output JSON")
	var compileCommands, policyPath, prefix, group, tag string
	status := "included"
	if command == "build" {
		flags.StringVar(&compileCommands, "compile-commands", "build/compile_commands.json", "compilation database relative to the checkout")
		flags.StringVar(&policyPath, "policy", "", "JSON inventory rules")
	}
	if command == "files" {
		flags.StringVar(&prefix, "path", ".", "repository-relative file or directory")
		flags.StringVar(&group, "group", "", "inventory group")
		flags.StringVar(&tag, "tag", "", "inventory tag")
		flags.StringVar(&status, "status", "included", "included, excluded, all, missing-command, header-unmapped")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() > 1 || (command == "build" || command == "list") && flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments for repose inventory %s", command)
	}
	selector := flags.Arg(0)
	if command == "approve" && (selector == "" || selector == "latest" || selector == "current") {
		return errors.New("approval requires an explicit inventory ID; inspect it with repose inventory show first")
	}
	if command == "files" {
		if err := validateInventoryPolicy(InventoryPolicy{Rules: []InventoryRule{{Prefix: prefix}}}); err != nil {
			return fmt.Errorf("invalid --path: %w", err)
		}
		switch status {
		case "included", "excluded", "all", "missing-command", "header-unmapped":
		default:
			return fmt.Errorf("unknown file status %q", status)
		}
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repoPath))
	if err != nil {
		return err
	}
	if command == "build" {
		return runInventoryBuildCLI(ctx, repository, compileCommands, flags.Changed("compile-commands"), policyPath, *asJSON, environment)
	}
	databasePath, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	var store *inventoryStore
	if command == "approve" {
		store, err = openInventoryStore(ctx, databasePath, false)
	} else {
		store, err = openInventoryReadOnly(ctx, databasePath)
	}
	if err != nil {
		return err
	}
	defer store.Close()
	if command == "list" {
		items, err := store.ListInventories(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, items)
		}
		for _, item := range items {
			current := ""
			if item.Current {
				current = " (current)"
			}
			fmt.Fprintf(environment.Stdout, "%s  %s  %s  %s%s\n", item.ID[:12], shortSHA(item.SnapshotSHA), item.CreatedAt.Format("2006-01-02 15:04:05Z07:00"), inventoryReviewLabel(item.ReviewedAt != nil), current)
		}
		if len(items) == 0 {
			fmt.Fprintln(environment.Stdout, "No saved inventories.")
		}
		return nil
	}
	record, err := store.Inventory(ctx, selector)
	if err != nil {
		return err
	}
	switch command {
	case "show":
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, record)
		}
		return printInventorySummary(environment.Stdout, record)
	case "files":
		files := []InventoryFile{}
		for _, file := range record.Inventory.Files {
			if !inventoryPrefixMatches(file.Path, prefix) || group != "" && file.Group != group ||
				tag != "" && !slices.Contains(file.Tags, tag) || !inventoryFileMatchesStatus(file, status) {
				continue
			}
			files = append(files, file)
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, files)
		}
		writer := tabwriter.NewWriter(environment.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "PATH\tGROUP\tKIND\tBYTES\tCOMMANDS\tEXCLUSION")
		for _, file := range files {
			fmt.Fprintf(writer, "%q\t%q\t%s\t%d\t%d\t%q\n", file.Path, file.Group, file.Kind, file.Bytes, len(file.CommandIDs), file.ExclusionReason)
		}
		return writer.Flush()
	case "check", "approve":
		if err := checkInventory(ctx, repository, record.Inventory); err != nil {
			return err
		}
		if command == "approve" {
			if err := store.MarkInventoryReviewed(ctx, record.ID, environmentNow(environment)); err != nil {
				return err
			}
			record, err = store.Inventory(ctx, record.ID)
			if err != nil {
				return err
			}
		}
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, record)
		}
		if command == "approve" {
			fmt.Fprintf(environment.Stdout, "Inventory %s approved.\n", record.ID[:12])
		} else {
			fmt.Fprintf(environment.Stdout, "Inventory %s matches the checkout and recorded build inputs.\n", record.ID[:12])
		}
		return nil
	}
	return nil
}

func runInventoryBuildCLI(ctx context.Context, repository *GitRepository, compilePath string, explicitCompilePath bool, policyPath string, asJSON bool, environment cliEnvironment) error {
	options := inventoryBuildOptions{CompileCommands: compilePath, Policy: InventoryPolicy{Rules: []InventoryRule{}}}
	if policyPath != "" {
		data, err := os.ReadFile(reposeAbsolutePath(environment.Cwd, policyPath))
		if err != nil {
			return fmt.Errorf("read inventory policy: %w", err)
		}
		options.Policy, err = readInventoryPolicy(strings.NewReader(string(data)))
		if err != nil {
			return err
		}
	}
	databasePath, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	var store *inventoryStore
	defer func() {
		if store != nil {
			store.Close()
		}
	}()
	currentID := ""
	if _, err := os.Stat(databasePath); err == nil {
		store, err = openInventoryStore(ctx, databasePath, false)
		if err != nil {
			return err
		}
		currentID, err = store.CurrentInventoryID(ctx)
		if err != nil {
			return err
		}
		if currentID != "" {
			current, err := store.Inventory(ctx, currentID)
			if err != nil {
				return err
			}
			if policyPath == "" {
				options.Policy = current.Inventory.Policy
				options.AllowUnmatchedPolicy = true
			}
			if !explicitCompilePath {
				options.CompileCommands = current.Inventory.BuildInputs[0].Path
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	inventory, err := buildInventory(ctx, repository, options)
	if err != nil {
		return err
	}
	if store == nil {
		store, err = openInventoryStore(ctx, databasePath, true)
		if err != nil {
			return err
		}
	}
	record, err := store.saveCurrentInventory(ctx, inventory, &currentID, environmentNow(environment))
	if err != nil {
		return err
	}
	if asJSON {
		return writeInventoryJSON(environment.Stdout, record)
	}
	return printInventorySummary(environment.Stdout, record)
}

func reposeAbsolutePath(cwd, filename string) string {
	if filepath.IsAbs(filename) {
		return filepath.Clean(filename)
	}
	return filepath.Join(cwd, filename)
}

func inventoryFileMatchesStatus(file InventoryFile, status string) bool {
	switch status {
	case "all":
		return true
	case "included":
		return !file.Excluded
	case "excluded":
		return file.Excluded
	case "missing-command":
		return !file.Excluded && file.Kind == "source" && len(file.CommandIDs) == 0
	case "header-unmapped":
		return !file.Excluded && file.Kind == "header" && len(file.CommandIDs) == 0
	}
	return false
}

func inventoryReviewLabel(reviewed bool) string {
	if reviewed {
		return "approved"
	}
	return "awaiting review"
}

func writeInventoryJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printInventorySummary(output io.Writer, record InventoryRecord) error {
	inventory := record.Inventory
	fmt.Fprintf(output, "Inventory: %s (%s)\nSnapshot:  %s\nCheckout:  %s\n", record.ID[:12], inventoryReviewLabel(record.ReviewedAt != nil), inventory.SnapshotSHA, inventory.WorkTree)
	fmt.Fprintf(output, "Build:     %s\n", inventory.BuildInputs[0].Path)
	type counts struct{ included, sources, headers, excluded, missing int }
	groups := make(map[string]*counts)
	var total counts
	tracked := make(map[string]bool, len(inventory.Files))
	unmappedHeaders := 0
	for _, file := range inventory.Files {
		tracked[filepath.Join(inventory.WorkTree, filepath.FromSlash(file.Path))] = true
		if groups[file.Group] == nil {
			groups[file.Group] = &counts{}
		}
		for _, count := range []*counts{groups[file.Group], &total} {
			if file.Excluded {
				count.excluded++
			} else {
				count.included++
				if file.Kind == "source" {
					count.sources++
					if len(file.CommandIDs) == 0 {
						count.missing++
					}
				} else if file.Kind == "header" {
					count.headers++
				}
			}
		}
		if inventoryFileMatchesStatus(file, "header-unmapped") {
			unmappedHeaders++
		}
	}
	external, untracked := make(map[string]bool), make(map[string]bool)
	for _, command := range inventory.Commands {
		relative, err := filepath.Rel(inventory.WorkTree, command.File)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			external[command.File] = true
		} else if !tracked[command.File] {
			untracked[command.File] = true
		}
	}
	fmt.Fprintf(output, "Files:     %d tracked; %d in scope (%d sources, %d headers); %d excluded\n", len(inventory.Files), total.included, total.sources, total.headers, total.excluded)
	fmt.Fprintf(output, "Commands:  %d entries; %d external and %d untracked input files outside the inventory\n", len(inventory.Commands), len(external), len(untracked))
	fmt.Fprintf(output, "Coverage:  %d in-scope sources without commands; %d headers without direct compilation commands\n", total.missing, unmappedHeaders)
	fmt.Fprintln(output, "Use inventory index-status for saved semantic coverage. Inventory approval does not run a model.")
	if unmatched := inventoryUnmatchedPrefixes(inventory); len(unmatched) > 0 {
		fmt.Fprintf(output, "Retained policy prefixes without matches in this snapshot: %s\n", strings.Join(unmatched, ", "))
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "\nGROUP\tIN SCOPE\tSOURCES\tHEADERS\tEXCLUDED\tMISSING COMMAND")
	for _, key := range keys {
		count := groups[key]
		fmt.Fprintf(writer, "%q\t%d\t%d\t%d\t%d\t%d\n", key, count.included, count.sources, count.headers, count.excluded, count.missing)
	}
	return writer.Flush()
}
