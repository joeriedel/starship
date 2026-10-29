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
   │            platform roots, denylist.txt, proposals/, local extensions
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
- **A writable clone, `~/starship`, exists only where the control plane is developed.**
  Improvements are made and committed there (§3.2), released as a tag, and picked up by
  each bridge when it updates. A machine that only uses starship doesn't need it.
- **Bridge sessions cannot write to the control plane** — neither the pinned clone nor
  `~/starship`. A permission deny on both paths, backed by a PreToolUse hook, makes this
  structural.

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
bridge session (cwd: <bridge>)
  │  notices a process improvement
  │  writes proposals/<slug>.md, stated abstractly
  ▼
control-plane session (cwd: ~/starship, fresh context)
  │  reads only the proposal and control-plane files
  │  makes the edit
  ▼
commit gates (git hooks in ~/starship)
  1. identity: author email must be the personal address
  2. denylist: diff grepped against the denylist of every bridge registered here (§12.6)
  3. clean-room review: an agent with no project context reads only the diff
  ▼
commit and push to personal GitHub, without asking (§10.1)
```

- The author of every control-plane change has never seen project code, so it cannot
  paraphrase project context into the files. The gates catch literal strings; the fresh
  session prevents paraphrase. Both are needed.
- **Proposals are linted on read**, against the same denylists, since they are where
  project context is first abstracted.
- **Denylists live in their bridge and never enter the control plane.** The check fails
  closed: a bridge present with a missing or empty `denylist.txt` refuses the commit.
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

Conventional names, reused before new ones are invented:

| Binding | Purpose |
|---|---|
| `@sync` | bring the workspace up to date with its remote (git: pull/rebase the task branch; Perforce: sync or unshelve) |
| `@checkout` | prepare files for editing (Perforce: `p4 edit`) |
| `@compile` | fast compile-only check; falls back to `@build` |
| `@build` | build |
| `@test` | run the test suite |
| `@lint` | linters and formatters |
| `@diff` | the current change set |
| `@style` | style rules beyond the linter, written by people |
| `@done` | this project's definition of finished |

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

Related bindings, handlers, augments, pipeline ops, whole pipelines and roles ship
together as a bundle:

```
extensions/<name>/
  extension.yaml
  <scripts>
  briefs/
  pipelines/       new pipelines the bundle adds
  roles/           role definitions the bundle adds
```

A bundle's pipeline ops apply right after the layer that enables it, before that layer's
own ops, so a project can still adjust what a bundle inserted.

The control plane ships generic bundles (no project names). A bridge may ship its own
in the same format. A project or task enables bundles with `use: [<name>, ...]`. A
bundle declares the bindings it requires, so it cannot be enabled where it doesn't fit.

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

**Delivery** is per skill:

- **`inject`** (default): the driver adds the skill as an augment at `stage.before` for
  each stage whose role is in the skill's role list. Guaranteed; costs the skill's full
  length on every such dispatch. For skills a role must not work without.
- **`on-demand`**: the skill is exposed as a Claude Code skill. Only its description sits
  in context; the agent loads the body when it judges it relevant. Cheaper for large
  reference skills. The driver records which on-demand skills each stage actually loaded
  (from the agent's transcript), so a skill that should have been loaded and wasn't shows
  up in the run history and can be switched to `inject`.

Probed on 2.1.312 (§7.1): subagents with a relevant on-demand skill loaded it in 10 of 10
runs and followed it each time, as did the session agent in 6 of 6. Those conditions
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
Promoting one into a public skill goes through the proposal path (§3.2) under the
high-risk gate (§3.3).

### 4.5 The curator

One `curator` role maintains every curated document: style guides (§9) and skills'
learned entries (§4.4). A stage marks itself with `curates: style` or `curates: skill`,
and its agents are the only writers of those documents. Each document type supplies a
brief (`briefs/curate-style.md`, `briefs/curate-skill.md`); the pass and the checks are
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
- a merge keeps the lower id;
- a new entry has evidence and starts as `candidate`, unless an authoritative source
  (`source:<id>`) backs it;
- a candidate is promoted only on its second sighting (hits ≥ 2) or an authoritative
  source; an active entry never goes back to candidate;
- the document fits in 150 lines.

### 4.6 Integrations

Integrations with outside systems — issue trackers, wikis, code review systems — live at
the **bridge** level, as bundles in the bridge: the scripts, their bindings (such as
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
          pass:  { finish: success }
          fail:  { goto: code, spend: test, reset: loop }
          error: { error: test-error }

on-exhausted:                         # run-wide, beside budgets
  loop: { error: "budget:loop" }
  test: { error: "budget:test" }
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

### 5.2.1 Starting elsewhere, and syncing first

A run may start at any stage, and may sync the workspace first:

```
ensign run <task> --sync --start test
```

- `--sync` runs `@sync` as a `require` handler at `run.start`, before anything else, so
  a `git-catalog` base is recorded after the pull.
- `--start <stage>` begins at that stage; budgets start full.

This covers "commits came in from another machine — pull them and make sure they compile
and pass": the coding pipeline started at `test`. `pass` ends the run as `success`;
`fail`, including a failed compile, goes back to `code` and the normal loop fixes it.

### 5.3 Extending a pipeline

Later layers edit a pipeline with explicit operations on stage IDs. A list is never
silently replaced.

Each layer has one config file, and they all share the same shape: `bridge.yaml` for the
bridge, `projects/<p>/project.yaml` for a project, and the frontmatter of `task.md` for a
task. Pipeline operations go under `pipelines.<name>.ops`, next to the layer's bindings:

```yaml
# projects/supernova/project.yaml (a task.md frontmatter block looks the same)
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

events:
  stage.after:test: [{ observe: scripts/post-summary.sh }]
```

A task's frontmatter also names its `pipeline` (default `coding`) and its `workspace`.
New pipelines are files in a `pipelines/` directory — the control plane's, or a bundle's.

`append` adds to a list field; `set` on a list field replaces it explicitly.

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

### 5.6 Locks

A few invariants are **locked** in the control plane: the `go` approval gate, no
workspace push without approval, no agent attribution, and the control-plane leak gates.
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
`test-error`, `budget:loop`, `budget:test`, `done-check`), so they are deterministic.
The driver adds its own:

| Code | When |
|---|---|
| `ensign` | an `ensign` error during the run (below) |
| `bad-report` | a stage agent failed to produce a valid report after being blocked twice |
| `lead-stopped` | the lead kept trying to stop before the pipeline finished (blocked three times) |

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

Every dispatched agent ends its final message with a report block:

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
the agent's stop with the valid outcomes listed. Stages may require further fields (the
`code` stage requires `files` when `git-catalog` is enabled).

Agents never have to be taught the format by a role file: the driver appends the report
instructions, with that stage's outcomes, to every dispatch (§7).

### 6.3 Creating a run: the resolver and the snapshot

`ensign run <task> [--sync] [--start <stage>]` creates a run:

1. **Merges** the pinned control plane → bridge → project → task, plus every bundle they
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
  runs/<run-id>/             <timestamp>-<project>-<task>, allocated exclusively
    pipeline.json            the snapshot: resolved pipeline with sources, bindings,
                             workspace, work item, and every brief the run can use
    state.json               written only by the driver
    reports/<dispatch>.md    each stage agent's final message
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
```

The snapshot's hash is recorded in the state file. A config change mid-run does not
affect that run.

### 6.4 State file

```json
{
  "run": "20261029-141649-coding",
  "status": "running",
  "approved": true,
  "spent": { "loop": 1, "test": 1 },
  "scopes": {
    "":    { "stage": "run" },
    "run": { "stage": "review", "branches": { "correctness": "d8", "tests": "d9" } }
  },
  "dispatches": [
    { "id": "d7", "stage": "code", "scope": "run", "role": "coder",
      "agent_id": "a5ba254c…", "done": true, "outcome": "compiled", "summary": "…" },
    { "id": "d8", "stage": "review", "scope": "run", "dim": "correctness", "role": "reviewer", "done": false }
  ],
  "history": [
    { "kind": "outcome", "stage": "test", "detail": "fail → goto code, spend test, reset loop" },
    { "kind": "enter",   "stage": "code", "detail": "loop 1/3, test 1/3" }
  ]
}
```

A **scope** is the list of stages one dispatcher works through: `""` is the session, and
each lead's id is the scope of the stages it dispatches. The history records every
dispatch, refusal, report, outcome, stage entry and block, so a run can be read back
afterwards exactly as it happened.

---

## 7. The hook driver

One binary, `ensign hook <event>`, called from harness hooks and git hooks. Every
invocation takes a file lock on the run, and outside a bridge every event is a no-op.

**PreToolUse (Agent tool)**

1. Identify the caller: `agent_id` maps to a dispatched stage; no `agent_id` is the
   session.
2. Find the caller's scope and its current stage. In a parallel stage, the next
   undispatched branch. A caller that is a stage agent rather than a lead may spawn only
   free roles.
3. If the requested `subagent_type` doesn't match the stage's role, deny with the
   expected stage named. Free roles pass without changing state.
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
2. Parse the report from `last_assistant_message`; if invalid, block with the valid
   outcomes.
3. Apply the transition, spend and reset budgets, run `stage.after` handlers. For a
   branch, record its outcome; the transition applies when the last branch joins.
4. If the stopping agent is a lead whose pipeline hasn't reached a run outcome, block it
   and name the stage that hasn't run. The exception is a lead with a stage still
   running, which can only happen if a stage ran in the background anyway: ending its
   turn is how it waits, so the stop is allowed and not counted.

**SessionStart**

Records the session's id for its commands (§5.5) and prints the heartbeat (§12.5).

**UserPromptSubmit**

If the user's message is exactly `go` and a run is pending, set `approved` and tell the
session, as added context, to dispatch the lead with the task's work item. Until then,
PreToolUse denies the lead dispatch. The approval gate is a harness check, not an
agent's promise.

If the message is `help` or `help <topic>`, print the help text and end the turn without
calling the model (§7.2).

**Git hooks** run `commit.before` and `push.before` through the same script, including
the control-plane leak gates (§3.2).

**How a lead knows what to dispatch.** Hooks can refuse a wrong spawn, but they cannot
speak to a lead between stages. So a lead's prompt includes a command,
`ensign next --bridge <bridge>`, which prints exactly what to dispatch now — "Dispatch
stage "review": 2 reviewer agents", "Wait: stage "test" is still running", or "The run is
finished: success" — and the budgets used. The lead runs it at the start and after every
stage. In live runs a haiku orchestrator followed it reliably, including when its work
item told it to skip stages.

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
| UserPromptSubmit `decision: block` shows its `reason` with no model call | yes; zero turns, zero cost. The harness frames it as "operation blocked by hook" |
| SessionStart hook stdout reaches the session's context | yes; appears as `SessionStart:startup hook success: <stdout>` |
| SessionStart stdout reaches the model whole, and only the session | yes; the model repeated ten markers spread through 9 KB and 60 KB blocks, and no subagent transcript contained any of it |
| The project `CLAUDE.md`, with its imports, reaches subagents | yes; every agent in a live run carried the session manual while it was imported there |
| A missing hook command blocks anything | no; non-blocking error, tool calls proceed |
| PreToolUse `allow` lifts a permission `ask` rule | no (tested headless); the ask still applies |
| SessionStart can pass values to the session's later commands | yes; a variable written to `CLAUDE_ENV_FILE` was visible to a Bash command in that session |
| `session_id` is the same for the session and its agents | yes; identical in hook payloads from the session, an orchestrator and its agents |
| UserPromptSubmit `additionalContext` reaches the session | yes; the session dispatched the lead as instructed on `go` |
| A full coding run, enforced only by hooks (sonnet session, haiku roles) | yes; four live scenarios, including a test failure that looped back and refused spawns |
| Subagents load relevant Claude Code skills on demand | yes; 10 of 10 subagent runs and 6 of 6 session runs loaded the matching skill and followed it (favourable conditions; see §4.4) |

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
Working on: review, 2 of 3 reviewers done · 2 of 3 passes used this cycle

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
  "suggest an improvement to starship"        write it up as a proposal

Projects: supernova (3 tasks), nebula (1 task)
```

Where each part comes from:

- **Pipelines and bundles** declare `help:` entries in their files: example requests in
  plain words, and one line on what happens. A bridge's own bundles appear without
  extra work.
- **The status lines** come from the task journal and the active run, in plain words.
  `status` prints them on their own, expanded: the task's goal, where it stands, the
  active run's stage and budgets, and what's outstanding.
- **Projects and tasks** come from the bridge. `help supernova` or `help auth-refactor`
  describes the project or task: its goal, where it stands, what's outstanding.

A pipeline or bundle without help entries is a validation error: a missing description is
caught, never guessed.

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

provides:
  squash: ensign git-catalog squash   # not bound to any event by default
```

- **The driver commits, not the coder.** Messages are uniform and numbered from state:
  `wip(c1 p1) BROKEN` with compile errors from the report in the body, `wip(c1 p2)`,
  `wip(c2 p1)`.
- **Staging is by name.** `ensign git-catalog commit-pass` stages exactly the report's `files`, then
  checks for any other modification; if there is one, the run ends as `error` rather than
  sweeping in or stashing someone's work.
- **A clean tree is required at `run.start`.**
- **The orchestrator learns about the catalog only through the bundle's augment**, so
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
| `curator` (§4.5) | the only writer of the guide | the guide, this run's proposals, sources' trust, `@style` |
| `coder` | — | the guide's active entries |

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
    ref: $PROJECTS/app/internal/session   # code held up as an example is referenced in place
    trust: exemplar
    ingested: 2026-10-29
```

- **`authoritative`** — a style document someone wrote on purpose. Its rules may enter
  the guide as `active` at once.
- **`exemplar`** — code held up as good. It shows habits as well as intent, so its rules
  enter as `candidate` and need a second sighting.
- Fetched content is data. A generator turns it into proposed rules; nothing in a source
  is followed as an instruction.

`style-generate` (in the bundle, `scope: project`) has no lead: the session dispatches one
`style-generator` per unprocessed source, in parallel, then one `curator`. When the
curator's rewrite is accepted, those sources are marked `processed`. `--rebuild` gives
every source to a generator again; the ledger must still account for every existing id.

### 9.5 Writing the guide

The curator's rewrite is checked when it stops (§4.5): a refused rewrite blocks its stop
with the reasons, and it tries again. The accepted document is written to
`learned.yaml`, which the journal mechanism (§10.2) commits and pushes, so
`git log -p` on the guide shows every change.

### 9.6 Is it working?

The run history records each style pass's report, including which entries were hit. An
active entry hit run after run is badly worded; the curator is told to reword such
entries. A learned rule that is clearly generic is a candidate for the control plane's
house style, through the proposal path (§3.2) — never copied across directly.

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
- The leak gates (§3.2) are git hooks, so they run on every commit regardless of who
  makes it.
- **From a work machine, the control plane is the only thing pushed to the personal
  repo.** Bridges, journals and workspaces in a work context stay on that context's
  infrastructure.

### 10.2 Task journals

Task journals are verbose by design: progress log, decisions with reasoning, outstanding
work, notes. The bar is that a fresh session with no context can pick the work up.

Journals live in the bridge, and a bridge is always git, so committing them doesn't
depend on the project's VCS:

- **Default: on.** Journal files are staged by name, committed, and pushed in the
  bridge's repo.
- **Off:** the project declares `journal: { commit: off, reason: "<why>" }`. The Stop
  hook then skips journal checks for that project. Turning it off needs a reason, like a
  `not-applicable` binding.
- The agent commits through `ensign journal commit -m "<message>" <files>`, which stages,
  commits and pushes in one step.

---

## 11. Build, platforms and installation

Starship runs on **Linux and Windows**. A machine that only uses it needs **Claude Code
and git**, nothing else.

### 11.1 `ensign`

The resolver, the hook driver, the built-in bundle handlers and the CLI are one Go
binary, `ensign`. The project is starship; the binary has its own name so that it never
collides with the starship shell prompt, whose binary is `starship`.

- **Config is YAML**, parsed by a library compiled into the binary.
- **Built-in handlers are subcommands**, not scripts — `ensign git-catalog record-base`,
  `ensign git-catalog commit-pass`, `ensign git-catalog squash` — and run inside the
  hook's own process. Other handlers run through the platform's shell in the workspace;
  `@name` runs a binding.
- **State-file locking uses a portable file-lock library**, not `flock`.

### 11.2 Prebuilt binaries in the repo

```
bin/
  linux-amd64/ensign
  windows-amd64/ensign.exe
```

- **A release is cut by CI.** You trigger the release workflow with a version
  (`gh workflow run release -f version=1.4.0`, or from the GitHub UI). On one Linux
  runner it cross-compiles every target, commits the binaries to `master`, and creates
  the tag `v1.4.0` **on that commit**, so a tagged checkout always contains its own
  binaries. Tags are never created by hand. Free for public repos.
- **Only CI builds committed binaries.** A local build embeds local file paths; building
  on a work machine would carry them into the public repo.
- **Builds are reproducible**: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -buildid="`.
  Anyone can rebuild from the tagged source and compare.
- **Rebuilt only on release**, which keeps repo growth to roughly one set of binaries
  per release. If it grows too heavy, binaries move to GitHub Releases with a fetch step.
- **CI also tests on a Windows runner**: paths, locking, and running `ensign.exe` from
  PowerShell and Git Bash. The harness probe (§7.1) needs a Claude login and stays manual.
- Go is needed only on a machine where `ensign` itself is being developed.

### 11.3 Bridges, pinning and installation

```
~/bridges/work/
  bridge.yaml                    control-plane: { remote: <url>, tag: v1.4.0 }
  .starship/control-plane/       the bridge's clone, checked out at the tag (gitignored)
  .claude/agents/                generated from the pinned clone (gitignored)
  .claude/skills/                on-demand skills, generated likewise (gitignored)
  CLAUDE.md                      setup steps and the heartbeat rule; no imports
```

**Pinning.** `bridge.yaml` names a release tag. The pinned clone is a shallow fetch of
exactly that tag; a bridge needs no history. Every run snapshot records the tag.

**`ensign update [<tag>]`** — on request only. You run it, or ask Claude to ("update
the bridge"); no hook runs it and Claude never starts it on its own. `help` notes when a
newer release exists. It fetches tags, moves the pin to the newest
release (or the one named), refuses while any run in the bridge is active, then runs
`ensign link`. The change to `bridge.yaml` is committed and pushed in the bridge's repo,
so the bridge's history records every version it ran.

**`ensign link`** makes the bridge match its pin: checks out the pinned tag and
regenerates `.claude/agents/` and `.claude/skills/` from it plus the bridge's own
bundles. It runs from `ensign update`, from `ensign init`, and from a SessionStart hook
that checks the generated files still match the pin (they won't after a fresh clone of
the bridge). Rules:

- **Never while a run is active.** A run's roles cannot change under it.
- **Hand edits are protected.** Link keeps a manifest of hashes of what it wrote and
  refuses to overwrite a modified file, naming the right place for the change: the
  control plane, or a role in a bridge bundle.
- **Name clashes are errors.** A bridge bundle's role with the same name as a
  control-plane role is refused unless it declares `replaces: <role>`.
- **Failures are loud.** A failed link stops the session's starship features with the
  reason; it never carries on with stale files.
- **No symlinks**: on Windows they need developer mode, and git checks them out as plain
  files by default.

**Developing the control plane.** A bridge may set `remote` to the local `~/starship`
and `ref` to any commit instead of a tag, to try changes before release. The binaries at
an untagged commit are those of the last release, so such runs are marked `dev` in
their snapshot and in `help`.

**The launcher.** `ensign setup` (§12) installs `ensign` into a per-user directory on
PATH; there are no per-platform install scripts. Hooks call `ensign hook <event>`,
identical on every platform. When invoked, `ensign` finds the
bridge (from `CLAUDE_PROJECT_DIR`, read by the program rather than expanded by a shell,
or by walking up from the working directory) and hands off to that bridge's pinned
binary, so every bridge runs the `ensign` that matches its pipelines. Outside a bridge,
hook events are no-ops.

### 11.4 Portability rules

- `.gitattributes` with `* text=auto eol=lf`, so config and scripts are never converted to
  CRLF.
- Forward slashes in every path; a per-platform `$PROJECTS` root for workspace paths.
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

Nothing else is installed by hand. Setup fetches code and puts a binary on PATH, so
Claude asks first; it is an ordinary confirmation, not `go`, because no agents are
spawned.

### 12.2 What setup does

The bridge's committed `CLAUDE.md` (written by `ensign init`) carries a short setup
section that is visible before anything else exists: if `.starship/control-plane/` is
missing, the bridge isn't set up on this machine. Setup is two commands:

```
git clone --depth 1 --branch <tag from bridge.yaml> <remote> .starship/control-plane
.starship/control-plane/bin/<platform>/ensign setup [--projects <path>] ...
```

`ensign setup`, in Go and therefore the same on both platforms:

- installs itself into a per-user directory and adds it to the user's PATH (the user
  PATH in the registry on Windows, `~/.local/bin` on Linux);
- runs `ensign link` to generate the bridge's role and skill files;
- writes `.starship/machine.yaml` (gitignored): this machine's paths, such as the
  `$PROJECTS` root. Claude asks for them in conversation and passes them as flags. A task
  that needs a path that isn't set stops with a clear message.

Product workspaces are not cloned by setup. Opening a task checks that its workspace
exists and stops if not.

### 12.3 The restart

The session that ran setup cannot switch over to the result: it started before the
control plane's `CLAUDE.md` and the role files existed, and its process still has the old
PATH, so its hooks cannot find `ensign`. Setup ends with "bridge ready — restart claude".

The same applies after `ensign update`. SessionStart records the pin a session started
on; after an update, the driver refuses to start a new run in that session and asks for a
restart, so a session never mixes two releases.

### 12.4 The very first install

Before any bridge exists, `ensign` comes from the control plane itself: clone this repo at
a release tag, run `bin/<platform>/ensign setup` to install the launcher, then
`ensign init <dir>` to create a bridge. The clone can be deleted afterwards. A one-line
download-and-run command may be documented as a convenience, never as the main path.

### 12.5 When `ensign` is missing: fail-open, and the heartbeat

Hook enforcement has a hole. If `ensign` cannot be found — PATH changed, binary deleted,
a broken update — Claude Code treats each hook as a **non-blocking error**, and every
gate stops being enforced: pipeline order, `go`, the commit gates. Only exit code 2
blocks, and a missing command cannot produce it.

There is no harness-level fix. A permission rule that forces a prompt for the Agent tool
would make a missing `ensign` visible, but a PreToolUse `allow` does not lift it (§7.1),
so it would also prompt when everything is working.

In practice the hole is only at session start. During a session `ensign` can hardly go
missing: the Claude Code process keeps the PATH it started with, and `ensign update`
swapping the binary is already handled (§12.3). Deleting the binary mid-session is the
only silent case, and rare enough to accept.

So there are two defences:

- **A heartbeat at session start.** SessionStart prints
  `STARSHIP-READY ensign <version> bridge <tag>`, followed by the session manual, which
  appear in the session's context (§7.1). The bridge's `CLAUDE.md`: **if that line is absent, or starship hooks report
  errors, say so plainly and do nothing except help fix it** — no pipeline, no agent
  spawn, no workspace change, no commit. This is a model-level rule, not a guarantee; it
  turns a silent failure into a visible one.
- **`ensign` fails closed, and its errors end the run** (§6.1). Any internal error — a crash, a corrupt state file, an
  unreadable snapshot — exits 2 with the reason, so the action is blocked rather than let
  through. (A Go panic already exits 2.) One exception: when a stop arrives with
  `stop_hook_active` set and the error repeats, the stop is let through, so a persistent
  fault cannot trap an agent in a loop.

### 12.6 Machines and containers

Bridges may share a machine, live on separate machines, or run in containers. Nothing in
the design assumes one bridge per machine:

- Machine-specific state is per bridge (`.starship/machine.yaml`), not per machine.
- `ensign setup` registers each bridge in a per-user registry (`~/.config/starship/` on
  Linux, `%APPDATA%\starship\` on Windows). The control plane's denylist gate checks the
  denylist of every registered bridge, so on a shared machine a change is checked
  against all of them.
- In a container, the container is the machine: setup runs inside it, and its registry
  holds the bridges in that container.

A bridge is normally **shared by every machine working in its context** — say a Linux and
a Windows machine on the same project, often doing platform-specific work. Splitting it
per machine would duplicate its projects, tasks and journals. Sharing needs only:

- **Fetch and rebase as you work.** SessionStart fast-forwards the bridge when it has no
  local changes, and every automatic commit fetches and rebases before pushing. A
  conflict where both sides only added lines is resolved directly; any other conflict
  stops and asks.
- Machine paths are per machine (`.starship/machine.yaml`) and task paths go through
  `$PROJECTS`, so the same `task.md` resolves on every machine. Run state stays on the
  machine that ran it.

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

1. **How Claude Code runs hook commands on Windows** (Git Bash, PowerShell, or direct
   process launch). `ensign hook <event>` is designed to work under all three; verify on
   a Windows machine.

---

## 15. Implementation status

Everything in §§2–12 is built, with unit tests (run in CI on Linux and Windows) and live
runs against Claude Code, except where listed below.

### 15.1 Commands

| Command | Does |
|---|---|
| `ensign init <dir> (--control-plane <path> \| --remote <url> --tag <tag>)` | create a bridge |
| `ensign setup [--projects <path>]` | fetch the pinned control plane, install the launcher, record machine paths, register the bridge, link |
| `ensign update [<tag>]` | move the bridge to a newer release |
| `ensign link [--force]` | regenerate the bridge's role, skill and manual files |
| `ensign project new <name>` / `ensign task new <p>/<t> --workspace <path>` / `ensign task open <p>/<t>` | scaffold and open |
| `ensign run <p>/<t> [--start <stage>] [--sync] [--dry-run]` | create a run for a task |
| `ensign run --pipeline <name> [--project <p>] [--rebuild] [--dry-run]` | create a project- or bridge-wide run |
| `ensign next [--run <id>]` | what to dispatch next |
| `ensign resume --answer <text> [--budget <b>=<n>]` | continue an `ask-user` run |
| `ensign journal commit -m <msg>` / `ensign journal push` | commit and push the bridge's records |
| `ensign git-catalog squash <p>/<t> -m <msg>` | collapse a task's catalog into one commit |
| `ensign style ingest <project> <path-or-url> --trust <t>` | add a style source |
| `ensign gate install --identity <email>` | install the leak gates in a control-plane clone |
| `ensign proposal check <file>` | check a proposal against every denylist |
| `ensign selftest` | re-check the Claude Code behaviour starship depends on |
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
  `ensign selftest` checks the hook's output reaches the model whole and stays out of
  subagents, and a unit test keeps the manual under the size it checks (9 KB).
- **A `claude` started from inside another Claude Code session is a child session.**
  Its prompt reaches UserPromptSubmit with a `Claude` line in front, so `help` doesn't
  match, and it inherits the parent's project directory. This is why `help` once went to
  the model in testing; a user's own session isn't affected. `ensign` starts its own
  `claude` (the selftest, the clean-room review) without the variables a session sets
  about itself, and live tests should do the same.
- **`ensign init` commits the new bridge.** Before, the first session in a new bridge
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
  makes its facts pending; `ensign skill discard <name>` drops them.
- **The `@done` check** runs before a run may end as `success`; a failure ends it as
  `error: done-check`.
- **Events on stages a pipeline doesn't have are inactive for it**, so a bundle or layer
  can serve several pipelines.

### 15.3 Not done

- **No release has been cut.** The release workflow exists but has never run, so there
  is no tag for a bridge to pin; pinning is tested against local tags.
- **How Claude Code runs hook commands on Windows** is unverified (§14). The code and its
  tests pass on Windows in CI.
- **Nested leads** are rejected by validation.
- **Style-guide metrics** (style diff size per run, flagging entries hit repeatedly)
  are recorded only as report history; nothing summarises them yet.
