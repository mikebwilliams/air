package main

import (
	"fmt"
	"io"
	"strings"
)

const (
	reposeCategoryGettingStarted = "Getting started"
	reposeCategoryInventory      = "Repository inventory"
	reposeCategoryReview         = "Repository review"
	reposeCategoryFindings       = "Finding review"
	reposeCategoryReports        = "Reports"
	reposeCategoryMaintenance    = "Configuration and maintenance"
)

var reposeCategoryOrder = []string{
	reposeCategoryGettingStarted,
	reposeCategoryInventory,
	reposeCategoryReview,
	reposeCategoryFindings,
	reposeCategoryReports,
	reposeCategoryMaintenance,
}

var reposeCLICommands = []cliCommandSpec{
	{
		Name: "doctor", Category: reposeCategoryGettingStarted,
		Summary:     "Check the repository and saved Repose state.",
		Description: "Perform a read-only preflight of repository identity, database integrity, inventories, semantic coverage, scans, runners, reviewer guidance, and model pricing.",
		Usage:       []string{"repose doctor [OPTIONS]"},
		Options:     reposeJSONHelpOptions(),
		Run:         runReposeDoctorCLI,
	},
	{
		Name: "status", Category: reposeCategoryGettingStarted,
		Summary:     "Summarize scans, findings, usage, and remaining work.",
		Description: "Show the current inventory, scan and assignment state, finding totals, token usage, worker time, and an optional wall-clock estimate for unfinished work.",
		Usage:       []string{"repose status [OPTIONS]"},
		Options: reposeJSONHelpOptions(
			cliHelpOption{"--scan ID|latest", "Restrict every total to one scan; a recheck uses its source scan for findings."},
			cliHelpOption{"--jobs N", "Estimate remaining wall-clock time at 1 through 32 parallel jobs."},
		),
		Run: runReposeStatusCLI,
	},
	{
		Name: "version", Category: reposeCategoryGettingStarted,
		Summary: "Print the Repose version.", Usage: []string{"repose version"},
		Run: runReposeVersionCommand,
	},
	{
		Name: "inventory", Category: reposeCategoryInventory,
		Summary:     "Build, inspect, organize, index, and approve repository scope.",
		Description: "Manage immutable repository inventories, scope policy, annotations, clangd semantic facts, and deterministic assignment previews.",
		Usage:       []string{"repose inventory COMMAND [ARGUMENTS]"},
		Children:    reposeInventoryHelpCommands(),
		Run:         runReposeInventoryCommand,
	},
	{
		Name: "scan", Category: reposeCategoryReview,
		Summary:     "Create and run durable parallel repository scans.",
		Description: "Freeze an approved inventory and reviewer configuration into assignments, then run, resume, inspect, pause, or interrupt their retained attempts.",
		Usage:       []string{"repose scan COMMAND [ARGUMENTS]"},
		Children:    reposeScanHelpCommands(),
		Run:         runAuditCLI,
	},
	{
		Name: "recheck", Category: reposeCategoryReview,
		Summary:     "Verify selected findings with another model pass.",
		Description: "Group open findings by their original assignments, give every finding an independent verdict, and retain the recheck as a resumable scan.",
		Usage:       []string{"repose recheck [OPTIONS] [FINDING_ID ...]"},
		Options: reposeJSONHelpOptions(
			cliHelpOption{"--scan ID|latest", "Select the original source scan (default: latest completed original scan)."},
			cliHelpOption{"--path PREFIX", "Restrict findings to a repository path prefix."},
			cliHelpOption{"--tag TAG", "Require an exact finding tag; repeatable with AND semantics."},
			cliHelpOption{"--hint TEXT", "Add one-off reviewer guidance; repeatable."},
			cliHelpOption{"--batch-max N", "Put at most N findings from one original assignment in each batch (default: 5)."},
			cliHelpOption{"--harness HARNESS", "Use codex, claude, or gemini (default: codex)."},
			cliHelpOption{"--model MODEL", "Required verification model identifier."},
			cliHelpOption{"--effort LEVEL", "Reasoning effort (default: high; Gemini requires default)."},
			cliHelpOption{"--binary PATH", "Override the selected harness executable."},
			cliHelpOption{"--timeout DURATION", "Per-batch timeout; also overrides it when resuming (default: 30m)."},
			cliHelpOption{"--jobs N", "Run 1 through 32 batches concurrently (default: 2)."},
			cliHelpOption{"--limit N", "Attempt at most N batches including retries; zero is unlimited."},
			cliHelpOption{"--duration DURATION", "Stop dispatching after this duration and drain active work."},
			cliHelpOption{"--retry-failed", "Retry failed batches while retaining their prior attempts."},
			cliHelpOption{"--dry-run", "Preview selected findings and batches without saving or invoking a model."},
			cliHelpOption{"--create-only", "Save the recheck queue without invoking a model."},
			cliHelpOption{"--force", "Create a fresh pass instead of resuming an identical saved recheck."},
		),
		Run: runAuditRecheckCLI,
	},
	{
		Name: "findings", Category: reposeCategoryFindings,
		Summary:     "Browse findings in the terminal or export a simple JSON list.",
		Description: "Open the interactive triage browser. With --json, write selected findings without starting the terminal UI.",
		Usage:       []string{"repose findings [OPTIONS]"},
		Options: reposeJSONHelpOptions(
			cliHelpOption{"--scan ID", "Restrict findings to one scan; a recheck selects its source scan."},
			cliHelpOption{"--verification VERDICT", "Filter by all, unchecked, confirmed, false_positive, or uncertain."},
			cliHelpOption{"--tag TAG", "Require an exact tag; repeatable with AND semantics."},
			cliHelpOption{"--all", "Include dismissed findings."},
		),
		Run: runReposeFindingsCommand,
	},
	{
		Name: "finding", Category: reposeCategoryFindings,
		Summary:     "List, inspect, and triage saved findings.",
		Description: "Show finding provenance and history, list filtered findings, inspect recorded source, open a matching checkout file, or record manual triage actions.",
		Usage:       []string{"repose finding FINDING_ID", "repose finding COMMAND [ARGUMENTS]"},
		Children:    reposeFindingHelpCommands(),
		Run:         runReposeFindingCommand,
	},
	{
		Name: "tags", Category: reposeCategoryFindings,
		Summary:     "List finding tags and their counts.",
		Description: "Count exact tags on open findings in the selected source scan. Include dismissed findings with --all.",
		Usage:       []string{"repose tags [OPTIONS]"},
		Options: reposeJSONHelpOptions(
			cliHelpOption{"--scan ID|latest", "Restrict counts to one original source scan."},
			cliHelpOption{"--all", "Include dismissed findings."},
		),
		Run: runAuditTagsCLI,
	},
	{
		Name: "stats", Category: reposeCategoryReports,
		Summary:     "Summarize attempts, usage, time, cost, and findings.",
		Description: "Aggregate retained scan and recheck attempts, including failures and retries, with per-model groups and current finding totals.",
		Usage:       []string{"repose stats [OPTIONS]"}, Options: reposeReportingHelpOptions(), Run: runReposeStatsCommand,
	},
	{
		Name: "cost", Category: reposeCategoryReports,
		Summary:     "Show the accounting-focused scan summary.",
		Description: "Present the same retained attempt accounting as stats with estimated or provider-reported cost first. Estimates do not represent Codex subscription use.",
		Usage:       []string{"repose cost [OPTIONS]"}, Options: reposeReportingHelpOptions(), Run: runReposeCostCommand,
	},
	{
		Name: "export", Category: reposeCategoryReports,
		Summary:     "Export findings as JSON, SARIF, or offline HTML.",
		Description: "Write a snapshot-aware report. The HTML format is a self-contained searchable and filterable findings viewer with shareable finding fragments.",
		Usage:       []string{"repose export --format FORMAT [-o FILE] [OPTIONS]"},
		Options: reposeRepoHelpOptions(
			cliHelpOption{"--format FORMAT", "Required format: json, sarif, or html."},
			cliHelpOption{"-o, --output FILE", "Write to FILE instead of standard output; use - for standard output."},
			cliHelpOption{"--scan ID|latest", "Restrict the report to one source scan."},
			cliHelpOption{"--path PREFIX", "Restrict findings to a repository path prefix."},
			cliHelpOption{"--verification VERDICT", "Filter by all, unchecked, confirmed, false_positive, or uncertain."},
			cliHelpOption{"--tag TAG", "Require an exact finding tag; repeatable with AND semantics."},
			cliHelpOption{"--all", "Include dismissed findings; for HTML, make all dispositions initially visible."},
		),
		Run: runAuditExportCLI,
	},
	{
		Name: "prompt", Category: reposeCategoryMaintenance,
		Summary:     "Inspect and customize scan and recheck prompts.",
		Description: "Manage the editable reviewer instructions while Repose keeps its safety, scope, and response protocol fixed.",
		Usage:       []string{"repose prompt COMMAND [ARGUMENTS]"},
		Children:    reposePromptHelpCommands(),
		Run:         runReposePromptCLI,
	},
	{
		Name: "hint", Category: reposeCategoryMaintenance,
		Summary:     "Manage supplemental reviewer guidance.",
		Description: "Save hints used by future scans and rechecks, or inspect the exact hints frozen into a saved scan.",
		Usage:       []string{"repose hint COMMAND [ARGUMENTS]"},
		Children:    reposeHintHelpCommands(),
		Run:         runReposeHintCLI,
	},
	{
		Name: "model", Category: reposeCategoryMaintenance,
		Summary:     "Inspect models and manage token pricing.",
		Description: "List built-in, overridden, and observed models; inspect one model; or store repository-specific pricing metadata.",
		Usage:       []string{"repose model COMMAND [ARGUMENTS]"},
		Children:    reposeModelHelpCommands(),
		Run:         runReposeModelCLI,
	},
	{
		Name: "db", Category: reposeCategoryMaintenance,
		Summary:     "Inspect the worktree-local database location.",
		Description: "Print Repose's absolute SQLite path without opening or creating the database.",
		Usage:       []string{"repose db path [OPTIONS]"},
		Children: []cliCommandSpec{{
			Name: "path", Summary: "Print the Repose database path.",
			Description: "Print the absolute worktree-local SQLite path. This works before Repose has created any state.",
			Usage:       []string{"repose db path [OPTIONS]"}, Options: reposeRepoHelpOptions(),
		}},
		Run: runReposeDBCLI,
	},
	{
		Name: "backup", Category: reposeCategoryMaintenance,
		Summary:     "Create or import a consistent database backup.",
		Description: "Create an integrity-checked SQLite snapshot while scans run, or validate and install a backup into the selected scan checkout.",
		Usage:       []string{"repose backup [PATH] [OPTIONS]", "repose backup import [OPTIONS] PATH"},
		Options:     reposeRepoHelpOptions(),
		Children: []cliCommandSpec{{
			Name: "import", Summary: "Replace Repose state with a verified backup.",
			Description: "Validate schema, integrity, foreign keys, inventories, and Git snapshots before replacing worktree-local state while coordinator locks are held.",
			Usage:       []string{"repose backup import [OPTIONS] PATH"},
			Options:     reposeRepoHelpOptions(cliHelpOption{"--force", "Replace existing state without prompting."}),
		}},
		Run: runReposeBackupCLI,
	},
}

func reposeInventoryHelpCommands() []cliCommandSpec {
	selection := func(status bool) []cliHelpOption {
		options := []cliHelpOption{
			{"--path PREFIX", "Select an exact repository path or directory descendants."},
			{"--group NAME", "Select one inventory group."},
			{"--tag TAG", "Select one inventory tag."},
		}
		if status {
			options = append(options, cliHelpOption{"--status STATUS", "Select included, excluded, all, missing-command, or header-unmapped files."})
		}
		return options
	}
	semanticSelection := func(options ...cliHelpOption) []cliHelpOption {
		return append(selection(true), options...)
	}
	return []cliCommandSpec{
		{
			Name: "build", Summary: "Capture HEAD, build inputs, tracked files, and policy.",
			Description: "Build an immutable inventory in a clean scan checkout. An identical inventory is reused; no compiler or model runs.",
			Usage:       []string{"repose inventory build [OPTIONS]"},
			Options: reposeJSONHelpOptions(
				cliHelpOption{"--compile-commands FILE", "Compilation database relative to the checkout (default: build/compile_commands.json)."},
				cliHelpOption{"--policy FILE", "Replace inherited policy with this JSON file after a successful build."},
			),
		},
		{Name: "list", Summary: "List immutable inventory versions.", Usage: []string{"repose inventory list [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "show", Summary: "Show one inventory and its saved document.", Usage: []string{"repose inventory show [ID] [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{
			Name: "files", Summary: "List files in an inventory selection.",
			Usage: []string{"repose inventory files [ID] [OPTIONS]"}, Options: reposeJSONHelpOptions(selection(true)...),
		},
		{
			Name: "tree", Summary: "Summarize selected files by directory.",
			Usage:   []string{"repose inventory tree [ID] [OPTIONS]"},
			Options: reposeJSONHelpOptions(append(selection(true), cliHelpOption{"--depth N", "Show N directory levels below --path (default: 1)."})...),
		},
		{Name: "groups", Summary: "Summarize selected files by group.", Usage: []string{"repose inventory groups [ID] [OPTIONS]"}, Options: reposeJSONHelpOptions(selection(true)...)},
		{Name: "inspect", Summary: "Inspect scope, policy, build coverage, and large files.", Usage: []string{"repose inventory inspect [ID] [OPTIONS]"}, Options: reposeJSONHelpOptions(selection(true)...)},
		{
			Name: "group", Summary: "Assign a group to path prefixes in current policy.",
			Usage:   []string{"repose inventory group PREFIX... --name NAME [OPTIONS]"},
			Options: reposeJSONHelpOptions(cliHelpOption{"--name NAME", "Required nonblank group name."}),
		},
		{
			Name: "annotate", Summary: "Add tags or a note to path prefixes.",
			Usage: []string{"repose inventory annotate PREFIX... [OPTIONS]"},
			Options: reposeJSONHelpOptions(
				cliHelpOption{"--tag TAG", "Add an inventory tag; repeatable."},
				cliHelpOption{"--note TEXT", "Add a review note."},
			),
		},
		{
			Name: "plan", Summary: "Preview deterministic scan assignments.",
			Description: "Partition selected files or saved semantic ranges without saving a scan or invoking a model.",
			Usage:       []string{"repose inventory plan [ID] [OPTIONS]"},
			Options: reposeJSONHelpOptions(append(selection(false),
				cliHelpOption{"--goal TEXT", "Question attached to every assignment (default: Find correctness issues)."},
				cliHelpOption{"--max-files N", "Maximum files per assignment (default: 8)."},
				cliHelpOption{"--max-bytes N", "Maximum target source bytes per assignment (default: 65536)."},
				cliHelpOption{"--semantic", "Use saved symbols and resolved include context."},
				cliHelpOption{"--index ID", "Use this semantic profile; implies --semantic."},
				cliHelpOption{"--tui", "Open the assignment, symbol, context, and diagnostic browser."},
			)...),
		},
		{
			Name: "browse", Summary: "Browse and edit inventory scope in the terminal.",
			Usage: []string{"repose inventory browse [ID] [OPTIONS]"}, Options: reposeRepoHelpOptions(selection(true)...),
		},
		{
			Name: "index", Summary: "Build or resume a clangd semantic index.",
			Description: "Parse selected C/C++ files through one shared clangd process and retain results incrementally.",
			Usage:       []string{"repose inventory index [ID] [OPTIONS]"},
			Options: reposeJSONHelpOptions(semanticSelection(
				cliHelpOption{"--clangd PATH", "clangd 21+ executable (default: clangd)."},
				cliHelpOption{"--jobs N", "Concurrent files in the shared process (default: 2)."},
				cliHelpOption{"--timeout DURATION", "Initialization and per-file timeout (default: 2m)."},
				cliHelpOption{"--max-files N", "Attempt at most N additional files; zero is unlimited."},
				cliHelpOption{"--retry-errors", "Retry partial, failed, and unavailable results."},
				cliHelpOption{"--refresh", "Reparse selected files while retaining older result history."},
			)...),
		},
		{Name: "index-status", Summary: "Show semantic coverage and diagnostics.", Usage: []string{"repose inventory index-status [ID] [OPTIONS]"}, Options: reposeJSONHelpOptions(semanticSelection(cliHelpOption{"--index ID", "Select a semantic profile instead of the latest matching one."})...)},
		{Name: "symbols", Summary: "List saved symbols for selected files.", Usage: []string{"repose inventory symbols [ID] [OPTIONS]"}, Options: reposeJSONHelpOptions(semanticSelection(cliHelpOption{"--index ID", "Select a semantic profile instead of the latest matching one."})...)},
		{Name: "includes", Summary: "List saved resolved include links.", Usage: []string{"repose inventory includes [ID] [OPTIONS]"}, Options: reposeJSONHelpOptions(semanticSelection(cliHelpOption{"--index ID", "Select a semantic profile instead of the latest matching one."})...)},
		{Name: "check", Summary: "Verify an inventory against the live checkout.", Usage: []string{"repose inventory check [ID] [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "approve", Summary: "Verify and approve an explicit inventory version.", Usage: []string{"repose inventory approve ID [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{
			Name: "exclude", Summary: "Exclude path prefixes in a new current inventory.",
			Usage:   []string{"repose inventory exclude PREFIX... --reason TEXT [OPTIONS]"},
			Options: reposeJSONHelpOptions(cliHelpOption{"--reason TEXT", "Required recorded reason for the exclusion."}),
		},
		{
			Name: "include", Summary: "Include path prefixes in a new current inventory.",
			Usage:   []string{"repose inventory include PREFIX... [OPTIONS]"},
			Options: reposeJSONHelpOptions(cliHelpOption{"--reason TEXT", "Optional recorded reason for the inclusion."}),
		},
		{Name: "exclusions", Summary: "List default and explicit scope rules with counts.", Usage: []string{"repose inventory exclusions [ID] [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{
			Name: "policy", Summary: "Export or replace inventory policy as JSON.",
			Usage: []string{"repose inventory policy COMMAND [ARGUMENTS]"},
			Children: []cliCommandSpec{
				{Name: "export", Summary: "Write one inventory's editable policy as JSON.", Usage: []string{"repose inventory policy export [ID] [OPTIONS]"}, Options: reposeRepoHelpOptions()},
				{Name: "import", Summary: "Replace current policy from a JSON file.", Usage: []string{"repose inventory policy import FILE [OPTIONS]"}, Options: reposeJSONHelpOptions()},
			},
		},
	}
}

func reposeScanHelpCommands() []cliCommandSpec {
	runOptions := func() []cliHelpOption {
		return reposeJSONHelpOptions(
			cliHelpOption{"--jobs N", "Run 1 through 32 assignments concurrently (default: 2)."},
			cliHelpOption{"--limit N", "Attempt at most N assignments including retries; zero is unlimited."},
			cliHelpOption{"--duration DURATION", "Stop dispatching after this duration and drain active work."},
			cliHelpOption{"--timeout DURATION", "Override the saved per-assignment timeout for this invocation."},
			cliHelpOption{"--retry-failed", "Retry failed assignments while retaining their prior attempts."},
		)
	}
	return []cliCommandSpec{
		{
			Name: "create", Summary: "Freeze an approved inventory into a durable scan queue.",
			Description: "Create deterministic assignments and freeze the model, prompts, hints, scope, and observed snapshot without making model calls.",
			Usage:       []string{"repose scan create --model MODEL [OPTIONS]"},
			Options: reposeJSONHelpOptions(
				cliHelpOption{"--inventory ID", "Approved inventory version (default: current)."},
				cliHelpOption{"--index ID", "Use a saved semantic profile; omit for file-based assignments."},
				cliHelpOption{"--path PREFIX", "Restrict targets to a repository path prefix."},
				cliHelpOption{"--group NAME", "Restrict targets to one inventory group."},
				cliHelpOption{"--tag TAG", "Restrict targets to one inventory tag."},
				cliHelpOption{"--goal TEXT", "Review question (default: Find correctness issues)."},
				cliHelpOption{"--max-files N", "Maximum files per assignment (default: 8)."},
				cliHelpOption{"--max-bytes N", "Maximum target source bytes per assignment (default: 65536)."},
				cliHelpOption{"--harness HARNESS", "Use codex, claude, or gemini (default: codex)."},
				cliHelpOption{"--model MODEL", "Required model identifier."},
				cliHelpOption{"--effort LEVEL", "Reasoning effort (default: high; Gemini requires default)."},
				cliHelpOption{"--binary PATH", "Override the selected harness executable."},
				cliHelpOption{"--timeout DURATION", "Per-assignment timeout (default: 30m)."},
				cliHelpOption{"--instructions FILE", "Freeze additional project guidance from FILE."},
				cliHelpOption{"--hint TEXT", "Freeze one-off reviewer guidance; repeatable."},
			),
		},
		{Name: "run", Summary: "Run one saved scan queue.", Usage: []string{"repose scan run ID [OPTIONS]"}, Options: runOptions()},
		{Name: "resume", Summary: "Resume a scan, or select the only resumable scan.", Usage: []string{"repose scan resume [ID] [OPTIONS]"}, Options: runOptions()},
		{Name: "list", Summary: "List saved scans and queue counts.", Usage: []string{"repose scan list [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "show", Summary: "Show one scan's frozen configuration and status.", Usage: []string{"repose scan show ID [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "tasks", Summary: "List assignments or recheck batches in a scan.", Usage: []string{"repose scan tasks ID [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "attempts", Summary: "List retained attempts, errors, timing, and usage.", Usage: []string{"repose scan attempts ID [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "prompt", Summary: "Print the exact prompt for one saved assignment.", Usage: []string{"repose scan prompt ID TASK [OPTIONS]"}, Options: reposeRepoHelpOptions()},
		{Name: "pause", Summary: "Stop dispatching and let active workers finish.", Usage: []string{"repose scan pause ID [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "interrupt", Summary: "Cancel active workers and stop dispatching.", Usage: []string{"repose scan interrupt ID [OPTIONS]"}, Options: reposeJSONHelpOptions()},
	}
}

func reposeFindingHelpCommands() []cliCommandSpec {
	return []cliCommandSpec{
		{
			Name: "list", Summary: "List findings without opening the terminal UI.", Usage: []string{"repose finding list [OPTIONS]"},
			Options: reposeJSONHelpOptions(
				cliHelpOption{"--scan ID|latest", "Restrict findings to one original source scan."},
				cliHelpOption{"--status STATUS", "Filter by open, dismissed, or all (default: open)."},
				cliHelpOption{"--severity SEVERITY", "Filter by error, warning, info, or all."},
				cliHelpOption{"--verification VERDICT", "Filter by all, unchecked, confirmed, false_positive, or uncertain."},
				cliHelpOption{"--path PREFIX", "Restrict findings to a repository path prefix."},
				cliHelpOption{"--tag TAG", "Require an exact tag; repeatable with AND semantics."},
				cliHelpOption{"--search TEXT", "Search title, description, location, tags, provenance, verdict, or ID."},
				cliHelpOption{"--sort SORT", "Sort by id, age, file, scan, severity, status, title, or verification."},
				cliHelpOption{"--limit N", "Return at most N findings; zero is unlimited."},
				cliHelpOption{"--all", "Include every disposition; conflicts with --status."},
			),
		},
		{Name: "show", Summary: "Show one finding's provenance and complete history.", Usage: []string{"repose finding show ID [OPTIONS]", "repose finding ID [OPTIONS]"}, Options: reposeJSONHelpOptions(cliHelpOption{"--scan ID", "Restrict lookup to one scan; a recheck selects its source scan."})},
		{Name: "dismiss", Summary: "Dismiss an open finding with a recorded reason.", Usage: []string{"repose finding dismiss ID --reason TEXT [OPTIONS]"}, Options: reposeRepoHelpOptions(cliHelpOption{"--reason TEXT", "Required dismissal reason stored in finding history."})},
		{Name: "reopen", Summary: "Return a dismissed finding to open.", Usage: []string{"repose finding reopen ID [OPTIONS]"}, Options: reposeRepoHelpOptions()},
		{Name: "note", Summary: "Add a note to a finding's history.", Usage: []string{"repose finding note ID --reason TEXT [OPTIONS]"}, Options: reposeRepoHelpOptions(cliHelpOption{"--reason TEXT", "Note text to store in finding history."})},
		{Name: "source", Summary: "Show source from the exact recorded snapshot.", Usage: []string{"repose finding source ID [OPTIONS]"}, Options: reposeJSONHelpOptions(cliHelpOption{"--context N", "Lines before and after the finding (default: 20; maximum: 1000)."})},
		{Name: "open", Summary: "Open the finding in the configured Git editor.", Description: "Open the live checkout file only when HEAD and that file match the finding's recorded snapshot.", Usage: []string{"repose finding open ID [OPTIONS]"}, Options: reposeRepoHelpOptions()},
		{Name: "tag", Summary: "Add tags to one or more findings atomically.", Usage: []string{"repose finding tag ID... --tag TAG [--tag TAG...] [OPTIONS]"}, Options: reposeJSONHelpOptions(cliHelpOption{"--scan ID", "Restrict IDs to one scan."}, cliHelpOption{"--tag TAG", "Tag to add; repeatable and at least one is required."})},
		{Name: "untag", Summary: "Remove tags from one or more findings atomically.", Usage: []string{"repose finding untag ID... --tag TAG [--tag TAG...] [OPTIONS]"}, Options: reposeJSONHelpOptions(cliHelpOption{"--scan ID", "Restrict IDs to one scan."}, cliHelpOption{"--tag TAG", "Tag to remove; repeatable and at least one is required."})},
	}
}

func reposePromptHelpCommands() []cliCommandSpec {
	return []cliCommandSpec{
		{Name: "list", Summary: "List prompt sources, identities, and active hint counts.", Usage: []string{"repose prompt list [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "show", Summary: "Show editable or full effective prompt text.", Usage: []string{"repose prompt show [--full] scan|recheck [OPTIONS]"}, Options: reposeJSONHelpOptions(cliHelpOption{"--full", "Include active hints and Repose's fixed protocol and response contract."})},
		{Name: "set", Summary: "Store repository-specific reviewer instructions.", Usage: []string{"repose prompt set --file FILE scan|recheck [OPTIONS]", "repose prompt set --stdin scan|recheck [OPTIONS]"}, Options: reposeJSONHelpOptions(cliHelpOption{"--file FILE", "Read UTF-8 reviewer instructions from FILE."}, cliHelpOption{"--stdin", "Read reviewer instructions from standard input."})},
		{Name: "reset", Summary: "Restore the built-in reviewer instructions.", Usage: []string{"repose prompt reset scan|recheck [OPTIONS]"}, Options: reposeJSONHelpOptions()},
	}
}

func reposeHintHelpCommands() []cliCommandSpec {
	return []cliCommandSpec{
		{Name: "add", Summary: "Save a hint and print its stable ID.", Usage: []string{"repose hint add TEXT [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "list", Summary: "List active hints or a saved scan's frozen hints.", Usage: []string{"repose hint list [OPTIONS]"}, Options: reposeJSONHelpOptions(cliHelpOption{"--scan ID|latest", "Show the exact hints frozen in a saved scan."})},
		{Name: "remove", Summary: "Remove an active hint without changing scan history.", Usage: []string{"repose hint remove HINT_ID [OPTIONS]"}, Options: reposeJSONHelpOptions()},
	}
}

func reposeModelHelpCommands() []cliCommandSpec {
	pricing := append(modelPricingHelpOptions(), cliHelpOption{"--json", "Write machine-readable JSON."}, cliHelpOption{"--repo DIR", "Use this dedicated scan checkout (default: current directory)."})
	return []cliCommandSpec{
		{Name: "list", Summary: "List known models and pricing status.", Usage: []string{"repose model list [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "show", Summary: "Show one model's source, usage, and pricing details.", Usage: []string{"repose model show NAME [OPTIONS]"}, Options: reposeJSONHelpOptions()},
		{Name: "set-pricing", Summary: "Store short- and long-context token prices.", Usage: []string{"repose model set-pricing NAME [OPTIONS]"}, Options: pricing},
		{Name: "mark-pricing-unknown", Summary: "Explicitly record that a model's pricing is unknown.", Usage: []string{"repose model mark-pricing-unknown NAME [OPTIONS]"}, Options: reposeJSONHelpOptions()},
	}
}

func reposeReportingHelpOptions() []cliHelpOption {
	return reposeJSONHelpOptions(
		cliHelpOption{"--scan ID|latest", "Restrict accounting to one saved scan."},
		cliHelpOption{"--model MODEL", "Include only attempts performed by this exact model."},
		cliHelpOption{"--since DATE", "Include attempts on or after YYYY-MM-DD or RFC3339."},
	)
}

func reposeRepoHelpOptions(options ...cliHelpOption) []cliHelpOption {
	result := append([]cliHelpOption(nil), options...)
	return append(result, cliHelpOption{"--repo DIR", "Use this dedicated scan checkout (default: current directory)."})
}

func reposeJSONHelpOptions(options ...cliHelpOption) []cliHelpOption {
	result := append([]cliHelpOption(nil), options...)
	result = append(result, cliHelpOption{"--json", "Write machine-readable JSON."})
	return reposeRepoHelpOptions(result...)
}

func findReposeCLICommand(name string) (*cliCommandSpec, bool) {
	for index := range reposeCLICommands {
		if reposeCLICommands[index].Name == name {
			return &reposeCLICommands[index], true
		}
	}
	return nil, false
}

func runReposeHelpCommand(args []string, environment cliEnvironment) error {
	if containsHelpFlag(args) {
		filtered := make([]string, 0, len(args))
		for _, argument := range args {
			if argument != "-h" && argument != "--help" {
				filtered = append(filtered, argument)
			}
		}
		args = filtered
	}
	if len(args) == 0 {
		printReposeUsage(environment.Stdout)
		return nil
	}
	command, found := findReposeCLICommand(args[0])
	if !found {
		return fmt.Errorf("unknown help topic %q; run repose help", strings.Join(args, " "))
	}
	target := command
	for _, name := range args[1:] {
		child, found := target.child(name)
		if !found {
			return fmt.Errorf("unknown help topic %q; run repose help %s", strings.Join(args, " "), command.Name)
		}
		target = child
	}
	printReposeCommandHelp(environment.Stdout, target)
	return nil
}

func printReposeUsage(output io.Writer) {
	fmt.Fprintln(output, "Repose performs sustained, parallel audits of fixed C/C++ repository snapshots.")
	fmt.Fprintln(output, "\nUsage:\n  repose COMMAND [OPTIONS]\n  repose help [COMMAND [SUBCOMMAND]]\n  repose --help\n  repose --version")
	for _, category := range reposeCategoryOrder {
		fmt.Fprintf(output, "\n%s:\n", category)
		for _, command := range reposeCLICommands {
			if command.Category == category {
				fmt.Fprintf(output, "  %-10s %s\n", command.Name, command.Summary)
			}
		}
	}
	fmt.Fprintln(output, "\nHelp:\n  help       Show global or command-specific help.")
	fmt.Fprintln(output, "\nIDs may be hexadecimal prefixes. Inventory selectors also accept current or latest where documented.")
	fmt.Fprintln(output, "Run \"repose help COMMAND\" for command details.")
}

func printReposeCommandHelp(output io.Writer, command *cliCommandSpec) {
	fmt.Fprintln(output, "Usage:")
	for _, usage := range command.Usage {
		fmt.Fprintf(output, "  %s\n", usage)
	}
	description := command.Description
	if description == "" {
		description = command.Summary
	}
	if description != "" {
		fmt.Fprintln(output)
		printWrappedHelp(output, "", "", description, 88)
	}
	if len(command.Children) != 0 {
		fmt.Fprintln(output, "\nCommands:")
		for _, child := range command.Children {
			printWrappedHelp(output, fmt.Sprintf("  %-24s ", child.Name), strings.Repeat(" ", 27), child.Summary, 96)
		}
	}
	fmt.Fprintln(output, "\nOptions:")
	for _, option := range command.Options {
		printWrappedHelp(output, fmt.Sprintf("  %-30s ", option.Syntax), strings.Repeat(" ", 33), option.Description, 96)
	}
	fmt.Fprintln(output, "  -h, --help                     Show this help.")
	if len(command.Children) != 0 {
		fmt.Fprintf(output, "\nRun \"repose help %s COMMAND\" for subcommand details.\n", command.Name)
	}
}
