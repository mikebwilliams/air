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
| `codex-timeout` | `--codex-timeout` | `AIR_CODEX_TIMEOUT` | `10m` |
| `base-url` | `--base-url` | `AIR_BASE_URL` | `https://api.openai.com/v1` |
| `api-key-env` | `--api-key-env` | `AIR_API_KEY_ENV` | none |
| `api-key` | `--api-key` | `AIR_API_KEY`, then `OPENAI_API_KEY` | none |

Each commit gets an independent ephemeral `codex exec` session. AIR supplies
the exact commit and first-parent identities, open findings, and a JSON output
schema. Codex discovers the diff and related repository context itself. Its
commands run with a read-only sandbox and an approval policy of `never`, so an
unattended scan fails instead of pausing or modifying the repository.

Additional optional configuration:

```bash
air config set codex-bin /path/to/codex
air config set codex-profile air-review
air config set codex-timeout 10m
```

The corresponding flags are `--model`, `--effort`, `--codex-bin`,
`--codex-profile`, and `--codex-timeout`. For example:

```bash
air scan --model your-codex-model --effort high
```

AIR passes both values explicitly to Codex and records them on every reviewed
commit. The timeout applies independently to each commit and defaults to ten
minutes. AIR does not read or copy Codex credentials.

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
processed. By default the scan stops at the first failure. To finish the rest
of a batch while still returning a nonzero result at the end:

```bash
air scan --continue-on-error
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
air export --format sarif
```

`air show` includes the model, reasoning effort, token usage, and estimated
cost used for that commit. `--reviews` lists retained attempts, and `--review N`
shows the findings and accounting recorded by one attempt.

`air stats` aggregates token usage and API-equivalent estimated cost across
every retained review attempt, including superseded rescans, and shows the
current repository finding counts. `air cost` provides the accounting-focused
view. Both accept an exact `--model` filter and a `--since` date or RFC3339
timestamp. Unknown model prices and unreported cache-write token counts remain
explicit instead of being silently treated as zero.

`status` and `show` support indented, stable-field-name JSON for automation.
`air export --format json` emits current open findings, while `--format sarif`
emits SARIF 2.1.0 with file and line locations when the reviewer supplied them.
Dismissed and resolved findings are excluded from both exports.

`air findings` opens a full-screen terminal browser. It starts with open
findings; `--all` starts with every disposition. Use the arrow keys or `j`/`k`
to move, `/` to search, `s` and `v` to cycle status and severity filters, and
`Ctrl+U`/`Ctrl+D` to scroll long details. The detail pane includes the finding's
description, location, introducing review and model/effort, and complete event
history. Press `d` to dismiss with a required reason, `r` to reopen after
confirmation, or `n` to add a note. These actions use the same audited lifecycle
as `air finding`; `?` shows the complete key reference. The command requires an
interactive terminal, while `air status --json` remains the non-interactive
interface.

Triage findings without losing their audit history:

```bash
air finding dismiss 17 --reason "Intentional compatibility behavior"
air finding note 17 "Verify after the parser rewrite"
air finding reopen 17
```

Dismissed findings do not appear in `air status` and are not supplied to later
reviews. Reopening clears either a manual dismissal or a model resolution.
Every action and note is timestamped in the finding's displayed history.

Run `air help` for the concise command reference.

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
- A failed model call or invalid response is retained in `air failures` without
  marking the commit processed. It stops the scan unless `--continue-on-error`
  is set; default scanning or `air retry` can try it again.
- A database transaction failure always stops the scan.
- Disjoint scans do not trigger historical lifecycle reconciliation. Scan
  chronologically when accurate finding resolution matters.

## Reviewer access

The default reviewer receives commit metadata and all currently open findings.
Codex is pointed at the exact commit and may inspect the diff, repository files,
and Git history using its normal local tools. AIR does not check out historical
commits.

The Codex subprocess is ephemeral and read-only. AIR asks it not to run builds,
tests, repository programs, or network commands. Local Codex configuration and
exec-policy rules remain active, but actions requiring approval fail because
the scan is noninteractive.

The HTTP fallback retains AIR's constrained read-only Git tool interface.

See [docs/initial_spec.md](docs/initial_spec.md) for the complete design.
