# AIR

AIR is a local semantic reviewer for commits on the first-parent history of
`master`. For each selected commit it starts a fresh local Codex review session,
lets Codex inspect the repository under a read-only sandbox, and stores reviews
and findings in SQLite.

The database lives at:

```text
<git-common-dir>/ai-review.sqlite
```

It is shared by worktrees and does not modify tracked files.

## Requirements

- Go 1.26 or newer
- Git
- A C compiler for the bundled SQLite driver
- The Codex CLI, authenticated with `codex login`

## Build and test

```bash
go build -o air .
go test ./...
```

## Codex configuration

AIR uses the `codex` executable and its existing local authentication,
configuration, repository instructions, and exec-policy rules. AIR requires an
explicit model and reasoning effort so every stored review has unambiguous
provenance.

```bash
codex login status
export AIR_MODEL=your-codex-model
export AIR_REASONING_EFFORT=low
air scan
```

Each commit gets an independent ephemeral `codex exec` session. AIR supplies
the exact commit and first-parent identities, open findings, and a JSON output
schema. Codex discovers the diff and related repository context itself. Its
commands run with a read-only sandbox and an approval policy of `never`, so an
unattended scan fails instead of pausing or modifying the repository.

Additional optional configuration:

```bash
export AIR_CODEX_BIN=/path/to/codex
export AIR_CODEX_PROFILE=air-review
export AIR_CODEX_TIMEOUT=10m
```

The corresponding flags are `--model`, `--effort`, `--codex-bin`,
`--codex-profile`, and `--codex-timeout`. For example:

```bash
air scan --model your-codex-model --effort high
```

AIR passes both values explicitly to Codex and records them on every reviewed
commit. The timeout applies independently to each commit and defaults to ten
minutes. AIR does not read or copy Codex credentials.

The original OpenAI-compatible HTTP reviewer remains available as an explicit
fallback:

```bash
export AIR_MODEL=your-model
export AIR_API_KEY=your-key
air scan --reviewer http
```

For that backend, `AIR_BASE_URL` defaults to `https://api.openai.com/v1`.
`AIR_API_KEY_ENV` may name another key variable, and `OPENAI_API_KEY` is the
final fallback.

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

Inspect results:

```bash
air status
air log
air show HEAD
air finding 17
```

`air show` includes the model and reasoning effort used for that commit.

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
