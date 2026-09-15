# Repose

Repose is a local tool for sustained, parallel AI audits of a fixed C/C++
repository snapshot. Its primary target is KiCad. An inventory can support
multiple scans with different models, questions, and selected areas of code.

Repose implements **inventory building, curation, clangd indexing, assignment
browsing, durable parallel model scans, and a findings TUI**. Cross-scan finding
deduplication and a full reference graph remain upcoming. Findings can be verified
in separate, resumable recheck passes with a chosen model.
The [design and implementation sequence](docs/repose-design.md) records their
contracts and the current boundaries.

Repose is forked from AIR. AIR's model runners, reporting, and triage code remain
available in the source and are being adapted to snapshot assignments. The
Repose executable uses separate inventory/scan storage. The [original README](docs/air-readme.md) and
[AIR specification](docs/initial_spec.md) describe the inherited implementation.

## Build

Requires Go 1.26+, Git, and a C compiler for SQLite.

```sh
make
make test
.build/repose help
```

`make install` installs `repose` and its manual page under `/usr/local` by default.
Override `PREFIX` or use `DESTDIR` for a staged installation.

## Prepare a scan checkout

Use a dedicated checkout, separate from your development tree. Keep its source
and build configuration unchanged throughout an audit. Repose does not create,
update, or configure the checkout itself.

Configure KiCad in that checkout using your usual CMake settings with
`-DCMAKE_EXPORT_COMPILE_COMMANDS=ON`. Compilation commands must reference the
scan checkout. A database copied from the development checkout normally points
at the wrong source paths.

## Build and review an inventory

```sh
.build/repose inventory build --repo /path/to/kicad-scan
.build/repose inventory list --repo /path/to/kicad-scan
.build/repose inventory show --repo /path/to/kicad-scan
.build/repose inventory files --repo /path/to/kicad-scan --path pcbnew
.build/repose inventory files --repo /path/to/kicad-scan --status missing-command
.build/repose inventory files --repo /path/to/kicad-scan --status excluded
```

The first compilation database defaults to `build/compile_commands.json`, relative
to the checkout. Later builds inherit the current inventory's build path and
policy. Use `--compile-commands PATH` to select another build configuration.
The current commit, compilation database, and an adjacent `CMakeCache.txt` when
present identify the inventory's source and recorded build inputs.

Every tracked path remains visible, including excluded files. C/C++ sources and
headers are in scope by default; `thirdparty/`, `build/`, other languages,
symlinks, and submodule contents are excluded with reasons. QA C/C++ code is
included. Missing compilation commands do not remove sources from the map.
Headers initially lack direct compilation commands; semantic indexing records
their observed include relationships and borrowed parsing contexts separately.
Generated or other untracked compilation inputs and external inputs are counted
separately. Generated code that is tracked outside `build/` needs an explicit
policy exclusion if you want to exclude it.

All inventory commands except `browse` accept `--json`. `inventory show --json` exports the full
saved document, including commands and policy. `files` also accepts `--group`,
`--tag`, and `--status header-unmapped`; filters combine by intersection.
`header-unmapped` selects headers without direct commands in the original file
map. Use `index-status` to see their semantic coverage.

## Adjust scope

```sh
.build/repose inventory exclude qa --reason "QA is outside audit scope" --repo /path/to/kicad-scan
.build/repose inventory exclude tools --reason "Development utilities" --repo /path/to/kicad-scan
.build/repose inventory include tools/newstroke --repo /path/to/kicad-scan
.build/repose inventory exclusions --repo /path/to/kicad-scan
```

Exclusions control review assignments and reportable defects. Excluded files
remain in the map and available to reviewers and the compiler as context.
`thirdparty/` is excluded by default; QA requires an explicit rule.

These commands edit the current inventory's policy and derive a new version from
the saved map, without accessing source or build files. Existing versions and
their approvals stay unchanged. The resulting version becomes current, and its
policy carries forward to future builds and snapshots. Repeated identical edits
reuse the same version. Concurrent changes are detected instead of overwritten.

Each edit reports the changed count and previews up to 20 files; `--json` returns
the complete list. `exclusions` shows default rules, user rules, and inclusion
overrides. Matched counts cover C/C++ paths under a prefix; effective counts show
how many paths have their final scope determined by that rule. Later rules win,
so an include command can restore a file or subtree under an excluded directory.

## Navigate and curate the map

```sh
.build/repose inventory tree --repo kicad-repose --path pcbnew --depth 1
.build/repose inventory inspect --repo kicad-repose --path pcbnew/router
.build/repose inventory group pcbnew/router --name router --repo kicad-repose
.build/repose inventory annotate pcbnew/router --tag geometry --note "Check shove rollback and ownership." --repo kicad-repose
.build/repose inventory groups --repo kicad-repose
.build/repose inventory inspect --group router --repo kicad-repose
```

These examples use a `kicad-repose` checkout or symlink in the current directory.
Tree, groups, and inspect accept the same `--path`, `--group`, `--tag`, and
`--status` filters as files. Filters combine by intersection and default to
included code. Use `--status all` to see exclusions and other tracked paths.
Tree shows directories, with totals covering every selected descendant even
below the displayed depth. Inspect shows compiler coverage gaps, annotations,
the ten largest files, and matching policy rules in precedence order.

Group commands assign a name across one or more prefixes, including excluded
paths. Later overlapping group assignments win. Annotate adds tags and notes;
repeat `--tag` for multiple tags. Repeating an identical edit reuses the version.
Use policy export/import to remove or replace annotations. These are human
curation notes; they make no claim about compiler-derived relationships.

## Preview review assignments

```sh
.build/repose inventory plan --repo kicad-repose --path pcbnew/router
.build/repose inventory plan --repo kicad-repose --group router --goal "Check shove rollback and ownership." --max-files 6 --max-bytes 65536 --json > router-plan.json
```

The `files-v1` planner packs sorted files within each group and directory.
Every selected included file appears exactly once. Defaults are eight files
and 65,536 target bytes per assignment. A larger individual file remains whole
in a flagged singleton; it requires a later split or explicit size decision.
Sources missing compilation commands and headers awaiting semantic association
remain visible. Byte counts measure target files, not tokens or total model
context. This first planner does not infer include relationships or guarantee
that a source and related header land in the same assignment.

JSON records the inventory ID, observed commit, question, filters, limits, target
facts and annotations, and deterministic plan/assignment IDs. Command IDs refer
to that saved inventory. Previews work without live source/build files and do
not check checkout freshness, save a scan, or call a model. Changed questions,
inventory versions, or limits produce a new plan identity. Durable scans
freeze their own model, inputs, tasks, and coverage against the inventory.

## Index code with clangd

Requires clangd 21+; this implementation has been exercised with clangd 21.1.8
against the dedicated KiCad checkout and its GCC/CMake compilation database.

```sh
.build/repose inventory index --repo kicad-repose --path pcbnew/router --jobs 2
.build/repose inventory index-status --repo kicad-repose --path pcbnew/router
.build/repose inventory symbols --repo kicad-repose --path pcbnew/router/pns_shove.cpp
.build/repose inventory includes --repo kicad-repose --path pcbnew/router
.build/repose inventory plan --repo kicad-repose --path pcbnew/router --semantic --json > router-plan.json
```

Indexing validates the saved checkout/build inputs and uses one shared clangd
process with a bounded number of file workers (default two). It creates a private
compilation database, disables user/project clangd configuration, background
indexing and clang-tidy, and never executes saved compilation command strings.
It retains the first saved command per source; other compile variants remain
unassessed. Header commands borrow the flags of an observed source includer,
including through indexed headers. The command ID, source context, and arguments
are recorded. A header may still fail to parse alone because of include ordering.

Results contain symbols and UTF-8 byte ranges, resolved direct include links,
and compiler diagnostics. `indexed` means those requests completed without error
diagnostics; warnings may remain. `partial` retains symbols alongside errors or
unreadable dependencies. `unavailable` means a command/includer is missing, while
`failed` records an unsuccessful analysis request. Compiler diagnostics describe
index quality; they are not AI findings.

Each file result is saved immediately. Ctrl-C, timeouts, or crashes leave saved
work available; rerun the same command to resume. `--max-files N` limits further
attempts, `--timeout 2m` sets the per-file/initialization deadline, and
`--retry-errors` retries partial/failed/unavailable files. Only one index writer
can run in a checkout; inventory inspection and policy edits remain available.
`--refresh` reparses the selected scope while preserving older result versions.

Profiles identify the snapshot, build fingerprints, clangd binary/version,
settings, and relevant include-path environment. Policy edits reuse these facts.
Direct include and explicit forced-include fingerprints detect known dependency
changes on rerun. This is **not a full transitive dependency fingerprint**: after
changing generated headers elsewhere in the dependency chain, installed SDKs,
or system libraries, use `--refresh` with the affected scope. Existing build
checks do not establish generated-file freshness. Keep the scan environment fixed.

`plan --semantic` uses the latest matching profile, or `--index ID` selects one.
It partitions at saved symbol boundaries, treating functions as indivisible and
splitting large classes at member boundaries. Overlapping ranges stay together;
comments, preprocessor text, and other gaps remain targets. Partial or unindexed
files stay whole with a warning. Every selected byte is covered once. Target
ranges are zero-based with exclusive ends; they need surrounding file context
and are not promised to compile independently. Resolved repository includes
remain available as context even when excluded from review targets. Context
paths do not count toward target byte limits and are not eagerly loaded.

Plans freeze the semantic profile and per-file result IDs, so later indexing
does not change an exported plan's meaning. Status/symbol/include inspection and
planning use saved data without launching clangd or reading the checkout.
Full call/reference graphs and alternative compile variants remain subsequent
work. See [semantic indexing details](docs/semantic-inventory.md).

## Run a bounded audit

Approve the inventory version you reviewed, create a scan, inspect its frozen
prompts, then dispatch a few assignments:

```sh
.build/repose inventory approve INVENTORY_ID --repo kicad-repose
.build/repose scan create --repo kicad-repose --path pcbnew/router --model MODEL --effort high
.build/repose scan prompt SCAN_ID 1 --repo kicad-repose
.build/repose scan run SCAN_ID --repo kicad-repose --jobs 2 --limit 4
.build/repose scan show SCAN_ID --repo kicad-repose
.build/repose findings --repo kicad-repose --scan SCAN_ID
```

Creation freezes the inventory, selected plan, goal, model/effort/executable,
default per-assignment timeout, optional `--instructions FILE`, and exact prompts with
target source text. It makes no model calls. A matching saved semantic index is
used automatically; `--index ID` selects one. Scope and size flags match
`inventory plan`. Oversized assignments must be addressed before scan creation.

`--harness` supports `codex`, `claude`, and `gemini`, using their installed,
authenticated CLIs. Model is required; effort defaults to `high` (Gemini requires
`--effort default`). New scans default to 30 minutes per assignment. Codex runs in
read-only mode with web search and automatic project instructions disabled, using
the explicit frozen prompt. It ignores ambient user config/rules; authentication
still uses the existing CLI login. See the [Codex automation documentation](https://learn.chatgpt.com/docs/non-interactive-mode).

`scan run` and `scan resume` use one coordinator per checkout with 1–32 workers.
The ID is optional for `scan resume`: it selects the only scan with pending or
abandoned work in that checkout. `--retry-failed` also includes scans with failed
assignments. Completed and invalidated scans are excluded. If several scans are
eligible, it lists them and requires an explicit ID; if none are eligible, it
reports that there is no resumable work. For example:

```sh
.build/repose scan resume --repo kicad-repose --jobs 32 --limit 32
```

`--limit N` caps total assignment attempts across all workers for this invocation,
including automatic throttle retries, while retaining the scan's entire frozen
scope. It defaults to unlimited. For a single batch of up to 32 assignments:

```sh
.build/repose scan run SCAN_ID --repo kicad-repose --jobs 32 --limit 32
```

With at least 32 pending assignments, this allows 32 to run in parallel and
starts no replacement work after the batch. Failed or throttled attempts also
consume the limit; the run never adds extra retries beyond it. A later
`scan resume ... --limit 32` permits another batch of up to 32 attempts.

`--duration 30m` stops dispatching after that interval and lets active work
finish; it can exceed the interval by an assignment timeout.

`scan create --timeout DURATION` saves the scan's default per-assignment timeout.
`scan run` and `scan resume` also accept `--timeout DURATION` to override it for
that invocation, including retries. The duration must be positive. Omitting it
uses the saved timeout, including older scans' original limits; an override does
not change the saved default. New attempts record their effective timeout before
dispatch, visible in `scan attempts` and its JSON export. For example:

```sh
.build/repose scan resume SCAN_ID --repo kicad-repose --retry-failed --timeout 30m
```

```sh
.build/repose scan pause SCAN_ID --repo kicad-repose       # drain active work
.build/repose scan interrupt SCAN_ID --repo kicad-repose   # cancel active work
.build/repose scan resume SCAN_ID --repo kicad-repose --jobs 2 --limit 4
.build/repose scan attempts SCAN_ID --repo kicad-repose --json > attempts.json
```

Ctrl-C or SIGTERM cancels workers and records interrupted attempts. Resume
recovers abandoned tasks after a coordinator crash and leaves completed tasks
alone. Failed assignments require `--retry-failed`; retries retain their previous
attempts.

Codex quota exhaustion stops dispatch for the whole scan and lets healthy active
workers finish. Quota-blocked assignments return to pending, with the provider
error retained in the attempt history. `scan show` displays the provider limit;
use ordinary `scan resume` once capacity is available. Repose does not buy credits,
consume account resets, or automatically restart a quota-exhausted scan.

Temporary Codex rate limits trigger a shared cooldown. After active workers drain,
one assignment probes the provider before parallel dispatch resumes. Retries wait
30 seconds, one minute, then two minutes, or longer when the provider supplies a
retry delay. After three unsuccessful probes the scan pauses with work pending.
Saved cooldowns survive resume, all retries count toward `--limit`, and pause,
interrupt, and `--duration` remain effective while waiting. Detection uses Codex
provider error events and startup diagnostics, not source/tool output. Other
runner errors remain explicit failures; quota handling for other harnesses and
automatic account-allowance checks are not implemented yet.

Empty findings only establish coverage
when the model explicitly returns `completed`; `unable_to_assess` remains a
coverage gap. Findings must identify a line within the assignment's target ranges.
Raw output, stderr, structured responses, elapsed time, and available usage/cost
are saved with each attempt. These can be large; there is no automatic retention
pruning or token/cost dispatch budget yet.

The inherited findings TUI now displays observed snapshots, scan/assignment/
attempt provenance, and snapshot source previews. Use `/` to search, `s`/`v` to
filter status/severity, `D` to dismiss with a reason, `r` to reopen, `n` to add a
note, `t`/`u` to add/remove tags, `T` to filter by exact tags, and `R` to reload
results while a scan runs. `findings --json` exports saved findings;
`finding show|dismiss|reopen|note ID` offers single-finding CLI access.

Finding tags are lowercase labels. Simple names such as `crash` and `parser` work;
`class:ownership` or `triage:high-value` can be used as an optional naming
convention. One atomic command can edit thousands of findings and multiple tags:

```sh
.build/repose finding tag 17 23 41 --tag class:ownership --tag triage:high-value --repo kicad-repose
.build/repose finding untag 17 23 --tag triage:high-value --repo kicad-repose
.build/repose tags --scan latest --repo kicad-repose
.build/repose findings --tag class:ownership --tag triage:high-value --json --repo kicad-repose
```

Every ID and tag is validated before the edit begins, so an invalid item leaves
the entire list unchanged. Adding an existing tag or removing an absent tag is a
successful no-op. Repeated `--tag` filters use AND semantics. Tag changes appear
in finding history. Findings from separate assignments/scans retain separate
observations; automatic deduplication remains future work.

## Recheck findings with another model

`recheck` groups findings by their original review assignment and verifies them
against the original observed snapshot. `--batch-max` caps each model call at
5 findings by default. Smaller groups stay small; batches never combine findings
from different assignments. Related findings share source inspection, but each
receives an independent conclusion. It defaults to the newest **completed original
scan**, ignoring queued pilots and verification passes. Model choice is explicit:

```sh
.build/repose recheck --repo kicad-repose --model MODEL --effort xhigh --dry-run
.build/repose recheck --repo kicad-repose --model MODEL --effort xhigh --jobs 2 --batch-max 5
```

`--scan ID` selects a source scan explicitly, including a partially completed
scan with findings. Optional positional finding IDs, `--path PREFIX`, and
repeatable `--tag TAG` filters narrow the selection. Multiple tags use AND
semantics. Dismissed findings are excluded. `--create-only` saves the frozen
prompts and queue without making model calls; `scan prompt RECHECK_ID N` inspects
them. `--dry-run` neither saves work nor upgrades the database.

Repeating the same source/finding selection, model, effort, executable, batch
maximum, and prompt identity resumes its saved recheck without repeating completed
checks. A different model, selection, or batch maximum creates a separate pass.
`--force` creates a fresh pass even for the same identity; resume that pass using
its printed ID. Selection and batching are frozen when a pass is created, so later
manual dismissals do not remove queued checks. Older passes with one finding per
call retain their frozen layout and can still resume with `scan resume ID`.

Rechecks use the scan coordinator: `--jobs`, `--limit`, `--duration`, quota pauses,
shared throttle cooldowns, and `--retry-failed` behave as they do for scans.
`--jobs` counts concurrent batch calls; `--limit` counts batch attempts, including
retries. Timeout defaults to 30 minutes per batch; an explicit `--timeout` also
overrides the timeout of a resumed pass. Use `--batch-max 1` for separate calls
for every finding.

```sh
.build/repose recheck --repo kicad-repose --scan SOURCE_SCAN_ID --model MODEL --effort xhigh --jobs 32 --limit 32 --batch-max 5
.build/repose scan pause RECHECK_ID --repo kicad-repose
.build/repose scan resume RECHECK_ID --repo kicad-repose --jobs 2 --timeout 1h
.build/repose scan attempts RECHECK_ID --repo kicad-repose --json > recheck-attempts.json
```

Each finding records **confirmed**, **false_positive**, or **uncertain**, with
its own reasoning, model identity, snapshot, and attempt provenance. Uncertain is a
completed verification with inconclusive evidence, not a failed request. Original
findings and manual dispositions remain intact; verdicts do not automatically
dismiss findings. Attempt transcripts and all earlier verdicts are retained.
Every batch must return exactly one valid verdict per finding. Missing, duplicate,
or malformed results fail the batch without publishing partial verdicts; the
response is retained and `--retry-failed` retries the whole batch.

The findings TUI shows the latest verdict and reasoning, with older checks in its
history. Press `V` to filter by latest verdict and `R` to reload. CLI and JSON
inspection also expose verification history:

```sh
.build/repose findings --repo kicad-repose --scan SOURCE_SCAN_ID --verification confirmed
.build/repose finding show FINDING_ID --repo kicad-repose
```

Schema version 7 supports multiple finding verdicts per attempt and user-managed
finding tags. Use the rebuilt Repose binary after a writer upgrades the database;
older builds cannot open v7.
Existing inventories, scans, findings, attempts, and earlier verification history
are preserved by the upgrade.

## Export findings

The inherited `export` command supports JSON, SARIF, and a self-contained HTML
report that opens locally in a browser:

```sh
.build/repose export --repo kicad-repose --format html -o findings.html
.build/repose export --repo kicad-repose --scan latest --format json -o findings.json
.build/repose export --repo kicad-repose --format sarif --verification confirmed -o findings.sarif
```

`-o`/`--output` is relative to your current directory. Omit it or use `-o -` for
stdout. JSON and SARIF export open findings by default; `--all` includes dismissed
findings. HTML includes every disposition and initially displays open findings;
`--all` initially shows all of them. Its search, sorting, status, severity, and
verification filters work offline.

`--scan ID` limits the source scan; `--scan latest` selects the newest completed
original scan. A recheck ID selects findings from its source scan, including all
their verification history. Omit `--scan` to include findings across original
scans. `--path PREFIX`, `--verification VERDICT`, and repeatable exact
`--tag TAG` filters narrow the exported records;
the HTML filters cannot reveal records excluded by these command-line options.

All formats retain the observed snapshot, scan/assignment/attempt IDs, original
review model, manual history, and each recheck's independent verdict and reasoning.
JSON uses a versioned report with a `findings` array. SARIF uses Repose's tool/rule
identity and carries this history in each result's properties. The HTML report
embeds source excerpts from the observed commit and displays recheck history.
An unavailable source excerpt is reported without dropping the finding.

Export opens the database read-only and neither upgrades it nor invokes models.
It works while scans run and when the checkout differs from the observed snapshot.

## Back up and restore scan state

`backup` creates a consistent snapshot of the worktree-local Repose database:

```sh
.build/repose backup --repo kicad-repose
.build/repose backup kicad-audit-before-triage.sqlite --repo kicad-repose
.build/repose backup import kicad-audit-before-triage.sqlite --repo kicad-repose
.build/repose backup import --force kicad-audit-before-triage.sqlite --repo kicad-repose
```

With no path, the output is named
`repose-backup-YYYYMMDD-HHMMSS.sqlite` in the current directory; a numeric suffix
avoids collisions. Explicit relative paths also use the current directory. Backup
uses SQLite's online backup API, so it includes committed WAL state and can run
while a scan is active. It never overwrites a destination, checks the result, and
sets mode `0600`. Creating a backup neither upgrades the source schema nor invokes
a model.

Import accepts supported Repose schema versions and leaves the source backup
unchanged. Before replacement it checks SQLite integrity and foreign keys,
validates every inventory document, and verifies that every recorded snapshot is
available in the target Git repository. Replacing an existing database prompts
for confirmation unless `--force` is supplied. The replacement is blocked while
a scan or semantic index coordinator holds its lock, and a failed install restores
the previous database and SQLite sidecars.

## Browse in the terminal

```sh
.build/repose inventory browse --repo kicad-repose --path pcbnew/router
```

Use arrows or `j/k` to move, Enter to open, `h` to go up, `d` for details, `/` to
filter visible paths, and `a` to toggle all/included files. Press `g` to set a
group, `t` to add a tag, `n` to add a note, `x` to exclude with a reason, and `i`
to include. Enter submits an edit; Escape cancels it. Edits apply to **all paths
under the selected prefix**, including files hidden by display filters. The
`./` row selects the directory itself. `s` shows saved symbols, includes, and
diagnostics. `p` previews assignments with a question, using semantic facts when
an index is available; `r` reloads both policy and indexing results.
The CLI offers configurable limits and JSON export. `?` shows all keys.

Open the assignment browser directly with the same goal, scope, and size flags
as the CLI planner:

```sh
.build/repose inventory plan --repo kicad-repose --path pcbnew/router --tui
```

Select an assignment on the left, then use `t` for target byte ranges, `s` for
symbols, `c` for context dependencies, or `d` for parse diagnostics and plan
warnings. `Tab` switches panes; arrows or `j/k` move within a pane. Enter opens
complete, wrapped details, including symbol signatures, locations, and compiler
command provenance. `/` searches assignment IDs, groups, target paths, and
symbols. `?` shows the full goal, plan/assignment IDs, limits, and help. On narrow
terminals, `Tab` switches between the assignment list and its items.

Symbol lists show declarations overlapping the assignment's target bytes;
enclosing namespaces and classes are labeled when they extend outside the target.
Context details retain inclusion reasons and exclusions. Diagnostics describe
file parsing and may originate outside the selected target range. Escape returns
from details; `q` closes the plan (returning to inventory when opened with `p`).

`--tui` uses a matching saved semantic index when available, falling back to whole
files otherwise. `--semantic` requires an index and `--index ID` selects one.
The plan and index results stay fixed while browsing; reopen to pick up new
indexing results. `--tui` cannot be combined with `--json`. Browsing a plan does
not save or start a scan.

The default/current inventory is editable. An explicit ID or `latest` opens
read-only. Browsing opens read-only connections; saving opens a temporary writer
and checks that the current inventory has not changed. After a concurrent edit,
`r` reloads current so you can inspect it and retry. Old versions and approvals
remain intact. A scan checkout/build change does not prevent browsing saved facts.

## Edit the policy in bulk

```sh
.build/repose inventory policy export --repo /path/to/kicad-scan > policy.json
# Edit policy.json, including group names, exclusions, tags, and notes.
.build/repose inventory policy import policy.json --repo /path/to/kicad-scan
```

Export and import use the same saved rules as the scope commands. Import replaces
the entire policy and derives a version without rebuilding compiler facts.
Export accepts an inventory ID when you want to restore an earlier policy.

Copy [the example policy](docs/inventory-policy.example.json) and edit it for your
checkout. Rules use literal repository-relative file or directory prefixes;
`.` matches every path. Rules apply in order. Group and exclusion settings use
the last explicit value; tags and notes accumulate. Exclusions require a reason.
`"exclude": false` can re-include C/C++ code excluded by an earlier/default rule.
New scope commands and explicitly imported policies reject unmatched prefixes.
Inherited rules whose paths disappear in a later snapshot are retained and
reported as unmatched, so they also apply if the paths return.

```sh
.build/repose inventory build --repo /path/to/kicad-scan --policy ./policy.json
.build/repose inventory show INVENTORY_ID --repo /path/to/kicad-scan
.build/repose inventory approve INVENTORY_ID --repo /path/to/kicad-scan
.build/repose inventory check INVENTORY_ID --repo /path/to/kicad-scan
```

`build --policy FILE` replaces the saved policy after a successful build. Policy
paths are relative to your current directory. Each distinct inventory has
an immutable content-derived ID; rebuilding identical inputs reuses the saved
version. Policy changes create a new version requiring its own review. Approval
records your review of a specific ID and does not run a model. ID prefixes of at
least four hexadecimal characters are accepted when unambiguous. Inspection
defaults to `current`, which may be an older version after restoring a policy.
`latest` explicitly selects the most recently created version. Approval requires
an explicit ID. `list` marks the current version.

State is stored in `<worktree-git-dir>/repose/repose.sqlite`, separately from AIR
and from other linked worktrees. Building, checking, and approving require no
tracked changes. Untracked files are outside the source inventory. Reviewing a
saved inventory remains possible after the checkout changes. Inspection opens
SQLite read-only and never migrates its schema. The first write with this release
upgrades existing storage while preserving inventories and their approvals.
SQLite WAL/SHM sidecars are retained for inspection from read-only directories;
older databases without those sidecars need one writable open first.

## License

[GNU Affero General Public License, version 3](LICENSE) (`AGPL-3.0-only`).
