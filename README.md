# AIR - AI Reviewer

## WARNING

This is the only human-written text or code in this whole project. You have been
warned. I have not reviewed the code, nor do I intend to. I have tested this only
for my own use cases. 

![AIR findings terminal interface](screenshot.png)

## Overview

AIR is a local AI code reviewer for the stream of commits landing on a
project's primary branch (`master` or `main`). It reviews code after commits
land and keeps a running list of findings you can revisit as the project
evolves.

AIR is not a pre-commit checker, a pull-request or merge-request reviewer, or an
approval gate. It complements those workflows with ongoing, after-the-fact
review.

Use AIR from the command line, browse and triage findings in its terminal UI,
or export results as JSON, SARIF, or a self-contained HTML report. The current
release reads commits from `master`; the same workflow is intended for `main`.

## Quick start

Run AIR from the repository you want to review:

```bash
air init HEAD~10
air config set model gpt-5.6-luna
air config set effort xhigh
air scan
air findings
```

The starting commit is not reviewed, so this example scans the ten commits
after it on `master`. Run `air scan` again whenever new commits land; AIR skips
commits it has already reviewed or intentionally skipped.

AIR carries open findings forward as it scans. If a later commit fixes one, AIR
recognizes the fix and marks the finding resolved automatically.

Before pushing, review every local `master` commit not yet in its configured
upstream, or review only the staged index:

```bash
air precheck --fail-on warning
air precheck --staged --fail-on warning
```

Prechecks print provisional results and never change AIR's database. They can
also produce JSON, SARIF, or a self-contained HTML report with `--format` and
write it directly with `-o`, for example:

```bash
air precheck --format html -o air-precheck.html
```

Codex is the default review harness. AIR can also use an already authenticated
Claude Code or Gemini CLI. Run `air help` or `man air` for the complete command
reference.

## Build requirements

- Go 1.26 or newer
- Git
- A C compiler for SQLite

## Build and install

```bash
make
make test
sudo make install
```

This installs the `air` executable and its manual page under `/usr/local`.

## License

AIR is licensed under the [GNU Affero General Public License, version 3](LICENSE)
(`AGPL-3.0-only`).
