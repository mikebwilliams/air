# Repose

Repose is a local tool for sustained, parallel AI audits of a fixed C/C++
repository snapshot. Its primary target is KiCad. An inventory can support
multiple scans with different models, questions, and selected areas of code.

Repose implements **inventory building, curation, assignment previews, and a
terminal browser**. Model scanning, the durable parallel queue, and clangd
symbol/reference indexing are upcoming.
The [design and implementation sequence](docs/repose-design.md) records their
contracts and the current boundaries.

Repose is forked from AIR. AIR's model runners, reporting, and triage code remain
available in the source for adaptation. The Repose executable exposes the new
inventory workflow. The [original README](docs/air-readme.md) and
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
Headers remain awaiting semantic association until the indexing milestone.
Generated or other untracked compilation inputs and external inputs are counted
separately. Generated code that is tracked outside `build/` needs an explicit
policy exclusion if you want to exclude it.

All inventory commands except `browse` accept `--json`. `inventory show --json` exports the full
saved document, including commands and policy. `files` also accepts `--group`,
`--tag`, and `--status header-unmapped`; filters combine by intersection.

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
inventory versions, or limits produce a new plan identity. Durable scans will
freeze their own model, inputs, tasks, and coverage against the inventory.

## Browse in the terminal

```sh
.build/repose inventory browse --repo kicad-repose --path pcbnew/router
```

Use arrows or `j/k` to move, Enter to open, `h` to go up, `d` for details, `/` to
filter visible paths, and `a` to toggle all/included files. Press `g` to set a
group, `t` to add a tag, `n` to add a note, `x` to exclude with a reason, and `i`
to include. Enter submits an edit; Escape cancels it. Edits apply to **all paths
under the selected prefix**, including files hidden by display filters. The
`./` row selects the directory itself. `p` previews assignments with a question;
the CLI offers configurable limits and JSON export. `?` shows all keys.

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
