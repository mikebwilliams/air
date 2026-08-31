# AIR

AIR is a local semantic reviewer for commits on the first-parent history of
`master`. For each selected commit it starts a fresh local Codex review session,
lets Codex inspect the repository under a read-only sandbox, and stores reviews
and findings in SQLite.

The database lives at:

```text
<git-common-dir>/air/reviews.sqlite
```

Print the resolved path for the current repository, even before initialization:

```bash
air db path
```

Create a consistent, private backup in the current directory:

```bash
air backup
air backup /path/to/project-air.sqlite
```

With no path, AIR uses a timestamped `air-backup-YYYYMMDD-HHMMSS.sqlite`
filename and adds a numeric suffix if necessary. The command uses SQLite's
online-backup mechanism, includes committed WAL state, verifies the completed
copy, sets mode `0600`, and never overwrites an existing destination.

AIR's scan lock is stored beside it as `<git-common-dir>/air/scan.lock`. The
state directory is private to the user, shared by worktrees, and does not
modify tracked files.

## Requirements

- Go 1.26 or newer
- Git
- A C compiler for the bundled SQLite driver
- The Codex CLI, authenticated with `codex login`

## Build and test

```bash
make build
make test
```

Install into `/usr/local/bin` by building as your normal user and elevating
only the copy step:

```bash
make build
sudo make install
```

The first AIR command after this upgrade automatically advances schema-v4 or
schema-v5 databases to schema v6. The migrations add successful scan timing and
separate HEAD-recheck history without rewriting existing reviews or findings.
Historical review attempts retain an explicitly unknown duration.

The install prefix is configurable. For example, a user-local or packaging
install can use:

```bash
make install PREFIX="$HOME/.local"
make install DESTDIR=/tmp/air-package-root
```

## Codex configuration

AIR uses the `codex` executable and its existing local authentication,
configuration, repository instructions, and exec-policy rules. AIR requires an
explicit model and reasoning effort so every stored review has unambiguous
provenance.

```bash
codex login status
air config set model your-codex-model
air config set effort low
air scan
```

Reviewer settings may come from a scan flag, an environment variable, the AIR
database, or a built-in default, in that order of precedence:

```text
command-line flag > environment variable > database > built-in default
```

The database is the convenient persistent default for a repository. Environment
variables remain useful for automation, and flags provide one-off overrides:

```bash
AIR_MODEL=temporary-model air scan
air scan --model one-off-model --effort high
```

Manage stored settings and inspect the effective configuration with:

```bash
air config set codex-profile air-review
air config get model
air config unset codex-profile
air config list
air config list --effective
```

Effective output includes the winning source for every setting. API keys are
always redacted in `config get` and `config list` output.

| Database setting | Scan flag | Environment variable | Built-in default |
| --- | --- | --- | --- |
| `reviewer` | `--reviewer` | `AIR_REVIEWER` | `codex` |
| `model` | `--model` | `AIR_MODEL` | none |
| `effort` | `--effort` | `AIR_REASONING_EFFORT` | none |
| `codex-bin` | `--codex-bin` | `AIR_CODEX_BIN` | `codex` |
| `codex-profile` | `--codex-profile` | `AIR_CODEX_PROFILE` | none |
| `codex-timeout` | `--codex-timeout` | `AIR_CODEX_TIMEOUT` | `20m` |
| `base-url` | `--base-url` | `AIR_BASE_URL` | `https://api.openai.com/v1` |
| `api-key-env` | `--api-key-env` | `AIR_API_KEY_ENV` | none |
| `api-key` | `--api-key` | `AIR_API_KEY`, then `OPENAI_API_KEY` | none |

Reviewer instructions can also be customized per repository. AIR has separate
instructions for commit review and HEAD recheck, and for the Codex and HTTP
backends:

```bash
air prompt list
air prompt show review --reviewer codex
air prompt show review --reviewer codex --full
air prompt set review --reviewer codex --file review-prompt.txt
printf '%s\n' 'Focus on transaction and lifetime safety.' | \
  air prompt set review --reviewer codex --stdin
air prompt reset review --reviewer codex
```

`show` prints the editable instructions. `show --full` also includes AIR's
fixed protocol, safety constraints, and response contract. `set` replaces only
the editable instructions; AIR continues to append the fixed portion so output
remains parseable and repository inspection remains read-only. The override is
stored in the repository database and is included by `air backup`.

Each custom prompt receives a stable `custom:sha256:...` identity derived from
its complete static prompt, kind, backend, and built-in protocol version. AIR
records that identity on every commit-review or recheck attempt. Resetting an
override restores the numeric built-in version. Changing a recheck prompt makes
findings eligible for recheck again because it is a distinct review identity.

Each commit gets an independent ephemeral `codex exec` session. AIR supplies
the exact commit and first-parent identities, bounded resolution candidates,
and a JSON output schema. Codex discovers the diff and related repository
context itself. Its commands run with a read-only sandbox and an approval
policy of `never`, so an unattended scan fails instead of pausing or modifying
the repository.

Additional optional configuration:

```bash
air config set codex-bin /path/to/codex
air config set codex-profile air-review
air config set codex-timeout 20m
```

The corresponding flags are `--model`, `--effort`, `--codex-bin`,
`--codex-profile`, and `--codex-timeout`. For example:

```bash
air scan --model your-codex-model --effort high
```

AIR passes both values explicitly to Codex and records them on every commit
review or HEAD recheck. The timeout applies independently to each commit review
or recheck batch and defaults to twenty minutes. AIR does not read or copy
Codex credentials.

AIR records input, cached-input, cache-write, output, and reasoning-output token
counts from each review. Its model registry includes a dated snapshot of the
Standard short- and long-context prices for `gpt-5.6-sol`, the `gpt-5.6` alias,
`gpt-5.6-terra`, and `gpt-5.6-luna`. The selected model and its complete pricing
configuration are stored in SQLite automatically; no pricing environment
variables are required.

Requests above 272,000 input tokens use the stored long-context rates.
Reasoning tokens are included in output tokens and are not charged twice. When
the backend reports cache-write usage, AIR calculates one estimate. The Codex
JSONL format may omit cache-write usage; in that case AIR stores a minimum and
maximum estimate spanning ordinary-input and cache-write pricing. Models absent
from AIR's registry remain usable and are stored with explicitly unknown
pricing.

Inspect or override the database-backed model registry:

```bash
air model list
air model show gpt-5.6-luna
air model mark-pricing-unknown private-model
```

Custom prices use USD per million tokens. All eight short/long-context billing
categories are required:

```bash
air model set-pricing private-model \
  --source internal-price-sheet --as-of 2026-08-16 \
  --long-context-threshold 272000 \
  --short-input 1 --short-cached-input .1 \
  --short-cache-write 1.25 --short-output 6 \
  --long-input 2 --long-cached-input .2 \
  --long-cache-write 2.5 --long-output 9
```

Stored model records override compiled pricing snapshots during later scans.

These are API-equivalent USD estimates. A Codex run authenticated through a
ChatGPT account does not expose an authoritative per-run monetary charge.

The original OpenAI-compatible HTTP reviewer remains available as an explicit
fallback:

```bash
air config set reviewer http
air config set model your-model
printf '%s\n' 'your-key' | air config set --stdin api-key
air scan
```

For that backend, `AIR_BASE_URL` defaults to `https://api.openai.com/v1`.
`AIR_API_KEY_ENV` may name another key variable, and `OPENAI_API_KEY` is the
final environment fallback. The corresponding database settings are `base-url`,
`api-key-env`, and `api-key`; the corresponding flags are `--base-url`,
`--api-key-env`, and `--api-key`. A stored API key is plaintext in the mode-0600
SQLite database, so an environment variable is preferable on shared machines.

## Usage

Long options use two hyphens and may appear before or after positional
arguments. Use `--` to stop option parsing when a positional value begins with
a hyphen:

```bash
air show --json HEAD
air show HEAD --json
air config set codex-profile -- --literal-profile-name
```

Initialize AIR with a baseline commit on the first-parent history of
`refs/heads/master`:

```bash
air init v1.0.0
```

The baseline itself is not reviewed.

Review every unprocessed commit between the baseline and local `master`:

```bash
air scan
```

Reviewer and validation failures are retained without marking the commit as
processed. The current scan continues with later commits and returns a nonzero
result at the end if any failed. Later scans defer recorded failures; `air
retry` is the explicit way to try them again. To stop the current scan as soon
as it records a failure:

```bash
air scan --stop-on-error
air failures
air retry --continue-on-error
```

`air failures --json` exposes the same queue to automation. Each record keeps
the latest error, timestamp, model/effort, whether it was a rescan, and the
number of failed attempts. `air retry` processes live failed commits oldest
first using current reviewer configuration and accepts the normal reviewer,
model, effort, timeout, and limit overrides. A successful review or intentional
skip atomically removes its failure record. `air clean` removes failure records
whose commits no longer exist on master.

Preview the same work without creating a reviewer or changing the database:

```bash
air pending
air scan --dry-run
```

Both commands list commits in processing order as `review` or `skip` and print
a summary. `air pending` also accepts `--limit` and an explicit range.

Manually mark one unprocessed commit as skipped:

```bash
air skip a1b2c3d --reason "localization-only change"
```

Or skip every unprocessed commit whose full commit message contains a literal
substring, matched case-insensitively:

```bash
air skip --filter "translations" --dry-run
air skip --filter "translations" --reason "localization-only changes"
```

Skip selection is limited to commits after AIR's baseline on the first-parent
history of `master`. Already processed filter matches are reported and left
unchanged. A bulk skip is atomic, and skipping a failed commit removes it from
the retry queue in the same transaction. `--dry-run` does not change
commit/review state.

After master history is rewritten, preview and remove stored commits that no
longer occur on its first-parent history:

```bash
air clean --dry-run
air clean
```

To discard the entire repository-specific AIR database and configuration:

```bash
air reset
```

`reset` shows the exact `.git/air` directory and requires confirmation.
Automation may use `air reset --force`.

Process at most ten of those commits in this invocation:

```bash
air scan --limit 10
```

`--limit 0` is the default and means unlimited. The limit counts commits
processed by AIR, including commits recorded as skipped.

Review an explicit, possibly disjoint range:

```bash
air scan v1.1.0..v1.2.0
```

The lower endpoint is excluded and the upper endpoint is included. Both must
be on `master`'s first-parent history.

Review an already reviewed commit again with a different model or effort:

```bash
air rescan HEAD --model gpt-5.6-sol --effort xhigh
```

The current commit record is replaced by the latest result, while every review
attempt retains its model, effort, prompt version, token usage, cost, summary,
and raw response. Finding updates are conservative and additive: rescanning
does not silently delete findings created by an earlier attempt.

Reconcile current open findings against the exact `HEAD` snapshot, optionally
using a stronger model than the ordinary scanner:

```bash
air recheck --model gpt-5.6-sol --effort xhigh
air recheck --model gpt-5.6-sol --effort xhigh 17 31 562
air recheck --limit 50
air recheck --dry-run
```

With no IDs, `recheck` processes all eligible open findings. It requires `HEAD`
to be the tip of `master`, pins that SHA for the entire invocation, and uses
the same reviewer configuration precedence as `scan`. Findings are processed
in batches of 20 by default; `--batch-size N` accepts 1 through 50.

Every finding receives one recorded outcome: `resolved`, `still_present`, or
`uncertain`. Only `resolved` closes a finding. The other outcomes leave it open,
and all outcomes and reasons appear in `air finding` and the interactive
browser history. Recheck never creates findings and does not restrict model
inspection to the recorded file, allowing it to recognize renames and
cross-file fixes.

Successful results are resumable per finding. A later invocation skips a
finding already checked at the same HEAD with the same reviewer, model, effort,
and recheck prompt version. A changed HEAD or different model/effort checks it
again; `--force` repeats an otherwise identical check. Each successful batch
is committed independently, and `--continue-on-error` continues after a failed
model batch. Failed batches do not change finding state; rerunning naturally
selects them while skipping successful batches.

Inspect results:

```bash
air status
air findings
air findings --all
air log
air show HEAD
air show HEAD --reviews
air show HEAD --review 1
air finding 17
air stats
air cost --model gpt-5.6-luna --since 2026-08-01
air status --json
air show HEAD --json
air export --format json
air export --format sarif
air export --format html > air-findings.html
```

`air show` includes the model, reasoning effort, token usage, elapsed scan time,
and estimated cost used for that commit. `--reviews` lists retained attempts,
and `--review N` shows the findings and accounting recorded by one attempt.

`air stats` aggregates token usage and API-equivalent estimated cost across
every retained commit-review and HEAD-recheck attempt, including superseded
rescans, and shows the current repository finding counts. `air cost` provides
the accounting-focused view. Both accept an exact `--model` filter and a
`--since` date or RFC3339 timestamp. Unknown model prices and unreported
cache-write token counts remain explicit instead of being silently treated as
zero. Recheck attempts are reported separately from commit-review attempts.
`air stats` computes average scan time per commit only from successful commit
reviews; recheck batch timings do not affect that estimate. Historical commit
attempts without timing remain explicit and do not count as zero.

`air status` is a compact summary of total/open/dismissed/resolved findings and
unscanned/failed/deferred commits. Unscanned commits are ready for an ordinary
scan. Failed commits include the complete durable failure queue; deferred
commits are the live unprocessed subset omitted by ordinary scans. Use
`air findings`, `air finding`, or `air export` for finding details.
Status also estimates the time needed for unscanned commits from the average of
all successful timed attempts. Deferred failures are excluded from that
estimate. If no timing samples exist, the estimate is reported as unknown.

`status` and `show` support indented, stable-field-name JSON for automation.
Status JSON contains the same aggregate counts as text output.
`air export --format json` emits current open findings, while `--format sarif`
emits SARIF 2.1.0 with file and line locations when the reviewer supplied them.
Dismissed and resolved findings are excluded from those machine-readable
exports.

`air export --format html` emits one self-contained, offline HTML file for
sharing with people who do not have AIR or its database. The viewer includes
all open, dismissed, and resolved findings; search, status and severity
filters; newest, file, and severity sorts; finding details; review attribution;
event history; and bounded excerpts from introducing diffs. It starts on open
findings. The report is read-only: lifecycle controls and actions that invoke
an editor or Git difftool are intentionally absent. No repository configuration,
API keys, raw model responses, or external assets are included.

`air findings` opens a full-screen terminal browser. It starts with open
findings; `--all` starts with every disposition. Use the arrow keys or `j`/`k`
to move, left/right to change the sort between newest-first and file/line order,
`/` to search, `s` and `v` to cycle status and severity filters, and
`Ctrl+U`/`Ctrl+D` to scroll long details. The detail pane includes the finding's
description, location, introducing review and model/effort, and complete event
history. On sufficiently wide and tall terminals, its lower section loads the
relevant hunk from the finding's introducing commit and marks the recorded
new-file line; missing locations and unavailable textual hunks are reported in
place. Press `d` to open the introducing commit in the user's configured
`git difftool`, or `o` to open the recorded file and line using Git's configured
editor. Press `D` to dismiss with a required reason, `r` to reopen after
confirmation, or `n` to add a note. Lifecycle actions use the same audit trail
as `air finding`; `?` shows the complete key reference. The command requires an
interactive terminal, while `air status --json` remains the non-interactive
summary interface and `air export --format json` provides detailed open
findings. On color-capable terminals, the browser highlights severity,
disposition, selection, headings, and messages; plain terminals retain the same
labels and selection marker.

Triage findings without losing their audit history:

```bash
air finding dismiss 17 --reason "Intentional compatibility behavior"
air finding note 17 "Verify after the parser rewrite"
air finding reopen 17
air finding diff 17
air finding open 17
```

Dismissed findings contribute only to the aggregate status count and are not
supplied to later reviews. Reopening clears either a manual dismissal, commit
resolution, or HEAD-recheck resolution. Every action, recheck outcome, and note
is timestamped in the finding's displayed history.
`diff` delegates the entire introducing commit to `git difftool`, honoring the
user's Git diff-tool configuration. `open` uses `git var GIT_EDITOR` and opens
the current working-tree file at the recorded line for common editors; it
reports a clear error when the finding has no location or the file no longer
exists.

Run `air help` for the grouped command reference, or request help for a command
or nested action without requiring an initialized repository:

```bash
air help scan
air scan --help
air help finding dismiss
air finding dismiss --help
```

Before an unattended or expensive run, inspect the complete local setup:

```bash
air doctor
air doctor --json
```

The doctor checks the Git repository and master ref, state permissions, schema
version, SQLite integrity, effective reviewer/model configuration, model
pricing, and backend prerequisites. For Codex it locates the configured binary
and runs `codex login status`; for HTTP it validates the endpoint configuration
and confirms that a key is available without printing it. Unknown pricing is a
warning, while missing review credentials or an unusable database is a failed
check and a nonzero exit.

## Review and skip behavior

- Commits are processed oldest to newest within each scan.
- Previously reviewed or skipped commits are not processed again.
- Branch commits are not reviewed individually. A merge on `master` is
  reviewed against its first parent.
- Binary-only commits are not sent to a reviewer; mixed commits are reviewed
  based on their textual changes.
- Binary-only, empty, and textual diffs larger than 256 KiB are recorded as
  skipped.
- Comments, string-content changes, translations/localization resources, and
  documentation remain in the review input but are explicitly out of scope.
  For a mixed commit, the reviewer considers only executable behavior. For a
  commit containing only excluded content, it returns a clean review.
- Resolution context is limited to at most 50 open findings whose recorded file
  exactly matches a textual file changed by the commit. Unlocated, cross-file,
  and over-limit findings are deferred without changing their state. Each scan
  result reports supplied and deferred candidate counts.
- A failed model call or invalid response is retained in `air failures` without
  marking the commit processed. The scan continues by default and returns a
  nonzero result after the batch; `--stop-on-error` stops immediately. Later
  ordinary scans defer it; `air retry` tries it again explicitly.
- A database transaction failure always stops the scan.
- Disjoint scans do not trigger historical lifecycle reconciliation. Scan
  chronologically when accurate finding resolution matters.

## Reviewer access

The default reviewer receives commit metadata and a bounded set of open
resolution candidates associated with files changed by the commit. Codex is
pointed at the exact commit and may inspect the diff, repository files, and Git
history using its normal local tools. AIR does not check out historical commits
or permit the reviewer to resolve findings outside the supplied candidate set.

The Codex subprocess is ephemeral and read-only. AIR asks it not to run builds,
tests, repository programs, or network commands. Local Codex configuration and
exec-policy rules remain active, but actions requiring approval fail because
the scan is noninteractive.

The HTTP fallback retains AIR's constrained read-only Git tool interface.

See [docs/initial_spec.md](docs/initial_spec.md) for the complete design.
