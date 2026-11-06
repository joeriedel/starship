# Starship design

Starship is an agent-orchestration control plane for Claude Code. It holds the
configuration used to work on *other* repositories: roles, pipelines, and the rules that
govern them. It contains no product code and no knowledge of any particular project.

This document records the design as agreed. Open questions are collected at the end.

---

## 1. Goals

1. **The control plane is project-independent.** Nothing about any project, employer,
   codebase, or task may enter this repo. It lives on a personal GitHub and must be safe
   to make public.
2. **Improvements flow back from anywhere.** Process improvements discovered at home or
   at work can be committed to the control plane from either machine, without carrying
   project knowledge with them.
3. **Everything is extensible below the control plane.** A bridge, a project, or a
   single task can change bindings, reshape pipelines, and attach behaviour to events,
   without editing the control plane.
4. **The process is enforced, not remembered.** Stage order, budgets, gates and approvals
   are enforced by deterministic code through harness hooks. Agents supply judgement;
   they do not do bookkeeping.

---

## 2. Layers

```
control plane   ~/starship            personal GitHub; public-safe
   │            roles, pipelines, extensions, hook driver, resolver,
   │            operating manual, harness notes, generic skills
   ▼
bridge          bridges/work/         hosted wherever that context requires
   │            bridges/home/         (a work bridge never touches personal GitHub)
   │            projects, tasks, project skills, integration scripts,
   │            platform roots, local extensions
   ▼
project         <bridge>/projects/<project>/
   │            bindings, pipeline extensions, rules
   ▼
task            <bridge>/projects/<project>/tasks/<task>/task.md
                workspace paths, VCS state, per-task extensions, journal
```

Each later layer may extend or override the one before it, except locked invariants (§5.6).

### 2.1 How a bridge consumes the control plane

- **Sessions open in the bridge, not in the control plane.** The bridge's repo is the
  session's repo, so tasks, scratch, settings and commits stay inside its boundary.
- **A bridge is always a git repo.** Workspaces may use any VCS; the bridge, which holds
  starship's own state, does not vary. Nothing from starship is ever written into a
  workspace, so no project repo needs to ignore anything.
- **Each bridge runs a pinned release of the control plane.** It keeps its own clone at
  `.starship/control-plane/`, checked out at a release tag recorded in `bridge.yaml`
  (§11.3). Runs are reproducible, bridges can sit on different releases, and nothing
  reaches a bridge until it is released and the bridge is updated with `ensign update`.
- **A writable checkout of starship exists only where starship is developed.** A bridge
  works on it as an ordinary project (§3.2); changes land as pull requests the user asks for, are released
  as a tag, and are picked up by each bridge when it updates. A machine that only uses
  starship doesn't need one.
- **A bridge's pinned copy of starship is never edited.** A PreToolUse hook refuses
  edits there, and to a development bridge's `control-plane: path`, and points at the
  project route instead. A development bridge's path should not also be the workspace of
  its starship project.
- **Edits stay inside each agent's folders.** A PreToolUse hook checks every Edit, Write and NotebookEdit against the folders the caller may edit and denies the rest, naming the folders. The session may edit the bridge, the system temp folder, `~/.claude` and the folders in `bridge.yaml`'s `session-edit` list. A live coder or style reviewer may edit the run's code folders, its scratch folder and temp. Any other agent may edit temp and, if it belongs to a run, that run's scratch folder. Shell writes are not detected; the refusal and the role texts say the limit covers them too.

---

## 3. Keeping the control plane clean

### 3.1 Where leaks come from

Moving projects out of the repo is necessary but not sufficient. The ways project
knowledge reaches a control plane:

- concrete examples written into shared docs (paths, build commands, type names)
- lessons-learned anecdotes that describe a real system recognisably, even unnamed
- promoting platform facts discovered on a project into shared skills
- integration scripts and settings carrying project configuration
- the session agent editing the control plane while it has project context loaded

The last three are channels, not mistakes, and need structural answers.

### 3.2 The improvement path

Starship is improved the way any code is: as a task in a bridge project whose workspace
is a starship checkout.

```
bridge session
  │  "improve starship": a task in the starship project
  │  plan, go
  ▼
run (the coding pipeline; starship's project uses the github-pr extension)
  │  run.start      the checkout moves to the task's branch, task/<name>
  │  code ⇄ review  reviewers include the `public` focus: nothing from a private context
  │  style
  │  (commit)       the run's changes are committed on the task branch
  │  test           plain: push the branch and wait for CI on that commit
  │  run passed     the tested commit waits on the task branch
  ▼
the user asks for the pull request (or a push straight to master)
  ▼
pull request on GitHub
  │  the user reads the diff and merges
  ▼
release (a tag), then each bridge updates
```

- **Starship ships its project settings** in `.starship/project.yaml`, which
  `ensign project new` copies in when the project is created (§4.1). Any repository can
  do the same. Starship's settings use the `github-pr` extension, make `@build` not
  applicable, since no Go toolchain is installed locally, and make `@test` its CI.
- **`github-pr`** is a general extension for any repository on GitHub with CI, built
  into ensign so it runs the same on every platform; it needs git 2.30 or newer (for `push --force-if-includes`; a run start refuses an older git, also under `--stale-branch`, and passes a version it can't read) and a signed-in `gh`.
  Each step is its own: `@branch` at `run.start` moves the workspace to `task/<name>`,
  created from the default branch the first time, and a run won't start over
  uncommitted work on another branch; an existing task branch that is behind the default
  branch is fast-forwarded to it if it has no commits of its own (uncommitted work that
  doesn't overlap the incoming commits is kept, and overlap refuses the run), and is
  refused if it has, unless the run starts with `--stale-branch`, which takes the branch as
  it is. A task branch on origin holding commits the workspace lacks refuses the run, with or without a local task branch, and nothing moves; a remote tip counts as held if the local branch contains it, if it is already in the default branch (when the task branch is new or about to be fast-forwarded), or if it is only in the local branch's reflog and a dry run of the push would pass (a branch rebased and not yet pushed; the dry run skips the `pre-push` hook, which runs on the real push), and when origin has no such branch a leftover tracking ref for it is removed. `--stale-branch` skips this check, with no contact with origin for it; the pushes below still refuse to overwrite. A rewritten branch (rebase or amend) whose reflog no longer holds origin's tip is refused, at start and at push, because the workspace can't show it ever held that tip; both refusals name the remedy `git push --force-with-lease=<branch>:<sha> origin <branch>`, to be used once you've checked nothing on origin is needed. Before the test stage, the run's changes are
  committed on the task branch; the tests are the project's own `@test`; and when the run
  passes, the tested commit is recorded and noted in the finish message. Nothing is pushed
  or opened by the run. Under a sequence, a passing run that the sequence's pending publish step for the task would publish (the last run of its step, with no further subtask group to come and no later run of the task before the publish) closes with a note that the publish step takes the work from there, instead of the usual note that nothing has been opened; the note is written before the publish, so it covers either result, and the sequence's report to the session gives the outcome. Every other run keeps the usual note. A project whose tests
  are its CI binds `@test` to the plain built-in `ensign github-pr ci`, which pushes the
  branch and polls the commit's check runs until all have finished: a failing check is
  a `fail`, with the end of each failing Actions log, and goes back to the coder; no
  checks appearing, or a push that fails, is an `error`. The push, like the one in `pull-request`, refuses to overwrite commits that reached origin's task branch and were never in the local branch, including commits that arrive during the run. The push is plain, with no lease, when origin has no such branch or its tip is an ancestor of the local branch, since nothing is forced and git rejects a non-fast-forward; so a workspace with reflogs off or expired can still push a fast-forward, and a tracking ref left by a branch deleted on origin doesn't fail the push under `--stale-branch`. Any other push, or one where origin's tip can't be read, uses `--force-with-lease --force-if-includes`. Its CI must run on pushes to
  `task/**`. What end-of-run steps report is noted in the run's finish message and its
  progress-log line.
- **Landing is the user's step, after the run.** Asked for a pull request,
  `ensign github-pr pull-request <project>/<task>` pushes the task branch and opens it,
  or finds the one already open. Asked to skip it, `ensign github-pr merge
  <project>/<task>` moves the default branch to the tested commit, if that is a
  fast-forward, and removes the task branch, on origin only if the workspace holds the remote branch's tip (the delete is pinned to that tip); otherwise the branch stays on origin, the landing still succeeds, and the output names the branch in a note; moving the workspace onto the default branch afterwards never autostashes, whatever `merge.autoStash` says, so a merge that would overwrite uncommitted work is refused, the work stays as it is, and the landing still stands; if the default branch moved meanwhile it
  refuses and nothing is pushed. Either one refuses unless the task branch still holds
  the commit that passed (or only later commits that touch `@docs` paths, which keep it tested and are what lands), with nothing uncommitted on it, and a new run of the
  task has to pass again first. The marker is recorded when a run succeeds with its test stage passed, or, with no `test` stage in its pipeline, when the run went through the whole pipeline (no `--start`, or a `--start` naming the stage a plain run enters first); on such a pipeline any other `--start` besides `docs` clears the marker and records none even when it succeeds, so the task is not publishable until a full `ensign run <task>`, even after an earlier passing run; a `--start docs` run never writes or clears it, on any pipeline (on a pipeline with no `test` stage an existing marker is kept, even if commits beyond `@docs` paths came after it, and with none the branch stays unrecorded: the run's note advises `ensign run <task>` and publishing refuses until a run records the branch), so on a pipeline with a test stage a tested task stays tested while a task with no marker still needs `--start test`. A successful merge removes it too. Any other started run also removes the marker at once, before `@sync` and run.start, so a run that dies early leaves none; and both refuse when a run of the task in that location ended without success after the last run that finished `success` (whatever its start stage, so a successful `--start docs` run counts; ask-user ends count as not successful), or the marker is missing. A run cancelled, dropped, discarded, skipped, withdrawn or replaced before it started is ignored. Finish times are compared at full precision when both runs have one and they differ; otherwise the failed run's last history time is compared with the pass's finish to the second, then run id decides. which covers a marker that survives a run that failed before starting.
- **What keeps project knowledge out is structural.** The bridge holds everything about
  its projects and starship never reads a bridge, so nothing crosses on its own. What
  remains is an agent with project context writing a detail into a role or doc. The
  `public` review focus looks for that in every starship run, and the pull request puts
  every diff in front of the user before it lands.
- **Examples in the control plane use a fictional project** (`supernova`). Lessons are
  written as the mechanism alone, or retold in the fictional project.
- **A task can skip the pull request** when the user asks. Nobody reads the diff before
  it is public, so the `public` review is the only check; it is for changes the user has
  chosen to land directly.
- **Use a checkout of its own** for the starship project: runs switch its branch, and
  refuse to start if it has uncommitted changes on another branch.

---

## 4. Customization points

Three kinds, kept separate:

| Kind | Answers | Example |
|---|---|---|
| **binding** | what does X mean here? | `@build` → `go build ./...` |
| **pipeline** | what runs, in what order? | code ⇄ review → style → test → docs |
| **event** | what else happens when Y occurs? | after `test`, post a summary |

Every layer may supply all three.

### 4.1 Bindings

A binding is a command, prose guidance, or an explicit `not-applicable` with a reason.
Carried over from the reference design:

- **Missing is a stop.** A pipeline referencing an undefined binding does not run.
  Silence means nobody decided; `not-applicable` means somebody did.
- **A binding that contradicts the workspace is also a stop** — a path that isn't there,
  a target that doesn't exist. Never substitute an equivalent.
- Platform variants are declared per platform; a binding with variants but none for the
  detected platform is a stop.
- **A repository can ship its project settings** in `.starship/project.yaml`: bindings,
  pipeline ops, events, extensions, in the project.yaml format but without a workspace.
  `ensign project new` copies it into the bridge's project.yaml when the project is
  created; from then on the bridge's copy is the one used, and the user can change it.
  A shipped `docs` binding that a run would refuse (a command or guidance, or a bad path) is reported in the drift report as `binding docs: a run would refuse this: <problem>`, which the reviewed record doesn't silence, and `ensign project reviewed` refuses it, writing nothing. Only the shipped file is checked; runs read the bridge's copy.
- **A project can keep its code in several repos.** `project.yaml` declares `locations:` (name → `depends-on: [...]`, `bindings: {...}`) and a `cross-location:` section of bindings. Location names follow the project name rule; dependencies must name declared locations, with no self-dependency and no cycle. A task names the locations it uses (`locations: [client, models]` in its frontmatter) and each run edits exactly one, chosen with `ensign run --location`, or the task's only location. The run's workspace is that location's folder on this machine, and its bindings layer bridge → project → location → task. A run tests its affected sides: the edited location and every location that depends on it, directly or transitively; `ensign run --sides <a,b>` narrows them to a subset that includes the edited location. The test stage builds and tests each side with that side's own bindings in its own folder, then runs the cross-location `test` once from the edited location's folder when there are several sides. Other sides build from their checkout as it stands. A change that needs another location's unlanded branch names it with `ensign run --dependency <location>=<branch>` (repeatable; the location must be a transitive dependency of the edited one and have the branch in its local checkout). Per-location `@apply-dependency` wires it into a side: ensign runs the command in the side's folder with `STARSHIP_DEPENDENCY`, `STARSHIP_DEPENDENCY_BRANCH`, `STARSHIP_DEPENDENCY_PATH` and `STARSHIP_APPLIED_FILES` (a file the binding fills with the paths it touched), records each path with its restore target (HEAD, the blob it held if it was already modified, or absent) and its content right after the apply in the workspace's git dir (`starship-dependency-applied`), and restores them after the side's build and test; a file edited since the apply is left and named, an ignored file present before the apply is left in place with a notice (whatever its name, including one starting with `:`), and `ensign dependency restore --side <location> --done <file>` settles it. `not-applicable` on it tests the side as it is; `ensign run` refuses when a side needs wiring and has no binding. The cross-location test runs with the edited location's wiring applied. A run that tests dependents applies the edited location's own branch on each dependent. Agent stages use `ensign dependency apply|restore`. The commit steps of github-pr and git-catalog refuse while recorded wiring differs from its restore target. Ensign never pushes the dependency branch. A project that declares no locations works as before.
- **`plain: true`** says a command needs no judgement to run: it passes or it fails by
  its exit code. On `@test` it means ensign runs the test stage itself, with no tester agent. At that stage `next` gives the lead an `ensign run-stage` command to run as a background shell command, and the lead ends its turn to wait. The command runs `@build` (if bound) and `@test`, records the result as that stage's report and transition, keeps the full output in the run's `reports/`, and prints the outcome, the end of any failing output, and the next step. The hook refuses a spawn of the tester (a tester stage with `parallel:` is the exception: its testers are spawned as usual), and `next` runs nothing. No agent reads the log. Exit 125 (a script saying
  it could not run), 126 or 127, a timeout, or a command that can't start is `error`; any other non-zero exit is `fail`.
  A suite that needs a judgement about whether it really ran keeps the tester, as does one whose services must be started by hand: a `@services-up` or `@services-down` with guidance but no command makes the stage not plain. Each command (`@build`, then `@test`) is limited to 60 minutes unless its binding sets `timeout:` (a duration such as `90m`, also allowed inside a platform key); a command over its limit is `error`. While `run-stage` runs a plain stage it holds a lock file in the run folder; a plain stage still running with that lock free means its `run-stage` died. `next` says so, and `ensign run-stage` (or the lead's SubagentStop) records the stage as `error`. It is never restarted.

  Services bindings (`@services-up`, `@services-down`) are per side, run in that side's workspace, and have no cross-location form. In a plain stage ensign runs each side's `@services-up` in side order before any `@build`, then the commands, then each side's `@services-down` in reverse side order after the dependency wiring is restored, whatever the outcome (pass, fail, error, timeout, a failing up). A failing `@services-up`, for any exit code, makes the stage `error` (the tests did not run), and the downs of the sides reached still run. A failing `@services-down` makes the stage `error` even when the tests passed or failed; the summary keeps the earlier result. A SIGINT, SIGTERM or SIGHUP (Ctrl-C, Ctrl-Break or termination on Windows) received while services are up waits for the downs, then ensign records `error` and exits as the signal would. When a plain stage's `run-stage` has died, recording it as `error` (by `ensign run-stage` or the lead's SubagentStop) also runs every side's `@services-down`, so nothing is left for the next run; the same happens for a plain dispatch still running when the lead's stop fails the run. A live `run-stage` is never taken over. There are no readiness or reset bindings: when `@services-up` must reset data before the start, or the start returns before the services are ready, the project writes that, in the order it needs, into the command. A tester stage is told both bindings and runs them itself.

Conventional names, reused before new ones are invented:

| Binding | Purpose |
|---|---|
| `@sync` | bring the workspace up to date with its remote (git: pull/rebase the task branch; Perforce: sync or unshelve) |
| `@checkout` | prepare files for editing (Perforce: `p4 edit`) |
| `@compile` | fast compile-only check; falls back to `@build` |
| `@build` | build |
| `@test` | run the test suite |
| `@services-up` | start the services the test stage needs, returning only once they are ready; runs in the side's workspace before `@build` and `@test`, in the test stage only |
| `@services-down` | stop them; runs after the test stage whatever its outcome |
| `@lint` | linters and formatters |
| `@diff` | the current change set |
| `@style` | style rules beyond the linter, written by people |
| `@done` | this project's definition of finished |
| `@docs` | the workspace-relative files or folders the documentation stage may change; paths or not-applicable only, and with none declared the stage is skipped |
| `@release` | release the project; receives the version in `VERSION` and runs in the project's workspace, with no time limit |

### 4.2 Events

The initial set:

```
session.open   task.open   task.close   plan.approved   run.start
stage.before:<id>   stage.after:<id>
run.finish:success   run.finish:error   run.finish:ask-user
commit.before   push.before
```

Handler kinds:

| Kind | Effect | On handler failure |
|---|---|---|
| `require` | runs a command; on a `before` event it can block | the run ends as `error` (or the action is refused) |
| `observe` | runs a command for its side effect | logged; the run continues |
| `augment` | adds text to the next dispatched prompt | — |

**All events are fired by deterministic code** (§7), never by an agent. An event an
agent forgets to fire is a silent skip, whatever the handler kind.

`task.close` is fired by `ensign task close <p>/<t>`, outside any run, for tasks with and without code. `require` handlers run first, in order; one that fails ends the close with nothing written, so it can be retried. A handler that doesn't set exactly one of `require`, `observe` and `augment` ends the close the same way, before any handler runs. Then the task is marked closed (`status: closed` in its frontmatter, which makes a second close a no-op) and `observe` handlers run; a failing one only warns, as does a failure to journal the close. Handlers run in the bridge root with `STARSHIP_BRIDGE`, `STARSHIP_PROJECT` and `STARSHIP_TASK`, and without `STARSHIP_RUN`, `STARSHIP_RUN_DIR` or `STARSHIP_WORKSPACE`. `@binding` commands, `ensign …` handlers and generated `@name` augments need a run and are refused. The output of handlers and the text of `augment` files are printed after `Closed <p>/<t>.` for the session to act on in the same turn: this is how an extension asks the session (and through it the user) a question.

### 4.3 Extensions

Related bindings, handlers, augments, pipeline ops, whole pipelines and roles ship
together as an extension:

```
extensions/<name>/
  extension.yaml
  <scripts>
  briefs/
  pipelines/       new pipelines the extension adds
  roles/           role definitions the extension adds
```

An extension's pipeline ops apply right after the layer that enables it, before that layer's
own ops, so a project can still adjust what an extension inserted. Its bindings are
defaults in the same way: they apply when the layer enables it, and that layer or any
later one can replace them.

The control plane ships generic extensions (no project names). A bridge may ship its own
in the same format. A project or task enables extensions with `use: [<name>, ...]`. An
extension declares the bindings it requires, so it cannot be enabled where it doesn't fit.

An extension with `quiet: true` in its `extension.yaml` leaves no trace in a bridge whose own `bridge.yaml` `use:` does not name it: its `help:` entries and its pipelines' help are left out of the `help` word, `ensign help requests` and the session-start pointer. One session-start warning is an exception: a project or open task whose `use:` names `focal-sheet` in a bridge that doesn't is warned about (see §9a); no other quiet extension warns.

### 4.4 Skills

**A skill is knowledge of a technology.** The test for whether a fact belongs in one:
*would it be true on a different project using the same technology?* If yes, it is skill
material. Facts about one codebase — its types, conventions, servers — belong in the
project's rules or the task journal.

**Definitions** live at three layers:

| Layer | Holds | Example |
|---|---|---|
| control plane `skills/` | public technology knowledge | `go`, `cpp`, `proto` |
| bridge `skills/` | technology knowledge that must stay in that context | an internal framework; engine facts learned on the job |
| project `skills/` | rare: a technology only this project uses | a project-private DSL |

A skill of the same name at a lower layer **extends** the one above it (the bridge's
`go.md` adds to the control plane's). Replacing it outright needs `replace: true`.

**Selection** happens at the project and task layers:

```yaml
# skills/go.md frontmatter
description: Go language and standard library
default-roles: [coder, reviewer, style-reviewer]
delivery: inject

# project config
skills: [go, proto]

# task.md: this job also touches the build scripts
skills:
  add: [cmake]
  remove: [proto]
  roles: { cmake: [coder] }
```

**Subtasks.** A task can be split into subtasks: `<task>/subtasks/<NN>-<name>.md`, one file each, ordered by the number its name starts with and then by name. A subtask has a `## Goal` and/or `## Scope` (a subtask with neither is refused) and optional frontmatter, `location: <name>`, which is required when the task uses several locations and must be one of them. Each is run on its own, one after another, on the task's one branch: `ensign run <project>/<task>` takes the first pending subtask, `--subtask <name>` a named pending one. The run's work item is the task's goal and scope, the subtask's goal and scope under a `## Subtask <name>` heading, and an `## Already on the branch` list of the earlier done subtasks; later ones are not mentioned. Progress is read from the task's journal, which the bridge commits, so it reaches every machine: a run line `subtask <name>: success` marks the subtask done, and `ensign task subtask done|drop <project>/<task> <name>` writes a journal line that marks it by hand. `task open` and the listings show `subtasks <done or dropped>/<total>` and the next pending one; the run summary and the history line name the subtask. Publishing stays task-level. A task with no `subtasks/` folder works as before; consecutive subtasks in independent locations (unrelated, with different code folders) run side by side, in a group of runs, one per subtask; the sequence runs the groups one after another.

**Delivery** is per skill:

- **`inject`** (default): the driver adds the skill as an augment at `stage.before` for
  each stage whose role is in the skill's role list. Guaranteed; costs the skill's full
  length on every such dispatch. For skills a role must not work without.
- **`on-demand`**: the listing in the agent's prompt gives the skill's description and the path of a file in the run's folder holding its full text (every layer, plus learned entries), and the agent Reads the file when it judges the skill relevant. Cheaper for large reference skills. ensign writes the files once, when the run is created.

Probed on 2.1.312 (§7.1), for the Claude Code skill mechanism: subagents with a relevant skill loaded it in 10 of 10 runs and followed it each time, as did the session agent in 6 of 6. Delivery no longer uses that mechanism, because an agent with a `tools:` list has no Skill tool. Those conditions
were favourable — two skills, descriptions that matched the task closely, rules the model
could not have guessed. The risk grows with many skills, vague descriptions, and
technology the model believes it already knows, which is why `inject` stays the default.

**Learning.** When a run has skills, every stage agent is told it may add a line per
fact it had to establish about a technology itself to its report:

```
fact: shell | set -e does not apply inside a function called in a condition | tested with bash 5
```

The driver saves each one as its own file, `skills/pending/<skill>/<run>-<dispatch>-<n>.yaml`
in the bridge, without slowing the run. `ensign run --pipeline skill-curate` (scope
`bridge`, no lead) dispatches one `curator` per skill with pending facts; each folds its
facts into `skills/<skill>.learned.yaml` under the rules of §4.5, and the facts it was
given are cleared either way. A skill's learned entries are appended to its text
wherever it is delivered.

**Learned facts land in the bridge's copy of the skill, never the control plane's.**
Promoting one into a public skill is a starship task like any other change (§3.2), stated
as a fact about the technology alone.

### 4.5 The curator

One `curator` role maintains every curated document: style guides (§9), skills'
learned entries (§4.4) and declared documents (below). A stage marks itself with `curates: style`, `curates: skill` or `curates: document`,
and its agents are the only writers of those documents. Two more values, `curates: brag` and `curates: brag-feedback`, mark the stages that write and check a focal document's brag file (§9a); they are not the curator's. Any other `curates` value fails validation. Each document type supplies a
brief (`briefs/curate-style.md`, `briefs/curate-skill.md`, `briefs/curate-document.md`); the pass and the checks are
the same for all of them. The curator's context is deliberately small — the document,
the proposals, and anything that outranks them — so its attention goes to consistency.

A document is YAML: entries with an id (`S12` for style, `K12` for skills), a status
(`candidate` or `active`), the text, a hit count and evidence. The curator first
consolidates what is there, then folds in the proposals, and returns the whole document
plus a ledger in fenced blocks:

````
```starship-document
entries:
  - id: S3
    status: active
    text: "Return early on error; no else after a returning if."
    hits: 3
    evidence: ["src/session.go:88 — rewrote if/else into a guard clause"]
```

```starship-ledger
S3: reworded
S7: "merged-into S3"
S9: "dropped — contradicts @style"
S14: new
```
````

When the curator stops, the driver checks the rewrite before writing it, and blocks the
stop with the reasons if it fails:

- every existing id appears in the ledger as `kept`, `reworded`, `promoted`,
  `merged-into <id>` or `dropped <reason>` — nothing disappears silently;
- `kept` means unchanged, for style, skill and document curation alike: `text`, `theme`, `why`, `part` and `status` identical, `hits` may rise, `evidence` may grow (every old element still present; order is not compared). Anything else under `kept` is refused, and the curator uses `reworded`, or `promoted` for a status change; a candidate becoming active under `kept` is refused;
- a merge keeps the lower id;
- a new entry has evidence and starts as `candidate`, unless an authoritative source
  (`source:<id>`) backs it;
- a candidate is promoted only on its second sighting (hits ≥ 2) or an authoritative
  source; an active entry never goes back to candidate;
- the document fits in 150 lines.

**Declared documents.** `bridge.yaml` may declare `documents:`, a map from a name (`^[a-z0-9][a-z0-9-]*$`) to `path` and, optionally, `rubric` and `instructions` (§9a), all relative to the bridge. A `path` ends in `.md` and lies under `notes/` or `projects/<project>/documents/`, the two places the journal commits and nothing else writes; neither path has a backslash, an empty segment, a segment starting with a dot or a `..`. A name is declared once, no two names share a path (compared without regard to case), and a rubric is another `.md` file than the document; `instructions` follows the same path rules and is neither the document nor its rubric. `documents:` is parsed on its own, apart from `BridgeFile`, so a malformed key breaks nothing but the commands that use it; errors begin `bridge.yaml: documents: <name>: `. A document needs no template for a period: edit its `path`, or declare another name.

A declared document is Markdown so `notes/` can carry it: a `# <name>` line, a note that the curator maintains it, and exactly one `starship-document` block holding the YAML above, with ids `D1`, `D2`, … and no authoritative sources. A missing file is an empty document, so a first curation works. A file with no block, two blocks or invalid YAML is an error and is never overwritten; a rewrite replaces only the block and keeps the rest of the file and its line endings. A block ends at the first line that is exactly three backticks, and YAML indents entry text, so text may hold such a line. The write goes through a temp file and a rename. The notes index at session start lists file names only, never contents.

Proposals arrive from any place and time: `ensign document propose <name> <text> [--evidence <text>]` writes one YAML file, `documents/pending/<name>/<time>-<random>.yaml`, holding the text, the date and the evidence if given (a proposal without evidence is cited as `proposal:<date>`), written through a temp file under `.starship/tmp/` so a killed write leaves nothing to commit, and which the journal commits (§10.2) so it follows the user between machines. The name must be declared. `ensign run --pipeline document-curate` (`curates: document`, parallel over the declared documents with a pending file; the pipeline's later stages write and check the brag files, §9a) gives the curator the document, the rubric if there is one (which says what belongs and outranks the proposals), its entries and the proposals. Before any dispatch the run fails, naming the document and the problem and never its content, if a selected document does not load or its rubric is missing. A pending folder for an undeclared name is skipped with a warning. When the curator stops, the files it was given are removed whether or not it changed anything, so a proposal the curator leaves out is dropped, not deferred; one that arrived since survives.

A declared document may be private, so its content stays out of anything that is committed or printed: the `curated` log line carries the path and entry count, not the ledger; the dispatch stores no summary and logs only the outcome; no `fact:` is asked of the stage or recorded from it; a refusal sent back to the curator names ids, and quotes no YAML value; the brief tells the curator to give counts and ids only in its summary. `ensign document propose` prints the document's name, never the text. The run's `reports/<dispatch>.md` is left as written; it is in the run folder, which is not tracked.

### 4.6 Integrations

Integrations with outside systems — issue trackers, wikis, code review systems — live at
the **bridge** level, as extensions in the bridge: the scripts, their bindings (such as
`@review-fetch`, `@review-post`), and their configuration. A project overrides any of it
for its own needs, and a task overrides it again, through the usual layering. None of it
lives in the control plane; which systems a context uses, and how, is part of that
context.

---

## 5. Pipelines

### 5.1 Format

A pipeline is a small state machine. Each stage names a role and the outcomes it may
report; each outcome names a transition. There is no expression language.

Transitions:

| Transition | Meaning |
|---|---|
| `next` | the following stage |
| `{ goto: <stage> }` | jump to a stage |
| `{ finish: success }` | end the run successfully |
| `{ error: <code> }` | end the run as `error` with this reason code |
| `{ ask-user: "<question>" }` | end the run as `ask-user` |

Budgets:

`loop` is the old name of `coder-loop` and is still read as it in pipelines, overrides, extensions, `--budget` and saved runs.

- `spend: <budget>` on a **stage** spends once each time the stage is entered.
- `spend: <budget>` on an **edge** spends once each time that transition is taken.
- `reset: <budget>` on an edge restores that budget to its full value.
- A budget is the maximum number of spends. The spend that would exceed it triggers
  `on-exhausted` instead.

Nesting: a stage with its own `stages` is a lead that dispatches them, and those may
nest further. The driver tracks the chain of leads through dispatch tags, so the format
places no limit; a safety cap of **5 levels** stops a runaway pipeline (one that
includes itself) with `error: nesting`.

### 5.2 The default coding pipeline

```yaml
pipeline: coding
version: 1
requires-bindings: [build, test]
budgets: { coder-loop: 3, test: 3 }
free-roles: [explorer]        # may be spawned at any point; does not advance the pipeline

stages:
  - id: run
    role: orchestrator
    dispatched-by: session
    stages:
      - id: code
        role: coder
        spend: coder-loop
        outcomes:
          compiled: next
          broken:   { goto: code }
          blocked:  { error: blocked }
      - id: review
        role: reviewer
        parallel: { over: dimensions, join: all }
        dimensions: [correctness]
        outcomes:                     # precedence order: the join takes the first one any branch reported
          findings: { goto: code }
          clean:    next
      - id: style
        role: style-reviewer
        outcomes:
          done: next
      - id: test
        role: tester
        outcomes:
          pass:  next
          fail:  { goto: code, spend: test, reset: coder-loop }
          error: { error: test-error }
      - id: docs
        role: documentation
        report-fields: [files, checked]
        outcomes:
          updated:   { finish: success }
          unchanged: { finish: success }
          blocked:   { error: blocked }
        skip: { unless-binding: docs, outcome: unchanged }

on-exhausted:                         # run-wide, beside budgets
  coder-loop: { error: "budget:coder-loop" }
  test: { error: "budget:test" }
```

What this encodes:

- **`coder-loop` bounds one cycle.** Every pass through `code` spends it, whether the pass was
  a compile retry or a response to review findings. Three passes without reaching a
  clean, compiling state ends the run.
- **`test` bounds cycles.** A test failure starts a new cycle with a fresh `coder-loop`. Worst
  case: 3 cycles × 3 passes = 9 coder passes.
- **The coder reports whether it compiled** (`compiled` / `broken`), so every compile
  attempt is a pass the driver can count.
- **`success` is reachable only through `pass`.** A suite that didn't run is an `error`,
  never a success. The `@done` binding is a `require` check on the way to `success`; if it
  fails, the run ends as `error: done-check`.

### 5.2.1 Starting elsewhere, and syncing first

A run may start at any stage, and may sync the workspace first:

```
ensign run <task> --sync --start test
```

- `--sync` runs `@sync` as a `require` handler at `run.start`, before anything else, so
  a `git-catalog` base is recorded after the pull.
- `--start <stage>` begins at that stage; budgets start full. It is refused for a stage that would be skipped, such as `docs` when no `@docs` is declared or `documentation: off` is set.

This covers "commits came in from another machine — pull them and make sure they compile
and pass": the coding pipeline started at `test`. `pass` ends the run as `success`;
`fail`, including a failed compile, goes back to `code` and the normal loop fixes it.
`ensign run <task> --start docs` runs the documentation stage by itself and tests nothing: a github-pr task already tested stays tested, and one that isn't needs `--start test` before it is published (on a pipeline with no `test` stage, which has no `--start test`, a task with no marker needs a full `ensign run <task>` instead, since the docs run never records one).

### 5.3 Extending a pipeline

Later layers edit a pipeline with explicit operations on stage IDs. A list is never
silently replaced.

Each layer has one config file, and they all share the same shape: `bridge.yaml` for the
bridge, `projects/<p>/project.yaml` for a project, and the frontmatter of `task.md` for a
task. The one exception is `rules:`, which the task layer does not take (see below). Pipeline operations go under `pipelines.<name>.ops`, next to the layer's bindings. `bridge.yaml` alone also takes `session-edit`, a list of extra folders the session agent may edit (relative entries are taken from the bridge folder; code folders never go there):

```yaml
# projects/supernova/project.yaml (a task.md frontmatter block looks the same)
platforms: [linux, macos, windows]   # project.yaml only; required for code
pipeline: none                       # project.yaml only; its tasks have no code by default
pipelines:
  coding:
    ops:
      - append: { stage: review, dimensions: [concurrency, security] }
      - insert: { before: test, stage: { id: migrations, role: tester,
                  brief: briefs/migrations.md,
                  outcomes: { fail: { goto: code }, pass: next } } }
      - remove: style
      - set:    { budgets.test: 1 }
      - set:    { on-exhausted.test: { ask-user: "test budget spent — another cycle?" } }

use: [git-catalog]

bindings:
  build: { command: "./build.sh --fast" }
  test:  { not-applicable: "proto-only repo, no suite" }

documentation: off                   # bridge, project or task (a `documentation:` key under a location is refused with an error; a location can carry a `docs` binding); the documentation stage is on unless a layer says off, and a later layer wins

rules:
  - Never touch the vault.

events:
  stage.after:test: [{ observe: scripts/post-summary.sh }]
```

`rules` is a list of strings, accepted in the bridge, extension, project and location layers. Their union in layer order, duplicates dropped, is appended to every work item under `## Project rules` and shown in the run summary with each rule's source. The task layer is the exception to the shared shape: a task's frontmatter `rules:` is ignored, and task-specific rules go in the task's Scope.

A task's frontmatter also names its `pipeline` (default `coding`) and its `workspace`. `pipeline: none` marks a task without code: goal, status, decisions and history, but no workspace, locations, VCS checks or runs. A project's `pipeline: none` (the only value that key takes; anything else is refused) makes that the default for its tasks, and a task's own `pipeline:` wins. `platforms:` stays a code rule: a code task still fails validation at run time without them.
New pipelines are files in a `pipelines/` directory — the control plane's, or an extension's.

`append` adds to a list field; `set` on a list field replaces it explicitly.

`bridge.yaml` alone also takes `keep-runs`, how many finished runs (status `success` or `error`) the bridge keeps in all; the default is 500, and 0 keeps none. Runs are stored per bridge, so there is no project or task setting. Runs that are pending, running or waiting for the user are never deleted. SessionStart deletes the oldest finished runs beyond the limit, at most 50 delete attempts per start (a delete that fails still uses up the cap, and the cap is shared with the pipeline-only folders below), drops session links whose folders are gone, and reports on the start line how many runs it cleaned up, how many it could not delete and how many scratch folders it could not empty (see below); `ensign gc [<keep>]` does the same on request, with no cap, and prints `removed N runs, emptied N scratch folders, dropped N session links`, appending `, could not delete N runs` and `, could not empty N scratch folders` only when those counts are above zero. `ensign gc` exits 0 even when deletes or scratch emptying failed. When the runs folder exists but can't be read, `ensign gc` warns on stderr and appends `, could not list the runs` to its line, and the session-start line says `could not list the runs`; no run is considered for deletion, a missing folder reports nothing, and the exit code is unchanged. The cleanup keeps a corrupt run. The cleanup also deletes a run folder that has a `pipeline.json` and no `state.json` once the `pipeline.json` is older than 5 minutes, unless an open sequence holds the run or the run's lock is held (`state.json` is checked again under that lock). These folders are left by a delete that failed or crashed, or a run creation that failed; they don't count toward `keep-runs`, they are handled before the finished runs, and a session link is dropped only once its folder is gone, so a folder that survived or was skipped keeps its link. The cleanup also removes the `<session-id>.task`, `.pin`, `.env`, `.sequence`, `.writes` and `.topic-*` files of sessions untouched for 30 days (the newest file of the session, link included, sets its age), unless the session has a run that has not finished or an open sequence, or its sequence lock is held. It also removes the session's `.lock`, `.sequence.lock`, `.sequence.tmp`, `.writes.lock`, `.writes.tmp` and `.tmp` files once the session is that idle (the leftovers' own mtimes count, as do the session's other files and link) and not busy, unless the lock that goes with the file is held: only the locks for the files present are tried, `.sequence.lock` for a `.sequence.lock` or `.sequence.tmp`, `.writes.lock` for a `.writes.lock` or `.writes.tmp`, and `.lock` for a `.lock` or `.tmp`. Sweeping session files runs only for the cleanup that gets `.sweep.lock`; dead links are dropped regardless. `<pid>.pid` and `*.connectors` files older than 30 days are removed, each on its own age.

A run empties its `scratch/` folder when it finishes (success or error), and the cleanup empties the scratch folders of the finished runs it keeps; runs an open sequence step still needs are left alone. If the cleanup can't list the held sessions it deletes no run and empties no scratch folder, but the dead-link pass and the session-file sweep still run. The cap counts delete attempts, so a run or folder skipped for a held lock, or one that fails `openRun`, uses none; reads aren't capped. On Windows each failed attempt can take about 2s (100s for the cap in all), and each read of a persistently unreadable `state.json` or `pipeline.json` can take up to 2s with no attempt counted. A failed delete removes all it can, because `os.RemoveAll` keeps going. A folder that keeps `pipeline.json` uses the cap at every start, with or without `state.json`, and 50 or more of them hold back newer candidates at session start; `ensign gc` has no cap and reaches them. The start line reports each outcome on its own, in this order, each only when its count is above zero: `cleaned up N old runs` (with a note that more will be cleaned at the next start if the cap was reached; the note appears only with this part), `could not delete N old runs` (N being the failed attempts), and `could not empty N scratch folders`. A folder counts as failed when it is not confirmed gone after the delete. If no delete was attempted and no scratch folder failed, the start line says nothing about cleanup. A scratch folder counts as failed when it can't be read for any reason other than not existing, or when an item in it can't be removed; a missing or empty folder is not a failure, and a run that can't be opened or isn't finished is skipped without one. The count is per folder and covers only kept finished runs. A folder with neither `pipeline.json` nor `state.json` is left alone on purpose and uses no attempt. It comes from a run creation that failed early, a crash mid-delete, or a failed delete. After a failed delete it holds only what couldn't be deleted; the other two can leave deletable content, which stays until someone removes it by hand. Reclaiming these folders would bring undeletable ones back into the cap at every start. A folder with a finished `state.json` and no `pipeline.json` takes a `keep-runs` slot, uses no attempt, and is never deleted. On Windows, a session link naming a folder with `pipeline.json` and no `state.json` costs up to 2s each time the session's runs are checked, until the link is dropped; that is the retry that protects a `state.json` mid-save.

A folder or file listing that fails is reported rather than read as empty: a role dispatch is refused when `.claude/agents` can't be listed, `skill new` refuses before writing, starting a run fails when the skills can't be listed, and a stage fails to start when its pending inputs can't be listed. A path that is a plain file, or that has a plain file higher up it, counts as a failed listing on every platform, while a folder that is missing under real folders still reads as empty with no error. The run listing is reported too, except that a missing runs folder, or a plain file at or above its path, reads as no runs because none can exist there. A run folder that can't be ruled out as live fails the listing the same way, with an error naming the run and the folder to repair or delete: one whose `state.json` can't be read or is corrupt, or whose state is not finished and whose `pipeline.json` can't be read. Skipped silently are a finished run with a bad `pipeline.json`, a folder with no `state.json` (the cleanup owns it) and an entry in the runs folder that is not a folder. So while such a run exists, every guard below refuses. No ensign command clears it: `run retire` and `run stop` open the run and fail, and the cleanup keeps it, so it is deleted by hand (for example a `supernova` run's folder under `.starship/runs`). When `.starship/runs` exists but can't be listed, the guards that depend on the listing refuse with the error (`link`, `update`, the catalog squash, the rebase and landing checks, starting a run, connector writes; an unreadable or corrupt sequence file also fails the check for a run or sequence waiting for `go`, so `ensign writes add` errors and a `go` with connector writes pending fails the hook and approves nothing), a stale session link reports not ready at session start, the stage-role check records it as unread and still denies a role found in the pipeline files, and the cleanup's session sweep deletes nothing. Two callers ignore the failure because the result is safe: a skipped step stays skipped, which cancels the publish, and no remembered location leaves the sole location or asks for `--location`. Not every listing is reported: the cleanup's own scans of the runs and session-link folders drop a read error and read it as empty. A glob character such as `[` in a bridge, control-plane or workspace path means itself and hides no file.

`bridge.yaml` alone also takes `connectors:`, per-role rules for connector (MCP) tools: a map of role name to a list of exact tool names, each `mcp__<server>__<tool>`, or one of Claude Code's built-in `ListMcpResourcesTool` and `ReadMcpResourceTool`.

```yaml
connectors:
  reader:
    - mcp__supernova_mail__read_message
```

A bridge that declares nothing behaves as if the key did not exist. Declaring at least one role (a role with an empty list included, which grants nothing) turns enforcement on (§7). A wildcard, a bare `mcp__<server>`, which would grant a whole server, and any entry for `coder` are errors. The control-plane `reader` role (`roles/reader.md`, which reads external content as data and returns facts marked with source and date) is installed only when `connectors:` has a `reader` key; a key with an empty list counts. Whenever a role has a declared list, `link` appends those tools, sorted and without repeating one already there, to the `tools:` line in the frontmatter of `.claude/agents/<role>.md`, for roles from every source. The list is found by the role's frontmatter `name` (the `agent_type` the hook sees), or by file name when there is none. The line is edited as text, so it must be a plain comma list on one line with no quotes, brackets, block list or comment; otherwise `link` fails naming the role. A role with no frontmatter `tools:` line (a body line that starts that way doesn't count) is left as it is and the hook still enforces, except for `reader`: when `reader` is declared, a bridge or extension role whose frontmatter `name` is `reader` (file name if none) is refused at link unless it has a usable `tools:` line that grants none of `Bash`, `PowerShell`, `Agent`, `Task` (each in any case and with or without `(...)`) or `*`, since a reader with a shell, or one that can start a role that has one, would get round the edit guard's limit (§7). Tokens are split on commas and on spaces outside parentheses, as Claude Code does. Whether connectors serve content through the two built-in resource tools is not measured; they are covered so that they cannot be a way round the rules. The declaration applies from the next session start, when SessionStart writes the matching `.claude/settings.json` (§11.3). What the reader keeps is prose shaped by `roles/reader.md`; nothing in Go parses it, and the last look at a linked item (§5.3, `links:`) is the reader's own kept entry in `facts.md`: Go stores no per-link state and compares nothing. Under `<bridge>/reader/` there are three levels: facts (`facts.md`, current state, edited not appended, each fact with source, as-of date and date read, understandable without the source; `history.md`, append-only, where a superseded fact moves, dated, with its replacement), short exact quotes kept under their fact where wording matters, and pinned snapshots in `sources/`, made only when the brief says the user said to keep the source. When sources disagree the reader keeps and reports both. Snapshots are text only (an attachment is recorded as a fact), at most 65536 bytes each, with no total cap on `sources/`; the edit guard (§7) enforces the form. The reader's files are committed through the journal (§10.2).

`bridge.yaml` also takes `connector_writes:`, a list of exact `mcp__<server>__<tool>` names the session may call after the user approves the exact call with `go` (§7). The key being present turns it on, an empty list included; absent, nothing changes anywhere, `go` included. The built-in resource tools and wildcards are errors, and so is a tool that a role also lists under `connectors:`. It needs neither `connectors:` nor a `reader`. Write tools never go on a role's `tools:` line.

`bridge.yaml` is decoded strictly: an unknown top-level or nested key is refused, naming the file and key, so a misspelt declaration can't quietly do nothing. `sources:` and `documents:`, which are read elsewhere, stay accepted, and `workspace` stays as a legacy key. A refused `bridge.yaml` makes SessionStart report starship as not ready. `connectors:` and `connector_writes:` are bridge settings: in `project.yaml`, a task's frontmatter or an extension's `extension.yaml` they are refused with `<file>: <key>: is a bridge setting; declare it in bridge.yaml`, a null or empty value and a key reached through a merge included, at the same places that refuse `platforms:`. An unknown key in those three files is refused too, with `<file>: <key>: unknown key (line N)`, a nested key named by its dotted place (`events.run.start[0].requre`). A warning was judged not enough. The keys are checked at the top level and inside `journal`, each handler of `events`, `pipelines.<name>`, a project's `locations.<name>` and `cross-location`, a task's `vcs` and an extension's `help` items; the contents of `bindings`, `rules`, `skills`, `links`, ops and similar sections are not. `project.yaml` also allows top-level `x-*` keys, whose contents are not checked, to hold YAML anchors; a task or extension has no such keys. A key brought in by a `<<` merge is checked where it lands. A file that has `connectors:`, `connector_writes:` or (in a task or extension) `platforms:` gets that key's own message instead. An extension with an unknown key is a broken extension: it does not stop a command that doesn't use it, help names it, and it is refused when a layer enables it. The code repo's `.starship/project.yaml` and `shipped-reviewed.yaml`, and the front matter of roles, skills, connectors and subtasks, are not checked this way.

`links:` is a separate key, on a task (in its frontmatter) and on a project (in `project.yaml`), not on `bridge.yaml`: a list of `kind` (`ticket`, `wiki`, `mail` or `slack`) and `ref` (a non-empty one-line identifier or URL that Go treats as opaque), with no other key. It is parsed on its own, not as part of the layer, so a malformed `links:` breaks nothing but its own listing. A task's links are its project's plus its own, a repeated kind and ref shown once; a missing `project.yaml` means no project links. `ensign task open` lists them on every path, a task without code included. When `bridge.yaml` declares the reader, it adds a line telling the session to dispatch the `reader` agent with the links before starting work and to report what changed; without the reader, or without links, it says nothing more. Invalid links, or a `bridge.yaml` that can't be read for the reader check, print a `STOP:` line naming the problem, and `task open` still succeeds and prints the task.

**Declared sources and catch-up.** `bridge.yaml` may declare `sources:`, a map from a source name (`^[a-z0-9][a-z0-9-]*$`) to a `kind` (`mail`, `slack`, `calendar`, `ticket` or `wiki`) and a `watch` list of one or more unique, trimmed, one-line items (a channel, a person, a folder, a project). For `slack` an item is `#channel` (no whitespace) or `@person` (a DM, spaces allowed), never a pattern (`*`, `?`) or `@here`, `@channel`, `@everyone`, checked for `--also` items too; a Slack source also covers the threads the user is in or mentioned in, and the brief says so, so a pass never reads the whole workspace. A `calendar` item is a calendar's name, and the pass reads who met and across which orgs. `sources:` is parsed on its own, apart from `BridgeFile`, so a malformed `sources:` breaks nothing but `ensign catch-up`; errors begin `bridge.yaml: sources: ` and name the source. Nothing writes the key; the user edits it. `ensign catch-up [--also <source>=<item>]...` prints a brief and changes no file: it refuses when the reader is not declared or no sources are, lists each source (sorted by name) with its kind and watched items, and lists `--also` items (for a declared source, not already watched, repeats collapsed) as extras for this pass with no cursor. The session dispatches the reader with that brief; a connector call from the session is denied, so a pass goes through the reader. The reader's `## Catch-up` rules (`roles/reader.md`) own the files: `reader/cursors.md` (a line per watched item with a resumable position and the date read) and `reader/proposals.md` (proposed notes with source and date). Go parses neither; both are Markdown, so the journal commits them (§10.2) and the cursors follow the user between machines. A pass writes no fact: the session lists the proposals, the user says which to keep, and the session files those as notes. The `catch-up` help topic carries the session's side.

**Declared documents.** `bridge.yaml` may also declare `documents:`, each a `path` and an optional `rubric` for the curator to maintain (§4.5), and on a focal document an optional `instructions` file (§9a), parsed on its own like `sources:`, with errors that begin `bridge.yaml: documents: `.

For mail, the reader also appends a line per message read to `reader/mail-read.md` (source, folder, message id, received and read dates, sender, subject; never the body), so the session, which cannot call the reader's connectors, has a record to work from. A pass never changes mail or offers cleanup. Cleanup happens only when the user asks, through the `mail-cleanup` help topic: only recorded inbox mail, moved to the folder "archive" and never deleted, before registering a move the session has the reader check where each picked message is now, so mail that has since been filed or has gone is not moved; each move is a registered write that `go` approves (§7). Go enforces none of these rules and parses nothing in `reader/`; the session keeps them, and the move tool must be listed in `connector_writes:`.

### 5.4 Parallel stages

Review is the one stage that fans out. A stage with `parallel` dispatches one agent per
entry in the named list, all on the same commit, and joins their outcomes.

```yaml
      - id: review
        role: reviewer
        parallel: { over: dimensions, join: all }
        dimensions: [correctness, concurrency, security]
        outcomes:
          findings: { goto: code }
          clean:    next
```

- **Branches are assigned by the driver, not chosen by the lead.** The orchestrator spawns
  the reviewers with one shared brief; PreToolUse gives each spawn the next undispatched
  dimension, tagging it (`stage=run.review dim=security`) and appending
  `briefs/review/<dimension>.md` from whichever layer supplies it. A spawn beyond the
  list is denied.
- **The list is configuration.** The default is `[correctness]`, a single reviewer.
  Projects and tasks add dimensions with `append`. A dimension with no brief anywhere in
  the layers is a stop, like a missing binding.
- **`join: all` is the only join rule**, and it waits for every branch. The lead cannot dispatch the next stage until
  the join completes; PreToolUse denies it and names the outstanding branches.
- **Outcomes combine by precedence.** A parallel stage lists its outcomes in precedence
  order, and the join takes the highest one any branch reported: one `findings` among
  three `clean` is `findings`. Each branch's report is kept separately in `reports/`.
- **Branches don't spend.** A stage-level `spend` is charged once, on entry; the edge out
  of the join spends as usual.
- **Reviewers are read-only**, so branches can't conflict in the workspace.
- **A stage may take an outcome with no dispatch.** `parallel.empty` names one of the stage's outcomes, which the stage takes, with no dispatch, when its list has no items. `over` may also be `focal-documents`, the declared focal documents (§9a). `empty` cannot be used with `over: dimensions`, and `--start` refuses a stage with `empty` set and no items. A run whose every `empty` stage has no items fails with the usual "nothing to do" reason, so `document-curate` with no pending proposals and no focal document still fails with "no declared document has pending proposals to curate".
- **A stage may be advisory.** `advisory: <outcome>` on a stage names an outcome (not an error) that a dispatch takes in place of any failure to report or to start, with no stop budget spent and no retry, so the join goes on and the run does not fail. `ensign run stop` during an advisory stage finishes the run through that outcome instead of cancelling it.

### 5.5 Concurrency model

Parallel work is **separate tasks in separate sessions**. Within a run, only a parallel
stage runs concurrently; there are no parallel fragments within one task and no
speculative stages. Each session has at most one active run, so a spawn from the session
belongs to that run without ambiguity. Two runs must not share a workspace; nothing
coordinates edits between them, so `ensign run` refuses a second live run on the same
workspace.

**How a run is tied to its session.** Every hook payload carries Claude Code's
`session_id`, the same for the session and every agent inside it. `ensign run`, though,
is a command Claude runs through Bash, so the SessionStart hook writes
`STARSHIP_SESSION=<session_id>` to the file Claude Code provides for that
(`CLAUDE_ENV_FILE`), and every later command in the session sees it. The run is recorded
as belonging to that session (`.starship/sessions/<session_id>`), and hooks look runs up
by session. A run created outside any session — from a terminal — belongs to no one until
a session types `go`, which claims the newest such run.

The session id can change while the run lives: restarting Claude Code, resuming a session or compacting may give the same conversation a new `session_id`. A hook that carries an `agent_id` looks the agent's run up first, before the session's: if the session's own run already owns the agent, that run is used; otherwise the agent's live run is searched for, and used for the call even when the session is bound to a different run (a restarted session may have started a new run while the old run's agents are still working). A session bound to another run keeps that binding; no `bindSession` and no `State.Session` change happens for either run. If the agent belongs to no live run, the session's run is used as before. Each run is opened and closed in turn, so no two run locks are held at once. The agent's run is found by: first the run named by the dispatch tag in the agent's transcript (`agent_transcript_path`, else the path derived from `transcript_path`), then any live run with a dispatch bound to that agent id. A live run found this way is rebound only when the session has no run to open: the new session's file is written, `State.Session` is updated, and the history records the old and new ids. An agent of the session's own run, where the run is owned by another session and its history doesn't show a rebind away from the calling session, finishes the rebind the same way, so a rebind whose save failed is retried on the next hook. When the session has several live runs, the agent's run is found as above and, if the session's file lists it and the same conditions hold, `State.Session` and the history are updated the same way, so a failed rebind is repaired there too; this repair does not call `bindSession`, so it leaves the session's set of runs and its setup file alone. A finished run, or one rebound away from the calling session, is left alone. If no run is found and the agent is spawning a stage role (a stage role in a live run's pipeline or any pipeline of the control plane or its extensions, and not a free role), the spawn is denied with a message that the agent's run can't be found; other spawns, and calls with no `agent_id`, pass as before. If the runs listing failed, a read-only role is denied too; the read-only exception below applies only when the unread files are a pipeline file, a bridge file or the control plane. When a pipeline file, a bridge file (other than a missing one) or the control plane cannot be read, so the roles cannot be told, a read-only role is let through with a system message naming the file and any other role is denied; read-only means the role is matched by agent name (frontmatter `name`, else file stem) across `<bridge>/.claude/agents/*.md`, with exactly one match and every file there readable and parseable, and the matching file has a plain one-line `tools:` listing only Read, Grep, Glob and Bash, so built-ins with no definition (Explore, Plan, general-purpose) stay denied.

SessionStart also tries to carry a binding over. It assumes that when `source` is `resume` or `compact`, the session transcript at `transcript_path` still holds records stamped with the earlier session's `sessionId`, and that the payload has a `source` field at all; if either is false nothing is carried over and the agent-based lookup remains. For `clear` the first assumption is false: the cleared session has a new id and a transcript with no records under the old one. So every SessionStart records its session id in `.starship/sessions/<pid>.pid`, where `<pid>` is the `CLAUDE_PID` the hook receives, which Claude Code keeps across `/clear`; the previous value is read before it is overwritten. For `clear` to a different id, that previous value is the first prior session, and the task is carried to the new id even when no run is live. `startup` overwrites the record like every other source, so a reused pid can't link unrelated sessions. A record not touched for 30 days is swept with the stale session links. `CLAUDE_PID` is undocumented and must be all digits; when it is absent or not, nothing is recorded or carried for a `clear`. `ensign selftest` checks that SessionStart receives it but cannot perform a `/clear`. When the new session id has no binding, the earlier ids found in the transcript are tried latest first, and the first one bound to a live run hands that run to the new session. This never fails SessionStart; a run that could not be moved is reported in the session's context (see the report rules below).

**Moved sessions.** On a SessionStart that carries (`resume`, `compact`, `clear`), `carryOverSession` records every prior session it finds as moved, in `.starship/sessions/<id>.moved`, which holds the new id. This happens before any sequence is renamed and also when the new session already has a link file. Every SessionStart of an id removes that id's own marker, for any source, because an id can come back. A process that keeps the old `STARSHIP_SESSION` then can't quietly strand work on it: on a moved session `ensign sequence run|publish|do|release`, a plain `ensign run` and `ensign writes add` are refused with an error that names the new session, and nothing is added. Nothing redirects the call to the new session, because nothing maps a long-lived process to the current session. A dry run is still allowed, and work that continues an existing step (the next subtask group, a retry) is not refused. The sequence check is made again under the sequence lock. A marker that exists but can't be read refuses too; only a missing marker means the session has not moved. `ensign task open` on a moved session still prints the task but does not record it on the session, and says so. The marker is recorded only for priors a carrying SessionStart finds, so a session is not marked when nothing links its old id to the new one (a `startup` into a new id, a `clear` with no usable `CLAUDE_PID`, or a pid record already swept); no signal exists to mark from, and nothing stops a stale process there. A sequence left behind by a carry (its rename failed, or it stays on the earlier session) is not a moved session and has no command to re-own it: resuming the earlier session clears that session's marker, and its Stop hook drives the sequence again, or it can be halted from there. The report of a left-behind sequence names every left-behind session and reaches the user as well as the session agent: it is in the SessionStart context and in the `systemMessage` line, on all three reply paths including not-ready. A sequence whose rename failed but that has no open work is reported in the context only, with no resume hint, because there is nothing to drive; a sequence that can't be read counts as open work. A session's `.moved` is swept with its other per-session files after 30 days and never makes the session busy, but it stays as long as that session has an unfinished sequence, because the session is busy for its sequence, not for the marker.

These assumptions are checked against a real Claude Code by the restart probe of `ensign selftest`, which runs a session whose agent makes a Bash call, then resumes that session with `claude -p --resume` and messages the agent by id. It checks that SessionStart carries `source` (`startup`, then `resume`), that the resumed session keeps its id or its transcript keeps records stamped with the first session's `sessionId`, and that the agent keeps its `agent_id` and its transcript path across the restart. Claude Code's documentation implies that resume keeps the session id and transcript file but is silent on the rest, so the probe records what it observes. The documentation also lists a `fork` source (`--fork-session`, `/fork`, `/branch`), where the new session gets a new id; starship does not carry a binding over for `fork`. A fork does not mark its parent moved. As observed on Claude Code 2.1.319 on 2026-11-06 with `claude -p --resume <id> --fork-session`, lines copied into a fork are restamped with the fork's own `sessionId`, the parent's id appears nowhere in the fork's file, and a plain resume of the fork keeps the fork's id and file; the probe ran not logged in, so the replies were synthetic, and the interactive `/fork` and `/branch` were not probed. This is an observation, not a guarantee: the transcript format is internal to Claude Code.

### 5.6 Locks

A few invariants are **locked** in the control plane: the `go` approval gate, no
workspace push without approval, and no agent attribution.
A lower layer may add to them (another gate, a stricter rule) but cannot weaken or remove
them; the resolver rejects any layer that tries. There is no unlock. If an invariant is
wrong, it is changed in the control plane, for everyone.

---

## 6. Runs

### 6.1 Outcomes

| Run outcome | Meaning |
|---|---|
| `success` | compiles, tests pass, `@done` satisfied |
| `error` | the run stopped; a reason code set by the driver plus the agent's summary |
| `ask-user` | waiting on a decision; carries the question; resumable |

Reason codes come from whichever edge or budget ended the run (`blocked`,
`test-error`, `budget:coder-loop`, `budget:test`, `done-check`), so they are deterministic.
The driver adds its own:

| Code | When |
|---|---|
| `ensign` | an `ensign` error during the run (below) |
| `bad-report` | a stage agent failed to produce a valid report: its third refused stop or handback |
| `lead-stopped` | the lead kept trying to stop before the pipeline finished: its third refused stop or handback |

**An `ensign` error during pipeline work is an immediate failure:** the run ends as
`error: ensign` with `ensign`'s message. This covers a hook that fails and a command
Claude runs (`ensign run`, `ensign journal commit`, …) that fails. It is never
retried in a loop and never worked around — no falling back to raw `git`, no dispatching
without the driver. The fix is to `ensign` or its configuration, then a new run or
`ensign resume`.
Handlers attach to `run.finish:error`; filtering by code is not supported until a real
case needs it.

`ask-user` is resumable because a run's state lives in files, not in an agent's context:
`ensign resume <run>` dispatches a fresh lead against the same state, with the user's
answer and any budget change recorded there.

### 6.2 Stage reports

Every dispatched agent hands back a report block as the `message` of its
`SubagentHandback` call (§7), and the report is that message, not its closing text:

````
```starship-report
outcome: findings
summary: two off-by-one errors in range clamping
files: [src/range.go]
```
````

Each line is `key: value`, split at the first colon, so a summary may contain colons
(an earlier YAML parse tripped over exactly that in a live run). The driver rejects a
report whose `outcome` is missing or not among the stage's declared outcomes, and blocks
the agent with the valid outcomes listed: a handback is denied at the call, so the agent
can resend it; a final message with no handback is blocked at its stop. A stop with no
handback and nothing of the agent's own still running is rejected the same way. Stages may
require further fields (the `code` stage requires `files` when `git-catalog` is enabled; the `docs` stage requires `files` and `checked`). A `checked` line lists the code the stage read, may repeat, and is required unless the outcome is `blocked`; the driver checks only that one is present and non-empty, not what it says.

Agents never have to be taught the format by a role file: the driver appends the report
instructions, with that stage's outcomes, to every dispatch (§7).

### 6.3 Creating a run: the resolver and the snapshot

`ensign run <task> [--sync] [--start <stage>]` creates a run:

1. **Merges** the pinned control plane → bridge → project → task, plus every extension they
   enable, applying each layer's ops in order and recording every field's source.
2. **Validates**: every required binding defined for this platform (or declared
   not-applicable), every brief, skill and dimension present, no lock weakened, nesting
   within the cap, help text present. Any failure stops with the reason.
3. **Writes a frozen snapshot** and an initial state, stamped with the pinned tag. Hooks
   read only the snapshot, so editing config mid-run changes nothing for that run.
4. **Prints a summary** — stages, budgets, bindings resolved to commands, skills per
   role, overrides — which Claude puts in the plan it shows.

The run then waits for `go`. Until then the hooks refuse its lead dispatch.

```
<bridge>/.starship/
  sessions/<session-id>      the run that session owns
  sessions/<pid>.pid         the id of the session last started under that CLAUDE_PID
  sessions/<pid>.connectors  the connector parts of the settings and roles that claude process started with
  runs/<run-id>/             <timestamp>-<project>-<task>, allocated exclusively
    pipeline.json            the snapshot: resolved pipeline with sources, bindings,
                             workspace, work item, and every brief the run can use
    state.json               written only by the driver
    skills/<name>.md         the full text of each on-demand skill, written when the run is created
    reports/<dispatch>.md    each stage's report: the agent's handback, else its final message; ensign writes a plain stage's report itself
    lock
```

Briefs are frozen into the snapshot along with the pipeline, so editing a brief mid-run
doesn't change what later stages of that run are told.

`ensign run <task> --dry-run` does steps 1, 2 and 4 and stops: it shows what the run
would be, with the source of every field, and creates nothing:

```
    review   reviewer        parallel over [correctness tests]
             from control-plane:pipelines/coding.yaml:20; dimensions append by project:projects/supernova/project.yaml:8
               findings  → goto code
               clean     → next
budgets
  test   1   from task:projects/supernova/tasks/auth-refactor/task.md:7
bindings
  @build    ./build.sh   from project:projects/supernova/project.yaml:2
rules
  Never touch the vault.   from project:projects/supernova/project.yaml:5
```

The summary ends with the `bindings` section and, when there are any rules, a `rules` section; a rule's source is `<layer>:<path>:<line>`, or `extension:<name>:<line>` for an extension's rule (for example `extension:house:3`), and a location's is `location:<path>#<location>:<line>`.

The snapshot's hash is recorded in the state file. A config change mid-run does not
affect that run.

#### Sequences

A sequence is an ordered list of steps in one session that the user approves with a single `go`; the session then works through it unattended. A step is a `run` (a run created queued: it belongs to the session but is not bound to it, replaces no waiting run, and does not count as a live run), a `publish` (the publish operation an extension declares, `ensign github-pr merge` for github-pr, carried out in process), a `do` (an instruction the session carries out and reports with `ensign sequence done`) or a `release` (`ensign sequence release <project> <version>`: the project's `@release` binding (resolved through the usual layers), run with `VERSION` set; the project must have one, and the version is fixed when the step is added). A release step takes `--location <name>`: its `@release` is the project's overridden by that location's and it runs in that location's folder; without the flag the project's only location is used, with several the step is refused, a project with no locations refuses the flag, and the name is validated when the step is added. Steps are kept in `sessions/<session-id>.sequence`, changed under its lock. A step added after `go` goes into a new sequence that needs its own `go`.

After `go`, and each time a run ends, `ensign sequence done|fail` is called or a paused run resumes, the next pending step starts: a run is bound, approved and handed to the session like a plain `go`; a publish is carried out at once. Only one step is under way at a time, and the Stop hook holds the turn open (three times per step) while a run's lead has not been dispatched or a `do` or `release` step is active. A `do` step does not hold it while the session has a background task in flight (the Stop input's `background_tasks` is not empty). A release is never run inside a hook: when it is next the step goes active and the Stop hook tells the session to start `ensign sequence release-run <seq>.<step>` as a background command; the step is marked started, so the hook stops holding the turn, and the command's completion wakes the session. A release that fails is a plain failure (`stop` halts; `keep-going` also skips the later steps of that project and its tasks), and release steps cannot be retried. On a failure the default mode `stop` halts the sequence and cancels the rest; `keep-going` skips the later steps of the same task and carries on with the others. In stop mode, when a run step's run fails, the step waits (status `decide`) and the Stop hook holds the turn once per failed attempt, since the session has then been told, and again in the continuing session if the sequence moves to it: every failed attempt waits, with no cap on retries, and the session takes it to the user, never retrying on its own, offering exactly retry, skip and halt in that order. The message to the user is a full brief every time (`decideBriefText`): what failed and why in plain words, the state of the work, what each of retry, skip and halt would do, and every decision a retry needs with each option spelled out and a recommendation, never referring to an option by letter or to an earlier message. This applies to run steps; a failed release is a plain failure and can't be retried. Retry is `ensign sequence retry <seq>.<step> --diagnosis <why> --state <what is left>`; halt is `ensign sequence accept <seq>.<step>`, which applies the failure rules above; skip is `ensign sequence skip <seq>.<step>` (refused once the sequence is halted or for a step that is not waiting): the step ends as failed with its reason prefixed `skipped by the user`, the pending publish steps of its task are cancelled, the sequence is not halted and the next step starts. Skip lists the later release steps, `do` steps naming the task and run steps of the same task as possibly affected, in its own sequence and in every later sequence that is not closed or halted (approved or not), where it also lists the task's pending publish steps, since skip cancels only its own sequence's. Run steps of other tasks aren't flagged because each starts from master; the offer and the skip output instead read the folders of the failed runs and name their branch and changed paths (or that they are clean), since a later run in a folder with uncommitted changes won't start until they are committed or stashed. A publish step that comes after a skipped run step of its task, in any sequence, is cancelled when reached unless a later run of that task in that location passed after the skip (a plain `ensign run` counts; a skip recorded before the step stored its time always stands); this is checked in the sequence because a stale `.tested` marker can still match the branch head (the merge also refuses when a run of the task in that location failed after its last pass). A sequence that closes with skipped steps says so. In keep-going mode a failed run step never waits: reconcile ends it `failed` at once (the run's reason unchanged, `SkippedAt` set so a later sequence's publish of the task is cancelled; every failed run step of a keep-going sequence gets it, set where the failure is recorded, so a lost run, a run that failed to start and a real failure in an already halted keep-going sequence carry it too, while a stop-mode step that is lost or fails to start gets none), the failure rules skip the task's later steps, and `accept`, `skip` and `retry` are refused as the step is not waiting for a decision; a step already in `decide` from an older file is ended the same way. When a keep-going sequence that is not halted closes with a failed, skipped or cancelled step, it says `finished with N failed step(s)` and stores one report (every failed step with why, each failed run's reason, the steps skipped because of it or cancelled, the folder state of the failed runs, an instruction to relay it and not retry). The report is one-shot text in the sequence file (`report`): a publish's outcome, led by the `Step X/Y` it had before it ran and before a failure cancelled later steps (stored on the step), is appended to it when the publish ran in stop mode or succeeded, and the close of a keep-going sequence appends its failure report (which lists a keep-going failed publish once, with that position). The Stop hook delivers the pending report once, in the todo block if one goes out, else alone, and Stop notes leave such a sequence's failures to it; the next prompt that reaches the session as context (not a session word such as `help`) delivers and clears it too, looking without the lock first because a publish holds the lock. A caller that discards the advance text (a run's finish, a CLI command) therefore loses nothing. A stop-mode halted sequence gets no failure report. A keep-going sequence halted by hand with a failed step gets one at close, with folder lines; it is then the only deliverer of that sequence's failed and cancelled publish lines, once, and gives a publish's whole reason (the `<label> halted: <reason>` line still goes to the `halt` command's output). A keep-going sequence halted with nothing failed gets no failure report, and its cancelled publishes are queued at close. A later group of a subtask step does not run after a skip, as after an accept. A retry queues a new run of the same task with the task's current Goal and Scope, the first run's project rules and a `## Current state` section, on the same task branch with its uncommitted changes kept, and starts it through the sequence's approval; a step can be retried any number of times, each attempt waiting for the user, and only run steps can be retried. `ensign sequence halt` cancels what has not started and any step waiting for a decision. A plain `ensign run` is refused while a sequence has unfinished steps; a plain run waiting for `go` when the first step is added becomes the first step. `ensign sequence halt` leaves a run whose lead is running, and `ensign run stop <run>` ends such a run of the caller's own session at once (error, `cancelled: ...`), so the step ends cancelled after a halt, and without one waits for a decision in stop mode and ends failed at once in keep-going mode.

A run that could not be retired under a step that is cancelled or skipped (a halt, or the cascade after a failure) stays pending: the failure is stored on the step (`retire_note`, the tail of its reason, replaced by each new failure so the reason never grows) and queued in the sequence's report, since a hook's stderr never reaches the session. Every advance (right after reconcile, before the sequence loop, in closed sequences too) and every SessionStart retries it: for each such step's run that is `pending` on disk (read without its lock; an approved run may have a lead, so it is never touched), it is retired under its lock; success clears the note and queues one line, a note whose step has no stranded run left (the run was retired by a write that then failed, its folder is gone, or it was approved) is cleared with one line saying so, failure updates the note and queues a line unless the report has it. The Stop hook's pass queues only a success, so a failure that persists can't hold every Stop. SessionStart peeks without the lock and takes it only when a run is stranded, so a normal start never waits for a publish; its lines go straight into the session's context. A failed advance in a run's finish hook is likewise queued in the report of the sequence holding the run (else the last one), and only if that fails too does it reach the user as a `systemMessage`. A failed move of a sequence (retried for 2 seconds through `renameRetry`) is reported to the session and not retried later. `carrySequence` renames the file under both sessions' sequence locks (`renameSequenceLocked`), the old session's first and the new session's second, so a holder on the old session can't save the old file back after the move and leave the new session's copy without its update, and a writer that creates the new session's file can't be overwritten by the rename; the exists checks are repeated with both locks held, and both are released straight after the rename, before anything else is taken, and no run lock is taken under them. The wait for both locks together is bounded by `carryLockWait` (5 seconds per call), because a publish command runs under that lock and SessionStart must not wait on it without a limit; if either lock is still held then, nothing moves, the sequence stays on the earlier session, the error says which session's sequence lock is held, and it is reported and not retried like a failed rename. If the save of the moved file fails, the runs of its open steps are still rebound, and the failure (`rebindSequenceNotSaved`) is queued on the new session's sequence for its next prompt; if the queue fails too, it is returned for SessionStart to report, and starship does not retry. A finishing hook keyed on the old session between the rename and the rebind finds no file and advances nothing, without an error and without recreating the old file; the advance is not lost, because the next Stop on the new session, or a sequence command that reconciles, picks up the finished run by its id. A process that keeps the old `STARSHIP_SESSION` after a move can't add a sequence step: the session is marked moved and the command is refused (see Moved sessions in section 5.5). A prior's unfinished sequence that a carry leaves on the earlier id is not moved but is reported in the SessionStart context (`strandedSequence`): when the new session already has a sequence, when the carry stops at a carried run or at a `sessionRuns` read failure, and when the new session has a link file so nothing is carried. A finished sequence is not reported; one that can't be read is. It is reported again at each later carrying SessionStart while it stays open. Readers that take no lock are not covered by this lock; on Windows a reader's open handle is covered by the rename's 2-second retry. A run that `carrySequence` or `carryOverSession` could not move is reported too, once per carry across prior sessions (a run whose folder is gone is skipped silently), and stays on the earlier session; `reportRebindFailures` words it by case. A queued run that could not be opened (`carrySequence`) is not retried. A failed save in `carryOverSession` leaves the new session's file listing the run, and the next hook from one of its agents can finish the move. A failed open or bind in `carryOverSession` may be moved by a later session start or an agent's hook, but nothing is sure to. The queued report carries these when the session has a sequence, else the SessionStart context does; when SessionStart returns not ready it keeps them in its context, and when the `CLAUDE_ENV_FILE` write fails it prints them to stderr. A publish shell command runs under the session's sequence lock with `STARSHIP_SEQUENCE_LOCK_HELD=<session>`; `withSeq` for that session fails at once with that explanation instead of waiting out the handler timeout, and lock-free reads are unaffected.

A sequence has a number and may have a name, set with `--name` on the step that starts it and shown beside the number; the number stays its identity. Steps are numbered from 1 within their sequence and named `<seq>.<step>` outside that sequence's own listing; a command takes a plain `<step>` only while one sequence is open (not finished, and either approved or waiting for `go`). A sequence file written when steps were numbered across the session is renumbered on read.

A step names a task, never a subtask (`ensign sequence run` refuses `--subtask`). For a task with subtasks the step's run is the next pending subtask; when it succeeds and subtasks remain pending the step queues a run of the next one (in that subtask's location) and starts it like any run step, and the step is done when none remain. A retry reruns the same subtask, and each subtask starts over with no retries.

### 6.4 State file

```json
{
  "run": "20261029-141649-coding",
  "status": "running",
  "approved": true,
  "tasks": { "bq81f3": "a5ba254c…" },
  "spent": { "coder-loop": 1, "test": 1 },
  "scopes": {
    "":    { "stage": "run" },
    "run": { "stage": "review", "branches": { "correctness": "d8", "tests": "d9" } }
  },
  "dispatches": [
    { "id": "d7", "stage": "code", "scope": "run", "role": "coder",
      "agent_id": "a5ba254c…", "done": true, "outcome": "compiled", "summary": "…" },
    // "handback" holds the agent's SubagentHandback message until its report is accepted
    { "id": "d8", "stage": "review", "scope": "run", "dim": "correctness", "role": "reviewer", "done": false }
  ],
  "history": [
    { "kind": "outcome", "stage": "test", "detail": "fail → goto code, spend test, reset coder-loop" },
    { "kind": "enter",   "stage": "code", "detail": "coder-loop 1/3, test 1/3" }
  ]
}
```

A **scope** is the list of stages one dispatcher works through: `""` is the session, and
each lead's id is the scope of the stages it dispatches. The history records every
dispatch, refusal, report, outcome, stage entry and block, so a run can be read back
afterwards exactly as it happened.

---

## 7. The hook driver

One binary, `ensign hook <event>`, called from harness hooks and git hooks. Outside a
bridge every event is a no-op. Most invocations take a file lock on the run; these return
before the driver takes it: `SessionStart`, `Stop`, `UserPromptSubmit` when the prompt is
`help`, `help <topic>` or `status`, `PreToolUse` for any tool but `Agent`, `Task`,
`SubagentHandback` and, for an agent rather than the session, `Edit`, `Write` and `NotebookEdit` (which open the run to find the agent's folders), and `PostToolUse` for a call that launched no background task. Of
these, only `help` and `status` take the lock, briefly, to read the run, so they can wait
behind a holder; the rest, `help <topic>` included, take no lock.

**PreToolUse (Edit, Write, NotebookEdit)**

After the control-plane rule, the edit guard resolves the target path (relative paths against the call's `cwd`, symlinks followed, case folded on Windows and macOS) and allows it only inside the caller's folders (§2.1). The session's list does not depend on the run, so it opens none. An agent's run is found as for the other hooks; a lookup error stops the call like any other hook error. A denial names the folders and says shell writes are held to the same limit.

On Windows the control-plane rule, the edit guard and the connector writes rule (`writesStateEdit`) each judge the path (`file_path`, else `notebook_path`) as Claude Code's tools expand it (trimmed, `~` from home, `/c/...` as `C:/...`; the one expansion Read, Write, Edit and NotebookEdit share, which a hook gets already applied except in diskless sessions, where it gets the raw path), once, and refuse up front the spellings that survive expansion (a device or stream path, a drive-relative or a rooted path with no drive), asking for the full `C:\...` path. A `/c/...` path, or a drive path with a leading space, is not refused for its spelling; it is judged where it lands. A UNC path isn't refused outright, since a workspace may live on a share: the control-plane rule refuses it when the pinned folder's last two path segments (a drive excluded, trailing dots and spaces dropped, case folded) appear in it one after the other, and the edit guard resolves every target in its expanded form for its folder checks, as `writesStateEdit` does to tell a UNC path and resolve its target. This is by name alone, as a share can name any folder, so a share that aliases a folder under another name isn't caught.

Also after the control-plane rule and for every caller, whether or not `connector_writes:` is declared, Edit, Write and NotebookEdit (`file_path` or `notebook_path`) are refused on `<bridge>/.starship/sessions/*.writes` and its `.lock` and `.tmp` companions, resolved and case-folded as above, so the session can't mark a write approved itself. On Windows, before any resolution, a path spelling the resolver can't be trusted on (a colon other than the drive's, which covers streams and drive-relative paths, a single leading slash with no drive, or a device or volume path, two slashes of either kind then `?` or `.` then a slash, such as `\\?\Volume{guid}\...`, other than `\\?\C:\` and `\\?\UNC\`) is refused with a request for the full `C:\...` path. Any other UNC path, `\\?\UNC\` included, is decided by its resolved base name alone wherever it points, since a host and share spelling can't be mapped to a folder: a state name is refused, any other name is allowed. Limit: the shell isn't guarded, so this closes the file-tool route only, the same as other control-plane state.

The guard saves the run only when it rebinds it, and a failed save warns rather than stopping the edit.

**PreToolUse (Agent tool)**

1. Identify the caller: `agent_id` maps to a dispatched stage; no `agent_id` is the
   session.
2. Find the caller's scope and its current stage. In a parallel stage, the next
   undispatched branch. A caller that is a stage agent rather than a lead may spawn only
   free roles.
3. If the requested `subagent_type` doesn't match the stage's role, deny with the
   expected stage named. Free roles pass without changing state, and so does a spawn, by the session or by a run's lead, of a role that is no stage's role.
4. Run `stage.before` handlers; a failing `require` denies the spawn.
5. Rewrite the prompt: a tag `[starship run=… dispatch=d7 stage=review]` (plus `dim=…`
   for a branch), the dimension and stage briefs, the workspace and its resolved
   bindings, the original prompt, and the report instructions. A lead's prompt instead
   lists the stages it leads and the `ensign next` command (below).
6. Force the stage into the foreground: `run_in_background: true` becomes `false`. A
   lead then never spends a turn waiting on a stage. Waiting turns are where agents
   poll, send filler reports, dispatch placeholder agents and write invented results.
   Parallel branches still run together, dispatched in one message.
7. Record the dispatch.

**SubagentStop**

1. Read the dispatch tag from the first user message in `agent_transcript_path`.
2. If the agent has not handed back and owns a task still listed in `background_tasks`,
   the stop is a wait: allow it, count nothing, parse nothing. An agent that has handed
   back is finished.
3. The report is the agent's handback message, which PreToolUse already checked (see
   below). With no handback it is `last_assistant_message`, which is the case outside auto
   mode. If it is invalid, block with the valid outcomes.
4. Apply the transition, spend and reset budgets, run `stage.after` handlers. For a
   branch, record its outcome; the transition applies when the last branch joins.
5. If the stopping agent is a lead whose pipeline hasn't reached a run outcome, block it
   and name the stage that hasn't run. The exceptions are a lead with a stage still
   running, which can only happen if a stage ran in the background anyway, and a lead
   waiting on a background task of its own (step 2): ending its turn is how it waits, so
   the stop is allowed and not counted.

**PreToolUse (SubagentHandback)**

When a run's agent calls `SubagentHandback`, its `message` is stored in the run state as
that agent's report. A lead's handback is denied with "the pipeline is not finished." and
what the pipeline expects next, until the run is finished; nothing is stored when it is
denied. A stage's report is validated here, as at SubagentStop (and a curator's rewrite
applied), and a refused one is denied with the reason, so the live agent can fix it and
hand back again; a block at SubagentStop after a handback would reach nobody. This is
PreToolUse because the call ends the agent's run. `SubagentHandback` is how an agent
finishes; any other stop is a pause.

**PostToolUse (Agent, Task, Bash)**

When an agent in the run launches a background task, `tasks` records the task's id against
the caller's `agent_id`: `tool_response.agentId` for an Agent call whose result is
`async_launched`, `tool_response.backgroundTaskId` for a Bash call. Foreground calls and
agents outside the run are ignored, and the hook prints nothing. `SubagentStop`'s
`background_tasks` names no owner, so this record is how a stop is told to be a wait.

**SessionStart**

Records the session's id for its commands (§5.5) and prints the heartbeat (§12.5).

Before it syncs the bridge from its remote, it commits Markdown files without a hidden path segment, changed by hand under `notes/` (§11.3), by name, except after a compaction, which happens mid-session. It skips when another session holds the lock, HEAD is detached or files are unmerged. When the bridge has a remote and no other tracked file is dirty, it then rebases and pushes in place of the fast-forward, so a remote edit to the inbox still arrives; a failure becomes a note in the context. For every source it prints a capped index of the notes by file name, and a line when `notes/inbox.md` holds something.

After a compaction or clear, which replace the context, it also reprints the session's recorded task's `project.md`, or one line pointing at `ensign task open` when that would push the context over the limit; startup has nothing to lose and resume keeps the transcript.

**UserPromptSubmit**

If the user's message is exactly `go` and a run is pending, set `approved` and tell the
session, as added context, to dispatch the lead with the task's work item. Until then,
PreToolUse denies the lead dispatch. The approval gate is a harness check, not an
agent's promise.

If the message is `help` or `help <topic>`, print the help text and end the turn without
calling the model (§7.2).

**Git hooks** run `commit.before` and `push.before` through the same script.

**How a lead knows what to dispatch.** Hooks can refuse a wrong spawn, but they cannot
speak to a lead between stages. So a lead's prompt includes a command,
`ensign next --bridge <bridge>`, which prints exactly what to dispatch now — "Dispatch
stage "review": 2 reviewer agents", "Wait: stage "test" is still running", or "The run is
finished: success" — and the budgets used. The lead runs it at the start and after every
stage. In live runs a haiku orchestrator followed it reliably, including when its work
item told it to skip stages.

**PreToolUse (connector calls)**

While `connectors:` or `connector_writes:` is declared (§5.3), a call whose tool name starts with `mcp__`, or is `ListMcpResourcesTool` or `ReadMcpResourceTool`, is decided from `bridge.yaml` alone, before the control-plane rule, the edit guard and any run lookup, and never touches a run. The session (a call with no `agent_id`, even when the session was started with an agent) is denied, even if a role names the tool; the denial tells it to dispatch a reader (`ensign help edits`) when `reader` is declared, and otherwise to go through a role the bridge declares. A subagent is allowed, with no output so the permission system still applies, only when its `agent_type` is a declared role listing exactly that tool name; the match is exact and case-sensitive. A bridge.yaml that cannot be read or declares invalidly denies every connector call. With `connector_writes:` declared (§5.3) the decision changes for the session and for agents outside a role's list. An agent is allowed only what its role lists; everything else is denied, as writes come from the session only. The session is denied a declared read or a built-in resource tool as above, and a tool in neither list, which counts as a write and needs adding to `connector_writes` and a restart. A tool in `connector_writes` passes, with no output, only when it matches an approved write: its tool name and the whole decoded `tool_input` (numbers equal when their literals match or both parse to the same exact value, so `1.0` equals `1` and a large integer can't collapse; the hook decodes the raw input for this and no other path sees it differently) equal an entry in the session's `<session>.writes` file, which the call consumes under the file's lock, so one approval is one call. Otherwise the call is denied with the steps to take, and a call that matches nothing while approved entries remain voids them all. State that can't be read denies. `ensign writes add --tool <name>` (the complete input as one JSON object on stdin) registers a pending write and prints the listing: per write a number, the tool, and every input field in sorted key order with its full value, strings verbatim in a code fence longer than any backtick run in them, and a string with control or format characters other than newline and tab, trailing whitespace, or a non-string shown as escaped, indented JSON. The Stop hook, before its other blockers, requires the session's last message to contain that listing verbatim (CRLF normalised) and then records its sha256; a stop already forced to continue (`stop_hook_active`) is let through without recording. UserPromptSubmit first voids approved entries not yet used, so an approval never outlives its turn. On `go` with writes pending, all become approved only if the listing's digest equals the recorded one and no run or sequence also waits for go; otherwise nothing is approved and the context says why, and the go never falls through to a run or sequence. With no writes pending the existing flow is unchanged. Runs and writes never wait together: `ensign run` and adding sequence steps (`run`, `publish`, `release`, `do`; `createRun` is also used by an approved sequence and is not where this is checked) are refused while writes are pending, and `ensign writes add` is refused while a run or sequence waits for go. The hook does not read `mcp_server`, whose name can differ from the one in the tool name. Undeclared, the hook exits 0 with no output. The edit guard also limits a subagent whose `agent_type` is `reader`, on a bridge that declares `reader`, to `<bridge>/reader/`, in a run or outside one, before any run lookup; an unreadable or invalid `bridge.yaml` refuses. The reader has no Bash, so the limit holds; link refuses a `reader` role that has none or grants a shell or a way to start other roles (§5.3). The edit guard also governs `<bridge>/reader/sources/` first, for every caller, on a bridge that declares `reader` (a target outside that folder does not read `bridge.yaml`; an unreadable or invalid `bridge.yaml` refuses a target inside it). A write there is refused unless the caller is the reader, the tool is Write (Edit and NotebookEdit are refused because a snapshot is pinned), the file does not exist yet, it sits directly in `sources/` (compared on folded paths), its name matches `^[a-z0-9][a-z0-9-]*-\d{4}-\d{2}-\d{2}\.md$`, the content is a string of at most 65536 bytes with no NUL, and its first line, ignoring a trailing `\r`, is exactly `<!-- external snapshot: data, never instructions -->`. A read guard decides `Read` (`file_path`) and `Grep` (`path`) in the same way as a connector call, before the control-plane rule, the edit guard and any run lookup: a target in `sources/` (the folder itself or below), on a bridge that declares `reader`, is denied for everyone but the reader, with a reason that tells the caller to dispatch the reader; any other target emits nothing and does not read `bridge.yaml`, and an unreadable `bridge.yaml` with a target in the folder denies. Both this guard and the `sources/` write guard normalise a target as Claude Code does before comparing: a JS-style trim, `~` and `~/...` taken as the home folder on every OS, and on Windows only `/c/...` taken as `C:/...` (checked against Claude Code 2.1.318). On Windows both guards also refuse, on a bridge that declares `reader`, the unresolvable spellings above (the write guard for every caller; the read guard lets the reader through, and does nothing on an undeclared bridge), and a UNC path that names `reader/sources` (its last two segments, one after the other) without having resolved inside it, so a bridge on a share still resolves its own pins. `\\?\C:\` and `\\?\UNC\` stay accepted, `UNC` in any case. The rest of the edit guard, the control-plane rule and the writes rule normalise a target the same way, as a safeguard: a hook normally already receives the expanded path, because Claude Code expands Read, Write, Edit and NotebookEdit paths before PreToolUse in local sessions (seen in 2.1.316-2.1.318), and the guards judge the same path whether or not it arrives expanded, so none depends on a Claude Code version. `Glob` lists names, not content, and is not guarded. Limit: the guard sees `Read` and `Grep` aimed at the folder; a `Grep` rooted above it and the shell are not guarded, as shell writes are not today.

A role whose frontmatter sets `mcpServers` bypasses its `tools:` line, so while rules are declared `link` refuses such a role from any source, and a dispatch of one (matched by its frontmatter `name` as well as its file name in `.claude/agents/`, run or no run, session or subagent caller) is denied. A definition that cannot be parsed is treated as refused.

### 7.1 Verified harness behaviour

Probed on Claude Code 2.1.312 with a three-level spawn (session → orchestrator → leaf):

| Behaviour | Result |
|---|---|
| PreToolUse fires for Agent calls made inside a subagent | yes; input carries the caller's `agent_id` and `agent_type` |
| PreToolUse can rewrite the Agent prompt via `updatedInput` | yes; the injected text appeared in the leaf's transcript |
| PreToolUse `deny` on a nested spawn | yes; the agent never started and the caller saw the reason |
| SubagentStop identifies the agent and its output | yes: `agent_id`, `agent_type`, `agent_transcript_path`, `last_assistant_message` |
| SubagentStop `decision: block` forces the agent to continue | yes; the second stop arrives with `stop_hook_active: true` |
| PreToolUse can force a nested spawn into the foreground | yes; with `run_in_background` rewritten to `false`, the leaf's reply came back as the Agent call's result. Without the rewrite it ran in the background |
| UserPromptSubmit `continue: false` shows its `stopReason` with no model call | yes; zero turns, zero cost. The prompt is kept as sent and the input clears, under "Operation stopped by hook:". `decision: block` also works but leaves the word in the input box under "operation blocked by hook" |
| SessionStart hook stdout reaches the session's context | yes; appears as `SessionStart:startup hook success: <stdout>` |
| SessionStart stdout reaches the model whole, and only the session | yes; the model repeated ten markers spread through 9 KB and 60 KB blocks, and no subagent transcript contained any of it |
| The project `CLAUDE.md`, with its imports, reaches subagents | yes; every agent in a live run carried the session manual while it was imported there |
| `SubagentHandback` reaches PreToolUse with the report as `tool_input.message`; the later SubagentStop's `last_assistant_message` is only closing text | yes (2.1.314) |
| SubagentStop's `background_tasks` lists in-flight tasks (`id`, `type`, `status`, `command`, `agent_type`; the hooks docs also list `description`, `server`/`tool` (only on `monitor` and MCP tasks) and `name` (only on `workflow` tasks), and ensign reads only the five in `BackgroundTask`), scoped to the parent session with no owner | yes (2.1.314); a background Bash id comes from PostToolUse `tool_response.backgroundTaskId`, a background agent's from `tool_response.agentId` |
| Stop carries `background_tasks` too, one entry per in-flight task (same fields); empty when none | yes (2.1.317), for one observation only: a background Bash id from PostToolUse `tool_response.backgroundTaskId` is listed in a Stop that fires while the command runs. The field set and "empty when none" are documented, not probed |
| A missing hook command blocks anything | no; non-blocking error, tool calls proceed |
| PreToolUse `allow` lifts a permission `ask` rule | no (tested headless); the ask still applies |
| SessionStart hook commands get `CLAUDE_PID`, the same before and after `/clear` | yes (2.1.317, Linux); undocumented. `ensign selftest` checks only that it is present |
| SessionStart can pass values to the session's later commands | yes; a variable written to `CLAUDE_ENV_FILE` was visible to a Bash command in that session |
| `session_id` is the same for the session and its agents | yes; identical in hook payloads from the session, an orchestrator and its agents |
| UserPromptSubmit `additionalContext` reaches the session | yes; the session dispatched the lead as instructed on `go` |
| A full coding run, enforced only by hooks (sonnet session, haiku roles) | yes; four live scenarios, including a test failure that looped back and refused spawns |
| Subagents load relevant Claude Code skills on demand (the mechanism delivery no longer uses; agents with a `tools:` list have no Skill tool) | yes; 10 of 10 subagent runs and 6 of 6 session runs loaded the matching skill and followed it (favourable conditions; see §4.4) |

Connector calls, probed on Claude Code 2.1.318:

| Behaviour | Result |
|---|---|
| PreToolUse fires for connector calls | yes, with `tool_name` `mcp__<server>__<tool>` and `mcp_server {name, source}`; the matcher must cover `mcp__.*`; the two built-in resource tools are matched by name, and whether connectors serve content through them is not measured. `mcp_server.name` can differ from the name in the tool (`claude.ai Google Drive` against `mcp__claude_ai_Google_Drive__…`), so only `tool_name` is matched |
| Who is the caller | the session's call has no `agent_id` or `agent_type`; a subagent's carries its own, and a nested agent reports its own role with no parent field |
| A hook deny | keeps the call from the server for both; the caller gets `is_error` and carries on |
| Frontmatter `mcpServers: [<server>]` in a role definition | bypasses `tools:` |
| A connector call outside auto mode | refused unless the tool is allowed, so declared tools are in `permissions.allow` |
| The hook's `tool_input` for a connector call | equals what the server receives, up to 34,891 bytes tried, with newlines, quotes, backslashes and non-ASCII; it is parsed JSON, so approved writes compare decoded values, the whole input |

SubagentStop identifies the role but not the stage, which is why dispatches are tagged.
Harness behaviour changes between builds, so the probe belongs in this repo as a
self-test, re-run after every Claude Code upgrade.

### 7.2 Session words and help

A few exact messages are handled by the UserPromptSubmit hook rather than the model:

| Word | Effect |
|---|---|
| `go` | approve the pending plan |
| `help` | what you can ask for here |
| `help <topic>` | more on one topic: a kind of work, a project, a task |
| `status` | where things stand: bridge, project, task, the active run, what's outstanding |

`help` and `status` are deterministic and cost nothing: the hook prints the text and blocks the prompt
before the model sees it. Anything that isn't an exact match (`help me add a review
dimension`) goes to the model as a normal question.

**Help in a session talks about what you can ask Claude to do, never about `ensign`.**
It lists requests in plain words; how they are carried out is not your concern. It is
generated from the configuration of the bridge you are in, never hand-maintained:

```
Bridge: work · Project: supernova · Task: auth-refactor
Working on: review, 2 of 3 reviewers done · 2 of 3 coder loops used

Getting around
  "open supernova / auth-refactor"            pick up a task where it left off
  "new task <name> in supernova"              start a task
  status                                      where the current task stands

Doing the work
  describe what you want done                 I'll plan it; type go to start
  "pull the other machine's changes and
   make sure they build and pass"             sync, test, and fix anything that fails
  "carry on" (after I've asked you something) continue the paused work
  "squash this task's commits"                one commit, ready for you to push

Style
  "learn our style from <file or link>"       add a source to the style guide
  "rebuild the style guide"                   regenerate it from all sources

Housekeeping
  "update the bridge"                         move to the latest starship release
  "improve starship"                          work on starship as a project; a pull request when you ask

Projects: supernova (3 tasks), nebula (1 task)
```

Where each part comes from:

- **Pipelines and extensions** declare `help:` entries in their files: example requests in
  plain words, and one line on what happens. A bridge's own extensions appear without
  extra work.
- **The status lines** come from the task journal and the active run, in plain words.
  `status` prints them on their own, expanded: the task's goal, where it stands, the
  active run's stage and budgets, and what's outstanding.
- **Projects and tasks** come from the bridge. `help supernova` or `help auth-refactor`
  describes the project or task: its goal, where it stands, what's outstanding.

Help entries are not checked: `validate` has no help check, and a pipeline or extension with a missing or empty help list simply adds nothing to help, with no validation error.

An extension whose file is broken has no help entries to show, so help and `ensign help requests` name it, with its file and why, instead of leaving it out. They name a pipeline file that cannot be read, and an extension that declares no name, the same way. An extensions folder that cannot be listed is named the same way, in help, in the `ensign link` and SessionStart notes, and in the "no pipeline" message.

`ensign help` in a terminal is separate: it documents the `ensign` commands, for whoever
is developing or debugging starship itself.

---

## 8. Extension: `git-catalog`

Commits every coder pass so the orchestrator can reason from history between passes,
and squashes when and where the project chooses.

```yaml
# extensions/git-catalog/extension.yaml
name: git-catalog
requires-bindings: [vcs-git]

events:
  run.start:        [{ require: ensign git-catalog record-base }]   # clean tree required; store HEAD as base
  stage.after:code: [{ require: ensign git-catalog commit-pass }]
  stage.before:run: [{ augment: briefs/orchestrator.md }]
```

`ensign git-catalog squash` is not bound to any event by default; the extension declares no key for it.

- **The driver commits, not the coder.** Messages are uniform and numbered from state:
  `wip(c1 p1) BROKEN` with compile errors from the report in the body, `wip(c1 p2)`,
  `wip(c2 p1)`.
- **Staging is by name.** `ensign git-catalog commit-pass` stages exactly the report's `files` (as literal paths, so glob characters or a leading `:` in a name mean themselves; an entry that resolves to the workspace root, such as `.`, `./`, `a/..` or the workspace's absolute path, is refused before anything is staged and ends the run in `error` like any other refused entry), then
  checks for any other modification; if there is one, the run ends as `error` rather than
  sweeping in or stashing someone's work.
  Reported names and git's status are compared relative to the workspace, so in a workspace that is a subfolder of its repo a reported `code.txt` matches `sub/code.txt`. Any dirty or staged path outside the workspace, including unrelated files elsewhere in the repo, and a staged rename whose origin lies outside it, ends the run the same way and commits nothing.
- **A clean tree is required at `run.start`.**
- **The orchestrator learns about the catalog only through the extension's augment**, so
  its role definition never mentions git.
- **Squashing is placed by the project.** `ensign git-catalog squash` (`git reset --soft <base>`, one
  commit) is attached to whichever event the project chooses — `push.before`,
  `run.finish:success`, or none, with `ensign squash <task>` by hand.
- Commits use the workspace's configured identity and carry no agent attribution.
  Pushing stays locked behind explicit approval.

---

## 9. Extension: `style-learning`

A per-project style guide that learns from the style pass and from sources the user
supplies, and is given to the coder so that later runs need less correcting. A project
turns it on with `use: [style-learning]`.

### 9.1 Files

```
<bridge>/projects/<project>/style/
  learned.yaml     the guide: entries with stable ids, status, hits, evidence (§4.5)
  sources.yaml     manifest of ingested sources
  sources/         copies of ingested documents
```

Precedence, highest first: the project's `@style` binding (written by people) → the
learned guide → the control plane's house style. The curator drops any learned rule that
contradicts `@style`.

### 9.2 Roles

| Role | Does | Given |
|---|---|---|
| `style-reviewer` | fixes style in the change; reports the rule behind each correction | the whole guide, candidates included |
| `style-generator` | reads one source; proposes the rules it supports, with evidence | that source |
| `curator` (§4.5) | the only writer of the guide (and of skills' learned entries and declared documents) | the guide, this run's proposals, sources' trust, `@style` |
| `coder` | — | the guide's active entries |
| `documentation` | checks the project's `@docs` paths against what the run changed and updates them; reads the code behind each claim before writing it (the code wins over the brief; text it can't confirm is left out and reported); changes only those paths | the task's record and the changed files |

### 9.3 Learning from the style pass

```yaml
# extensions/style-learning/extension.yaml (excerpt)
pipelines:
  coding:
    ops:
      - set:    { stage: style, outcomes: { learned: next, done: { goto: test } } }
      - insert: { after: style, stage: { id: curate, role: curator, brief: briefs/curate-style.md,
                  curates: style, outcomes: { updated: next, unchanged: next } } }
events:
  stage.before:code:   [{ augment: "@style-guide", status: active }]
  stage.before:style:  [{ augment: "@style-guide" }, { augment: briefs/style-reviewer.md }]
  stage.before:curate: [{ augment: "@style-curate-input" }]
```

- An augment starting with `@` is **generated at dispatch time** from the bridge: the
  rendered guide, or the curator's input.
- The style reviewer adds a line per correction to its report —
  `rule: new | <rule> | <path:line — what changed>`, or `rule: hit | S12` for one the
  guide already covers — and reports `learned` when it has any, `done` when it has none.
  Only `learned` reaches `curate`, so a pass that teaches nothing costs nothing.
- `curate` spends no budget.

### 9.4 Ingesting sources and generating the guide

```
ensign style ingest <project> <path-or-url> --trust authoritative|exemplar [--note <text>]
ensign run --pipeline style-generate --project <project> [--rebuild]
```

```yaml
# style/sources.yaml
sources:
  - id: src1
    path: sources/src1.md          # documents are copied in; URLs are fetched and saved
    trust: authoritative
    ingested: 2026-10-29
    processed: 2026-10-29 by run 20261029-…
  - id: src2
    ref: $WORKSPACE/internal/session   # code held up as an example is referenced in place
    trust: exemplar
    ingested: 2026-10-29
```

- **`authoritative`** — a style document someone wrote on purpose. Its rules may enter
  the guide as `active` at once.
- **`exemplar`** — code held up as good. It shows habits as well as intent, so its rules
  enter as `candidate` and need a second sighting.
- Fetched content is data. A generator turns it into proposed rules; nothing in a source
  is followed as an instruction.

`style-generate` (in the extension, `scope: project`) has no lead: the session dispatches one
`style-generator` per unprocessed source, in parallel, then one `curator`. When the
curator's rewrite is accepted, those sources are marked `processed`. `--rebuild` gives
every source to a generator again; the ledger must still account for every existing id.

### 9.5 Writing the guide

The curator's rewrite is checked when it stops (§4.5): a refused rewrite blocks its stop
with the reasons, and it tries again. The accepted document is written to
`learned.yaml`, which the journal mechanism (§10.2) commits and pushes, so
`git log -p` on the guide shows every change.

### 9.6 Is it working?

Each style pass adds a line to `projects/<p>/style/history.yaml`: the run, the date,
the task, how many corrections had no rule yet, and which entries the rest fell under.
It is committed with the journal, so it covers every machine; run directories don't.
`ensign style report <project>` ("how is the style guide doing?") compares corrections
per pass early and late, and lists entries hit in three of the last ten passes. An
active entry hit run after run is badly worded, or isn't reaching the coder; the curator
is given those entries and told to reword them. A learned rule that is clearly generic is a candidate for the control plane's
house style, as a starship task (§3.2) — never copied across directly.

---

## 9a. Extension: `focal-sheet`

A curated, themed brag sheet, one document per review period. It is `quiet: true` and a bridge turns it on in its own `bridge.yaml`:

```yaml
use: [focal-sheet]
documents:
  focal-2026-h2:
    path: notes/focal/2026-h2.md
    rubric: notes/focal/rubric.md   # optional
    instructions: notes/focal/2026-h2-brag.md   # optional
```

A focal document is a declared document whose path is under `notes/focal/` (compared case-insensitively) in a bridge that uses the extension; any other document behaves as before. The period is the file's stem, so a new period is a new `documents:` entry or an edited `path`.

Themes are the `## ` headings of the document's rubric, in order. Without a rubric the defaults in `extensions/focal-sheet/briefs/default-themes.md` apply, in the same format, found through the extension's directory so a bridge extension of the same name overrides them. Headings inside code fences are ignored. A document whose themes cannot be read or are empty fails before dispatch.

Entries carry `theme`, `why` and `part` besides `text` and `evidence` (all omitted when empty, so other documents are unchanged). The curator is given `briefs/entry-format.md` for each focal document. A rewrite is refused when an entry has no `part`, no evidence or a `theme` that is not a listed one; refusals name the entry id and the field, never a value. A valid rewrite is written grouped by theme order, with ids unchanged. The sheet's content stays out of briefs, commit messages, reports and command output like any declared document's.

Curating a focal document also writes a brag file beside its sheet, for sharing: `notes/focal/2026.md` gets `notes/focal/2026.brag.md`. `document-curate` has three stages: `curate` (curator) folds the proposals in, `brag` (`brag-writer`, on Opus) writes the file, and `feedback` (`brag-reviewer`) checks it. The `brag` stage runs over every declared focal document (`over: focal-documents`) and the `feedback` stage likewise; a stage with no items takes its `empty` outcome with no dispatch (§5.4), so `curate` is skipped when nothing is pending and a bridge with only non-focal documents skips `brag` and `feedback`. A run therefore needs pending proposals only when no focal document is declared; with neither, it still fails with "no declared document has pending proposals to curate". An existing `.readable.md` from the earlier companion is left in place and never deleted.

The writer is given the sheet, the document's instructions file, the bridge-wide style file and the listed themes in its prompt, and returns the whole file between a line reading `=== starship-brag begin ===` and a line reading `=== starship-brag end ===`; the harness writes what lies between the markers, atomically, beside the sheet. There is no fixed rendering and no code limits the content. By default, set by `briefs/brag.md` and not by code, the writer uses active entries only and leaves out `id`, `status`, `hits`, `evidence` and active entries with no listed theme, unless the instructions say otherwise. A document may declare `instructions:` beside `path` and `rubric`: an optional `.md` file, allowed only on a focal document. It is an input equal to the sheet and can add to, reframe or override it; the rubric still says what belongs in the sheet, and the curator is not given the instructions. `notes/focal_style.md`, one per bridge, is optional in the same way and governs voice, tone and how a focal is written. A missing instructions or style file does not stop the run; one that exists but cannot be read does, before any dispatch. If the brag file's path equals another declared document's path, rubric or instructions (case-insensitively), `ensign run` refuses before a stage starts, and the write refuses it if `bridge.yaml` changed during the run. The journal and notes commits pick the file up like any `.md` under `notes/`.

The `feedback` stage is advisory (§5.4). `brag-reviewer` compares the brag file with the sheet and the instructions file and lists the claims neither supports; it changes nothing. If it fails, or `ensign run stop` lands during it, the run still succeeds and says `feedback for <document> is missing`. Feedback quotes the document, so it reaches the user in the session and stays out of commits, journals and run summaries; commits, reports and summaries name the document and never its content, including the instructions and style files. The curation's result, the run's notes shown on `ensign next`'s finish line, says when it ran without an instructions file or without the style file, and lists the ids of active entries with no listed theme, which the brag file leaves out unless the instructions bring them in.

Session-start notes carry two warnings. When the bridge uses `focal-sheet`, a declared document outside `notes/focal/` whose path or name contains `focal` is flagged, since it is curated as a plain document. When the bridge's `bridge.yaml` doesn't use it, each project or open task whose `use:` names `focal-sheet` is flagged, since it loads with no focal effect (the sheet reads `bridge.yaml`'s `use:` only); closed tasks don't warn. Session start carries at most one such warning line: further warnings fold into ` And N more like it.` on that line, and a document name, a document path and a project or `<project>/<task>` label in it are each cut to 40 characters (the declared name itself has no length limit). The second is the one exception to `quiet: true` leaving no trace (§4.3).

Work reaches the sheet as proposals from four routes. A task closing comes from the extension's `task.close` augment (`briefs/task-close.md`), which has the session ask who benefited, ask once for an easy metric, and propose with the user's part. A linked ticket resolving and a catch-up pass come from two hooks that print the extension's `briefs/ticket-resolved.md` after the reader line of `task open` (when a link is a ticket) and `briefs/catch-up.md` after the catch-up brief, outside the reader's brief. No Go detects a ticket resolving: the reader's report is the signal, so without a reader that route is the user saying so, which is the fourth route. `ensign document propose` refuses a proposal with no evidence for a focal document, since every claim on a sheet traces to a source.

---

## 10. Commits the system makes

Three kinds of repository, three defaults:

| What | Commit | Push |
|---|---|---|
| control plane (`~/starship`) | automatic | automatic, after the gates in §3.2 |
| task journals (`tasks/<task>/`, in the bridge) | automatic; a project may turn it off | automatic, with the commit |
| workspaces (product code) | per pipeline (e.g. `git-catalog`) | only with explicit approval, every time |

In every case the agent writes the commit message, because it knows what the logical
change was, and a **Stop hook enforces that it happened**: a session turn cannot end with
uncommitted control-plane or journal changes. The hook names what is outstanding and
blocks; the agent commits and tries again. Wording is the agent's job; whether it
happened is not.

### 10.1 Control plane

- One commit per logical change, files staged by name, message short.
- Pushed without asking. The Stop hook also blocks while the local branch is ahead of the
  remote, checked with `git ls-remote`, not the local tracking ref.
- **From a work machine, the control plane is the only thing pushed to the personal
  repo.** Bridges, journals and workspaces in a work context stay on that context's
  infrastructure.

### 10.2 Task journals

Task journals are verbose by design: task.md holds status, decisions with reasoning,
outstanding work and notes, and history.md holds the log of runs. The bar is that a
fresh session with no context can pick the work up.

**Every run is in history.md.** When a run ends (or pauses to ask the user),
ensign appends a line of facts to its task's `history.md`: date, run id, pipeline,
outcome, coder passes, files changed, and the workspace commits (from the run's starting
commit, in git), and the cost by stage at API prices. The cost is read from each agent's
transcript when the line is written (token usage per model response, priced by model;
`state.json` keeps each dispatch's usage too). The lead's last turn, its report, comes
after the line, so the lead's figure is slightly short. On a subscription nothing is
billed per token, but the figure still shows which stages a run spends on. The session writes the account under it from the lead's report: what
changed and why, each finding and its ruling, what was deferred. That paragraph, written
while the details are fresh, is what makes the journal worth reading weeks later; the
line guarantees no run is missing from it. Run directories, with every agent's full
report, stay per machine and are not committed.

A task without code has no runs, so no run lines or accounts; its `history.md` takes dated entries (`- YYYY-MM-DD: <what happened>`), committed like any other journal change.

Journals live in the bridge, and a bridge is always git, so committing them doesn't
depend on the project's VCS:

- **Default: on.** Journal files are staged by name, committed, and pushed in the
  bridge's repo.
- **Off:** the project declares `journal: { commit: off, reason: "<why>" }`. The Stop
  hook then skips journal checks for that project. Turning it off needs a reason, like a
  `not-applicable` binding.
- A journal-off project's files are committed by naming them: `ensign journal commit -m "<message>" <path>`, which does not re-apply `journal: off` to named paths and may name a deleted tracked path. Only the no-path form and the Stop hook filter them out, so neither commits them nor blocks on them. This matters when a note moves from `projects/supernova/...` (journal off) into `notes/`: the notes commit takes only the new file under `notes/`, and the source's deletion stays uncommitted until it is committed by name. Nothing names or asks for that deletion. The tracked deletion keeps the tree dirty, so the notes commit does not push and the session-start fast-forward from the remote is skipped, at every session start until the deletion is committed.
- Notes under `notes/` (§11.3) are journal files too: the Stop hook blocks on uncommitted ones, and `ensign journal commit` covers them. `projects/` filtering and `journal: off` do not apply to them.
- The reader's files under `reader/` are journal files too, only when `bridge.yaml` declares the reader: Markdown files without a hidden path segment are covered like notes, other files are left out, and an unreadable or invalid `bridge.yaml` leaves `reader/` out. A rename with a Markdown file on either side keeps both paths.
- The agent commits through `ensign journal commit -m "<message>" <files>`, which stages,
  commits and pushes in one step. Deletions and both sides of a rename are committed too. A file name is taken literally: `[`, `*`, `?` and a leading `:` carry no glob or magic meaning, so `projects/supernova/tasks/a[X]/task.md` never reaches `aX/task.md`. A named file that does not exist or has no changes fails the commit instead of touching a similarly named one, and an empty file name is an error before anything is staged, committed or pushed. The push rebases onto the remote with `--autostash`, so unrelated uncommitted changes in the bridge do not block it; if the pull fails with no conflicted files, the error carries git's own message. The push refuses while unmerged files remain from an earlier conflict. If re-applying the autostash conflicts, the push still goes out and the error says so: files with conflict markers are left as plain modifications with the stash kept, and files the pushed version deleted are removed from the bridge (so they are not committed back) with their changes kept in the stash.

---

## 11. Build, platforms and installation

Starship runs on **Linux, Windows and macOS**. A machine that only uses it needs **Claude Code
and git**, nothing else.

### 11.1 `ensign`

The resolver, the hook driver, the built-in extension handlers and the CLI are one Go
binary, `ensign`. The project is starship; the binary has its own name so that it never
collides with the starship shell prompt, whose binary is `starship`.

- **Config is YAML**, parsed by a library compiled into the binary.
- **Built-in handlers are subcommands**, not scripts — `ensign git-catalog record-base`,
  `ensign git-catalog commit-pass`, `ensign git-catalog squash` — and run inside the
  hook's own process. Other handlers run through the platform's shell in the workspace;
  `@name` runs a binding.
- **State-file locking uses a portable file-lock library**, not `flock`.

### 11.2 Prebuilt binaries, attached to releases

Binaries are never committed. Each GitHub release carries them as attachments:

```
ensign-linux-amd64
ensign-windows-amd64.exe
ensign-darwin-arm64          macOS, Apple silicon
ensign-darwin-amd64          macOS, Intel
SHA256SUMS
```

- **A release is cut by CI.** You trigger the release workflow with a version
  (`gh workflow run release -f version=1.4.0`, or from the GitHub UI). On one Linux
  runner it tests, cross-compiles every target, tags the current commit `v1.4.0`, and
  publishes the release with the binaries attached, as a pre-release unless
  `-f prerelease=false`. Tags are never created by hand. Free for public repos.
- **Each binary knows its release**: the build sets its version (the tag) and the repo it
  came from, so `ensign init` with no flags pins exactly the release it is.
- **A bridge downloads the binary for its pin and platform** and checks it against the
  release's `SHA256SUMS` before using it (`.starship/bin/ensign`, with the tag beside it
  in `ensign.tag`). The first one is downloaded by Claude when it makes the bridge or
  sets up a machine; after that, `ensign update` downloads the new release's.
- **Only CI builds released binaries.** A local build embeds local file paths; building
  on a work machine would carry them into a public release.
- **Builds are reproducible**: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -buildid="`.
  Anyone can rebuild from the tagged source and compare.
- **CI also tests on a Windows runner**: paths, locking, and running `ensign.exe` from
  PowerShell and Git Bash. The harness probe (§7.1) needs a Claude login and stays manual.
- Go is needed only on a machine where `ensign` itself is being developed.

### 11.3 Bridges, pinning and installation

```
~/bridges/work/
  bridge.yaml                    control-plane: { remote: <url>, tag: v1.4.0 }
  .starship/control-plane/       the bridge's clone, checked out at the tag (gitignored)
  .claude/agents/                generated from the pinned clone (gitignored)
  CLAUDE.md                      setup steps, the session-only heartbeat rule; no imports
  machine.yaml                   where this machine keeps each project's code (gitignored)
  notes/                         general context above any project, plain Markdown, created on first use
    profile.md people.md         roles, goals and project involvement; people
    log/YYYY-MM.md               the monthly log, dated entries
    inbox.md                     unsorted notes, editable from anywhere; the session sorts it
```

Nothing creates `notes/` up front. The session reads its files on demand from an index printed at session start; run agents never see it.

**Pinning.** `bridge.yaml` names a release tag. The pinned clone is a shallow fetch of
exactly that tag; a bridge needs no history. Every run snapshot records the tag.

**`ensign update [<tag>]`** — on request only. You run it, or ask Claude to ("update
the bridge"); no hook runs it and Claude never starts it on its own. `help` notes when a
newer release exists. It fetches tags, moves the pin to the newest
release (or the one named), refuses while any run in the bridge is active, then runs
`ensign link`. The change to `bridge.yaml` is committed and pushed in the bridge's repo,
so the bridge's history records every version it ran.

**`ensign link`** makes the bridge match its pin: checks out the pinned tag and
regenerates `.claude/agents/` from it plus the bridge's own
extensions. It runs from `ensign update`, from `ensign init`, and from a SessionStart hook
that checks the generated files still match the pin (they won't after a fresh clone of
the bridge). Rules:

- **Never while a run is active.** A run's roles cannot change under it.
- **Hand edits are protected.** Link keeps a manifest of hashes of what it wrote and
  refuses to overwrite a modified file, naming the right place for the change: the
  control plane, or a role in a bridge extension.
- **Name clashes are errors.** A bridge extension's role with the same name as a
  control-plane role is refused unless it declares `replaces: <role>`. A role file clashes on its file name and also on its frontmatter `name`, the name Claude Code knows it by, and `replaces:` may give either; the replaced role is not generated, so only one agent file is written. A file that clashes with two different roles is refused, and so are two role files with the same name and no `replaces:`.
- **A broken extension's roles are left out, and said so.** The roles of a broken or nameless extension are not linked; `ensign link` and every session start say which extension and why, without failing. A broken bridge extension that declares the same name as a good control-plane extension replaces it: the good one's roles are not linked, and the focal sheet says the extension is broken instead of reading the good file. A broken file whose name can't be determined (known only by its folder name) doesn't displace a good extension.
- **Failures are loud.** A failed link stops the session's starship features with the
  reason; it never carries on with stale files.
- **No symlinks**: on Windows they need developer mode, and git checks them out as plain
  files by default.

**Developing the control plane.** A bridge may set `remote` to the local `~/starship`
and `ref` to any commit instead of a tag, to try changes before release. The binaries at
an untagged commit are those of the last release, so such runs are marked `dev` in
their snapshot and in `help`.

**Each bridge has its own `ensign`; nothing is installed for the machine.** `ensign
link` copies the pinned release's binary to `.starship/bin/ensign` (gitignored, per
machine), replacing it by rename so a running copy can be updated. Hooks call it by
path, `"$CLAUDE_PROJECT_DIR/.starship/bin/ensign" hook <event>`, so the committed
`settings.json` is the same on every machine and platform, and two bridges pinned to
different releases never share a binary. The SessionStart hook puts `.starship/bin` on
the PATH of the session's own shell (through `CLAUDE_ENV_FILE`), so the session runs
plain `ensign …`. A bridge that follows a local control plane (development) gets a copy
of the `ensign` that ran `link`. Outside a bridge, hook events are no-ops.

**Connector settings.** `init` writes `settings.json` without connector parts. While `connectors:` or `connector_writes:` is declared, the PreToolUse matcher gains `|mcp__.*|ListMcpResourcesTool|ReadMcpResourceTool|Read|Grep` (earlier ensigns wrote `|mcp__.*|ListMcpResourcesTool|ReadMcpResourceTool` or `|mcp__.*` alone; those forms are upgraded, with the restart note; undeclared settings carry neither `Read` nor `Grep`) and `permissions.allow` lists the declared tools, `connector_writes` included, sorted and without duplicates, after `Bash(ensign:*)`. SessionStart keeps the file in step: a file that equals a form ensign wrote once the connector parts are removed, and whose connector parts have the shape ensign writes (none, or the matcher with any `mcp__` tools sorted and deduplicated directly after `Bash(ensign:*)`), is rewritten and committed when a declaration is added, changed or removed (removing returns exactly the base form), and the session is told claude must be restarted for the change to apply (with `connector_writes:` declared, also that connector writes are not guarded until then); a change that arrived by the sync gets the same note. The note compares against what the claude process started with, kept in `<bridge>/.starship/sessions/<pid>.connectors` (keyed by `CLAUDE_PID`, created at the process's first SessionStart or on source `startup`, swept by the same age rule as the `.pid` record). So it repeats on every `/clear`, resume or compact until the process restarts, also warns a second process that didn't do the rewrite, and stops if the change is reverted. With `CLAUDE_PID` empty it compares the file before the sync with that after, within the one invocation. Limit: a reused pid starting with source `resume` trusts the stale record and gives a false note until the next restart. While rules are declared the same comparison also covers the role definitions, meaning the set of `.claude/agents/*.md` file names and each one's frontmatter `tools:` value, so moving a tool between roles or adding `reader: []` gets the note too; other edits to a role's prompt don't. Line endings are compared as LF, so a CRLF copy is rewritten the same way, except that an undeclared CRLF file with no connector parts is left alone. A hand-edited file, including one with `mcp__` allow entries but no matcher, is left alone; but if rules are declared and it lacks any of the first three matcher entries (`mcp__.*`, `ListMcpResourcesTool`, `ReadMcpResourceTool`) on the ensign PreToolUse hook or a declared tool in `allow`, connector calls would go unchecked, or, when `reader` is declared, lacks `Read` or `Grep`, reads of `reader/sources/` would go unchecked, so SessionStart reports starship as not ready and names what is missing. An invalid declaration is not ready either.

### 11.4 Portability rules

- `.gitattributes` with `* text=auto eol=lf`, so config and scripts are never converted to
  CRLF. A bridge gets the same file from `ensign init`, or from a SessionStart migration for an existing bridge.
- Forward slashes in every path. Where each project's code is lives in the gitignored
  `machine.yaml` (§12.6), never in the bridge's committed files.
- Hook commands in `settings.json` use no shell syntax — no `$VAR`, no quoting tricks —
  because the shell that runs them differs by platform.

---

## 12. Onboarding

### 12.1 A new machine joining an existing bridge

1. Install Claude Code and git.
2. Clone the bridge.
3. Run `claude` in it and approve the folder when Claude Code asks whether to trust it
   (until then its hooks don't run).
4. Claude notices the bridge isn't set up, says so, and asks to set it up. Say yes.
5. Restart `claude` when it says the bridge is ready.

Nothing else is installed by hand. Setup fetches code into the bridge and changes
nothing outside it; Claude still asks first; it is an ordinary confirmation, not `go`, because no agents are
spawned.

### 12.2 What setup does

The bridge's committed `CLAUDE.md` (written by `ensign init`) carries a short setup
section that is visible before anything else exists: if `.starship/control-plane/` is
missing, the bridge isn't set up on this machine. Claude downloads the pinned release's
`ensign` for the platform into `.starship/bin/` and runs `.starship/bin/ensign setup`,
which fetches the control plane at the pinned tag and checks the binary against the
release's checksums.

`ensign setup`, in Go and therefore the same on both platforms:

- runs `ensign link` to generate the bridge's role and skill files and its own
  `.starship/bin/ensign`;
- leaves `machine.yaml` to be filled in as projects are first needed (§12.6). A task
  whose project has no folder recorded on this machine stops with a clear message, and
  the session asks the user.

Product workspaces are not cloned by setup. Opening a task checks that its workspace
exists and stops if not.

A task with `pipeline: none` has no workspace, so opening it skips the check.

### 12.3 The restart

The session that ran setup cannot switch over to the result: it started before the
bridge's hook settings, manual and role files existed, and Claude Code reads those at
session start. Setup ends with "bridge ready — restart claude".

The same applies after `ensign update`. SessionStart records the pin a session started
on; after an update, the driver refuses to start a new run in that session and asks for a
restart, so a session never mixes two releases.

### 12.4 The very first install

The user makes a folder, starts `claude` in it, and asks for a bridge.
The user's request names `SETUP.md`, a short file of steps for Claude (the README tells
them what to ask, and tells Claude not to clone the repo). Claude follows it: it downloads the newest release's
`ensign` for the platform into the folder's `.starship/bin/` and runs
`.starship/bin/ensign init .`. With no flags, `init` pins the release that binary is,
fetches the control plane at that tag, checks the binary against the release's
checksums, and does what `setup` does. Where this machine
keeps code isn't asked here; the session asks for each project's folder the first time
it is needed. `init` refuses to overwrite a file already in the folder, and
commits only its own files. Then the user restarts `claude`.

### 12.5 When `ensign` is missing: fail-open, and the heartbeat

Hook enforcement has a hole. If `ensign` cannot be found — PATH changed, binary deleted,
a broken update — Claude Code treats each hook as a **non-blocking error**, and every
gate stops being enforced: pipeline order, `go`, the commit gates. Only exit code 2
blocks, and a missing command cannot produce it.

There is no harness-level fix. A permission rule that forces a prompt for the Agent tool
would make a missing `ensign` visible, but a PreToolUse `allow` does not lift it (§7.1),
so it would also prompt when everything is working.

In practice the hole is only at session start. During a session `ensign` can hardly go
missing: hooks call the bridge's own copy by path, and `ensign update` swapping it is
already handled (§12.3). Deleting the binary mid-session is the
only silent case, and rare enough to accept.

So there are two defences:

- **A heartbeat at session start.** SessionStart replies with JSON. Its
  `additionalContext` holds `STARSHIP-READY ensign <version> bridge <tag>`, followed by
  the session manual, which appear in the session's context (§7.1). Claude Code cuts
  `additionalContext` over 10,000 characters to a preview, so a test keeps the heading, the manual and its pointers, with a 200-character bridge path, and the capped notes index, inbox line and commit note, at least 400 characters under 8,000 (`manualLimit`, `cmd/ensign/selftest.go`), even with the capped focal warning; project notes reprinted after a compaction or clear are bounded only by `sessionContextLimit` (10,000).
  Its `systemMessage` shows the user one line at startup, "Starship ready · bridge
  <name> · <release>", without calling the model. When a sequence was left behind by a carry, the line also names the session or sessions to resume. The bridge's `CLAUDE.md`: **if that line is absent, or starship hooks report
  errors, say so plainly and do nothing except help fix it** — no pipeline, no agent
  spawn, no workspace change, no commit. This is a model-level rule, not a guarantee; it
  turns a silent failure into a visible one.

  The rule is for the session agent only. Subagents never see the `STARSHIP-READY` line, so the rule tells them to ignore it rather than conclude that starship is broken. Bridges made before the rule was scoped get the new wording at SessionStart: `updateHeartbeat` runs beside `dropSessionImport`, which removes the manual's import that older versions of `ensign init` left in `CLAUDE.md`. `updateHeartbeat` looks for the old heartbeat paragraph in the bridge's `CLAUDE.md` (also in its CRLF form, which it keeps) and replaces it with the session-only one. It commits just that file with the message "Scope the heartbeat rule to the session agent". It leaves the file alone if it lacks the old paragraph, or if git shows uncommitted edits to it. `dropSessionImport` leaves a `CLAUDE.md` with uncommitted edits alone in the same way. In a bridge that is not a git repo (no `<bridge>/.git`, `isGit`) there is no uncommitted-edits check and no commit: both rewrite `CLAUDE.md` in place.
- **`ensign` fails closed, and its errors end the run** (§6.1). Any internal error — a crash, a corrupt state file, an
  unreadable snapshot — exits 2 with the reason, so the action is blocked rather than let
  through. (A Go panic already exits 2.) One exception: when a stop arrives with
  `stop_hook_active` set and the error repeats, the stop is let through, so a persistent
  fault cannot trap an agent in a loop.

### 12.6 Machines and containers

Bridges may share a machine, live on separate machines, or run in containers. Nothing in
the design assumes one bridge per machine:

- Machine-specific state is per bridge (`machine.yaml`, `.starship/`), not per machine.
- Nothing is recorded per user: each bridge carries its own ensign and settings.
- In a container, the container is the machine: setup runs inside it.

A bridge is normally **shared by every machine working in its context** — say a Linux and
a Windows machine on the same project, often doing platform-specific work. Splitting it
per machine would duplicate its projects, tasks and journals. Sharing needs only:

- **Fetch and rebase as you work.** SessionStart fast-forwards the bridge when it has no
  local changes, and every automatic commit fetches and rebases before pushing. A
  conflict where both sides only added lines is resolved directly; any other conflict
  stops and asks.
- **Where the code is, is per machine.** `project.yaml` and `task.md` hold no paths.
  Each machine has its own `machine.yaml` at the bridge's root, gitignored, mapping a
  project (or a task with a checkout of its own, as `<project>/<task>`) to a folder:

  ```yaml
  workspaces:
    supernova: /home/me/src/supernova
    supernova/auth-refactor: /home/me/src/supernova-auth
  ```

  A project that declares code locations has a folder per location, in a `locations` section keyed by project and location, recorded with `ensign workspace <project> --location <name> <path>`:

  ```yaml
  locations:
    supernova:
      client: /home/me/src/supernova-client
      models: /home/me/src/supernova-models
  ```

  A project's toolchain setup is kept there too, in an `env` section keyed by project name: `set` holds variables to set, `path` folders to put first on `PATH`, in order. There is no machine-wide section.

  ```yaml
  env:
    supernova:
      set:
        GOROOT: /home/me/sdk/go
      path:
        - /home/me/sdk/go/bin
  ```

  Ensign applies it to every command it runs for the project (handlers, plain bindings, `@done`, `@release`, publish steps), reading `machine.yaml` at that moment, so a binding can be a plain command on every platform. Agents' shells get it from a setup file next to the session's files, which the session's env file sources; each run writes it, so the project's setup reaches agents' commands from the next run. On Windows without Git Bash, where Claude Code accepts only `NAME=value` lines in the env file, session start writes every project's folders and variables there instead (a variable two projects set differently is left out, and the session is told). It is recorded with `ensign env <project> [NAME=<value> | --path <folder> | --drop-path <folder>]`.

  A machine with no entry for a project asks the user once, the first time a task needs
  it, and records the answer (`ensign workspace <project>[/<task>] <path>`). It sits
  outside `.starship/` because, unlike everything there, it holds answers that can't be
  regenerated. A style source in a project's code is written `$WORKSPACE/<path>`, so it
  resolves on every machine too. Bridges from before this kept a `workspace:` line in
  their files, often under a `$PROJECTS` root; setup, update and session start move
  those into `machine.yaml` and leave the edited files for the journal to commit. Run
  state stays on the machine that ran it.

---

## 13. Carried over from the reference design

- Vocabulary kept distinct: skill, pipeline, role, binding, project, task.
- Plan first; nothing is dispatched without approval, and approval is the literal `go`.
- The session agent plans, resolves and triages; it does not write product code.
- Roles do not roam: loop stages get named files, and reconnaissance is the explorer's job.
- The task journal (`task.md`) is the durable memory; scratch is ephemeral.
- No agent attribution in anything that leaves the machine.

---

## 14. Open questions

None at the moment. Deferred work is in the backlog (§15.4).

---

## 15. Implementation status

Everything in §§2–12 is built, with unit tests (run in CI on Linux, Windows and macOS) and live
runs against Claude Code, except where listed below.

### 15.1 Commands

| Command | Does |
|---|---|
| `ensign init <dir> (--control-plane <path> \| --remote <url> --tag <tag>)` | create a bridge |
| `ensign setup` | fetch the pinned control plane, register the bridge, link (including the bridge's own `ensign`) |
| `ensign workspace <p>[/<t>] [<path>]` | record (or show) where a project's or task's code is on this machine |
| `ensign workspace <p> --location <name> [<path>]` | record (or show) where one of a project's code locations is on this machine |
| `ensign env <p> [NAME=<value> \| --path <folder> \| --drop-path <folder>]` | record (or show) a project's toolchain setup on this machine: variables, and folders put first on `PATH` |
| `ensign update [<tag>]` | move the bridge to a newer release |
| `ensign gc [<keep>]` | delete finished runs beyond the limit (default: the bridge's `keep-runs`) and empty the scratch folders of the rest |
| `ensign link [--force]` | regenerate the bridge's role, skill and manual files |
| `ensign project new <name> --workspace <path>` / `ensign task new <p>/<t> [--workspace <path> \| --locations <a,b>]` / `ensign task open <p>/<t>` | scaffold and open; where the code is goes into this machine's `machine.yaml`, and a new task uses its project's unless given a different location. `project new --pipeline none` makes a project of tasks without code (workspace, locations and platforms optional); `task new --pipeline <none\|coding>` picks per task (default: the project's), and a no-code task refuses `--workspace`, `--locations` and `--branch`; `task open` also lists the task's and project's `links:` (§5.3); `run`, `sequence run`, publishing and landing refuse a no-code task; `project new` and `task new` refuse a Windows reserved name (`con`, `nul`, `com1` and the like) as a project, task or location name (`--locations`, or in the project.yaml `--workspace` ships), and `project new --workspace` also refuses a shipped location name that fails the name rule (lower-case letters, digits and dashes) and writes no project, but a name already in a bridge keeps loading and the other task commands still accept it |
| `ensign run <p>/<t> [--location <name>] [--subtask <name>] [--sides <a,b>] [--dependency <location>=<branch>] [--start <stage>] [--sync] [--dry-run]` | create a run for a task; `--subtask` names the subtask of a task with subtasks (default: the first pending); `--location` names the code location it edits, when the project has several; `--sides` narrows the locations it tests; `--dependency` names an unlanded branch of a location it depends on, applied with `@apply-dependency` |
| `ensign dependency apply --side <location>` / `restore [--side <location>]` | for agent stages: apply a run's dependencies on a side before building and testing there, and restore the tree before finishing |
| `ensign task close <p>/<t>` | close a task, with or without code: fire `task.close` (§4.2), set `status: closed` and `Done.` in `task.md`, and journal it; closing a closed task does nothing |
| `ensign task subtask done\|drop <p>/<t> <name>` | mark a subtask of a task done or dropped |
| `ensign run --pipeline <name> [--project <p>] [--location <name>] [--sides <a,b>] [--rebuild] [--dry-run]` | create a project- or bridge-wide run |
| `ensign sequence` | list the session's sequences and their steps |
| `ensign sequence run <p>/<t> [run flags]` / `publish <p>/<t>` / `do <instruction>` [`--name <text>`] | add a queued run, a publish, or a session-carried step to the open sequence; `--name` names the sequence a step starts |
| `ensign sequence retry <seq>.<step> --diagnosis <why> --state <what is left>` / `accept <seq>.<step>` / `skip <seq>.<step>` | after a run step fails, the user chooses: run it again on the same branch (any number of times), halt (`accept`), or end the step as failed and carry on (`skip`); stop mode only, since keep-going ends a failed step at once and reports at the end |
| `ensign sequence mode stop\|keep-going` / `halt [--reason <text>]` / `done <seq>.<step> [--note <text>]` / `fail <seq>.<step> --reason <text>` | set how failure is handled, stop the sequence, or end a `do` step |
| `ensign next [--run <id>]` | what to dispatch next |
| `ensign resume --answer <text> [--budget <b>=<n>]` | continue an `ask-user` run |
| `ensign journal commit -m <msg>` / `ensign journal push` | commit and push the bridge's records |
| `ensign git-catalog squash <p>/<t> -m <msg>` | collapse a task's catalog into one commit |
| `ensign style ingest <project> <path-or-url> --trust <t>` | add a style source |
| `ensign selftest` | re-check the Claude Code behaviour starship depends on, including that MCP tool calls carry the agent's identity, that a hook deny stops them, and that an agent's `tools:` list grants the MCP tools it names |
| `ensign hook <event>` | the hook driver |

### 15.2 Settled while building

- **Agents are given bindings as exact commands** (`cd <workspace> && ./build.sh`), and
  each run has a scratch folder for logs, named in every prompt. Agent instructions avoid
  shell variables such as `echo $?`, which Claude Code's permission checks refuse unless
  allowed.
- **Bridges allow `Bash(ensign:*)` by default**, and the lead calls plain `ensign next`.
- **The session manual comes from the SessionStart hook, not `CLAUDE.md`.** Claude Code
  gives `CLAUDE.md` and its imports to every subagent, so importing the manual put it
  in every coder's and tester's context, telling them not to edit or run tests. The hook
  prints `.starship/session.md` after the heartbeat; SessionStart fires only for the
  session, and again after a compaction. It removes the old import from an existing
  bridge's `CLAUDE.md` and commits that, unless the file has uncommitted edits.
  `ensign selftest` checks from the session transcript that the hook's output reached the session whole, and from each agent's own transcript that it stays out of subagents. A unit test keeps the heading, the manual and its pointers under the size it checks (8,000 characters); project notes reprinted after a compaction or clear are bounded only by `sessionContextLimit` (10,000).
- **A `claude` started from inside another Claude Code session is a child session.**
  Its prompt reaches UserPromptSubmit with a `Claude` line in front, so `help` doesn't
  match, and it inherits the parent's project directory. This is why `help` once went to
  the model in testing; a user's own session isn't affected. `ensign` starts its own
  `claude` (the selftest) without the variables a session sets
  about itself, and live tests should do the same.
- **`ensign init` commits the new bridge** (with a `.gitattributes` for LF line endings). Before, the first session in a new bridge
  was asked to commit the empty-folder placeholders as journal changes.
- **Stages run in the foreground.** PreToolUse rewrites `run_in_background` to `false`
  on every stage dispatch, and `ensign selftest` checks the rewrite works. Inside a
  subagent, asking for the background does launch the agent in the background: a
  control run without the rewrite failed the check. A lead that still ends up waiting
  on a running stage is allowed to end its turn, and that isn't counted towards
  `lead-stopped`. Blocking it would force the filler turns the waiting rules exist to
  prevent.
- **Facts for a skill that doesn't exist are held, not curated.** Agents are told which
  skill names exist. A fact for any other name goes to `skills/proposed/<name>/`, and
  `status` lists it. `ensign skill new <name> --description …` creates the skill and
  makes its facts pending; `ensign skill discard <name>` drops them. `skill new` refuses a Windows reserved name (`con`, `nul`, `com1` and the like), and a fact line naming one is skipped as an invalid name is; `skill discard` still accepts one, so facts already held under it can be cleared. `status` offers "make a <name> skill" or "discard the <name> facts" only for a held folder whose name is usable; for one that isn't (it fails the name rule, or Windows reserves it) it shows the fact count and tells you to remove the folder by hand, with no offer. A folder in `skills/pending` with facts whose name isn't a usable skill name (it fails the name rule, or Windows reserves it) is skipped with a warning when a skill-curate run lists its items, and stays on disk; a curation report for such a name is refused before anything is written.
- **Help entries name their command for the session.** A pipeline's or extension's help
  entry may carry `run:`, the command that carries the request out. `help` never shows
  it; `ensign help requests` lists every one, generated at call time, and the SessionStart hook points at it after the manual, so the session doesn't have to guess how to "squash this task's commits".
- **The `@done` check** runs before a run may end as `success`; a failure ends it as
  `error: done-check`.
- **Events on stages a pipeline doesn't have are inactive for it**, so an extension or layer
  can serve several pipelines.

### 15.3 Not done

- **No release has been cut.** The release workflow exists but has never run, so there
  is no tag for a bridge to pin; pinning is tested against local tags.

### 15.4 Backlog

Deliberately deferred; not reported as open work.

- **Windows hooks.** How Claude Code runs hook commands on Windows (Git Bash, PowerShell,
  or direct process launch) is unverified. `ensign hook <event>` is designed to work under
  all three, and the code and its tests pass on Windows in CI. Verify by running
  `ensign selftest` on a Windows machine.
- **Nested leads.** A pipeline can't put an orchestrator stage inside another; validation
  rejects it. Build it when a pipeline needs it.
