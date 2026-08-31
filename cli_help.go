package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

type cliCommandRunner func(context.Context, []string, cliEnvironment) error

type cliHelpOption struct {
	Syntax      string
	Description string
}

type cliCommandSpec struct {
	Name        string
	Category    string
	Summary     string
	Description string
	Usage       []string
	Options     []cliHelpOption
	Children    []cliCommandSpec
	Run         cliCommandRunner
}

const (
	commandCategoryGettingStarted = "Getting started"
	commandCategoryCommitReview   = "Commit review"
	commandCategoryFindingReview  = "Finding review"
	commandCategoryReports        = "History and reports"
	commandCategoryMaintenance    = "Configuration and maintenance"
)

var commandCategoryOrder = []string{
	commandCategoryGettingStarted,
	commandCategoryCommitReview,
	commandCategoryFindingReview,
	commandCategoryReports,
	commandCategoryMaintenance,
}

var cliCommands = []cliCommandSpec{
	{
		Name: "init", Category: commandCategoryGettingStarted,
		Summary:     "Initialize AIR at a baseline commit.",
		Description: "Initialize repository-specific AIR state. The baseline must be on master's first-parent history and is not reviewed.",
		Usage:       []string{"air init COMMIT"}, Run: runInit,
	},
	{
		Name: "doctor", Category: commandCategoryGettingStarted,
		Summary:     "Check repository, database, and reviewer setup.",
		Description: "Inspect AIR's repository state, database integrity, effective reviewer configuration, pricing, and backend prerequisites.",
		Usage:       []string{"air doctor [OPTIONS]"},
		Options:     []cliHelpOption{{"--json", "Write the report as JSON."}}, Run: runDoctor,
	},
	{
		Name: "status", Category: commandCategoryGettingStarted,
		Summary:     "Summarize findings and remaining commit work.",
		Description: "Show finding counts, unscanned and failed commit counts, and the estimated time needed to scan remaining commits.",
		Usage:       []string{"air status [OPTIONS]"},
		Options:     []cliHelpOption{{"--json", "Write the status as JSON."}}, Run: runStatus,
	},
	{
		Name: "pending", Category: commandCategoryCommitReview,
		Summary:     "Preview commits that a scan would process.",
		Description: "List pending commits in processing order without constructing a reviewer or changing AIR state.",
		Usage:       []string{"air pending [OPTIONS] [FROM..TO]"},
		Options:     []cliHelpOption{{"--limit N", "Show at most N commits; zero means unlimited."}}, Run: runPending,
	},
	{
		Name: "scan", Category: commandCategoryCommitReview,
		Summary:     "Review unprocessed commits on master.",
		Description: "Review unprocessed commits after AIR's baseline, or commits in an explicit first-parent range. Previously failed commits remain deferred until retry.",
		Usage:       []string{"air scan [OPTIONS] [FROM..TO]"},
		Options: append([]cliHelpOption{
			{"--limit N", "Process at most N commits; zero means unlimited."},
			{"--dry-run", "Show pending work without reviewing or writing."},
			{"--stop-on-error", "Stop after recording the first failed commit."},
		}, reviewerHelpOptions("per-commit")...), Run: runScan,
	},
	{
		Name: "retry", Category: commandCategoryCommitReview,
		Summary:     "Retry commits in the durable failure queue.",
		Description: "Review failed commits that still occur on master, oldest first, using the current reviewer configuration.",
		Usage:       []string{"air retry [OPTIONS]"},
		Options: append([]cliHelpOption{
			{"--limit N", "Process at most N failed commits; zero means unlimited."},
			{"--continue-on-error", "Continue after recording another failed attempt."},
		}, reviewerHelpOptions("per-commit")...), Run: runRetry,
	},
	{
		Name: "failures", Category: commandCategoryCommitReview,
		Summary:     "List commits in the durable failure queue.",
		Description: "Show each failed commit's latest error, attempt count, timestamp, and reviewer identity.",
		Usage:       []string{"air failures [OPTIONS]"},
		Options:     []cliHelpOption{{"--json", "Write the failure queue as JSON."}}, Run: runFailures,
	},
	{
		Name: "rescan", Category: commandCategoryCommitReview,
		Summary:     "Review an already reviewed commit again.",
		Description: "Review one processed commit again and make the new attempt current while retaining earlier review attempts.",
		Usage:       []string{"air rescan [OPTIONS] COMMIT"},
		Options:     reviewerHelpOptions("per-commit"), Run: runRescan,
	},
	{
		Name: "skip", Category: commandCategoryCommitReview,
		Summary:     "Mark matching unprocessed commits as skipped.",
		Description: "Skip one commit, or every unprocessed commit whose message contains a case-insensitive literal substring.",
		Usage: []string{
			"air skip [OPTIONS] COMMIT",
			"air skip [OPTIONS] --filter TEXT",
		},
		Options: []cliHelpOption{
			{"--filter TEXT", "Match a literal substring in full commit messages."},
			{"--reason TEXT", `Store this skip reason (default: "manual skip").`},
			{"--dry-run", "Show matching commits without changing AIR state."},
		}, Run: runSkip,
	},
	{
		Name: "clean", Category: commandCategoryCommitReview,
		Summary:     "Remove records for commits no longer on master.",
		Description: "Remove processed-commit and failure records whose commits no longer occur on master's first-parent history.",
		Usage:       []string{"air clean [OPTIONS]"},
		Options:     []cliHelpOption{{"--dry-run", "Show stale records without removing them."}}, Run: runClean,
	},
	{
		Name: "recheck", Category: commandCategoryFindingReview,
		Summary:     "Reconcile open findings against the current HEAD.",
		Description: "Ask a reviewer whether selected open findings are resolved in the exact HEAD snapshot. With no IDs, recheck every eligible open finding.",
		Usage:       []string{"air recheck [OPTIONS] [FINDING_ID ...]"},
		Options: append([]cliHelpOption{
			{"--limit N", "Recheck at most N findings; zero means unlimited."},
			{"--batch-size N", fmt.Sprintf("Send N findings per model call (default: %d; maximum: %d).", defaultRecheckBatchSize, maxRecheckBatchSize)},
			{"--force", "Repeat checks already completed with this reviewer at HEAD."},
			{"--dry-run", "Show pending work without reviewing or writing."},
			{"--continue-on-error", "Continue after a failed model batch."},
		}, reviewerHelpOptions("per-batch")...), Run: runRecheck,
	},
	{
		Name: "findings", Category: commandCategoryFindingReview,
		Summary:     "Browse findings in the interactive terminal UI.",
		Description: "Open the searchable, sortable findings browser. The command requires an interactive terminal.",
		Usage:       []string{"air findings [OPTIONS]"},
		Options:     []cliHelpOption{{"--all", "Start with findings of every disposition."}}, Run: runFindings,
	},
	{
		Name: "finding", Category: commandCategoryFindingReview,
		Summary:     "Inspect or modify one finding.",
		Description: "Show a finding and its event history, or perform a lifecycle, note, diff, or editor action.",
		Usage: []string{
			"air finding FINDING_ID",
			"air finding COMMAND [ARGUMENTS]",
		},
		Children: []cliCommandSpec{
			{
				Name: "dismiss", Summary: "Dismiss an open finding with a recorded reason.",
				Usage:   []string{"air finding dismiss --reason TEXT FINDING_ID"},
				Options: []cliHelpOption{{"--reason TEXT", "Required reason stored in the finding history."}},
			},
			{Name: "reopen", Summary: "Return a dismissed or resolved finding to open.", Usage: []string{"air finding reopen FINDING_ID"}},
			{Name: "note", Summary: "Add a note to a finding's history.", Usage: []string{"air finding note FINDING_ID TEXT"}},
			{Name: "diff", Summary: "Open the introducing commit in git difftool.", Usage: []string{"air finding diff FINDING_ID"}},
			{Name: "open", Summary: "Open the finding's file in the configured editor.", Usage: []string{"air finding open FINDING_ID"}},
		}, Run: runFinding,
	},
	{
		Name: "log", Category: commandCategoryReports,
		Summary:     "List processed commits and their outcomes.",
		Description: "Show reviewed and skipped commits in AIR's stored log.",
		Usage:       []string{"air log"}, Run: runLog,
	},
	{
		Name: "show", Category: commandCategoryReports,
		Summary:     "Show a commit review and retained attempts.",
		Description: "Show one processed commit's current review, accounting, findings, or a selected retained review attempt.",
		Usage:       []string{"air show [OPTIONS] COMMIT"},
		Options: []cliHelpOption{
			{"--reviews", "List all retained review attempts."},
			{"--review N", "Show retained review attempt N."},
			{"--json", "Write the selected review data as JSON."},
		}, Run: runShow,
	},
	{
		Name: "stats", Category: commandCategoryReports,
		Summary:     "Summarize review usage, cost, timing, and findings.",
		Description: "Aggregate retained commit-review and recheck attempts, along with current repository finding counts.",
		Usage:       []string{"air stats [OPTIONS]"}, Options: reportingHelpOptions(), Run: runStats,
	},
	{
		Name: "cost", Category: commandCategoryReports,
		Summary:     "Show the accounting-focused review summary.",
		Description: "Aggregate estimated API-equivalent cost and attempts by model and reasoning effort.",
		Usage:       []string{"air cost [OPTIONS]"}, Options: reportingHelpOptions(), Run: runCost,
	},
	{
		Name: "export", Category: commandCategoryReports,
		Summary:     "Export findings as JSON, SARIF, or offline HTML.",
		Description: "Write findings to standard output in the selected format. JSON and SARIF contain open findings; HTML is a static viewer containing every disposition.",
		Usage:       []string{"air export --format FORMAT"},
		Options:     []cliHelpOption{{"--format FORMAT", "Required output format: json, sarif, or html."}}, Run: runExport,
	},
	{
		Name: "prompt", Category: commandCategoryMaintenance,
		Summary:     "Inspect and customize reviewer prompts.",
		Description: "List repository prompt identities, inspect editable reviewer instructions or the full static prompt, and store or reset repository-specific overrides. AIR keeps its protocol and response contract fixed.",
		Usage:       []string{"air prompt COMMAND [ARGUMENTS]"},
		Children: []cliCommandSpec{
			{Name: "list", Summary: "List prompt sources and identities.", Usage: []string{"air prompt list"}},
			{
				Name: "show", Summary: "Show editable or full effective prompt text.",
				Usage: []string{"air prompt show --reviewer BACKEND [--full] KIND"},
				Options: []cliHelpOption{
					{"--reviewer BACKEND", "Required review backend: codex or http."},
					{"--full", "Include AIR's fixed protocol and response contract."},
				},
			},
			{
				Name: "set", Summary: "Store repository-specific reviewer instructions.",
				Usage: []string{
					"air prompt set --reviewer BACKEND --file PATH KIND",
					"air prompt set --reviewer BACKEND --stdin KIND",
				},
				Options: []cliHelpOption{
					{"--reviewer BACKEND", "Required review backend: codex or http."},
					{"--file PATH", "Read editable instructions from a UTF-8 text file."},
					{"--stdin", "Read editable instructions from standard input."},
				},
			},
			{
				Name: "reset", Summary: "Remove an override and restore built-in instructions.",
				Usage:   []string{"air prompt reset --reviewer BACKEND KIND"},
				Options: []cliHelpOption{{"--reviewer BACKEND", "Required review backend: codex or http."}},
			},
		}, Run: runPrompt,
	},
	{
		Name: "config", Category: commandCategoryMaintenance,
		Summary:     "Manage repository reviewer configuration.",
		Description: "Read and write reviewer settings stored in AIR's repository database. Effective values use command-line flags, environment variables, database settings, and built-in defaults in that order.",
		Usage:       []string{"air config COMMAND [ARGUMENTS]"},
		Children: []cliCommandSpec{
			{
				Name: "get", Summary: "Read one stored or effective setting.", Usage: []string{"air config get [OPTIONS] NAME"},
				Options: []cliHelpOption{{"--effective", "Show the effective value and its source."}},
			},
			{
				Name: "set", Summary: "Store one repository setting.", Usage: []string{"air config set [OPTIONS] NAME [VALUE]"},
				Options: []cliHelpOption{{"--stdin", "Read the setting value from standard input."}},
			},
			{Name: "unset", Summary: "Remove one stored repository setting.", Usage: []string{"air config unset NAME"}},
			{
				Name: "list", Summary: "List stored or effective settings.", Usage: []string{"air config list [OPTIONS]"},
				Options: []cliHelpOption{{"--effective", "Include overrides, defaults, and value sources."}},
			},
		}, Run: runConfig,
	},
	{
		Name: "model", Category: commandCategoryMaintenance,
		Summary:     "Inspect models and manage their pricing.",
		Description: "List configured and built-in models, inspect pricing, or store a model's token prices.",
		Usage:       []string{"air model COMMAND [ARGUMENTS]"},
		Children: []cliCommandSpec{
			{Name: "list", Summary: "List known models and pricing status.", Usage: []string{"air model list"}},
			{Name: "show", Summary: "Show one model's pricing details.", Usage: []string{"air model show NAME"}},
			{
				Name: "set-pricing", Summary: "Store short- and long-context token prices.", Usage: []string{"air model set-pricing [OPTIONS] NAME"},
				Options: modelPricingHelpOptions(),
			},
			{Name: "mark-pricing-unknown", Summary: "Record that a model's pricing is unknown.", Usage: []string{"air model mark-pricing-unknown NAME"}},
		}, Run: runModel,
	},
	{
		Name: "db", Category: commandCategoryMaintenance,
		Summary:     "Inspect the repository database location.",
		Description: "Print AIR's database path for the current Git repository.",
		Usage:       []string{"air db path"},
		Children:    []cliCommandSpec{{Name: "path", Summary: "Print the AIR database path.", Usage: []string{"air db path"}}},
		Run:         runDB,
	},
	{
		Name: "backup", Category: commandCategoryMaintenance,
		Summary:     "Create a consistent SQLite database backup.",
		Description: "Back up the current repository's AIR database to a new file. With no path, choose a timestamped filename in the current directory.",
		Usage:       []string{"air backup [PATH]"}, Run: runBackup,
	},
	{
		Name: "reset", Category: commandCategoryMaintenance,
		Summary:     "Delete all AIR state for the repository.",
		Description: "Remove the repository's complete .git/air state directory after confirmation.",
		Usage:       []string{"air reset [OPTIONS]"},
		Options:     []cliHelpOption{{"--force", "Delete AIR state without prompting."}}, Run: runReset,
	},
}

func reviewerHelpOptions(timeoutScope string) []cliHelpOption {
	return []cliHelpOption{
		{"--reviewer BACKEND", "Override the review backend: codex or http."},
		{"--model MODEL", "Override the configured model identifier."},
		{"--effort EFFORT", "Override the Codex reasoning effort."},
		{"--codex-bin PATH", "Override the Codex CLI executable."},
		{"--codex-profile PROFILE", "Override the Codex configuration profile."},
		{"--codex-timeout DURATION", "Override the " + timeoutScope + " Codex timeout."},
		{"--base-url URL", "Override the OpenAI-compatible HTTP API base URL."},
		{"--api-key-env NAME", "Read the HTTP API key from this environment variable."},
		{"--api-key KEY", "Override the HTTP API key; prefer an environment variable."},
	}
}

func reportingHelpOptions() []cliHelpOption {
	return []cliHelpOption{
		{"--model MODEL", "Include only attempts performed by this model."},
		{"--since DATE", "Include attempts on or after YYYY-MM-DD or RFC3339."},
	}
}

func modelPricingHelpOptions() []cliHelpOption {
	return []cliHelpOption{
		{"--service-tier TIER", fmt.Sprintf("Service tier (default: %s).", standardServiceTier)},
		{"--source TEXT", `Pricing source (default: "manual").`},
		{"--as-of DATE", "Pricing date (default: today)."},
		{"--long-context-threshold N", fmt.Sprintf("Long-context input-token threshold (default: %d).", longContextInputTokens)},
		{"--short-input PRICE", "Short-context input USD per million tokens."},
		{"--short-cached-input PRICE", "Short-context cached-input USD per million tokens."},
		{"--short-cache-write PRICE", "Short-context cache-write USD per million tokens."},
		{"--short-output PRICE", "Short-context output USD per million tokens."},
		{"--long-input PRICE", "Long-context input USD per million tokens."},
		{"--long-cached-input PRICE", "Long-context cached-input USD per million tokens."},
		{"--long-cache-write PRICE", "Long-context cache-write USD per million tokens."},
		{"--long-output PRICE", "Long-context output USD per million tokens."},
	}
}

func findCLICommand(name string) (*cliCommandSpec, bool) {
	for index := range cliCommands {
		if cliCommands[index].Name == name {
			return &cliCommands[index], true
		}
	}
	return nil, false
}

func (command *cliCommandSpec) child(name string) (*cliCommandSpec, bool) {
	for index := range command.Children {
		if command.Children[index].Name == name {
			return &command.Children[index], true
		}
	}
	return nil, false
}

func commandHelpTarget(command *cliCommandSpec, args []string) *cliCommandSpec {
	target := command
	for _, argument := range args {
		if argument == "--" || strings.HasPrefix(argument, "-") {
			break
		}
		child, found := target.child(argument)
		if !found {
			break
		}
		target = child
	}
	return target
}

func containsHelpFlag(args []string) bool {
	for _, argument := range args {
		if argument == "--" {
			return false
		}
		if argument == "-h" || argument == "--help" {
			return true
		}
	}
	return false
}

func runHelpCommand(_ context.Context, args []string, environment cliEnvironment) error {
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
		printUsage(environment.Stdout)
		return nil
	}
	command, found := findCLICommand(args[0])
	if !found {
		return fmt.Errorf("unknown help topic %q; run air help", strings.Join(args, " "))
	}
	target := command
	for _, name := range args[1:] {
		child, found := target.child(name)
		if !found {
			return fmt.Errorf("unknown help topic %q; run air help %s", strings.Join(args, " "), command.Name)
		}
		target = child
	}
	printCommandHelp(environment.Stdout, target)
	return nil
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, "AIR reviews commits and rechecks findings on master.")
	fmt.Fprintln(output, "\nUsage:\n  air COMMAND [OPTIONS]\n  air help [COMMAND [SUBCOMMAND]]\n  air --help")
	for _, category := range commandCategoryOrder {
		fmt.Fprintf(output, "\n%s:\n", category)
		for _, command := range cliCommands {
			if command.Category == category {
				fmt.Fprintf(output, "  %-10s %s\n", command.Name, command.Summary)
			}
		}
	}
	fmt.Fprintln(output, "\nHelp:\n  help       Show global or command-specific help.")
	fmt.Fprintln(output, "\nReviewer configuration precedence:\n  command-line flag > environment variable > database > built-in default")
	fmt.Fprintln(output, "\nRun \"air help COMMAND\" for command details.")
}

func printCommandHelp(output io.Writer, command *cliCommandSpec) {
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
	if len(command.Options) != 0 {
		fmt.Fprintln(output, "\nOptions:")
		for _, option := range command.Options {
			printWrappedHelp(output, fmt.Sprintf("  %-30s ", option.Syntax), strings.Repeat(" ", 33), option.Description, 96)
		}
	} else {
		fmt.Fprintln(output, "\nOptions:")
	}
	fmt.Fprintln(output, "  -h, --help                     Show this help.")
	if len(command.Children) != 0 {
		fmt.Fprintf(output, "\nRun \"air help %s COMMAND\" for subcommand details.\n", command.Name)
	}
}

func printWrappedHelp(output io.Writer, firstPrefix, continuationPrefix, value string, width int) {
	words := strings.Fields(value)
	if len(words) == 0 {
		fmt.Fprintln(output, firstPrefix)
		return
	}
	line := firstPrefix
	prefix := firstPrefix
	for _, word := range words {
		separator := ""
		if len(line) > len(prefix) {
			separator = " "
		}
		if len(line)+len(separator)+len(word) > width && len(line) > len(prefix) {
			fmt.Fprintln(output, line)
			prefix = continuationPrefix
			line = prefix + word
			continue
		}
		line += separator + word
	}
	fmt.Fprintln(output, line)
}
