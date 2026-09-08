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

A finding for which neither a resolving commit nor a resolving HEAD recheck has
been recorded and which has not been manually dismissed.

### Review

A single LLM analysis of a Git commit.

Successful rescans retain immutable review-attempt history while replacing the
commit's current review fields.

### Recheck

A model assessment of one or more existing open findings against an exact HEAD
snapshot. A recheck never discovers new findings or replaces a commit review.

## 4. Repository Storage

The database should be associated with the Git repository rather than the current worktree.

Determine its location using:

```bash
git rev-parse --git-common-dir
```

Default database path:

```text
<git-common-dir>/air/reviews.sqlite
```

The scan lock lives at `<git-common-dir>/air/scan.lock`. AIR creates the state
directory with mode `0700` and the database and lock with mode `0600`. This
ensures multiple Git worktrees belonging to the same repository share review
state without placing AIR artifacts at the top level of the Git common
directory.

The database must not modify or require files in the tracked working tree.

## 5. SQLite Schema

The schema should remain intentionally small. The current schema version is 8,
and AIR requires an exact version match when opening a database.

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
```

Optional public review-setting keys are:

```text
harness
model
effort
codex-bin
codex-profile
codex-timeout
claude-bin
claude-timeout
gemini-bin
gemini-timeout
```

The CLI owns validation for these values and does not expose the internal
`start_sha` key through `air config`.

Optional multiline prompt overrides use these internal keys:

```text
prompt.review
prompt.recheck
```

They are managed by `air prompt` and are not exposed as ordinary scalar values
through `air config`.

### 5.2 `models`

Every model used for a review has one configuration record. Known models store
a complete, dated pricing snapshot; other model identifiers are stored with
explicitly unknown pricing.

```sql
CREATE TABLE models (
    name                                TEXT PRIMARY KEY,
    pricing_status                      TEXT NOT NULL,
    service_tier                        TEXT,
    pricing_source                      TEXT,
    pricing_as_of                       TEXT,
    long_context_input_tokens           INTEGER,
    short_input_nanousd_per_token       INTEGER,
    short_cached_nanousd_per_token      INTEGER,
    short_cache_write_nanousd_per_token INTEGER,
    short_output_nanousd_per_token      INTEGER,
    long_input_nanousd_per_token        INTEGER,
    long_cached_nanousd_per_token       INTEGER,
    long_cache_write_nanousd_per_token  INTEGER,
    long_output_nanousd_per_token       INTEGER
);
```

`pricing_status` is `known` or `unknown`. All pricing columns are populated for
known pricing and null for unknown pricing. Rates are integer nanodollars per
token, which exactly represents the published decimal USD-per-million-token
rates without floating-point storage. The initial built-in configurations use
the OpenAI Standard API prices retrieved from
`https://developers.openai.com/api/docs/pricing` on 2026-08-15. They cover
`gpt-5.6-sol`, its `gpt-5.6` alias, `gpt-5.6-terra`, and `gpt-5.6-luna`, with
272,000 input tokens as the boundary above which long-context pricing applies.

Prices in USD per million tokens:

| Model | Context | Input | Cached input | Cache writes | Output |
| --- | --- | ---: | ---: | ---: | ---: |
| `gpt-5.6-sol` | short | 5.00 | 0.50 | 6.25 | 30.00 |
| `gpt-5.6-sol` | long | 10.00 | 1.00 | 12.50 | 45.00 |
| `gpt-5.6-terra` | short | 2.00 | 0.20 | 2.50 | 12.00 |
| `gpt-5.6-terra` | long | 4.00 | 0.40 | 5.00 | 18.00 |
| `gpt-5.6-luna` | short | 0.20 | 0.02 | 0.25 | 1.20 |
| `gpt-5.6-luna` | long | 0.40 | 0.04 | 0.50 | 1.80 |

The `gpt-5.6` alias uses the Sol rows.

The model configuration is inserted or refreshed transactionally whenever a
review or recheck using that model is recorded. Attempt rows retain their
computed cost, so a later pricing update affects only later attempts.

`air model list` merges compiled models with database records, with the
database authoritative on name collisions. `air model show` displays all
stored rates. `air model set-pricing` accepts the complete eight-category
short/long price set in USD per million tokens plus source, date, tier, and
long-context threshold. `air model mark-pricing-unknown` stores an explicit
unknown record, including when overriding a compiled model. Scans always load
the stored record first before falling back to compiled model metadata.

### 5.3 `commits`

```sql
CREATE TABLE commits (
    sha             TEXT PRIMARY KEY,
    parent_sha      TEXT,
    processed_at    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK(status IN ('reviewed', 'skipped')),
    skip_reason     TEXT,
    reviewer        TEXT,
    model           TEXT,
    reasoning_effort TEXT,
    prompt_version  TEXT,
    summary         TEXT,
    raw_response    TEXT,
    input_tokens            INTEGER,
    cached_input_tokens     INTEGER,
    cache_write_tokens      INTEGER,
    output_tokens           INTEGER,
    reasoning_output_tokens INTEGER,
    reasoning_tokens_reported INTEGER NOT NULL DEFAULT 1,
    estimated_cost_microusd INTEGER,
    estimated_cost_max_microusd INTEGER,
    cost_context            TEXT,
    cost_complete           INTEGER,
    reported_cost_microusd  INTEGER,

    FOREIGN KEY(model) REFERENCES models(name)
);
```

Fields:

- `sha`: full Git commit SHA.
- `parent_sha`: first parent against which the commit was reviewed.
- `processed_at`: UTC timestamp at which the commit was reviewed or skipped.
- `status`: either `reviewed` or `skipped`.
- `skip_reason`: reason a skipped commit was not sent to the model; null for a
  reviewed commit.
- `reviewer`: local harness identifier used for a review (`codex`, `claude`, or
  `gemini`);
  null for a skipped commit.
- `model`: model identifier used for a review; null for a skipped commit.
- `reasoning_effort`: harness effort used for a review; null for a
  skipped commit.
- `prompt_version`: version of the reviewer prompt; null for a skipped commit.
- `summary`: short human-readable review log; null for a skipped commit.
- `raw_response`: complete raw model response for debugging and
  reproducibility; null for a skipped commit.
- `input_tokens`: total input tokens reported by the reviewer, including
  cached input tokens; null for a skipped commit.
- `cached_input_tokens`: cached subset of `input_tokens`; null for a skipped
  commit.
- `cache_write_tokens`: subset of `input_tokens` newly written to the prompt
  cache; null when the harness does not report it or for a skipped commit.
- `output_tokens`: total output tokens reported by the reviewer, including
  reasoning output tokens; null for a skipped commit.
- `reasoning_output_tokens`: reasoning subset of `output_tokens`; null for a
  skipped commit.
- `reasoning_tokens_reported`: false when the harness reports total output but
  not its reasoning subset.
- `estimated_cost_microusd`: optional estimated cost rounded to millionths of
  a US dollar; the lower bound when cache-write usage is unavailable, and null
  when model pricing is unknown or for a skipped commit.
- `estimated_cost_max_microusd`: upper estimate bound. It equals
  `estimated_cost_microusd` when every billing category was reported.
- `cost_context`: `short` or `long`, recording the price tier selected from the
  review's reported input-token count.
- `cost_complete`: true when cache-write usage was reported and the two cost
  bounds are therefore equal.
- `reported_cost_microusd`: optional cost estimate supplied by the harness,
  stored independently of AIR's model-registry estimate.

A skipped commit remains in the table so later scans do not retry it
automatically.

These review fields describe the current attempt. A rescan overwrites them but
also appends the same values to `review_attempts`, so ordinary queries remain
simple without losing historical accounting.

### 5.4 `review_attempts`

`review_attempts` contains one immutable row for every successful model call.
It stores the commit SHA, review timestamp, harness, model, effort, prompt version, raw
response, summary, all token and cost fields, and the attempt's new/resolved
finding counts. `duration_ms` records elapsed wall time for the successful
attempt. Rows are ordered by their integer ID within a commit; the last row is
current and must match the denormalized review fields on `commits`.

### 5.5 `findings`

```sql
CREATE TABLE findings (
    id              INTEGER PRIMARY KEY,
    introduced_sha  TEXT NOT NULL,
	introduced_review_id INTEGER NOT NULL,
    resolved_sha    TEXT,
	resolved_recheck_id INTEGER,
	dismissed_at      TEXT,
	dismiss_reason    TEXT,
    severity        TEXT NOT NULL,
    title           TEXT NOT NULL,
    description     TEXT NOT NULL,
    file            TEXT,
    line            INTEGER,
    symbol          TEXT,

	FOREIGN KEY(introduced_sha) REFERENCES commits(sha) ON DELETE CASCADE,
	FOREIGN KEY(introduced_review_id) REFERENCES review_attempts(id) ON DELETE CASCADE,
    FOREIGN KEY(resolved_sha) REFERENCES commits(sha) ON DELETE SET NULL,
	FOREIGN KEY(resolved_recheck_id) REFERENCES recheck_attempts(id) ON DELETE SET NULL
);
```

Initial severity values:

```text
info
warning
error
```

Severity should describe likely impact, not model confidence.

### 5.6 `finding_events`

```sql
CREATE TABLE finding_events (
    id          INTEGER PRIMARY KEY,
    finding_id  INTEGER NOT NULL,
	review_id   INTEGER,
	sha         TEXT,
    action      TEXT NOT NULL,
    note        TEXT,
	created_at  TEXT NOT NULL,

    FOREIGN KEY(finding_id) REFERENCES findings(id) ON DELETE CASCADE,
	FOREIGN KEY(review_id) REFERENCES review_attempts(id) ON DELETE CASCADE,
    FOREIGN KEY(sha) REFERENCES commits(sha) ON DELETE CASCADE
);
```

Initial event types:

```text
opened
resolved
reopened
updated
dismissed
noted
```

The event table exists primarily for auditability.

Normal commits should not generate `still_open` events.

### 5.7 `scan_failures`

```sql
CREATE TABLE scan_failures (
    sha              TEXT PRIMARY KEY,
    parent_sha       TEXT,
    failed_at        TEXT NOT NULL,
    attempt_count    INTEGER NOT NULL,
    error            TEXT NOT NULL,
    reviewer         TEXT,
    model            TEXT,
    reasoning_effort TEXT,
    force            INTEGER NOT NULL
);
```

A reviewer invocation, malformed structured response, missing token usage, or
Git inspection failure is recorded here without adding the commit to
`commits`. Repeated failures upsert the latest details and increment
`attempt_count`. `force` distinguishes a failed rescan from an ordinary scan so
retrying it preserves the user's request for another review attempt. Recording
a successful review or intentional skip deletes the failure in the same SQLite
transaction.

### 5.8 `recheck_attempts` and `recheck_results`

HEAD reconciliation is stored independently of commit reviews:

```sql
CREATE TABLE recheck_attempts (
    id                      INTEGER PRIMARY KEY,
    head_sha                TEXT NOT NULL,
    checked_at              TEXT NOT NULL,
    reviewer                TEXT NOT NULL CHECK(length(trim(reviewer)) > 0),
    model                   TEXT NOT NULL,
    reasoning_effort        TEXT,
    prompt_version          TEXT NOT NULL,
    summary                 TEXT NOT NULL,
    raw_response            TEXT NOT NULL,
    input_tokens            INTEGER NOT NULL,
    cached_input_tokens     INTEGER NOT NULL,
    cache_write_tokens      INTEGER,
    output_tokens           INTEGER NOT NULL,
    reasoning_output_tokens INTEGER NOT NULL,
    reasoning_tokens_reported INTEGER NOT NULL DEFAULT 1,
    estimated_cost_microusd INTEGER,
    estimated_cost_max_microusd INTEGER,
    cost_context            TEXT,
    cost_complete           INTEGER,
    reported_cost_microusd  INTEGER,
    duration_ms             INTEGER NOT NULL,
    finding_count           INTEGER NOT NULL,
    resolved_count          INTEGER NOT NULL,
    still_present_count     INTEGER NOT NULL,
    uncertain_count         INTEGER NOT NULL,

    FOREIGN KEY(model) REFERENCES models(name)
);

CREATE TABLE recheck_results (
    id          INTEGER PRIMARY KEY,
    recheck_id  INTEGER NOT NULL,
    finding_id  INTEGER NOT NULL,
    outcome     TEXT NOT NULL,
    reason      TEXT NOT NULL,

    FOREIGN KEY(recheck_id) REFERENCES recheck_attempts(id) ON DELETE CASCADE,
    FOREIGN KEY(finding_id) REFERENCES findings(id) ON DELETE CASCADE,
    UNIQUE(recheck_id, finding_id)
);
```

New recheck attempts store the selected local harness. Recheck resumability is
keyed by HEAD, harness, model, effort, prompt identity, and finding ID, so a
second harness can independently reassess the same finding.

`outcome` is `resolved`, `still_present`, or `uncertain`. Only `resolved`
populates `findings.resolved_recheck_id`. The effective resolving SHA is the
attempt's `head_sha`; that SHA deliberately has no `commits` foreign key because
HEAD may not have received a commit review. Finding-history queries synthesize
recheck entries from these immutable result rows, including model, effort,
outcome, reason, timestamp, and HEAD SHA.

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

One invocation may be bounded to the oldest `N` currently unprocessed commits:

```bash
air scan --limit N
```

The default is `--limit 0`, meaning unlimited. Commits recorded as skipped
count toward the limit because they are processed for resumability.

Failed commits remain unprocessed but are excluded from later ordinary scans,
including explicit-range scans, dry runs, and `air pending`. They do not count
toward `--limit`. `air retry` selects the explicit failure queue, filters it
against the current first-parent history of master, and processes live failures
oldest first. Current harness configuration and command-line overrides are
used for the retry; the failed attempt's model and effort remain diagnostic
metadata.

### 7.1 Manual skip selection

```bash
air skip <commit-ish> [--dry-run] [--reason TEXT]
air skip --filter <message-substring> [--dry-run] [--reason TEXT]
```

The direct form resolves one commit and requires it to be after the configured
baseline on the first-parent history of `master`. The filter form performs a
case-insensitive literal substring match against the full commit message over
that same history range. Only unprocessed matches are selected; reviewed and
already-skipped matches are reported but never overwritten. The default reason
is `manual skip`.

AIR reads all selected metadata before writing, then records the entire batch
in one transaction. Any failure rolls back the batch. Recording the skip also
removes a matching `scan_failures` row atomically. `--dry-run` lists the exact
selection without acquiring the scan lock or changing commit/review state.

### 7.2 Pre-push and staged review

```bash
air precheck
air precheck --staged
air precheck <from>..<to>
```

Bare `air precheck` reviews the first-parent commits reachable from local
`master` but not from `master`'s configured upstream. AIR resolves both tips
once at startup and processes the resulting range oldest first. The upstream
tip must be on local `master`'s first-parent history. An explicit range uses the
same endpoint and first-parent validation as `air scan`.

`--staged` instead reviews the exact Git index against `HEAD`; `HEAD` must be
the tip of local `master`. Unstaged working-tree changes and untracked files are
not part of the target. AIR applies the same empty, binary-only, and 256 KiB
text-diff exclusions used for commit scans.

Precheck reads existing open findings as resolution candidates but never
writes AIR's database or scan-failure queue. During a multi-commit precheck,
new findings and resolutions are maintained in memory so a later target can
resolve a provisional finding from an earlier target. Only provisional
findings still open after the complete series are included in the final
finding list and considered by `--fail-on`. Predicted resolutions of existing
database findings are reported separately.

`--format text|json|sarif|html` selects standard output; text is the default.
`--fail-on info|warning|error` returns a nonzero status if any final provisional
finding has that severity or a more severe one. Without `--fail-on`, findings
do not change the exit status. The common harness, model, effort, executable,
profile, and timeout overrides accepted by `air scan` also apply.

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

### 8.2 Excluded content

Comments, string-content changes, translations, localization resources, and
documentation are outside review scope. AIR passes the textual diff through
unchanged, subject only to the binary and size transport limits above, and
the selected harness enforces these exclusions semantically. AIR deliberately does
not use path, extension, comment, or string heuristics that could hide relevant
executable context.

For a commit containing only excluded content, the reviewer returns no findings
or resolutions and the commit is recorded as a successful clean review rather
than skipped. In a mixed commit, the reviewer considers executable behavior
only.

### 8.3 Successful scan timing

For each reviewable commit, AIR starts a monotonic wall-time measurement before
reading commit metadata and the diff. It ends after the reviewer response and
its output/usage validation, immediately before the successful database
transaction. AIR stores the elapsed whole milliseconds only when that
transaction succeeds. Failed attempts and skipped commits do not receive a
duration record.

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

The model should be allowed to inspect repository contents when necessary. AIR
delegates inspection to a fresh local Codex, Claude Code, or Gemini CLI session
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

AIR invokes the selected local harness in the repository with a non-persistent,
read-only policy. The prompt identifies the exact commit and first parent.
Codex's normal repository context, `AGENTS.md` instructions, local
configuration, and exec-policy rules remain available. Claude's normal local
context, including `CLAUDE.md`, remains available while AIR disables hooks,
external MCP servers, slash commands, subagents, network access, and writes.
Gemini's normal repository context, including `GEMINI.md`, remains available
while AIR disables extensions, MCP servers, hooks, skills, network access, and
plan model routing, ignores repository `.env` files, and applies a read-only
tool policy. AIR does not reproduce the harnesses' context gathering.

Repository inspection must be read-only.

## 11. Review Context

The initial model context should contain:

- commit metadata;
- commit message;
- a bounded set of open resolution candidates associated with changed files.

AIR computes and bounds the textual diff to decide whether the commit should be
reviewed, but it does not embed that diff in the harness prompt. AIR supplies
the exact commit and first-parent SHAs, and the harness inspects that diff and
the surrounding repository state itself.

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

A HEAD recheck resolving the finding leaves `resolved_sha` null and sets
`resolved_recheck_id` to its immutable attempt. The effective resolving SHA is
the recheck's target HEAD, and history output is synthesized from the retained
recheck result. Reopening clears either resolution field without deleting that
historical assessment.

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

AIR supplies only open findings whose recorded repository-relative `file`
exactly matches a textual file changed by the target commit. Findings without a
file, findings in other files, and matches beyond the first 50 IDs are deferred
without a state change. For a rescan, findings introduced by the target commit
are excluded before this filtering is applied.

An open finding may be represented compactly:

```text
#27 [warning]
src/foo.cpp — FOO::FOO

Early return can leave m_bar uninitialized.

The destructor later assumes m_bar was initialized.
```

The reviewer treats the supplied list as the complete set of IDs it may resolve
for that attempt and must not inspect AIR's database for other IDs. AIR
validates that every returned resolution belongs to the supplied set. Scan
output reports how many candidates were supplied and how many otherwise-open
findings were deferred. This deliberately prefers missed cross-file resolutions
over context growth and incorrect finding IDs.

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

The database retains the entire history. Default user-facing status reports
aggregate finding counts rather than individual findings, so temporary bugs
that appeared and were subsequently fixed during historical scanning do not
clutter normal output.

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

`air pending` and `air scan --dry-run` run the identical enumeration,
processed-commit filtering, limit, diff extraction, and skip classification,
but do not construct a reviewer or write the database. They list each pending
commit as reviewable or skipped in the order a real scan would process it.

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

### Help

```bash
air help
air help <command>
air help <command> <subcommand>
air <command> --help
air <command> <subcommand> --help
```

The command registry is the source for dispatch, grouped global help, command
summaries, usage forms, documented options, and nested help topics. Help is
handled before repository discovery or database access, writes to standard
output, and succeeds. An unknown help topic is an error. Global help groups
commands by workflow and directs the user to command-specific details rather
than presenting every option at once.

Long options use the `--name` form. Options may occur before, between, or after
positional arguments, and `--` ends option parsing so a later positional value
may begin with a hyphen. Help displays options before positional arguments as
the canonical usage form even though both orders are accepted.

### Initialize

```bash
air init <commit-ish>
```

### Diagnostics and database discovery

```bash
air db path
air doctor [--json]
```

`air db path` prints the absolute `<git-common-dir>/air/reviews.sqlite` path and
does not require AIR to be initialized. This makes reset, backup, and direct
SQLite inspection scripts independent of worktree layout.

`air doctor` is a preflight for unattended or expensive scans. It checks
repository discovery, `refs/heads/master`, state-directory and database
permissions, supported schema version, SQLite `quick_check`, effective harness
settings, selected model and pricing, and harness prerequisites. It locates the
effective executable and runs `codex login status` or `claude auth status` with
a bounded timeout. For Gemini it runs `gemini --version` and emits a warning
that the CLI exposes no non-interactive authentication-status check, so the
first model call verifies access. Unknown pricing is a warning; missing
configuration or checkable harness authentication, master, or a usable database is a failed check. Any failed check produces a
nonzero exit after all safe applicable checks have been reported. `--json`
emits the same named checks and aggregate pass/warning/failure counts.

### Backup

```bash
air backup [PATH]
air backup import [--force] PATH
```

AIR creates a consistent SQLite backup containing all committed database and
WAL state. If `PATH` is omitted, it writes
`air-backup-YYYYMMDD-HHMMSS.sqlite` in the current directory, adding a numeric
suffix when that filename already exists. A relative explicit path is resolved
against the current directory. AIR refuses to overwrite any explicit
destination, creates the backup with mode `0600`, runs SQLite `quick_check` on
the completed snapshot, and removes an incomplete destination after any error.
The online backup does not require excluding concurrent readers or writers.

`air backup import` restores a backup into the current repository, including
one that is not currently initialized. AIR opens the source read-only and
requires the current schema version, a successful SQLite `quick_check`, valid
foreign keys, and a configured baseline on the first-parent history of local
`master`. The source file remains unchanged. AIR acquires the scan lock, copies
the source through SQLite's online-backup API into a private temporary file in
the repository state directory, verifies that copy, and replaces the current
database with rollback-safe filesystem renames. Stale SQLite journal, shared
memory, and WAL files from the replaced database cannot be applied to the
imported database.

When a database already exists, import displays the source and destination and
requires confirmation; `--force` is the noninteractive override. Importing
into an uninitialized repository does not prompt. A failed validation, active
scan, cancelled confirmation, or copy error leaves the current database
unchanged. If installation of the verified copy fails after replacement has
started, AIR restores the original database and its SQLite sidecars before
returning the error.

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

AIR records reviewer, response-validation, and per-commit Git-inspection
failures, continues through the selected batch, then returns a nonzero result
summarizing the number of failed commits. `air scan --stop-on-error` records the
first such failure and stops immediately. Subsequent ordinary scans defer
recorded failures and report their count; only `air retry` attempts them again.
Database failures and invalid global review configuration always stop
immediately.

### Failed commits

```bash
air failures [--json]
air retry [--continue-on-error] [reviewer flags]
```

`air failures` displays the durable failure queue. `air retry` processes only
live failed commits, oldest first, and accepts `--limit` plus the same reviewer
configuration overrides as `scan`. A failed rescan is retried as a rescan so
the prior successful review remains current until the retry succeeds. Failure
records for rewritten-away commits are left for `air clean`.

### Repository status

```bash
air status
```

Example:

```text
Findings: 12 total (4 open, 3 dismissed, 5 resolved)
Commits: 7 unscanned, 2 failed, 1 deferred
Estimated remaining scan time: 38m30s (14 timing samples)
```

Status is intentionally aggregate-only. `unscanned` counts commits ready for
an ordinary scan. `failed` counts the complete durable failure queue, including
failed rescans and stale failures. `deferred` counts the live, unprocessed
failed commits omitted by ordinary scans, and is therefore a subset of
`failed`. Use `air finding list`, `air findings`, `air finding <id>`, or `air
export` for finding details. The remaining-time estimate multiplies the unscanned count by the
average duration of all successful timed attempts. It excludes deferred
failures and is unknown when no timing samples exist.

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

### List findings noninteractively

```bash
air finding list
air finding list --all --sort file
air finding list --status dismissed --severity error
air finding list --all --sort severity --limit 50 --json
```

The default is every open finding in descending finding-ID order. The text
table includes fixed ID, severity, status, age, and author columns followed by
an untruncated location and title. Age is measured from the introducing
commit's Git timestamp. `--status` accepts `open`, `dismissed`, `resolved`, or
`all`; `--severity` accepts `error`, `warning`, `info`, or `all`; and `--sort`
accepts `id`, `age`, `file`, `author`, `severity`, `status`, or `title`.
ID order is descending; age puts the oldest commits first; file, author, and
title are alphabetical; severity puts errors before warnings and info; and
status puts open findings before dismissed and resolved findings. Ties use
descending finding ID. `--all` is shorthand for `--status all`, and a zero
`--limit` is unlimited.

`--json` returns the selected full finding records, their dispositions,
introducing commit date and author, and introducing-review attribution. Its
`total` is the number matching the filters before `--limit` is applied. The
command does not invoke a pager or depend on terminal width.

### Browse findings interactively

```bash
air findings [--all]
```

`air findings` is a full-screen terminal browser that initially contains open
findings only. `--all` initially includes open, dismissed, and resolved
findings. The list shows ID, severity, disposition, commit age, author,
location, and title. Left and right cycle through ID, age, file/line, author,
severity, status, and title order; the selected finding remains selected when
the order changes.
The detail view shows the description, commit references, introducing review's
model and reasoning effort, and event history. The browser supports keyboard
navigation, text search, status and severity filters, and independent detail
scrolling. On sufficiently wide and tall terminals, the right pane is split
with a bounded introducing-diff preview below the details. AIR asynchronously
loads and caches the selected file's relevant unified-diff hunk, choosing the
hunk nearest the recorded new-file line and marking that line. It reports
missing locations, unavailable commits, and non-textual diffs in place; narrow
or short terminals retain the details-only layout. It can launch the
introducing commit through `git difftool`, open a
located finding in the editor reported by `git var GIT_EDITOR`, dismiss a
finding with a required reason, reopen a dismissed or resolved finding after
confirmation, and append a note. Lifecycle changes call the same audited
operations as `air finding dismiss`, `reopen`, and `note`. It makes no reviewer
or network calls. When the terminal reports color support, semantic colors
highlight severity, disposition, selection, headings, and messages. Text labels
and the selection marker remain sufficient when color is unavailable.

The browser requires both input and output to be interactive terminals. Scripts
should use `air status --json` for aggregate counts, `air finding list --json`
for filtered findings, or `air export` for interchange formats.

### Manual lifecycle overrides

```bash
air finding dismiss 17 --reason "Intentional compatibility behavior"
air finding note 17 "Verify after the parser rewrite"
air finding reopen 17
air finding diff 17
air finding open 17
```

Dismissal requires a reason and removes the finding from the open status count
and model context without pretending that a commit resolved it. It remains in
the total and dismissed status counts. Reopening clears either a dismissal or a
recorded resolution. Notes do not change disposition. Manual events have no
commit or review ID, but always retain their timestamp, action, and note in
`finding_events`; `air finding <id>` displays that audit history.

`air finding diff <id>` launches `git difftool --no-prompt` for the finding's
introducing commit against its first parent, using the user's configured Git
diff tool. `air finding open <id>` launches the editor selected by
`git var GIT_EDITOR` on the current working-tree file. AIR supplies the recorded
line using the native argument convention for common editors and otherwise
opens the file without a line selector. A missing or invalid finding path is an
error. Both commands attach the child process to the terminal; from the browser,
AIR temporarily leaves full-screen mode and restores it after the child exits.

### Rescan

```bash
air rescan <commit-ish>
```

Rescanning behavior is conservative. The latest attempt replaces the review
fields on the commit row and becomes the current review, while an immutable
`review_attempts` row retains every attempt's model, reasoning effort, prompt
version, response, usage, cost, and finding counts. Findings and resolutions
are attributed to their attempt; prior findings are never silently deleted.
When building rescan context, AIR excludes open findings introduced by the same
target commit so the new attempt independently checks that change.

```bash
air show <commit-ish> --reviews
air show <commit-ish> --review 2
```

### Recheck open findings at HEAD

```bash
air recheck [reviewer flags] [<finding-id> ...]
air recheck --model gpt-5.6-sol --effort xhigh
air recheck --model gpt-5.6-sol --effort xhigh 17 31 562
```

With no IDs, the command selects all current open findings. Explicit IDs must
also be open. AIR resolves `HEAD` once, requires it to equal the tip of
`refs/heads/master`, and directs the reviewer to inspect that exact Git object
rather than unrelated working-tree contents. The finding's recorded location
is only a starting point; the reviewer may inspect related files and history to
recognize moved code, renames, and cross-file fixes. It must not discover or
return new findings.

The structured response contains exactly one result per supplied finding with
an outcome of `resolved`, `still_present`, or `uncertain` and a concrete reason.
Only `resolved` changes finding disposition. All successful outcomes are
retained for auditability and shown by `air finding` and the interactive
browser.

AIR processes batches of 20 findings by default. `--batch-size N` accepts 1
through 50, `--limit N` bounds the number of findings selected by one command,
and `--dry-run` reports pending work without invoking a reviewer or writing.
Each successful batch is committed atomically. `--continue-on-error` continues
after failed model batches and returns a nonzero result at the end. Failed
recheck batches do not change finding state or create successful-attempt rows;
rerunning the command naturally selects them while skipping completed batches.

Successful results are resumable by finding ID, target HEAD, harness, model,
effort, and recheck prompt version. The same identity at the same HEAD is skipped on later
runs; a changed HEAD or model/effort is eligible again.
`--force` repeats otherwise identical successful checks. Recheck uses the same
CLI/environment/database review-setting precedence as `scan`, so a one-off
stronger model needs no separate configuration record.

### Reviewer prompts

```bash
air prompt list
air prompt show [--full] <review|recheck>
air prompt set --file <path> <review|recheck>
air prompt set --stdin <review|recheck>
air prompt reset <review|recheck>
```

There are two independently configurable instruction sets: commit review and
HEAD recheck. `list` reports whether each uses the built-in or database source
and prints its prompt identity. `show` prints the editable instruction portion;
`--full` appends AIR's effective fixed protocol and response contract. A
relative `--file` path is resolved against the current directory. File and
standard-input content must be nonempty UTF-8 and no larger than 256 KiB.

`set` stores repository-specific instructions in the existing `config` table.
It replaces the editable instructions but cannot replace AIR's fixed
repository-data trust boundary, read-only inspection constraints, resolution
candidate restrictions, or structured response contract. `reset` deletes the
override and restores the compiled instructions. Backups naturally include the
overrides.

### Repository accounting

```bash
air stats [--model MODEL] [--since DATE]
air cost [--model MODEL] [--since DATE]
```

Accounting includes every retained commit-review and HEAD-recheck attempt
because superseded rescans and reconciliation calls still consumed tokens.
Totals include input, cached-input, cache-write, output, and reasoning-output
tokens and are grouped by model and reasoning effort. Costs are summed as lower
and upper bounds from the estimates stored on each attempt, preferring a
harness-reported cost when one exists. Attempts with unknown prices and
attempts for which a harness omitted cache-write or reasoning detail are
counted explicitly. `--since` accepts either a UTC date or an RFC3339 timestamp;
`--model` is an exact model identifier match. Commit reviews and rechecks have
separate attempt counts. `stats` reports average scan time per commit only from
successful timed commit reviews selected by those filters; recheck batch
durations do not affect scan-time estimates.

`air stats` also reports current open, dismissed, and resolved finding counts
and the number of skipped commits. These repository-state counts are not
affected by the review filters.

### Machine-readable output

```bash
air status --json
air show <commit-ish> --json
air precheck --format json
air precheck --format sarif
air precheck --format html > air-precheck.html
air export --format json
air export --format sarif
air export --format html > air-findings.html
```

JSON uses documented snake-case field names rather than mirroring Go field
names. Status JSON contains `findings` and `commits` objects with the same
aggregate counts, timing-sample count, and nullable millisecond estimate as
text output; it contains no finding log. JSON and SARIF export contain current
open findings.
Show JSON includes commit metadata, the current commit record, findings
introduced or resolved by the selected review, and retained attempts when
`--reviews` is given. Current and retained review records include `duration_ms`
when known. `--review N --json` selects a historical attempt.

SARIF export uses version 2.1.0. Each open finding becomes one result with AIR's
severity mapped to SARIF `error`, `warning`, or `note`, a stable finding-ID
fingerprint, introducing commit metadata, and an artifact URI/start line when
available. Resolved and dismissed findings are not exported.

HTML export writes one self-contained, offline viewer. It includes every
finding disposition, review attribution, finding event history, and a bounded
excerpt of the finding's introducing diff when a textual hunk is available.
The viewer starts with open findings and supports search; status and severity
filters; ID, age, file, author, severity, status, and title sorts; keyboard
selection; and a responsive list/detail layout. It is a static snapshot and
therefore cannot mutate the AIR database or invoke editors and Git difftools.
It contains no repository configuration, API keys, raw model responses, or
external assets.

## 19. Review Harness Configuration

AIR supports the locally installed Codex, Claude Code, and Gemini CLIs. It
reuses the selected harness's existing authentication, configuration, and
repository instructions and never reads or copies credentials. Codex remains
the default. There is no remote HTTP review backend.

Every review setting has a database representation, an environment-variable
override, and, where applicable, a `scan`, `retry`, `rescan`, or `recheck` flag
override. Values are resolved with this fixed precedence:

```text
command-line flag > environment variable > database > built-in default
```

| Database setting | Scan flag | Environment variable | Default |
| --- | --- | --- | --- |
| `harness` | `--harness` | `AIR_HARNESS` | `codex` |
| `model` | `--model` | `AIR_MODEL` | none |
| `effort` | `--effort` | `AIR_REASONING_EFFORT` | none |
| `codex-bin` | `--codex-bin` | `AIR_CODEX_BIN` | `codex` |
| `codex-profile` | `--codex-profile` | `AIR_CODEX_PROFILE` | none |
| `codex-timeout` | `--codex-timeout` | `AIR_CODEX_TIMEOUT` | `20m` |
| `claude-bin` | `--claude-bin` | `AIR_CLAUDE_BIN` | `claude` |
| `claude-timeout` | `--claude-timeout` | `AIR_CLAUDE_TIMEOUT` | `20m` |
| `gemini-bin` | `--gemini-bin` | `AIR_GEMINI_BIN` | `gemini` |
| `gemini-timeout` | `--gemini-timeout` | `AIR_GEMINI_TIMEOUT` | `20m` |

`air config set`, `get`, `unset`, and `list` manage only these public review
settings in the existing `config` table. `air config list --effective` includes
all settings, their resolved values, and their winning sources.

Reviews require explicit effective `model` and `effort` values so provenance is
known rather than inferred from changing harness defaults. AIR passes the model
and any supported effort control explicitly. The selected harness timeout is a
positive Go duration and bounds each commit review or recheck batch
independently.

Gemini CLI does not expose per-invocation reasoning effort. The Gemini harness
therefore accepts only the explicit sentinel `default`; other values fail
before a model call rather than recording provenance AIR could not enforce.
AIR passes the model explicitly and disables Plan Mode model routing. A result
must contain token statistics for exactly that requested model.

The harness must report token usage. AIR records input, cached-input,
cache-write, output, and reasoning-output counts for every commit review and
recheck batch, plus whether cache-write or reasoning detail was unavailable.
Codex usage comes from the final JSONL `turn.completed` event. For Claude,
ordinary input, cache creation, and cache reads sum to AIR total input; cache
creation maps to cache writes and cache reads map to cached input. Claude does
not expose the reasoning subset of output, so AIR records that category as
unreported rather than as a measured zero.

For Gemini, AIR maps prompt plus tool-use prompt tokens to total input, cached
tokens to cached input, candidates plus thoughts to total output, and thoughts
to reasoning output. Gemini does not report cache writes, so AIR retains that
category as unreported.

AIR's model registry produces an API-equivalent estimate from stored pricing.
If a harness also reports an invocation cost (Claude's `total_cost_usd`), AIR
stores it separately and prefers it when aggregating costs. Subscription-backed
harness execution may not represent a distinct billed USD amount.

Cached reads, cache writes, ordinary input, and output are separate billing
categories. Reasoning output is already included in output and is not added
again. When cache-write tokens are unavailable, AIR stores the range obtained
by treating the uncategorized input as ordinary input at one bound and cache
writes at the other. Unknown model pricing produces a null cost rather than a
fabricated estimate. `air show` identifies ranges and unknown costs clearly.

No model-specific behavior is embedded into the database schema. Every review
records the selected harness and exact supplied model identifier and effort.

## 20. Prompt Versioning

Every review must record a prompt version.

Example:

```text
prompt_version = 4
```

Changing compiled reviewer instructions or their fixed protocol should increment
the corresponding numeric value.

Prompt version 3 excludes comments, string-content changes, translations,
localization resources, and documentation as a model review policy. AIR does
not use path, extension, comment, or string heuristics to filter the textual
diff before reviewer invocation. The reviewer must return no findings or
resolutions for excluded-only changes and review only executable behavior in
mixed commits.

Prompt version 4 retains that scope policy and defines the supplied open
findings as the only permitted resolution candidates. Other open findings may
exist but are explicitly out of scope for that review.

HEAD rechecks have an independent prompt-version sequence because they do not
review a commit diff or discover findings. Recheck prompt version 1 requires
exactly one `resolved`, `still_present`, or `uncertain` result for every supplied
finding, prohibits new findings, and treats recorded locations as context rather
than an inspection boundary.

Compiled prompts retain these numeric identities. A database override instead
uses this form:

```text
custom:sha256:<64 lowercase hexadecimal digits>
```

The digest covers an AIR domain tag, prompt kind, compiled protocol version,
and complete effective static prompt. It therefore changes when the stored
instructions change or when AIR changes the fixed protocol. The custom identity
is stored in the `prompt_version` columns on commit-review and recheck attempts.
Recheck resumability includes this identity, so changing recheck instructions
makes otherwise identical findings eligible again.

This allows later analysis of behavior differences between review generations.
The database stores custom instruction text in `config`; built-in text remains
available from the installed binary through `air prompt show`.

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
are cleaned, stale findings remain included in `status` counts and may be
supplied to reviews.

The maintenance command is:

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

`air clean --dry-run` lists the same ordered set without changing the database.
Both modes hold the repository scan lock while comparing Git and SQLite state.

`air reset` removes the entire `<git-common-dir>/air` directory after displaying
the exact path and receiving interactive confirmation. `--force` is the
explicit non-interactive override. Reset first acquires and releases the scan
lock so it refuses to race an active scan.

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
`approval_policy="never"`. User and project exec-policy rules are still loaded;
a command requiring approval fails rather than pausing an unattended scan.
Each Codex review uses `--ephemeral`.

The local Claude reviewer requires Claude's sandbox and disables unsandboxed
commands, writes to the repository, network access, hooks, external MCP
servers, slash commands, subagents, web tools, and write tools. It enables only
file reading/search plus an allowlist of read-only Git commands. Sessions are
not persisted, and the fallback model is pinned to the requested model so
stored provenance remains exact.

The local Gemini reviewer uses noninteractive Plan Mode plus a higher-priority,
deny-by-default policy. AIR merges stricter controls into a temporary copy of
system settings, disables extensions, MCP, hooks, skills, network, interactive
shells, write todos, model routing, and repository `.env` loading, and allows
only file reading/search and specific read-only Git command prefixes. The
temporary settings and policy are removed after the invocation.

All harnesses receive the same fixed read-only inspection contract and JSON
response contract; AIR strictly validates every result even when a CLI has no
schema flag. AIR retains the raw harness response. A nonzero harness
exit, absent output or usage, invalid output, or attempted resolution of an
unknown finding fails the commit without recording it as reviewed.

## 24. Performance

The tool is optimized for inexpensive models and frequent execution.

Important principles:

- review only newly discovered commits;
- do not re-review unchanged commits;
- do not repeatedly ask whether findings remain open during ordinary scans;
- make explicit HEAD rechecks resumable by target and harness configuration;
- identify the exact target commit rather than packaging entire repositories;
- let the harness fetch the diff and additional context only when required;
- retain responses locally for debugging;
- avoid expensive indexing infrastructure.

Concurrency is not required initially.

Sequential processing is desirable because finding resolution depends on previous commits having already been processed.

Because worktrees share one database, `air scan`, `air recheck`, and mutating
maintenance commands such as `air skip` should hold a repository-level
exclusive lock for the duration of their operation. A concurrent writer should
fail clearly rather than duplicate model requests or use inconsistent finding
context. Model requests occur outside SQLite transactions; each parsed commit
review or recheck batch and all associated database changes are written in one
short transaction.

## 25. Expected Implementation Size

The initial implementation should be small enough to remain comprehensible as a standalone utility.

Suggested implementation:

```text
Go
SQLite via database/sql and a SQLite driver
Git via os/exec
Codex, Claude Code, or Gemini CLI via os/exec
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
claude_reviewer.go
gemini_reviewer.go
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

The subsequent proof-of-concept increments add retained rescans, cleanup,
manual lifecycle overrides, reporting, and automation output without expanding
AIR beyond the first-parent history of `master`.

## 27. Core Invariants

The implementation should preserve these rules:

1. Git is authoritative for repository history and contents.
2. SQLite stores only review information that Git does not.
3. Every finding is associated with the commit believed to have introduced it.
4. A finding remains open until explicitly resolved.
5. A reviewed commit may resolve an open finding known when that commit is
   processed.
6. Ordinary commit scans generate no ongoing database activity for unchanged
   findings; explicit HEAD rechecks retain their assessment results.
7. Historical transient problems remain queryable and contribute only aggregate counts to normal status output.
8. Each reviewed or skipped commit is processed atomically.
9. Review behavior is reproducible enough to identify the model and prompt version responsible.
10. The tool should remain a semantic linter, not evolve unnecessarily into an issue tracker or code-review platform.
11. Only the first-parent history of `refs/heads/master` is reviewed.
12. Disjoint scans do not trigger automatic lifecycle reconciliation.
13. HEAD rechecks never create findings or replace commit-review records.
