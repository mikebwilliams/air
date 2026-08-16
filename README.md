# AIR

AIR is a local semantic reviewer for commits on the first-parent history of
`master`. For each selected commit it starts a fresh local Codex review session,
lets Codex inspect the repository under a read-only sandbox, and stores reviews
and findings in SQLite.

The database lives at:

```text
<git-common-dir>/air/reviews.sqlite
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
air log
air show HEAD
air show HEAD --reviews
air show HEAD --review 1
air finding 17
```

`air show` includes the model, reasoning effort, token usage, and estimated
cost used for that commit. `--reviews` lists retained attempts, and `--review N`
shows the findings and accounting recorded by one attempt.

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

## Review and skip behavior

- Commits are processed oldest to newest within each scan.
- Previously reviewed or skipped commits are not processed again.
- Branch commits are not reviewed individually. A merge on `master` is
  reviewed against its first parent.
- Binary-only commits are not sent to a reviewer; mixed commits are reviewed
  based on their textual changes.
- Binary-only, empty, and textual diffs larger than 256 KiB are recorded as
  skipped.
- A failed model call, invalid response, or database transaction stops the
  scan. The failed commit is retried on the next invocation.
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
