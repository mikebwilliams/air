# AI Git Commit Reviewer

## Name
air - short for AI reviewer

## 1. Purpose

Build a lightweight, local tool that reviews Git commits using an LLM and persists findings in a repository-local SQLite database.

The tool is intended to behave like a semantic lint pass over Git history:

- review each incoming commit independently;
- identify concrete correctness problems introduced by that commit;
- retain a small log associated with each reviewed commit;
- persist unresolved findings;
- allow later commits to resolve findings introduced by earlier commits;
- scan historical commits beginning from a user-selected Git commit;
- show only findings believed to remain unresolved at the current reviewed HEAD by default.

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
- a generalized abstraction for non-Git data sources.

Git is a required dependency and part of the application model.

## 3. Terminology

### Commit

A Git commit reviewed by the tool.

Commits are identified internally by their full object SHA.

### Finding

A potential correctness problem reported by the LLM.

A finding has a lifecycle beginning with the commit believed to have introduced it and optionally ending with a later commit believed to have resolved it.

### Open finding

A finding for which no resolving commit has been recorded.

### Review

A single LLM analysis of a Git commit.

A commit may eventually have multiple reviews if manually rescanned, but one review is considered current.

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
history_mode
```

### 5.2 `commits`

```sql
CREATE TABLE commits (
    sha             TEXT PRIMARY KEY,
    parent_sha      TEXT,
    reviewed_at     TEXT NOT NULL,
    model           TEXT,
    prompt_version  TEXT,
    summary         TEXT,
    raw_response    TEXT
);
```

Fields:

- `sha`: full Git commit SHA.
- `parent_sha`: effective parent against which the commit was reviewed.
- `reviewed_at`: UTC timestamp of the current review.
- `model`: model identifier used for the review.
- `prompt_version`: version of the reviewer prompt.
- `summary`: short human-readable review log.
- `raw_response`: complete raw model response for debugging and reproducibility.

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

    FOREIGN KEY(introduced_sha) REFERENCES commits(sha),
    FOREIGN KEY(resolved_sha) REFERENCES commits(sha)
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

    FOREIGN KEY(finding_id) REFERENCES findings(id),
    FOREIGN KEY(sha) REFERENCES commits(sha)
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
- the specified object cannot be resolved to a commit;
- a database is already initialized unless an explicit reset/reinitialize option is supplied.

The starting commit itself is treated as the baseline and is not reviewed unless explicitly requested.

## 7. Commit Enumeration

Default historical scan:

```bash
git rev-list --reverse "$start_sha"..HEAD
```

Commits already present in the `commits` table are skipped.

The system should therefore be naturally resumable.

Example:

```bash
air scan
```

If 1,000 commits are eligible and 600 have already been reviewed, only the remaining commits are processed.

## 8. Commit Review

For each commit, determine its effective parent and obtain at minimum:

- commit SHA;
- parent SHA;
- commit author;
- commit date;
- commit subject/body;
- diff against the effective parent.

Typical commands:

```bash
git show -s --format=fuller "$sha"
git diff "$parent" "$sha"
```

The LLM must review the commit as a change from its parent, not simply review the final repository state.

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

The model should be allowed to inspect repository contents when necessary.

The preferred model is agentic rather than purely single-prompt.

At minimum, the reviewer may need equivalent access to:

```bash
git show <sha>:<path>
git show <sha>^:<path>
git grep <pattern> <sha>
git diff <sha>^ <sha> -- <path>
git log ...
```

The reviewer should generally inspect repository state through Git object access rather than requiring the worktree to be checked out at each historical commit.

The implementation may provide a restricted command interface instead of unrestricted shell access.

Repository inspection must be read-only.

## 11. Review Context

The initial model context should contain:

- commit metadata;
- commit message;
- commit diff;
- current open findings.

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
 HEAD
```

The database retains the entire history.

Default user-facing status at the end should report only currently open findings.

Thus temporary bugs that appeared and were subsequently fixed during historical scanning do not clutter normal output.

## 16. Continuous Operation

After an initial scan, normal use is:

```bash
git pull
air scan
```

The tool discovers commits after the last already-reviewed commit and processes them in chronological order.

It does not require a daemon.

A daemon, cron job, systemd timer, post-fetch hook, or other automation may be layered on later.

## 17. Merge Commits

Merge semantics must be explicit.

Initial supported modes:

```text
all
first-parent
```

### `all`

Enumerate ordinary Git history:

```bash
git rev-list --reverse START..HEAD
```

For a merge commit, review relative to its first parent unless otherwise specified.

### `first-parent`

Enumerate:

```bash
git rev-list --reverse --first-parent START..HEAD
```

This mode treats the mainline history as authoritative.

The selected mode should be stored in config.

Initial default:

```text
all
```

No attempt should initially be made to construct an alternate revision graph in SQLite.

Git remains the authority for ancestry.

## 18. CLI

### Initialize

```bash
air init <commit-ish>
```

### Scan new commits

```bash
air scan
```

Optional target:

```bash
air scan <commit-ish>
```

Default target:

```text
HEAD
```

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

### Manual lifecycle overrides

```bash
air close 17
air reopen 17
```

Manual actions should create corresponding `finding_events`.

### Rescan

```bash
air rescan <commit-ish>
```

Rescanning behavior should be conservative.

The previous raw review should remain recoverable either through event history or a future review-history table if needed.

The initial implementation may simply reject rescan if lifecycle reconciliation is not yet safely implemented.

## 19. Model Configuration

The reviewer should support configurable LLM endpoints.

At minimum:

```text
model
base_url
api_key environment variable
```

OpenAI-compatible HTTP APIs are sufficient for the first implementation.

Example configuration:

```ini
model = luna-high
base_url = https://example.invalid/v1
api_key_env = REVIEW_API_KEY
```

No model-specific behavior should be embedded into the database schema.

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

If processing fails:

```text
A reviewed
B reviewed
C failed
D not attempted
```

A later invocation should resume from `C`.

Partial finding updates for a failed commit must be rolled back.

## 22. Git History Changes

Rebases and force-pushes may cause previously reviewed commits to no longer be reachable from the selected target.

The initial tool should not delete historical review records automatically.

`scan` should operate on currently reachable commits and ignore unreachable historical database entries.

A later maintenance command may expose:

```bash
air gc
```

to identify or remove records for unreachable commits.

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

Where shell access is provided, commands should preferably be allowlisted.

The reviewer should rely primarily on Git inspection commands and ordinary file reading.

## 24. Performance

The tool is optimized for inexpensive models and frequent execution.

Important principles:

- review only newly discovered commits;
- do not re-review unchanged commits;
- do not repeatedly ask whether every existing finding remains open;
- send diffs rather than entire repositories;
- let the model fetch additional context only when required;
- retain responses locally for debugging;
- avoid expensive indexing infrastructure.

Concurrency is not required initially.

Sequential processing is desirable because finding resolution depends on previous commits having already been processed.

## 25. Expected Implementation Size

The initial implementation should be small enough to remain comprehensible as a standalone utility.

Suggested implementation:

```text
Python
SQLite via stdlib sqlite3
Git via subprocess
HTTP/model client
argparse or similar CLI
```

No ORM is required.

Likely components:

```text
cli.py
db.py
git.py
reviewer.py
prompt.py
```

A single-file prototype is also acceptable.

## 26. Initial Milestone

Version 0.1 should implement only:

- repository detection;
- database creation;
- `init`;
- commit enumeration;
- commit diff extraction;
- LLM invocation;
- structured JSON parsing;
- new finding creation;
- finding resolution;
- per-commit review summaries;
- `scan`;
- `status`;
- `show`;
- `finding`;
- atomic restartable processing.

Everything else should be deferred until actual usage demonstrates a need.

## 27. Core Invariants

The implementation should preserve these rules:

1. Git is authoritative for repository history and contents.
2. SQLite stores only review information that Git does not.
3. Every finding is associated with the commit believed to have introduced it.
4. A finding remains open until explicitly resolved.
5. Later commits may resolve earlier findings.
6. Unchanged findings generate no ongoing database activity.
7. Historical transient problems remain queryable but do not appear in normal current-status output.
8. Each successfully reviewed commit is processed atomically.
9. Review behavior is reproducible enough to identify the model and prompt version responsible.
10. The tool should remain a semantic linter, not evolve unnecessarily into an issue tracker or code-review platform.
