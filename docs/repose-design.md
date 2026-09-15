# Repose design

## Purpose and operating assumptions

Repose audits a fixed repository snapshot over hours or days. KiCad is the
primary target, so C/C++, CMake, and clangd are intentional dependencies rather
than adapters in a general-purpose language framework. Repose can retain Go,
SQLite, model subprocess runners, accounting, findings triage, and exports from
AIR.

The scan checkout is separate from development and stays unchanged during a
scan. Repose records its current commit; findings use `observed_sha` semantics.
There is no introducing-commit diagnosis, commit enumeration, first-parent
requirement, or restriction to a particular branch. A detached HEAD is valid.

An audit may involve repeated passes over the same codebase map with different
models and increasingly specific questions. The first goal can simply be to
find concrete correctness problems.

## Durable concepts

| Concept | Contract |
| --- | --- |
| Inventory version | Immutable source/build identity, tracked files, compiler facts, grouping rules, and annotations |
| Inventory review | User acknowledgement of a particular inventory version; stored separately from immutable content |
| Scan | One inventory version, frozen subset, goal, model/settings, prompts, project guidance, and optional prior evidence |
| Task | A bounded responsibility within a scan, referencing inventory entities with durable status |
| Attempt | One execution of a task, retaining output, failures, usage, timing, and provenance |
| Finding | Defect observed at the scan snapshot, with one or more evidence records from tasks and scans |

Inventory entities and scan assignments have different lifetimes. File/class
review, ownership/lifecycle review, and reader/writer consistency review can
generate different task shapes over the same inventory. Assignments define
responsibility; reviewers may follow related code outside the selected subset
to establish behavior.

Completion is per scan/task. No inventory entity is globally marked reviewed.
Results distinguish completed review, inability to assess, failures, and scope
exclusions. An empty finding list is insufficient to establish review coverage.

Independent model passes can omit previous findings. Follow-up passes can use
an explicitly selected, frozen set of prior evidence. Model agreement is
supporting information rather than proof. Deduplication must preserve each
observation and its original model/scan provenance. Absence from a later pass
does not automatically resolve a finding.

## Inventory workflow

1. Build the mechanical map from a clean dedicated checkout and its compilation
   database. Keep every tracked path accountable, including exclusions and
   code outside the active configuration.
2. Inspect group sizes, source files without compile commands, unassociated
   headers, generated inputs, and external inputs.
3. Adjust grouping, tags, exclusions, and notes through a reviewable policy.
4. Save a new immutable version when inputs or policy change.
5. Approve the specific version before creating scans over it.

Compiler-derived facts remain distinguishable from user instructions and later
AI interpretations. Optional model-written subsystem descriptions should retain
their provenance and must not silently replace compiler facts.

The current document contains committed file paths, modes, blob IDs, byte sizes,
source/header classification, initial groups, policy annotations, and all
compilation-command variants. It also includes the exact checkout path, commit,
compilation database fingerprint, and adjacent CMake cache fingerprint when
available. The ID hashes the serialized document; timestamps and approval are
stored outside that hash. Paths and compilation-database bytes are intentionally
part of identity, so relocation or reformatting can create another version.

Inventory building reads Git and JSON; it never executes compilation commands,
repository programs, builds, tests, or model calls. It accepts both `arguments`
and `command`, retains `output`, and resolves source paths relative to each
command's directory, following the
[Clang compilation database format](https://clang.llvm.org/docs/JSONCompilationDatabase.html).
This implementation requires absolute command directories. Multiple commands
for the same source remain distinct. A database with no matching tracked C/C++
sources is rejected to catch configurations pointing at another checkout.

Storage is local to the scan worktree's Git directory. Linked worktrees share
Git objects but have separate Repose databases. AIR state remains separate.
Inventory documents are stored atomically in a new SQLite schema with an
application ID; repeated/concurrent saves of identical content produce one row.
Additional queue and evidence tables will use explicit schema migrations.

Schema version 2 adds a current-inventory pointer. Its policy is the saved
project policy and the default for subsequent builds. Scope commands and JSON
imports derive immutable versions from the stored facts, then publish the
version and current pointer in one transaction. A compare against the input
version prevents a concurrent policy edit from being lost. Restoring an existing
document reuses its ID and approval while making it current; `latest` remains
the most recently created document. Older storage migrates on write without
rewriting inventory documents or approvals. Schema version 3 adds semantic
profiles and immutable per-file results. Version 4 adds scan, task, attempt,
finding, and lifecycle event tables. Inspection supports versions 1 through 4
without migration; writers upgrade older Repose databases transactionally.

New exclusions require a reason and a matching C/C++ file/directory prefix.
Inclusion overrides follow the same ordered policy. Exclusions remove direct
review assignments and reportable defects while leaving dependencies available
as context. Literal path rules apply to future snapshots; inherited rules with
no current matches remain visible and retained. Explicit imports reject unmatched
rules to catch typos. The exclusions listing shows both matched and effective
counts, accounting for subsequent overrides.

Read-only inspection uses SQLite's read-only/query-only connection modes and
acquires no writer transaction. Writers retain WAL/SHM sidecars so subsequent
readers can inspect state from a read-only directory. This preserves access to
committed WAL data; the database is never opened with `immutable=1`. Older
databases without sidecars need one writable open to prepare them. See SQLite's
[read-only WAL behavior](https://www.sqlite.org/wal.html#read_only_databases).

### Current boundaries

- The immutable inventory remains a file/build map. Separate semantic profiles
  now provide symbols, resolved direct includes, borrowed header contexts,
  diagnostics, and assignment previews using symbol boundaries. Full reference
  graphs and alternate compile variants remain upcoming. The initial durable
  queue and observed-snapshot findings workflow are implemented.
- The current build configuration provides partial compiler coverage. Missing
  commands and unmapped headers remain visible; platform/feature alternatives
  must be handled explicitly by later configurations or review tasks.
- The committed tree defines source scope. Untracked compilation inputs are
  counted separately, including generated files. Submodule contents and symlinks
  are explicitly excluded. C/C++ code in tracked generated files outside
  `build/` requires an explicit policy rule to exclude it.
- Start/check/approval validate tracked cleanliness, commit, and recorded build
  input fingerprints. These checks do not certify the freshness of generated
  headers, external dependencies, compiler binaries, or an index. Preparing and
  recording a compatible semantic environment is part of indexing. Direct and
  forced includes are fingerprinted; transitive/external changes need a refresh.
- A scan relies on the dedicated checkout remaining unchanged while it runs.
  It does not continuously monitor a development tree.

## Parallel execution and recovery contract

The current CLI provides directory/group summaries, detailed inspection, direct
group/tag/note curation, and `inventory plan`. The `files-v1` preview partitions
selected included files within group/directory boundaries using file and byte
limits. Oversized files remain flagged singletons. Plans include the inventory,
snapshot, question, selection, limits, target facts/annotations, and deterministic
IDs. These are file partitions rather than semantic units or persisted scan
records. Header relationships and model context budgets are not inferred.

`inventory browse` uses the same selection, curation, and planning functions.
It reads without writer transactions and opens a writer only to save an edit
against the previously viewed current ID. Explicit historical selectors are
read-only. Policy changes preserve existing inventories and approvals. Browser
filters control display; prefix edits also apply to hidden descendants.

The execution layer uses one local coordinator and a bounded pool
of workers. SQLite stores tasks before dispatch, including their frozen inputs.
Scans on multiple models share resource limits. Compiler indexing should be
shared for an inventory rather than restarted per model task.

Task states include pending, running, completed, failed, and unable to assess.
Attempts are append-only. A task has an atomic claim and attempt generation;
late results from an obsolete attempt must not overwrite a newer attempt.
Publishing results, evidence, and task completion is one database transaction.
Execution may repeat after a crash; publishing completion is idempotent.

With one coordinator, ownership and a process lock allow abandoned running tasks
to be recovered on restart. If independent coordinators are added, claims need
leases and heartbeats. Multiple local workers are required from the first
working scan; distributed scheduling is deferred.

- **Pause:** stop dispatching, let active tasks finish, persist results, exit.
- **Interrupt:** terminate active work; retain completed tasks and mark unfinished
  attempts interrupted. Resume retries unfinished assignments.
- **Crash:** recover abandoned assignments without losing completed results or
  duplicating publication. A live provider conversation need not be resumable.
- **Retry:** retain failures and retry history, apply bounded backoff, distinguish
  retryable provider failures from invalid inputs, and avoid indefinite loops.

Subset selection freezes scan scope. Execution limits control further task
dispatch without shrinking that scope: concurrency, task count, elapsed time,
and eventually token/cost budgets. Already-running calls may exceed a dispatch
budget; report that explicitly. Concurrency and per-assignment timeouts may change
on resume. New scans default to 30 minutes; run/resume overrides apply only to that
invocation, and each attempt records its effective timeout when claimed. Changes to
model, question, scope, prompt, inventory, or supplied evidence create a new
scan rather than changing the meaning of previous completion.

## Implementation sequence

1. **Inventory foundation — implemented.** Repose entry point, file/build map,
   policy annotations, immutable persistence, inspection/filtering, explicit
   review acknowledgement, snapshot/build-input checks, CLI curation, file
   assignment previews, and a terminal inventory browser.
2. **Semantic inventory — initial integration implemented.** Shared clangd
   process, bounded file workers, durable per-file results/resume, symbol ranges,
   resolved includes and borrowed header contexts, diagnostics, CLI/TUI inspection,
   and semantic assignment previews. Validated on the actual KiCad router.
   See [semantic indexing details](semantic-inventory.md) for limits and storage.
3. **Durable parallel scan — initial integration implemented.** Frozen scans,
   prompts, tasks, and attempts; bounded workers; pause/interrupt/recovery;
   task/time dispatch limits; and explicit failed-task retries. Codex, Claude,
   and Gemini subprocess adapters use assignment prompts and structured results.
   Tests exercise interruption, abandoned claims, stale/duplicate completion,
   malformed output, concurrent dispatch, and repeated resume. Codex quota errors
   pause dispatch and drain workers, retaining blocked tasks as pending. Temporary
   throttling uses durable cooldowns, bounded retries, and a single probe before
   parallel work resumes. Total dispatch caps include retries; a 32-worker,
   32-attempt batch is covered by a CLI integration test. Automatic allowance
   checks, other-harness quota handling, and token/cost budgets remain future work.
4. **Findings and verification — triage and rechecks implemented.** Findings now
   retain observed-at-snapshot evidence, scan/task/attempt provenance, raw output,
   usage, and available reported cost. AIR's triage UI supports the new store,
   notes/disposition history, and source previews. Recheck passes group findings by
   their original assignment, cap each batch with `--batch-max` (default 5), and
   freeze that layout. They use a separately selected model and the shared
   coordinator, retaining independent confirmed/false-positive/uncertain verdicts
   without changing manual dispositions. Worker and attempt limits count batch
   calls. Matching selections/models/batch maxima resume; explicit fresh passes
   retain earlier opinions. Schema version 7 stores every batch's verification
   results atomically with attempt completion and adds normalized user-managed
   finding tags with audited bulk edits. Incomplete or malformed responses
   fail the batch. Existing single-finding passes still resume by ID with their
   original protocol. The TUI and JSON expose verdicts, reasoning, and history.
   JSON, SARIF, and offline HTML exports retain snapshot provenance and verification
   history. HTML uses source excerpts at the observed snapshot with verdict filters.
   Repose backup creation and validated import preserve the complete worktree-local
   SQLite state, including committed WAL data, across supported schema versions.
   Duplicate consolidation and pricing estimates remain future work.
5. **Specialized repeated passes.** Add cross-file task generation, explicit
   prior-evidence inputs, model comparisons, and deeper KiCad-specific questions.

The inherited AIR code remains available during extraction. Its scan commands
are not exposed by the Repose entry point, and its database cannot serve as the
new inventory/scan database. There is no requirement to migrate AIR's historical
introducing-commit attribution into Repose.
