# clonezip

`clonezip` clones Git repositories from **Azure DevOps, GitHub, Codeberg and
Bitbucket** as *bare* repos, fetches the Git-LFS objects referenced by every
branch and tag tip so each archive works offline, and zips them. It can restore a
single archive back into a bare repo plus a ready-to-use working clone, and it
can run scheduled backups unattended with a monitoring web page.

```
clonezip <command> [flags]
```

---

## Features


- a service mode for unattended scheduled backups, and running a web dashboard to watch progress, history and schedule, and to trigger backups on demand
- a CLI mode for one-off runs, scripting, and automation.
- __Multiple remote hosts.__ Back up repositories from __Azure DevOps__ (`dev.azure.com`, `*.visualstudio.com`, and self-hosted Azure DevOps Server), __GitHub__ (`github.com`), __Codeberg__ (`codeberg.org`) and __Bitbucket Cloud__ (`bitbucket.org`) in the same group. GitHub and Codeberg repos have no project, so their archive is written straight into the output directory rather than a `<project>/` subfolder; Bitbucket repos nest under their workspace (`<out>/<workspace>/<repo>.git.zip`).
- __File-based config.__ One YAML file lists every group, its repo list and cron schedule, and the defaults applied to any group field left unset.
- __Groups overview.__ One page lists every backup group with its schedule, last/next run and pass/warn/fail summary.
- __Disk usage bar.__ A single bar shows the current on-disk size of the backup root (the config's top-level `out`) against the free space left on its volume - the whole of every backup, measured as one directory.
- __Group details.__ Per-group page with its resolved configuration, repository list and full run history.
- __Run on demand.__ Trigger a single group, or every group, outside its cron schedule with one click.
- __Live progress.__ Watch an in-progress run's log tail and per-repository clone progress update in real time.
- __Create, edit, delete groups.__ Manage the config file's groups from the dashboard, including a cron schedule preview and presets, without hand-editing YAML.
- __Restore from the dashboard.__ Pick a group, pick one of its backed-up repositories, set the restore options and an output directory, and watch the restore's log and result without leaving the browser.
- __Run history.__ Every past run per group, with trigger, duration, ok/warn/failed counts, disk usage and output path.
- __Light/dark theme.__ Toggle the dashboard's color theme; the choice is remembered across visits.

## Global flags

These persistent flags are accepted by every command:

| Flag | Description |
|------|-------------|
| `--no-tui` | Plain line output instead of the interactive view. |
| `--verbose` | Include raw git output in the log stream. |
| `--ascii` | Force ASCII status words instead of unicode glyphs. |
| `--unicode` | Force unicode glyphs. (Mutually exclusive with `--ascii`.) |
| `--version` | Print `clonezip version <v>` and the copyright notice, then exit. |
| `-h`, `--help` | Help for `clonezip` or any subcommand. |

### Exit codes

The exit status distinguishes "nothing was attempted" from "some work failed" so
a scheduled task can react correctly:

| Code | Meaning |
|------|---------|
| `0` | Completed, zero failures (warnings allowed). |
| `1` | Completed, but at least one repository / check failed. |
| `2` | Usage, repo-list validation, or startup-check failure — nothing was attempted. |
| `3` | Precondition failure (not a zip, target directory already exists, shallow archive needs confirmation). |
| `130` | Aborted by Ctrl-C. |

---

## Commands

### `clonezip backup <repolist.yaml>`

Clone every repository in a list and zip each as a bare archive.

Reads a YAML file of repository URLs, clones each one as a bare repository,
fetches the Git-LFS objects referenced by every branch and tag tip (so the
archive works without the LFS server), and writes
`<out>/<project>/<repo>.git.zip`.

* Bitbucket URLs use the *workspace* as the project.
* GitHub and Codeberg URLs have no project, so their archive is written straight
  to `<out>/<repo>.git.zip`.
* Lines that are not URLs are treated as decorative labels and skipped.
* An existing archive is overwritten.
* Before anything is cloned, the git / git-lfs / disk / auth checks run (one
  auth request per distinct host) unless `--skip-checks` is given.

**Repo-list format** — a single `repos:` key holding a sequence of clone URLs.
Grouping and annotation are left to YAML's own `#` comments and blank lines;
`clonezip` never reads a comment. An entry whose value itself starts with `#` is
treated as a *disabled* repo and skipped.

```yaml
repos:
  # Azure DevOps
  - https://company@dev.azure.com/company/project1/_git/repo1
  # GitHub
  - https://github.com/user/project2.git
```

| Flag | Default | Description |
|------|---------|-------------|
| `--out <dir>` | `./<yyyyMMdd-hhmmss>-<repolist name>` | Output directory. |
| `--stage <dir>` | `<out>/.stage` | Staging directory for clones. |
| `--clone-depth <n>` | `0` (full) | Truncate history to N commits per ref. A depth-limited backup is **lossy**. |
| `--jobs <n>` | `1` | Repositories to process concurrently. |
| `--timeout <dur>` | `30m` | Per-repository timeout. |
| `--retries <n>` | `2` | Retries per repository on transient network errors. |
| `--dry-run` | off | Resolve and validate every entry (and detect duplicate/colliding archive paths) without cloning. |
| `--fail-fast` | off | Stop the run on the first failed repository. |
| `--skip-checks` | off | Skip the git / lfs / disk / auth pre-flight checks. |
| `-i`, `--interactive` | off | Prompt for the options with a form before running. |

A summary is always printed at the end, including after Ctrl-C.

---

### `clonezip restore <archive.git.zip>`

Restore one archive into a bare repository, optionally with a working clone.

Extracts an archive produced by `backup` into `<repo>.git`, which holds every
backed-up ref and is what you push to a new remote.

* With `--clone` it also creates a working copy next to it, seeds the LFS object
  cache from the bare repo so LFS-tracked files become real content, and points
  `origin` back at the repository's original clone URL.
* Accepts `.git.zip` and the legacy `.git.7z` archives written by the old
  PowerShell scripts (which were ZIP containers despite the extension).
* Refuses to overwrite an existing `<repo>.git`, or an existing `<repo>` when
  `--clone` is given, unless `--force` is used.
* By default runs `git fsck` and a manifest cross-check to catch a truncated
  backup before you rely on it.

| Flag | Default | Description |
|------|---------|-------------|
| `--dest <dir>` | `.` | Directory to restore into. |
| `--clone` | off | Also clone a working copy from the restored bare repository. |
| `--origin <what>` | `source` | With `--clone`, what `origin` points at: `source` (the original clone URL) or `bare` (the local bare repo). |
| `--force` | off | Replace an existing `<repo>.git`, or `<repo>` with `--clone`. |
| `--confirm-shallow` | off | Proceed with restoring a shallow / truncated archive. |
| `--no-verify` | off | Skip `git fsck` and the manifest cross-check. |
| `--skip-checks` | off | Skip the git / lfs / disk checks. |
| `-i`, `--interactive` | off | Prompt for the destination and flags with a form before running. |

`--origin` only applies together with `--clone`.

---

### `clonezip doctor`

Check that git, git-lfs and the environment are ready.

Reports whether everything `clonezip` needs is in place: git, git-lfs and its
filters, writable output with enough free space, and the platform specifics that
bite during long runs. Every check is local **except** the authentication probe,
which needs a URL to connect to — pass `--probe`.

7-Zip is reported for information only; `clonezip` writes ZIP archives itself and
never shells out to it.

| Flag | Default | Description |
|------|---------|-------------|
| `--probe <list-or-url>` | none | Repo list or single URL to test authentication against (one request per distinct host). Omitted means the auth check is skipped. |
| `--out <dir>` | `.` | Directory to check for writability and free space. |
| `--json` | off | Emit machine-readable results. |
| `--strict` | off | Treat warnings as failures. |

A failing check is reported through the exit code (`1`), not as an error — doctor
did its job either way.

---

### `clonezip service`

Run scheduled backups in the background with a monitoring web page.

Reads a service config file (`clonezip-service.yaml` by default) listing named
backup groups, each with its own repo list and cron schedule, and runs them
unattended: firing groups on their schedule, and serving a web dashboard at
`--listen` to watch progress, schedule and run history, edit the config, trigger
a group (or all groups) on demand, and restore an archive.

`clonezip service` runs in the **foreground**; use systemd, a Windows Service or
Task Scheduler to keep it running in the background. Only one service process may
run per config file (enforced with a `.lock` file next to it).

| Flag | Default | Description |
|------|---------|-------------|
| `--config <path>` | `clonezip-service.yaml` | Path to the service config file. |
| `--listen <addr>` | from config | Override the listen address for this run only (never written back to the config). |
| `--check-config` | off | Validate the config file, print a one-line summary, and exit without starting the server. |

An invalid config lists every problem and exits `2`.

#### `clonezip service init`

Write a starter `clonezip-service.yaml` config file.

Writes the bundled template to disk. An existing file is **never** overwritten —
edit it, or remove it first, then run `init` again.

| Flag | Default | Description |
|------|---------|-------------|
| `--config <path>` | `clonezip-service.yaml` | Path to write the config file to. |
| `-i`, `--interactive` | off | Prompt for the service settings and per-group defaults with a form first; the example groups are still written for you to edit afterwards. |

---

### `clonezip migrate <old.txt> <new.yaml>`

Convert a legacy plain-text repo list into the current YAML format.

Reads a plain-text repo list (one clone URL per line, with optional decorative
label lines) and writes the equivalent YAML list read by `backup` and `service`.

* Decorative labels become YAML comments placed above the URLs that followed
  them.
* Blank lines and existing `#` comments are dropped, since YAML carries its own
  comment syntax.
* The result is parsed back immediately, so a bad line is caught here rather than
  at the first real backup run.

Takes exactly two positional arguments and no flags.

---

## `clonezip-service.yaml`

The configuration file read by `clonezip service`. All relative paths in it are
resolved against the **directory the config file lives in**, not the process's
working directory — which matters once the service is launched by systemd or a
Windows Service with its own working directory.

Generate a starter file with `clonezip service init`.

### Top-level keys

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `title` | string | — | Optional label shown in the dashboard header, for telling multiple instances apart. |
| `dateFormat` | BCP-47 locale tag | `en-US` | Locale the dashboard uses to render run timestamps (passed to the browser's `toLocaleString`), e.g. `de-DE`. |
| `listen` | `host:port` | `:8091` | Address the dashboard binds to. Prefer the bare `:8091` form over `0.0.0.0:8091` — the latter is IPv4-only, and browsers resolve `localhost` to `::1` first. |
| `allowActionsFrom` | list of CIDR blocks / bare IPs | loopback only (`127.0.0.0/8`, `::1/128`) | Client networks allowed to **change** anything: create / edit / delete groups, edit settings, trigger runs and restores. Every other client gets the dashboard read-only (non-GET → `403`). A bare IP means a single host. Deliberately **not** editable through the dashboard. |
| `out` | path | `backups` | The backup **root**. Every group writes under it; a group's own `out`/`stage` is resolved relative to it and may never climb above it, so a stray `../` or absolute path in a group config cannot write elsewhere. |
| `defaults` | mapping | — | Per-group defaults, applied to any group field left unset. See below. |
| `groups` | list | — | The scheduled backup groups. At least one is required. See below. |

> **Note:** `defaults.out` is no longer supported. If present, the service
> refuses to start and points you at the top-level `out:` key.

### `defaults`

Each value here is the fallback for the same-named field on any group that
doesn't set it.

| Key | Type | Meaning |
|-----|------|---------|
| `cloneDepth` | int | Commits per ref to keep (`0` = full clone; a depth limit is lossy). |
| `jobs` | int | Repositories cloned concurrently (coerced to at least `1`). |
| `retries` | int | Retries per repository on transient network errors. |
| `timeout` | duration | Per-repository timeout, e.g. `30m0s` (non-positive → `30m`). |
| `failFast` | bool | Stop a group's run on its first failed repository. |
| `skipChecks` | bool | Skip the git / lfs / disk / auth pre-flight checks for the group. |
| `historyLimit` | int | How many past runs each group keeps — in memory and as `clonezip-*.log` files under its `out` directory (`< 1` → `50`). |

### `groups`

A list of scheduled backup groups. Each entry:

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| `name` | string | yes | Unique, non-empty group name. Also the default `out` sub-directory. |
| `repos` | list of clone URLs | yes (≥ 1) | The repositories to back up. Same rules as a standalone repo list: non-URL lines are ignored; an entry starting with `#` is a disabled repo. Every URL is validated. |
| `schedule` | standard cron expression | yes | When the group fires, e.g. `0 1 * * *` (daily at 01:00). Parsed with the standard 5-field cron syntax. |
| `enabled` | bool | no (default `true`) | A disabled group is never fired on its schedule, but can still be triggered from the dashboard. |
| `out` | path | no (default: the group `name`) | Output directory, resolved **under the backup root**; may not climb above it. Group `demos` → `<root>/demos`. |
| `stage` | path | no (default `<out>/.stage`) | Staging directory for clones, also confined to the backup root. |
| `cloneDepth` | int | no | Overrides `defaults.cloneDepth`. |
| `jobs` | int | no | Overrides `defaults.jobs`. |
| `retries` | int | no | Overrides `defaults.retries`. |
| `timeout` | duration | no | Overrides `defaults.timeout`. |
| `failFast` | bool | no | Overrides `defaults.failFast`. |
| `skipChecks` | bool | no | Overrides `defaults.skipChecks`. |
| `historyLimit` | int | no | Overrides `defaults.historyLimit`. |

The override fields are only applied when present, so an explicit `false` / `0`
stays distinguishable from "not set, use the default".

### Validation

`clonezip service` (and `--check-config`) rejects the config, listing every
problem at once, if:

* an `allowActionsFrom` entry is not a valid IP or CIDR block;
* a group has no name, or two groups share a name;
* a group has no repository URLs, or one of them is unparseable / not a
  recognized clone URL;
* a group has no `schedule`, or its cron expression does not parse;
* a group's `out` or `stage` resolves outside the backup root.

### Example

```yaml
title: My Backups
dateFormat: en-US
listen: :8091
allowActionsFrom:
    - 127.0.0.0/8
    - ::1/128
out: ./backups

defaults:
    cloneDepth: 1
    jobs: 4
    retries: 2
    timeout: 30m0s
    failFast: false
    skipChecks: false
    historyLimit: 10

groups:
    - name: AzureDevOps - Project
      repos:
        - https://<company>@dev.azure.com/<company>/project1/_git/subrepo1
        - https://<user>@dev.azure.com/<user>/project2/_git/subrepo2
      schedule: 0 1 * * *
      enabled: true
      out: ./azuredevops
    - name: GitHub - Private
      repos:
        - https://github.com/<user>/project1.git
      schedule: 0 1 * * *
      enabled: false
      out: ./github/private
    - name: Codeberg - public
      repos:
        - https://codeberg.org/<user>/project2.git
      schedule: 0 1 * * *
      enabled: true
      out: ./codeberg/public
```
