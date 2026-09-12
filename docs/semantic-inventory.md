# Semantic inventory

`inventory index` enriches a fixed file/build inventory with compiler information.
The first integration uses one local clangd process and a bounded set of workers,
with sources processed before headers. The default is two workers. The dedicated
checkout and build environment must stay stable throughout an indexing run.

## Persistence and reuse

Repose schema version 3 adds `semantic_profiles`, append-only `semantic_results`,
and a `semantic_current` pointer per profile/path. Opening a writer migrates older
Repose databases; read-only inspection accepts versions 1 through 3 and does not
migrate them. Existing inventory documents and approvals are unchanged.

A profile includes the snapshot/worktree/build identity, clangd executable hash
and version, fixed settings, selected environment variables, and the policies for
command selection and dependency recording. Grouping, exclusions, and review
notes do not participate in this identity. They are applied when selecting index
work and generating a plan. Different questions and models can reuse the same
compiler facts.

Completed file results are content-addressed and immutable. Publishing a result
and updating its current pointer is one transaction. There is no long-running
database transaction around a parse. A separate process lock prevents competing
index coordinators, while CLI/TUI readers and inventory policy writers continue
to operate. Interrupted requests are retried on resume; already published results
remain intact. Failed and partial results require `--retry-errors` or `--refresh`,
unless recorded dependencies have changed. Previously unavailable headers become
eligible when a broader indexing run discovers an includer.

The chosen scope is a dispatch selection, not a stored scan or coverage promise.
`--max-files` limits additional attempts. Indexing work has no model identity,
review question, or finding state. The durable model queue is a separate milestone.

## Compiler protocol and provenance

The client uses LSP `initialize`, `didOpen`/`didClose`, `documentSymbol`,
`documentLink`, and versioned `publishDiagnostics`. It negotiates UTF-8 positions,
and waits for the opened document's diagnostics before publishing a result.
These positions are converted to absolute byte offsets and validated against the
source contents. The source text must match its committed Git blob.

The exact command is supplied through clangd's
[compilation database protocol extension](https://clangd.llvm.org/extensions#compilation-commands).
Source files use the first saved compilation command. Headers borrow a command
from an observed includer and replace the source input with the header, with an
explicit header language. The source command ID remains attached to the result.
Multiple source variants and full include-order context are not represented by
this first header parse. Errors such as types supplied by an earlier include
remain visible as partial results.

User and project configuration are disabled to avoid implicit changes outside
the recorded profile. See clangd's
[configuration behavior](https://clangd.llvm.org/config#loading-and-combining-fragments)
and [compile-command design](https://clangd.llvm.org/design/compile-commands).
Saved command strings are tokenized as data, with no shell expansion or execution.
Driver querying is not enabled. The existing GCC/CMake precompiled-header wrapper
is retained in the KiCad command; clangd builds its own parsing preamble.

Direct include links and explicit forced includes are fingerprinted. These are
observations from the parsed configuration, not a complete transitive include
graph. A successful result does not certify every compile variant, generated
artifact, SDK, or system library. Keep the environment fixed; refresh affected
scope after external/transitive changes. No AI finding is created from a compiler
diagnostic.

## Planning and inspection

`index-status`, `symbols`, and `includes` read saved results. Their JSON exports
include provenance, ranges, diagnostics, and dependency fingerprints. Human text
uses one-based line numbers; JSON LSP positions and byte offsets are zero-based.
`--index ID` selects a profile belonging to the inventory's snapshot/build.
Otherwise the latest matching profile is selected, including one still building.

`plan --semantic` uses `symbols-v1`. It freezes the profile and selected result
IDs, splits targets at symbol boundaries, and retains files with incomplete
indexing as whole-file targets with warnings. Functions remain indivisible;
large namespaces/classes can be divided between their children. Overlapping
symbol extents are merged before partitioning. Source gaps, inactive code,
comments, and preprocessor text are retained, giving complete nonoverlapping
byte coverage. Adjacent target pieces in one assignment are coalesced for display.

Repository include paths are supplied as context, including excluded dependencies.
Target byte/file limits do not budget that context. The future scanner must decide
what context to retrieve and enforce its own model budget. A fragment may depend
on surrounding namespace/class declarations; a byte range is not a standalone
translation unit. Full call/reference graphs and additional build configurations
remain future enrichment work.

The inventory browser's `s` key displays saved semantic facts. `p` opens an
assignment browser and chooses semantic planning when a matching profile is
available; `inventory plan --tui` opens this view directly with CLI scope and size
options. Assignments expose target ranges, overlapping symbol declarations,
context paths with inclusion reasons, and file-level parse diagnostics. Enclosing
symbols that extend beyond a target range are explicitly labeled. Full item
details wrap long signatures and show the saved result and command provenance.
The view holds the exact semantic snapshot used to plan; later indexing does not
change its labels. Reopen the plan to see newer results (the inventory browser's
`r` reloads index results). Browsing starts neither clangd nor a model. Index
construction remains an explicit CLI operation.
