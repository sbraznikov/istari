# Setting up istari

This guide takes you from nothing to a first draft pull request made by istari. Each step gives the command and what you should see, so you, or an agent in a terminal session, can check a step before going on.

What works today (milestones M1–M3): tickets synced from GitHub Issues, the task queue and the runner, a git worktree per ticket, and the refine, plan and implement stages up to a draft pull request. Not built yet: Slack (M4), the TUI (M5), and the review, approval and merge stages (M6). The [design spec](design.md) describes the full system; this guide describes what the binary does now.

Commands below use `--config` only where it matters. Without it, istari reads `~/.config/istari/config.toml`.

## 1. Prerequisites

istari needs these on the machine that runs the daemon:

| Tool | Why |
|---|---|
| Go 1.27.3 or newer | to build istari |
| `git` | worktrees, commits, pushes |
| `gh`, logged in | the tracker reads and writes issues with the token of `gh auth token` |
| `claude` (Claude Code) | the harness that runs the agents |

Check them:

```sh
go version && git --version && gh --version && claude --version
```

## 2. Install

Either install the module:

```sh
go install github.com/sbraznikov/istari/cmd/istari@main
```

or build from a checkout:

```sh
git clone https://github.com/sbraznikov/istari && cd istari
go build -o ~/bin/istari ./cmd/istari
```

If your Go is older than 1.27.3, `go install` downloads the newer toolchain itself:

```
go: github.com/sbraznikov/istari@v0.0.0-... requires go >= 1.27.3; switching to go1.27.4
```

Check the binary:

```sh
istari --version
```

You should see:

```
istari dev
```

Run the daemon from a stable build at a fixed path. `istari daemon install` writes the path of the binary it runs from into the service unit, and the daemon passes its own path to every agent run as the git guard hook. Do not run the daemon with `go run`.

## 3. GitHub access

### The gh CLI (always needed)

The ticket tracker always uses the token of `gh auth token`, with or without a GitHub App.

```sh
gh auth login
gh auth status
```

You should see a line like:

```
  - Token scopes: 'gist', 'read:org', 'repo', 'workflow'
```

The token needs `repo`: istari reads issues, sets labels (`istari:<state>`, `istari:hold:<reason>`, `size:…`) and writes comments. If tickets will change files under `.github/workflows/`, GitHub also requires the `workflow` scope for the push (`gh auth refresh -s workflow`).

istari's clone of a repository is fetched with plain `git` from `https://github.com/<owner>/<repo>.git`. For a private repository, git must be able to authenticate there, for example with `gh auth setup-git`.

Without a GitHub App, pushes and pull requests are also made with the gh token, as your user. `istari doctor` says so (step 8).

### A GitHub App (optional)

With a GitHub App, pushes, pull requests and check reads use short-lived installation tokens that only the daemon holds. Create an App, install it on the repositories istari works on, and download its private key. The App needs these repository permissions, derived from the API calls istari makes:

| Permission | Access | Used for |
|---|---|---|
| Contents | Read and write | pushing ticket branches |
| Pull requests | Read and write | opening and updating the draft pull request |
| Checks | Read | following CI on the pull request |
| Commit statuses | Read | following CI on the pull request |
| Metadata | Read | required by GitHub |

Add it to the config file (step 4):

```toml
[github_app]
app_id      = 123456
private_key = "~/.config/istari/istari-app.pem"
```

istari looks up the installation per repository; there is no installation id to configure. A missing or unreadable key makes `istari doctor` fail:

```
vcs github           FAIL    github app: the private key: open /path/to/missing.pem: no such file or directory
```

For GitHub Enterprise, set `GITHUB_API_URL` (API) and `GITHUB_SERVER_URL` (git) in the daemon's environment.

## 4. The config file

The config lives at `~/.config/istari/config.toml`; `--config <path>` on any command points elsewhere. A missing default file means all defaults. `istari init` (step 5) creates the file and adds the project, so you can start with nothing, or with this:

```toml
[daemon]
data_dir = "~/.local/share/istari"
```

Keep `data_dir` short. The daemon's unix socket is `<data_dir>/istari.sock`, and a socket path longer than about 100 characters fails on macOS:

```
istari: listen …/data/istari.sock: listen unix …/data/istari.sock: bind: invalid argument
```

Optional keys. The values shown are the defaults, except where a comment says "example". Unknown keys are an error.

```toml
[daemon]
data_dir            = "~/.local/share/istari"
max_concurrent_runs = 4          # claimed tasks at once in the installation

[defaults]
max_review_rounds = 3
max_attempts      = 3            # attempts per task before the attempts_exhausted hold
plan_gate         = false        # true: a human approves the plan; a repository's pipeline.toml overrides it (step 7)
priority          = "P2"         # for tickets that set none
zones             = "serialize"  # serialize | warn | off

[defaults.budget_usd]            # the budget of a ticket, by size (step 12)
XS = 2
S  = 5
M  = 15
L  = 30

[defaults.max_concurrent_runs_by_role]   # a role not listed has no limit of its own
implementer = 1                  # example

[[project]]
name    = "demo"                 # example
repo    = "example/demo"         # example
trusted = ["your-login"]         # example; whose tickets are admitted without a human; absent: everyone with write access
max_concurrent_runs = 2          # example; absent: no limit of its own
```

`[defaults.merge]`, `[slack]`, `[tui]`, `[report]` and `[[project.repos]]` are parsed and validated too, but merging, Slack and the TUI are not built yet.

## 5. `istari init`

Run it in a checkout of the repository istari will work on. The repository needs an `origin` remote on GitHub.

```sh
cd ~/src/demo
istari init
```

You should see:

```
registered project demo (example/demo) in /Users/you/.config/istari/config.toml
created .istari/pipeline.toml
created .istari/ticket-standard.md
created .istari/ticket-standard.toml
created .istari/roles/implementer.md
created .istari/roles/planner.md
created .istari/roles/refiner.md
created .istari/roles/reviewer.md
created .istari/skill.md
```

and in the config file:

```toml
[[project]]
name = "demo"
repo = "example/demo"
```

The project is named after the repository; `--name` chooses another. Files that exist are kept. Running it again prints:

```
example/demo is already part of project demo
```

Review the files, then commit and push `.istari/` to the main branch. istari reads the checks from the main branch's `.istari/pipeline.toml`, not from your working copy.

### More repositories: `istari repo add`

A project can span several repositories. From the primary repository's checkout:

```sh
istari repo add ../demo-core
```

istari detects how the two are linked (package, submodule, path or none), proposes `done_when` (`merged` or `released`) and asks you to accept. Without a terminal, pass `--yes` to accept the proposal or `--done-when merged|released` to choose. `istari repo list` and `istari repo remove <owner/repo>` show and remove repositories.

## 6. The ticket standard

A ticket is a GitHub issue. Code checks its structure against `.istari/ticket-standard.toml`; the refiner judges its quality against `.istari/ticket-standard.md`. By default a ticket has:

- the sections `## Context`, `## What needs to be done`, `## Acceptance criteria`, `## Out of scope`, `## Dependencies`, none empty;
- a type (`feature`, `bug`, `chore`, `epic`), a size (`XS`, `S`, `M`, `L`, `XL`) and a priority (`P0`–`P3`), either as labels `type:…`, `size:…`, `priority:…` or in a table at the end; a missing priority takes `[defaults] priority`;
- the repository (`owner/repo`) in the table's Repository column;
- dependencies on a line `- Needs: #12, gh:acme/core#57` under Dependencies; each must exist and not be canceled, and they must not form a cycle;
- paths in backticks that exist in the repository, unless marked `(new)`.

An example, `ticket.md`:

```markdown
# Print a greeting

## Context

The demo has no entry point yet, so there is nothing to run.

## What needs to be done

- Add `main.go` (new) with a `main` function that prints `hello`.
- Add a test that runs `main` and checks the output.

## Acceptance criteria

- [ ] `go run .` prints `hello`.
- [ ] `go test ./...` passes.

## Out of scope

- Command-line flags.

## Dependencies

- Needs: none

---

| Type | Size | Priority | Repository |
|---|---|---|---|
| feature | XS | P2 | `example/demo` |
```

Check it before you create it. Run this in the repository's checkout, so the repository's standard and paths are used:

```sh
istari ticket check ticket.md
```

You should see:

```
the ticket meets the standard
```

A ticket that falls short lists each deviation and exits 1:

```
RULE     DEVIATION
section  the section "Out of scope" is missing
size     size "XXL" is not one of XS, S, M, L, XL
path     `cmd/demo/main.go` does not exist in the repository (mark a file the ticket creates with (new))
istari: 3 deviation(s) from the ticket standard
```

`istari ticket check` also takes an issue: `#12` in the repository's checkout, or a full ref `gh:owner/repo#12`. It never changes anything.

## 7. The pipeline file

`.istari/pipeline.toml`, written by `istari init`, sets the stages and the checks of a repository. The checks are commands istari runs itself; an agent's claim that tests pass is never trusted. For a Go module, init proposes:

```toml
[checks]
typecheck = "go vet ./..."
test      = "go test ./... -short"
full      = "go test ./..."
verify_red = true
```

- `typecheck` and `test` run at every `istari checkpoint` the implementer makes.
- `verify_red`: the new tests of a checkpoint must fail on the code of the previous checkpoint.
- `full` gates `istari pr open`: if it fails, the pull request is not opened. Without `full`, `test` is used.

For other languages init proposes npm or pytest commands, or leaves them commented out; fill them in.

The file also lists the stages. Its `plan` stage carries `gate = false`, which overrides `[defaults] plan_gate`: to have a human approve each plan in this repository, set `gate = true` there. In `[limits]`, `max_attempts`, `max_review_rounds`, `max_size` and `self_review` take effect. `questions` and `max_assumptions` are read but not used yet.

## 8. The harness and `istari doctor`

The only harness is Claude Code. istari needs `claude` on the daemon's `PATH` and runs `claude --version` to check it. Agents run with these models and tools:

| Role | Model | Permission mode |
|---|---|---|
| refiner, planner, reviewer | `claude-opus-5-5` | `dontAsk`, read-only tools and `istari` |
| implementer | `claude-opus-5-5` | `acceptEdits`, edits, `git` and `istari` |

Run doctor in the repository's checkout:

```sh
istari doctor
```

You should see:

```
CHECK                           STATUS  DETAIL
tracker github                  ok      -
vcs github                      ok      no [github_app]: pushes and pull requests are made as the gh CLI's user, whose credentials an agent running as you can reach
harness claude-code             ok      -
pipeline .istari/pipeline.toml  ok      -
```

With the daemon running (step 9) a `workspaces` row is added. The `vcs github` detail is the warning about running without a GitHub App. A failing check reads like this and exits 1:

```
tracker github                  FAIL    github: no token from `gh auth token` (install the GitHub CLI and run `gh auth login`): exec: "gh": executable file not found in $PATH
harness claude-code             FAIL    claude-code: the "claude" binary was not found in PATH; install Claude Code (https://docs.claude.com/claude-code) or put it on the daemon's PATH
istari: 2 check(s) failed
```

## 9. The daemon

### In the foreground

```sh
istari daemon
```

It prints nothing when all is well and logs problems to stderr. It stops on Ctrl-C. In another terminal:

```sh
istari ticket list
```

You should see the table header (and your tickets once the first sync is done):

```
TICKET  STATE  HOLD  PROJECT  UPDATED
```

If the daemon is not running:

```
istari: cannot reach the istari daemon at /Users/you/.local/share/istari/istari.sock (is `istari daemon` running?): …
```

Only one daemon runs per data directory; a second one exits with:

```
istari: another istari daemon is already running on this data directory
```

At start the daemon logs what it cannot do, and keeps running without it:

```
istari: ticket sync is off: github: no token from `gh auth token` (install the GitHub CLI and run `gh auth login`): …
istari: pushes and pull requests are off: …
istari: runner: harness claude-code cannot run here, its roles get no slots: …
```

The data directory holds `istari.db` (SQLite), `daemon.lock`, the socket `istari.sock`, istari's clones in `repos/`, the ticket worktrees in `workspaces/<project>/<repo>/<issue>`, and run transcripts in `runs/`. The tracker syncs every 5 minutes; `istari sync` syncs now.

### As a service

```sh
istari daemon install
```

On macOS you should see:

```
Wrote /Users/you/Library/LaunchAgents/dev.istari.daemon.plist with the PATH of this shell
It is not enabled. To enable and start it, run:
  launchctl bootstrap gui/$(id -u) '/Users/you/Library/LaunchAgents/dev.istari.daemon.plist'
```

On Linux (`--kind systemd`) it writes `~/.config/systemd/user/istari.service` and prints `systemctl --user enable --now istari.service`. It only writes the file; enabling it is up to you. With `--config`, the unit passes that config to the daemon.

Logs: launchd writes stderr to `~/Library/Logs/istari.log`; for systemd use `journalctl --user -u istari`.

The unit carries the `PATH` of the shell you run `istari daemon install` in, because a service starts with a minimal one. Run it in a shell where `gh`, `git` and `claude` are found, or install again after you move them.

## 10. The required check `istari/ticket-link`

Add the workflow and make `istari/ticket-link` a required status check of the main branch, as described in the README: [Required check: `istari/ticket-link`](../README.md#required-check-istariticket-link). istari's own branches (`istari/<issue>/<slug>`) and commits (`Ticket: gh:owner/repo#<n>`) always pass it.

## 11. The first ticket, up to a draft pull request

Every open issue of a registered repository becomes a ticket at the first sync. Only a ticket that would be admitted gets a refiner run, which costs money: one by a trusted author, or one a trusted member asked to admit (step 2 below). Anyone else's ticket that falls short waits in `new`.

1. Create the issue. Either in GitHub, with the body in the form of step 6, or with the daemon running:

   ```sh
   istari ticket create --file ticket.md
   ```

   It prints the new ticket's ref, `gh:owner/repo#<n>`. `--repo` chooses the repository; the default is the first configured.

2. Admit it. A ticket of a trusted author (the project's `trusted`, or anyone with write access when the list is absent) that meets the standard is admitted by the sync on its own. Otherwise a trusted member admits it:

   ```sh
   istari ticket ready gh:owner/repo#<n>
   ```

   or sets the label `istari:ready` in GitHub. istari reverts the label, with a comment, if the person is not trusted. A ticket that does not meet the standard yet is refined first and admitted as soon as it does; the command says so:

   ```
   istari: gh:owner/repo#<n> does not meet the standard yet: it is refined first and admitted as soon as it does
   ```

   and the label goes back to `istari:new` with a comment that says the same.

3. What happens. The ticket's state is the label `istari:<state>`:
   - `new`: a ticket that falls short gets a refiner run. It rewrites the ticket (keeping the original text), sets the size, or asks the author questions and puts the ticket on the `needs_input` hold.
   - `ready` → `planning`: istari makes the ticket's worktree on a branch `istari/<n>/<slug>` and a planner run publishes the plan in the ticket. With `plan_gate = true`, a human approves it: `istari gate list`, `istari gate approve <gate>`.
   - `in_progress`: the implementer works in slices. Each `istari checkpoint` commits, runs `typecheck` and `test`, pushes the branch and records the result in the ticket. `istari pr open` runs `full` and opens a **draft** pull request.
   - After that, istari follows CI on the pull request: red starts another implementation round, green moves the ticket to `in_review`. The review, approval and merge stages are not built yet. Review and merge the pull request yourself; `istari ticket pause gh:owner/repo#<n>` stops further runs on the ticket.

4. Watch it:

   ```sh
   istari ticket show gh:owner/repo#<n>   # state, hold, project
   istari task list                       # the ticket's tasks: refine, plan, implement, verify, …
   istari run list                        # agent runs
   istari run logs -f <run>               # a run's transcript, until it ends
   istari tail --follow                   # every event; --ticket, --type, --project filter
   istari workspace list                  # ticket worktrees and their branches
   ```

   Every command takes `--json`.

## 12. Budgets

Each ticket has a budget by its size, from `[defaults.budget_usd]` (XS $2, S $5, M $15, L $30). A ticket without a size counts as M. Every run is capped at what is left of the budget; tickets created as children of a ticket share its budget. When the budget is spent, the run stops and its task is blocked as `budget_exceeded`.

To go on: raise the budget in the config file, restart the daemon, then unblock the task:

```sh
istari task list
istari task unblock <task-id>
```

A task unblocked with no budget left is blocked again.

## 13. When something fails

Holds stop a ticket's tasks from being handed out; the ticket keeps its state. The hold is the label `istari:hold:<reason>`. Removing that label by hand does not lift it: istari puts it back and comments.

| Hold | Lifted by |
|---|---|
| `paused` | `istari ticket resume <ref>` |
| `attempts_exhausted` (a task failed `max_attempts` times, a ticket was refined three times and still falls short, or it used up `max_review_rounds`) | `istari ticket retry <ref>`: restarts the failed tasks and lifts the hold |
| `waiting_dependency` | istari, when the needed ticket is done |
| `needs_input` (the refiner asked the author a question, or planning ended without a plan, or the ticket is larger than `max_size`) | an answer, which arrives through Slack (M4). There is no CLI command for it yet. |

Other tools:

- `istari ticket resume <ref>` also takes back claimed tasks whose run is gone, without waiting for the lease to run out. The next run starts from the ticket, its journal and its branch.
- `istari pause --all` (or `--project <name>`) stops handing out tasks; running ones finish. `istari resume --all` starts again:

  ```
  paused the hand-out of tasks for all projects
  resumed the hand-out of tasks for all projects
  ```

- `istari run stop <run>` stops a run; its task fails.
- `istari workspace prune` removes worktrees without a ticket or of a finished ticket; unsaved work is kept.
- `istari ticket cancel <ref>` cancels a ticket.

### After losing the database

The tracker is the memory. If the data directory is lost, start the daemon on an empty one and run:

```sh
istari recover
```

It restores every open ticket labelled past `ready` that istari does not have, with its state, hold and journal (from the issue's comments), and watches its pull request again. A state label counts only if istari or a trusted member set it. The tracker is not changed.

## 14. Advice for first use

- Start with XS and S tickets with precise acceptance criteria.
- At first, set `gate = true` on the `plan` stage of `.istari/pipeline.toml` (step 7), to read each plan before code is written.
- There is no Slack yet, so nobody is notified of questions: check `istari ticket list` for `needs_input` holds and read the questions in the ticket's comments.
- Run the daemon from a stable build (step 2) and do not rebuild that binary while runs are in progress.
