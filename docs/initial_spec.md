# AI Git Commit Reviewer

## Name
air - short for AI reviewer

## 1. Purpose

Build a lightweight, local tool that reviews Git commits using an LLM and persists findings in a repository-local SQLite database.

The tool is intended to behave like a semantic lint pass over Git history:

- review each selected commit on the first-parent history of `master`
  independently;
- identify concrete correctness problems introduced by that commit;
- retain a small log or skip reason associated with each processed commit;
- persist unresolved findings;
- allow later commits to resolve findings introduced by earlier commits;
- scan all or selected, possibly disjoint, commit ranges;
- show only findings currently recorded as unresolved by default.

Only the first-parent history of `refs/heads/master` is in scope. Commits
that exist only on other branches are not reviewed individually. A merge into
`master` is reviewed as the change from its first parent.

The tool is not intended to replace static analysis, testing, Coverity, or conventional linters. Its purpose is to catch problems an LLM may recognize from code semantics and surrounding context.

Examples include:

- early returns causing partially initialized objects;
- state transitions leaving inconsistent state;
- lifecycle or ownership mistakes;
- cleanup code depending on initialization that no longer occurs;
- incorrect error-path behavior;
- mismatched assumptions between changed code and related code elsewhere in the repository.

## 2. Design Goals

### 2.1 Primary goals

The implementation should be:

- tightly coupled to Git;
- local-first;
- lightweight;
- inexpensive to run on every commit;
- suitable for large repositories;
- model-agnostic;
- resumable;
- useful for both historical scanning and continuous scanning;
- simple enough to understand and modify without a large framework.

### 2.2 Non-goals

The initial implementation should not provide:

- pull-request or merge-request integration;
- multi-user operation;
- a web UI;
- issue assignment;
- project management workflows;
- source-code indexing;
- AST or dependency graph generation;
- embeddings or vector databases;
- automatic code modification;
- CI integration beyond what can be built around the CLI;
- a generalized abstraction for non-Git data sources;
- review of branch histories other than the first-parent history of `master`;
- ancestry-aware finding state across branches.

Git is a required dependency and part of the application model.

## 3. Terminology

### Commit

A Git commit processed by the tool. A processed commit is either reviewed or
explicitly skipped.

Commits are identified internally by their full object SHA.

### Finding

A potential correctness problem reported by the LLM.

A finding has a lifecycle beginning with the commit believed to have introduced it and optionally ending with a later commit believed to have resolved it.

### Open finding

A finding for which no resolving commit has been recorded.

### Review

A single LLM analysis of a Git commit.

The initial implementation stores at most one review for a commit. Rescanning
and review history are deferred.

## 4. Repository Storage

The database should be associated with the Git repository rather than the current worktree.

Determine its location using:

```bash
git rev-parse --git-common-dir
```

Default database path:

```text
<git-common-dir>/ai-review.sqlite
```

This ensures multiple Git worktrees belonging to the same repository share review state.

The database must not modify or require files in the tracked working tree.

## 5. SQLite Schema

The initial schema should remain intentionally small.

### 5.1 `config`

```sql
CREATE TABLE config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
```

Required initial keys:

```text
start_sha
prompt_version
```

Optional keys may later include:

```text
default_model
```

### 5.2 `commits`

```sql
CREATE TABLE commits (
    sha             TEXT PRIMARY KEY,
    parent_sha      TEXT,
    processed_at    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK(status IN ('reviewed', 'skipped')),
    skip_reason     TEXT,
    model           TEXT,
    reasoning_effort TEXT,
    prompt_version  TEXT,
    summary         TEXT,
    raw_response    TEXT
);
```

Fields:

- `sha`: full Git commit SHA.
- `parent_sha`: first parent against which the commit was reviewed.
- `processed_at`: UTC timestamp at which the commit was reviewed or skipped.
- `status`: either `reviewed` or `skipped`.
- `skip_reason`: reason a skipped commit was not sent to the model; null for a
  reviewed commit.
- `model`: model identifier used for a review; null for a skipped commit.
- `reasoning_effort`: Codex reasoning effort used for a review; null for a
  skipped commit or a backend where the concept does not apply.
- `prompt_version`: version of the reviewer prompt; null for a skipped commit.
- `summary`: short human-readable review log; null for a skipped commit.
- `raw_response`: complete raw model response for debugging and
  reproducibility; null for a skipped commit.

A skipped commit remains in the table so later scans do not retry it
automatically.

### 5.3 `findings`

```sql
CREATE TABLE findings (
    id              INTEGER PRIMARY KEY,
    introduced_sha  TEXT NOT NULL,
    resolved_sha    TEXT,
    severity        TEXT NOT NULL,
    title           TEXT NOT NULL,
    description     TEXT NOT NULL,
    file            TEXT,
    line            INTEGER,
    symbol          TEXT,

    FOREIGN KEY(introduced_sha) REFERENCES commits(sha) ON DELETE CASCADE,
    FOREIGN KEY(resolved_sha) REFERENCES commits(sha) ON DELETE SET NULL
);
```

Initial severity values:

```text
info
warning
error
```

Severity should describe likely impact, not model confidence.

### 5.4 `finding_events`

```sql
CREATE TABLE finding_events (
    id          INTEGER PRIMARY KEY,
    finding_id  INTEGER NOT NULL,
    sha         TEXT NOT NULL,
    action      TEXT NOT NULL,
    note        TEXT,

    FOREIGN KEY(finding_id) REFERENCES findings(id) ON DELETE CASCADE,
    FOREIGN KEY(sha) REFERENCES commits(sha) ON DELETE CASCADE
);
```

Initial event types:

```text
opened
resolved
reopened
updated
```

The event table exists primarily for auditability.

Normal commits should not generate `still_open` events.

Every SQLite connection must enable foreign-key enforcement:

```sql
PRAGMA foreign_keys = ON;
```

## 6. Initialization

Command:

```bash
air init <commit-ish>
```

Example:

```bash
air init v9.0.0
```

The tool resolves the provided revision immediately:

```bash
git rev-parse '<commit-ish>^{commit}'
```

The resulting full SHA is stored as `start_sha`.

The symbolic expression such as `v9.0.0` or `HEAD~100` does not need to be retained.

Initialization should fail if:

- the current directory is not inside a Git repository;
- `refs/heads/master` does not exist;
- the specified object cannot be resolved to a commit;
- the resolved commit is not on the first-parent history of
  `refs/heads/master`;
- a database is already initialized unless an explicit reset/reinitialize option is supplied.

The starting commit itself is treated as the baseline and is not reviewed.

## 7. Commit Enumeration

Default historical scan:

```bash
git rev-list --reverse --first-parent "$start_sha"..refs/heads/master
```

An explicit range may be scanned instead:

```bash
air scan <from>..<to>
```

Both endpoints are resolved to full commit SHAs and must be on the
first-parent history of `refs/heads/master`. `<from>` must precede or equal
`<to>` on that history. The lower endpoint is excluded and the upper endpoint
is included, matching ordinary Git two-dot range semantics.

Explicit ranges may be disjoint and may be scanned in any order. Within each
invocation, eligible commits are processed oldest to newest. Commits already
present in the `commits` table, including skipped commits, are not processed
again.

The system should therefore be naturally resumable.

Example:

```bash
air scan
```

If 1,000 commits are eligible and 600 have already been processed, only the
remaining commits are processed.

## 8. Commit Review

For each commit, use its first parent and obtain at minimum:

- commit SHA;
- parent SHA;
- commit author;
- commit date;
- commit subject/body;
- diff against the first parent.

Typical commands:

```bash
git show -s --format=fuller "$sha"
git diff "$parent" "$sha"
```

The LLM must review the commit as a change from its first parent, not simply
review the final repository state.

### 8.1 Large and binary diffs

Binary file changes are excluded from review. If a commit changes
both text and binary files, the textual portion may still be reviewed. A
binary-only commit is recorded with `status = skipped` and a corresponding
`skip_reason`.

If the textual diff exceeds 256 KiB, the entire commit is recorded as skipped.
The initial implementation does not split, summarize, or partially review an
oversized textual diff.

Skipped commits are considered processed for enumeration and resumability,
but they produce no findings or finding events.

An empty commit is likewise recorded as skipped with `empty diff` as its
reason.

## 9. LLM Reviewer Responsibilities

For every commit, the model should perform two tasks:

1. Find new correctness problems caused by the commit.
2. Identify existing open findings clearly resolved by the commit.

The model should avoid reporting:

- formatting;
- naming preferences;
- style;
- documentation quality;
- subjective architecture preferences;
- generic refactoring suggestions;
- speculative concerns without a plausible failure mode;
- existing defects unrelated to the reviewed commit.

The tool is intended to generate a low-volume, high-signal stream.

False negatives are preferable to large quantities of speculative warnings.

## 10. Repository Inspection

The model should be allowed to inspect repository contents when necessary. The
initial implementation delegates inspection to a fresh local Codex CLI session
for each commit.

At minimum, the reviewer may need equivalent access to:

```bash
git show <sha>:<path>
git show <sha>^:<path>
git grep <pattern> <sha>
git diff <sha>^ <sha> -- <path>
git log ...
```

The reviewer should generally inspect repository state through Git object access rather than requiring the worktree to be checked out at each historical commit.

AIR invokes `codex exec` in the repository with an ephemeral, read-only sandbox
and a noninteractive approval policy. The prompt identifies the exact commit
and first parent. Codex's normal repository context, `AGENTS.md` instructions,
local configuration, and exec-policy rules remain available. AIR does not
reproduce Codex's context gathering or tool harness.

Repository inspection must be read-only.

## 11. Review Context

The initial model context should contain:

- commit metadata;
- commit message;
- current open findings.

AIR computes and bounds the textual diff to decide whether the commit should be
reviewed, but it does not embed that diff in the Codex prompt. AIR supplies the
exact commit and first-parent SHAs to `codex exec`, and Codex inspects that diff
and the surrounding repository state itself.

An approximate prompt structure:

```text
You are reviewing Git commit <sha>.

Identify concrete correctness regressions introduced by this commit.

You may inspect the repository at this commit using the available
read-only Git tools.

Focus on issues such as:

- incorrect initialization
- lifetime and ownership errors
- invalid state transitions
- incorrect conditions
- broken error handling
- resource leaks
- incorrect assumptions introduced by this change
- interactions between the changed code and other existing code

Do not report:

- style
- naming
- formatting
- documentation
- subjective design preferences
- unrelated pre-existing defects
- speculative improvements without a concrete failure mode

Also inspect the listed open findings. If this commit clearly fixes one,
report it as resolved. Do not mention findings that remain unchanged.
```

## 12. Model Output

The model should return structured machine-readable output.

Initial JSON format:

```json
{
  "new_findings": [
    {
      "severity": "warning",
      "title": "Early return leaves member uninitialized",
      "description": "The new early return can leave m_bar uninitialized, while the destructor assumes it was initialized.",
      "file": "src/foo.cpp",
      "line": 123,
      "symbol": "FOO::FOO"
    }
  ],
  "resolved_findings": [
    {
      "id": 27,
      "reason": "m_bar is now initialized before the early-return path."
    }
  ],
  "summary": "Reviewed the constructor changes and related object lifecycle."
}
```

`file`, `line`, and `symbol` are advisory anchors and may be null.

The database identity of a finding must not depend on line numbers.

## 13. Finding Lifecycle

A new finding causes:

```text
findings.introduced_sha = current commit
findings.resolved_sha = NULL
```

and:

```text
finding_events.action = opened
```

A later commit resolving the finding causes:

```text
findings.resolved_sha = resolving commit
```

and:

```text
finding_events.action = resolved
```

A finding not mentioned by a later review remains open.

The model should not be required to repeatedly assert that findings remain present.

Example:

```text
commit A
    opens #12

commit B
    no event for #12

commit C
    no event for #12

commit D
    resolves #12
```

## 14. Open Findings Supplied to Reviews

The simplest implementation should supply all open findings to each review.

An open finding may be represented compactly:

```text
#27 [warning]
src/foo.cpp — FOO::FOO

Early return can leave m_bar uninitialized.

The destructor later assumes m_bar was initialized.
```

If the number of open findings eventually becomes large, filtering can be introduced later.

No filtering system should be part of the initial architecture unless demonstrated necessary.

Finding state reflects the order in which commits are processed. When ranges
are scanned out of historical order, the tool does not revisit commits that
were reviewed earlier. For example, if a newer commit fixed a problem from an
older, not-yet-reviewed commit, scanning the newer commit first cannot record
that resolution because the finding does not exist yet.

The initial implementation does not reconcile this situation. Users who need
accurate finding lifecycles should scan relevant ranges in chronological
order. Individual commit reviews remain useful when ranges are scanned
disjointly or out of order.

## 15. Historical Scan Behavior

Historical scanning should process commits oldest to newest.

Example:

```text
START
  |
  A     opens #1
  |
  B
  |
  C     opens #2
  |
  D     resolves #1
  |
  E     resolves #2
  |
 master
```

The database retains the entire history.

Default user-facing status at the end should report only findings currently
recorded as open in the database.

Thus temporary bugs that appeared and were subsequently fixed during historical scanning do not clutter normal output.

## 16. Continuous Operation

After an initial scan, normal use is to update the local `master` ref and scan
again. For example, while `master` is checked out:

```bash
git pull
air scan
```

The tool enumerates the configured baseline through `refs/heads/master`, skips
commits already recorded in the database, and processes the remainder in
chronological order.

It does not require a daemon.

A daemon, cron job, systemd timer, post-fetch hook, or other automation may be layered on later.

## 17. Master History and Merge Commits

AIR operates exclusively on the first-parent history of
`refs/heads/master`. There is no configurable history mode in the initial
implementation.

For an ordinary commit, the review diff is against its sole parent. For a
merge commit on `master`, the review diff is against its first parent. The
merged branch's individual commits are not enumerated, but the net change
introduced to `master` by the merge is reviewed as part of the merge commit.

The currently checked-out branch does not change scan behavior. Scans resolve
and inspect `refs/heads/master` directly.

## 18. CLI

### Initialize

```bash
air init <commit-ish>
```

### Scan new commits

```bash
air scan
```

This scans all unprocessed commits from the configured baseline through
`refs/heads/master`.

An explicit first-parent range may be supplied:

```bash
air scan <from>..<to>
```

The range endpoints must both be on the first-parent history of `master`.

### Current findings

```bash
air status
```

Example:

```text
4 open findings

#17 warning  pcbnew/foo.cpp
    Early constructor exit may leave m_bar uninitialized.

#31 error    common/cache.cpp
    Failure path can retain a dangling pointer.
```

### Review history

```bash
air log
```

Example:

```text
3ac917c  2 new, 0 resolved
44dd180  clean
882af41  skipped: textual diff exceeds 256 KiB
916cc21  0 new, 1 resolved
```

### Show a commit review

```bash
air show <commit-ish>
```

Example:

```text
3ac917c Fix widget startup

New findings:
  #17 warning
  Early constructor exit may leave m_bar uninitialized.

Resolved:
  none

Review:
  Reviewed changes to widget construction and destruction paths.
```

### Show a finding

```bash
air finding 17
```

Example:

```text
#17 warning
Early constructor exit may leave m_bar uninitialized.

Introduced:
    3ac917c Fix widget startup

Resolved:
    916cc21 Initialize backing object before validation
```

### Deferred manual lifecycle overrides

```bash
air close 17
air reopen 17
```

These commands are not part of version 0.1. When implemented, manual actions
should create corresponding `finding_events` and define how the acting commit
is recorded.

### Deferred rescan

```bash
air rescan <commit-ish>
```

Rescanning behavior should be conservative.

The previous raw review should remain recoverable either through event history or a future review-history table if needed.

Rescan is not part of version 0.1. Until review history and lifecycle
reconciliation are implemented, `air rescan` should reject the request without
changing the database.

## 19. Reviewer Configuration

The default reviewer is the locally installed Codex CLI. It reuses Codex's
existing authentication and configuration; AIR never reads or copies Codex
credentials.

The initial CLI reads:

```text
AIR_REVIEWER
AIR_MODEL
AIR_REASONING_EFFORT
AIR_CODEX_BIN
AIR_CODEX_PROFILE
AIR_CODEX_TIMEOUT
AIR_BASE_URL
AIR_API_KEY
AIR_API_KEY_ENV
OPENAI_API_KEY
```

`AIR_REVIEWER` defaults to `codex`. `AIR_CODEX_BIN` defaults to `codex`, and
`AIR_CODEX_PROFILE` may select an optional local Codex configuration profile.
Codex reviews require explicit `AIR_MODEL` and `AIR_REASONING_EFFORT` values so
the exact review provenance is known rather than inferred from changing local
defaults. AIR passes them to Codex as `--model` and
`model_reasoning_effort=<value>`. `AIR_CODEX_TIMEOUT` is a positive Go duration
and defaults to `10m`; it bounds each commit review independently.

The corresponding scan flags are:

```text
--reviewer
--model
--effort
--codex-bin
--codex-profile
--codex-timeout
--base-url
--api-key-env
```

The `http` reviewer remains as an explicit fallback. For that backend,
`AIR_BASE_URL` defaults to `https://api.openai.com/v1`; `AIR_API_KEY_ENV` may
name a different key variable; and `OPENAI_API_KEY` is the final key fallback.
The HTTP reviewer requires `AIR_MODEL` and an API key and uses an
OpenAI-compatible `/chat/completions` endpoint with function tool calls.

No model-specific behavior is embedded into the database schema. A Codex
review records the exact supplied model identifier and reasoning effort. The
HTTP fallback records its supplied model identifier and leaves reasoning effort
null because Chat Completions does not expose that Codex setting.

## 20. Prompt Versioning

Every review must record a prompt version.

Example:

```text
prompt_version = 3
```

Changing reviewer instructions should increment this value.

This allows later analysis of behavior differences between review generations.

The exact prompt text may optionally also be stored in metadata.

## 21. Failure Handling

A commit must not be inserted into `commits` as successfully reviewed until:

- the model request succeeds;
- the response parses successfully;
- database changes are committed atomically.

Use one SQLite transaction per reviewed Git commit.

A commit skipped because of its diff must likewise be recorded atomically, but
requires no model request.

If processing fails:

```text
A reviewed
B reviewed
C failed
D not attempted
```

A later invocation should resume from `C`.

Partial finding updates for a failed commit must be rolled back.

## 22. Cleaning After Git History Changes

Rebases and force-pushes may cause processed commits to leave the current
first-parent history of `refs/heads/master`.

Scanning does not delete or automatically reconcile those records. Until they
are cleaned, stale findings may remain visible in `status` and may be supplied
to reviews.

The initial implementation may defer cleanup. A later maintenance command
should provide:

```bash
air clean
```

`air clean` builds the set of commits currently present in:

```bash
git rev-list --first-parent refs/heads/master
```

It removes database commit records whose SHAs are not in that set. Git object
existence alone is not sufficient: rewritten commits may remain available
through reflogs even though they are no longer on `master`.

Cleanup occurs in one transaction and relies on the schema's foreign-key
actions:

- a finding introduced by a removed commit is deleted;
- finding events attached to a removed commit are deleted;
- if a removed commit resolved a surviving finding, `resolved_sha` becomes
  null, reopening that finding;
- the stale commit record is deleted.

A future implementation may also provide `air clean --dry-run` to list the
records that would be removed.

Automatic history reconciliation is not required initially.

## 23. Security

Repository inspection should be read-only.

The LLM must not be allowed to:

- modify tracked files;
- modify Git refs;
- commit;
- push;
- invoke arbitrary repository scripts merely because they exist;
- execute build products from untrusted historical commits.

The local Codex reviewer runs with `--sandbox read-only` and
`approval_policy="never"`. It is instructed to rely on Git inspection, search,
and ordinary file reading, and not to run builds, tests, repository scripts, or
network commands. User and project exec-policy rules are still loaded; a
command requiring approval fails rather than pausing an unattended scan.

Each review uses `--ephemeral`. AIR captures Codex's JSONL event stream for the
raw review record and reads the final answer through a strict JSON output
schema. A nonzero Codex exit, absent output, invalid output, or attempted
resolution of an unknown finding fails the commit without recording it.

## 24. Performance

The tool is optimized for inexpensive models and frequent execution.

Important principles:

- review only newly discovered commits;
- do not re-review unchanged commits;
- do not repeatedly ask whether every existing finding remains open;
- identify the exact target commit rather than packaging entire repositories;
- let Codex fetch the diff and additional context only when required;
- retain responses locally for debugging;
- avoid expensive indexing infrastructure.

Concurrency is not required initially.

Sequential processing is desirable because finding resolution depends on previous commits having already been processed.

Because worktrees share one database, `air scan` should hold a repository-level
exclusive lock for the duration of the scan. A concurrent scan should fail
clearly rather than duplicate model requests or use inconsistent finding
context. The model request should occur outside the per-commit SQLite
transaction; the parsed result and all associated database changes are then
written in one short transaction.

## 25. Expected Implementation Size

The initial implementation should be small enough to remain comprehensible as a standalone utility.

Suggested implementation:

```text
Go
SQLite via database/sql and a SQLite driver
Git via os/exec
Codex CLI via os/exec
optional HTTP fallback via net/http
JSON via encoding/json
flag-based CLI
```

No ORM is required.

Likely components:

```text
cli.go
db.go
git.go
reviewer.go
codex_reviewer.go
prompt.go
```

A single-file prototype is also acceptable.

## 26. Initial Milestone

Version 0.1 should implement only:

- repository detection;
- database creation;
- `init`;
- validation and enumeration of the first-parent history of `master`;
- default and explicit-range scanning;
- commit diff extraction, binary filtering, and oversized-diff skipping;
- LLM invocation;
- structured JSON parsing;
- new finding creation;
- finding resolution;
- per-commit review summaries;
- `scan`;
- `status`;
- `show`;
- `finding`;
- repository-level scan locking;
- atomic restartable processing.

Everything else, including `clean`, manual lifecycle overrides, and rescan,
should be deferred until actual usage demonstrates a need.

## 27. Core Invariants

The implementation should preserve these rules:

1. Git is authoritative for repository history and contents.
2. SQLite stores only review information that Git does not.
3. Every finding is associated with the commit believed to have introduced it.
4. A finding remains open until explicitly resolved.
5. A reviewed commit may resolve an open finding known when that commit is
   processed.
6. Unchanged findings generate no ongoing database activity.
7. Historical transient problems remain queryable but do not appear in normal current-status output.
8. Each reviewed or skipped commit is processed atomically.
9. Review behavior is reproducible enough to identify the model and prompt version responsible.
10. The tool should remain a semantic linter, not evolve unnecessarily into an issue tracker or code-review platform.
11. Only the first-parent history of `refs/heads/master` is reviewed.
12. Disjoint scans do not trigger automatic lifecycle reconciliation.
