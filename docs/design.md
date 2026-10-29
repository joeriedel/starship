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
3. **Everything is extensible below the control plane.** An overlay, a project, or a
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
overlay         starship-work/        hosted wherever that context requires
   │            starship-home/        (a work overlay never touches personal GitHub)
   │            projects, tasks, project skills, integration scripts,
   │            platform roots, denylist.txt, proposals/, local extensions
   ▼
project         <overlay>/projects/<project>/
   │            bindings, pipeline extensions, rules
   ▼
task            <overlay>/projects/<project>/tasks/<task>/task.md
                workspace paths, VCS state, per-task extensions, journal
```

Each later layer may extend or override the one before it, subject to locks (§5.4).

### 2.1 How an overlay consumes the control plane

- **Sessions open in the overlay, not in the control plane.** The overlay's repo is the
  session's repo, so tasks, scratch, settings and commits stay inside its boundary.
- **One writable clone of the control plane per machine**, at `~/starship`. Overlays
  consume it directly (`@import` from the overlay's `CLAUDE.md`, symlinked agents and
  hook scripts), so control-plane edits are live immediately. A Claude Code plugin was
  considered and dropped: it installs into a cache, which would mean keeping a writable
  clone and a published copy in sync.
- **Overlay sessions cannot write to the control plane.** A permission deny on the
  control-plane path, backed by a PreToolUse hook, makes this structural.

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

```
overlay session (cwd: <overlay>)
  │  notices a process improvement
  │  writes proposals/<slug>.md, stated abstractly
  ▼
control-plane session (cwd: ~/starship, fresh context)
  │  reads only the proposal and control-plane files
  │  makes the edit
  ▼
commit gates (git hooks in ~/starship)
  1. identity: author email must be the personal address
  2. denylist: diff grepped against every overlay denylist on this machine
  3. clean-room review: an agent with no project context reads only the diff
  ▼
commit and push to personal GitHub, without asking (§9.1)
```

- The author of every control-plane change has never seen project code, so it cannot
  paraphrase project context into the files. The gates catch literal strings; the fresh
  session prevents paraphrase. Both are needed.
- **Proposals are linted on read**, against the same denylists, since they are where
  project context is first abstracted.
- **Denylists live in their overlay and never enter the control plane.** The check fails
  closed: an overlay present with a missing or empty `denylist.txt` refuses the commit.
- **Commit identity is enforced** by a pre-commit hook, not just configured, so a
  machine's global git identity cannot reach public history.
- **Examples in the control plane use a fictional project** (`supernova`). Lessons are
  written as the mechanism alone, or retold in the fictional project.
- **New vocabulary is reviewed too.** A binding or stage name invented for one project
  becomes public once a pipeline uses it; the clean-room review checks that new names
  make sense without the project behind them.

### 3.3 Gates by kind of change

| Change | Risk | Gates |
|---|---|---|
| role or pipeline wording, budgets, choreography | low | identity, denylist |
| operating lessons with a worked example | medium | + clean-room review; example uses the fictional project |
| skill facts (engine or framework behaviour) | high | + clean-room review |

All tiers commit and push automatically once their gates pass. A failing gate refuses
the commit and nothing is pushed.

---

## 4. Extension points

Three kinds, kept separate:

| Kind | Answers | Example |
|---|---|---|
| **binding** | what does X mean here? | `@build` → `go build ./...` |
| **pipeline** | what runs, in what order? | code ⇄ review → style → test |
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

### 4.2 Events

The initial set:

```
session.open   task.open   plan.approved   run.start
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

### 4.3 Extension bundles

Related bindings, handlers and augments ship together as a bundle:

```
extensions/<name>/
  extension.yaml
  <scripts>
  briefs/
```

The control plane ships generic bundles (no project names). An overlay may ship its own
in the same format. A project or task enables bundles with `use: [<name>, ...]`. A
bundle declares the bindings it requires, so it cannot be enabled where it doesn't fit.

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

- `spend: <budget>` on a **stage** spends once each time the stage is entered.
- `spend: <budget>` on an **edge** spends once each time that transition is taken.
- `reset: <budget>` on an edge restores that budget to its full value.
- A budget is the maximum number of spends. The spend that would exceed it triggers
  `on-exhausted` instead.

### 5.2 The default coding pipeline

```yaml
pipeline: coding
version: 1
requires-bindings: [checkout, compile, build, test, diff, done]
budgets: { loop: 3, test: 3 }
free-roles: [explorer]        # may be spawned at any point; does not advance the pipeline

stages:
  - id: run
    role: orchestrator
    dispatched-by: session
    stages:
      - id: code
        role: coder
        spend: loop
        outcomes:
          compiled: next
          broken:   { goto: code }
          blocked:  { error: blocked }
      - id: review
        role: reviewer
        outcomes:
          clean:    next
          findings: { goto: code }
      - id: style
        role: style-reviewer
        outcomes:
          done: next
      - id: test
        role: tester
        outcomes:
          pass:  { finish: success }
          fail:  { goto: code, spend: test, reset: loop }
          error: { error: test-error }
    on-exhausted:
      loop: { error: budget:loop }
      test: { error: budget:test }
```

What this encodes:

- **`loop` bounds one cycle.** Every pass through `code` spends it, whether the pass was
  a compile retry or a response to review findings. Three passes without reaching a
  clean, compiling state ends the run.
- **`test` bounds cycles.** A test failure starts a new cycle with a fresh `loop`. Worst
  case: 3 cycles × 3 passes = 9 coder passes.
- **The coder reports whether it compiled** (`compiled` / `broken`), so every compile
  attempt is a pass the driver can count.
- **`success` is reachable only through `pass`.** A suite that didn't run is an `error`,
  never a success. The `@done` binding is a `require` check on the way to `success`; if it
  fails, the run ends as `error: done-check`.

### 5.3 Extending a pipeline

Later layers edit a pipeline with explicit operations on stage IDs. A list is never
silently replaced.

```yaml
# a project's pipelines/coding.yaml, or a block in task.md frontmatter
extends: coding
ops:
  - insert: { after: review, stage: { id: security, role: reviewer,
              brief: briefs/security.md,
              outcomes: { clean: next, findings: { goto: code } } } }
  - remove: style
  - set:    { budgets.test: 1 }
  - set:    { on-exhausted.test: { ask-user: "test budget spent — another cycle?" } }

use: [git-catalog]

bindings:
  build: { command: "./build.sh --fast" }
  test:  { not-applicable: "proto-only repo, no suite" }

events:
  stage.after:test: [{ observe: scripts/post-summary.sh }]
```

A file without `extends:` defines a new pipeline. The same resolver and driver run it.

### 5.4 Locks

The control plane marks invariants as locks — for example the approval gate, no
workspace push without approval, and no agent attribution. Lower layers may tighten a lock freely.
Loosening one needs an explicit entry:

```yaml
unlock: { lock: no-push, reason: "..." }
```

The resolver prints every unlock prominently.

---

## 6. Runs

### 6.1 Outcomes

| Run outcome | Meaning |
|---|---|
| `success` | compiles, tests pass, `@done` satisfied |
| `error` | the run stopped; a reason code set by the driver plus the agent's summary |
| `ask-user` | waiting on a decision; carries the question; resumable |

Reason codes come from whichever edge or budget ended the run (`blocked`,
`test-error`, `budget:loop`, `budget:test`, `done-check`), so they are deterministic.
Handlers attach to `run.finish:error`; filtering by code is not supported until a real
case needs it.

`ask-user` is resumable because a run's state lives in files, not in an agent's context:
`starship resume <run>` dispatches a fresh lead against the same state, with the user's
answer and any budget change recorded there.

### 6.2 Stage reports

Every dispatched agent ends its final message with a report block:

````
```starship-report
outcome: findings
summary: two off-by-one errors in range clamping
files: [src/range.go]
```
````

The driver rejects a report whose `outcome` is missing or not among the stage's declared
outcomes, and blocks the agent's stop with the valid outcomes listed. Stages may require
further fields (the `code` stage requires `files` when `git-catalog` is enabled).

### 6.3 Resolver and snapshot

`starship resolve <task>` merges control plane → overlay → project → task and writes a
frozen snapshot. Hooks read only the snapshot.

```
<overlay>/.starship/runs/<run-id>/
  pipeline.json    resolved pipeline; every field records its source file and line
  state.json       written only by the driver
  reports/         each stage's report block as received
```

`starship explain <task>` prints the resolved pipeline with provenance:

```
stage security          from project:supernova/pipelines/coding.yaml:4
stage style             REMOVED by project:supernova/pipelines/coding.yaml:7
budgets.test=1          from task:tasks/auth-refactor/task.md
```

The snapshot's hash is recorded in the state file. A config change mid-run does not
affect that run.

### 6.4 State file

```json
{
  "run": "20261029-1203-coding",
  "task": "projects/supernova/tasks/auth-refactor",
  "pipeline_sha": "3f9c…",
  "approved": true,
  "status": "running",
  "base": "a1b2c3d",
  "dispatches": {
    "d7": { "stage": "run.review", "parent": "run", "agent_id": "a5ba254c…" }
  },
  "position": { "run": "review" },
  "spent": { "loop": 2, "test": 0 },
  "history": [
    { "d": "d5", "stage": "run.code",   "cycle": 1, "pass": 2, "outcome": "compiled" },
    { "d": "d6", "stage": "run.review", "cycle": 1, "pass": 2, "outcome": "findings" }
  ]
}
```

---

## 7. The hook driver

One script, `starship hook <event>`, called from harness hooks and git hooks. Every
invocation takes an `flock` on `state.json`.

**PreToolUse (Agent tool)**

1. Identify the caller: `agent_id` maps to a dispatched stage; no `agent_id` is the
   session.
2. Find the next expected stage for that caller from `position`.
3. If the requested `subagent_type` doesn't match the stage's role, deny with the
   expected stage named. Free roles pass without changing state.
4. Run `stage.before` handlers; a failing `require` denies the spawn.
5. Rewrite the prompt: prepend a tag `[starship run=… dispatch=d7 stage=run.review]`,
   then any augments.
6. Record the dispatch.

**SubagentStop**

1. Read the dispatch tag from the first user message in `agent_transcript_path`.
2. Parse the report from `last_assistant_message`; if invalid, block with the valid
   outcomes.
3. Apply the transition, spend and reset budgets, run `stage.after` handlers.
4. If the stopping agent is a lead whose pipeline hasn't reached a run outcome, block it
   and name the stage that hasn't run.

**UserPromptSubmit**

If the user's message is exactly `go` and a plan is pending, set `approved`. Until then,
PreToolUse denies the lead dispatch. The approval gate is a harness check, not an
agent's promise.

**Git hooks** run `commit.before` and `push.before` through the same script, including
the control-plane leak gates (§3.2).

### 7.1 Verified harness behaviour

Probed on Claude Code 2.1.312 with a three-level spawn (session → orchestrator → leaf):

| Behaviour | Result |
|---|---|
| PreToolUse fires for Agent calls made inside a subagent | yes; input carries the caller's `agent_id` and `agent_type` |
| PreToolUse can rewrite the Agent prompt via `updatedInput` | yes; the injected text appeared in the leaf's transcript |
| PreToolUse `deny` on a nested spawn | yes; the agent never started and the caller saw the reason |
| SubagentStop identifies the agent and its output | yes: `agent_id`, `agent_type`, `agent_transcript_path`, `last_assistant_message` |
| SubagentStop `decision: block` forces the agent to continue | yes; the second stop arrives with `stop_hook_active: true` |

SubagentStop identifies the role but not the stage, which is why dispatches are tagged.
Harness behaviour changes between builds, so the probe belongs in this repo as a
self-test, re-run after every Claude Code upgrade.

---

## 8. Extension: `git-catalog`

Commits every coder pass so the orchestrator can reason from history between passes,
and squashes when and where the project chooses.

```yaml
# extensions/git-catalog/extension.yaml
name: git-catalog
requires-bindings: [vcs-git]

events:
  run.start:        [{ require: record-base.sh }]   # clean tree required; store HEAD as base
  stage.after:code: [{ require: commit-pass.sh }]
  stage.before:run: [{ augment: briefs/orchestrator.md }]

provides:
  squash: squash.sh                                 # not bound to any event by default
```

- **The driver commits, not the coder.** Messages are uniform and numbered from state:
  `wip(c1 p1) BROKEN` with compile errors from the report in the body, `wip(c1 p2)`,
  `wip(c2 p1)`.
- **Staging is by name.** `commit-pass.sh` stages exactly the report's `files`, then
  checks for any other modification; if there is one, the run ends as `error` rather than
  sweeping in or stashing someone's work.
- **A clean tree is required at `run.start`.**
- **The orchestrator learns about the catalog only through the bundle's augment**, so
  its role definition never mentions git.
- **Squashing is placed by the project.** `squash.sh` (`git reset --soft <base>`, one
  commit) is attached to whichever event the project chooses — `push.before`,
  `run.finish:success`, or none, with `starship squash <task>` by hand.
- Commits use the workspace's configured identity and carry no agent attribution.
  Pushing stays locked behind explicit approval.

---

## 9. Commits the system makes

Three kinds of repository, three defaults:

| What | Commit | Push |
|---|---|---|
| control plane (`~/starship`) | automatic | automatic, after the gates in §3.2 |
| task journals (`tasks/<task>/`) | automatic, per project; may be disabled | automatic, with the commit |
| workspaces (product code) | per pipeline (e.g. `git-catalog`) | only with explicit approval, every time |

In every case the agent writes the commit message, because it knows what the logical
change was, and a **Stop hook enforces that it happened**: a session turn cannot end with
uncommitted control-plane or journal changes. The hook names what is outstanding and
blocks; the agent commits and tries again. Wording is the agent's job; whether it
happened is not.

### 9.1 Control plane

- One commit per logical change, files staged by name, message short.
- Pushed without asking. The Stop hook also blocks while the local branch is ahead of the
  remote, checked with `git ls-remote`, not the local tracking ref.
- The leak gates (§3.2) are git hooks, so they run on every commit regardless of who
  makes it.

### 9.2 Task journals

Task journals are verbose by design: progress log, decisions with reasoning, outstanding
work, notes. The bar is that a fresh session with no context can pick the work up. They
are committed by default.

Projects don't all use git, so how a journal is committed is a binding, not a fixed
command:

| Binding | Means |
|---|---|
| `@journal-status` | list journal changes not yet recorded remotely (uncommitted or unpushed) |
| `@journal-commit` | record the given journal files remotely with the given message |

`@journal-commit` means "durably recorded on the remote", not "committed locally". For
git that is a commit followed by a push; for a centralised VCS the submit already does
both.

- **Default:** the control plane's `journal-git` bundle, enabled for every project,
  binds both to git in the repo holding the task directory: stage by name, commit, push.
- **Another VCS:** the project binds them to its own commands.
- **Disabled:** the project declares
  `journal-commit: { not-applicable: "<reason>" }`. The Stop hook then skips journal
  checks for that project. As with every binding, a missing binding is a stop; only an
  explicit declaration turns the behaviour off.
- The agent commits through `starship journal commit -m "<message>" <files>`, which
  resolves the binding, so the agent never needs to know the project's VCS.

---

## 10. Carried over from the reference design

- Vocabulary kept distinct: skill, pipeline, role, binding, project, task.
- Plan first; nothing is dispatched without approval, and approval is the literal `go`.
- The session agent plans, resolves and triages; it does not write product code.
- Roles do not roam: loop stages get named files, and reconnaissance is the explorer's job.
- The task journal (`task.md`) is the durable memory; scratch is ephemeral.
- No agent attribution in anything that leaves the machine.

---

## 11. Open questions

1. **Config format and resolver language.** YAML (shown here) needs a parser dependency;
   TOML is in the Python standard library but nests awkwardly; a Go resolver gives one
   static binary.
2. **Parallel fan-out**, needed by the review pipeline: a `parallel` stage and a join rule.
3. **Nesting depth.** One lead level is specified; deeper nesting is forbidden until a
   second case needs it.
4. **Who may unlock** a lock: overlay and project only, or tasks too.
5. **Skill promotion policy** for technology facts learned inside an overlay.
6. **Integration scripts** (issue trackers, review systems): generic bundles in the
   control plane with configuration from the overlay, or overlay-only.
7. **Same machine or separate machines** for different overlays, which affects whether a
   denylist check sees more than one overlay.
8. **A fast path for trivial control-plane fixes**, or always the proposal handoff.
9. **Committing to the personal repo from a work machine** is an employment-agreement
   question, separate from content cleanliness. The user's call.