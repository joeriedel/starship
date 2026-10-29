# Starship

An agent-orchestration control plane for [Claude Code](https://claude.com/claude-code).

Starship holds no application. It holds the configuration used to work on *other*
repositories — roles, pipelines, and the rules that govern them. You open a session in a
bridge, name a project and a task, and say what you want done; which agents run, in what
order, on which model, under which project's rules, is resolved from configuration.

> **Status: working, not yet released.** Everything in the design is built and tested,
> including live runs against Claude Code; no release has been cut yet. The design is in
> [`docs/design.md`](docs/design.md), and §15 lists what is and isn't done.

## The idea in one picture

```
control plane   this repo: roles, pipelines, extension bundles, the hook driver
   ▼
bridge          one per context (home, work, ...), kept wherever that context requires:
   ▼            projects, tasks, project skills, integrations
project         bindings (@build, @test, ...), pipeline extensions, rules
   ▼
task            workspace paths, VCS state, per-task tweaks, the task journal
```

Each layer can extend or override the one above it. The control plane knows nothing
about any project, so it can be shared across contexts that must stay separate, and
improvements to the process can flow back from any of them.

## A conversation on top, enforcement underneath

You never drive starship with commands. You talk to Claude Code in a bridge the way you
would anywhere else, and the machinery runs underneath:

```
you      open supernova / rate-limit
claude   Task rate-limit, workspace ~/src/supernova on branch rate-limit, clean.
you      add per-account rate limiting to the login endpoint; the limits come from config
claude   (reads the code, then proposes a plan: the work item, what done looks like,
         what's out of scope; then shows the run it set up)
         Coding pipeline: code ⇄ review (correctness, security) → style → test,
         3 passes per cycle. Type go to start.
you      go
claude   (dispatches the lead; agents work)
         Success after 2 passes. Changed: auth/limiter.go, auth/login.go, config.go.
         One review finding fixed, one deferred: ...
```

What happened underneath:

- **The session follows a manual, not a script.** Claude reads [`manual/session.md`](manual/session.md):
  plan first, never implement directly, write standing instructions into files, keep the
  task journal current. Inspecting the code and discussing it are free. Changing the
  workspace only happens through a run.
- **`ensign` turns the plan into a run.** Claude runs `ensign run supernova/rate-limit`,
  which merges the control plane, bridge, project and task into one snapshot: the
  pipeline, the bindings (what `@build` and `@test` mean here), the bundles, skills and
  budgets. It refuses if anything is missing. The snapshot is what the run follows, so
  editing config mid-run can't change it.
- **Only the word `go` starts it.** A hook intercepts the message and approves the run.
  "Yes" or "sounds good" do not.
- **Hooks enforce the pipeline; agents supply judgement.** The lead agent (the
  orchestrator) asks `ensign next` what to dispatch and writes the briefs. Every
  dispatch passes through Claude Code's hooks, where `ensign`:
  - refuses a stage out of order, over budget, or not approved;
  - rewrites the prompt to carry the stage brief, the workspace and its exact binding
    commands, and the relevant skills;
  - runs each stage in the foreground, so no agent spends a turn waiting.

  When a stage finishes, `ensign` reads its structured report, applies the transition,
  spends or resets budgets, and fires events. An agent can't skip a stage, invent an
  outcome, or quietly stop early.
- **Memory lives in files.** The task journal is committed and pushed in the bridge's
  git; a hook won't let a turn end with it uncommitted. A fresh session on another
  machine picks the work up from there.
- **Words that cost nothing.** `help`, `help <topic>`, `status` and `go` are answered
  by a hook without a model call. Help is generated from the configuration you're in, in
  plain requests ("squash this task's commits"), never commands.

The pipeline is a small state machine
([`pipelines/coding.yaml`](pipelines/coding.yaml)): code ⇄ review, fanned out one
reviewer per dimension in parallel → style → test. Success means it compiles and the
tests actually ran and passed. A run ends as `success`, `error` with a reason (a spent
budget, a blocked coder, a failing check), or `ask-user`, which pauses for your
decision and resumes.

## Extending it

The control plane knows nothing about any project. Everything specific is added below
it, by the layer that knows it, in one config file per layer: `bridge.yaml`,
`projects/<p>/project.yaml`, or the frontmatter of a task's `task.md`. Each layer
extends or overrides the one above, except for locked invariants.

| To change | Use | Example |
|---|---|---|
| what a step means in a project | **bindings** | `@build` → `./build.sh --fast`; `@test` declared not applicable, with a reason |
| what runs and in what order | **pipeline ops** | add a security review dimension; insert a migrations check before test; drop style |
| what else happens at a point in a run | **events** | before code: require a clean tree; after test: post a summary; on success: squash |
| what agents know | **skills** | a project's conventions for its build system, delivered to the stages that need them |
| a whole new process | **pipelines** | a new YAML state machine, run with `ensign run --pipeline <name>` |
| all of the above, packaged | **bundles** | `extensions/<name>/extension.yaml`, enabled with `use: [<name>]` |

```yaml
# projects/supernova/project.yaml
use: [git-catalog]

bindings:
  build: { command: "./build.sh --fast" }
  test:  { command: "./test.sh" }

pipelines:
  coding:
    ops:
      - append: { stage: review, dimensions: [security] }
      - insert: { before: test, stage: { id: migrations, role: tester,
                  brief: briefs/migrations.md,
                  outcomes: { fail: { goto: code }, pass: next } } }
      - set:    { budgets.test: 1 }

events:
  stage.after:test: [{ observe: scripts/post-summary.sh }]
```

The rules that keep extension safe:

- **Nothing is guessed.** A pipeline that needs a binding the project hasn't defined
  doesn't run. "Not applicable" must be declared, with a reason.
- **Lists are edited, never silently replaced.** Ops name the stage or field they touch.
- **Events are fired by code, not agents.** A `require` handler that fails stops the
  run; an `observe` handler is logged; an `augment` adds text to the next prompt.
- **A bundle declares the bindings it needs**, so it can't be enabled where it doesn't
  fit.

Two bundles ship with the control plane:

- **[`git-catalog`](extensions/git-catalog)** commits each coder pass, so the lead can
  see how the work moved, and squashes when the project says.
- **[`style-learning`](extensions/style-learning)** feeds what the style pass finds into
  a per-project style guide. It's curated for consistency and read by the coder on later
  runs. Style documents and exemplar code can be ingested to seed it.

A bridge can ship its own bundles, skills, roles and pipelines in the same formats.

### Improvements flow back

Agents report facts they discover ("this build needs a clean before switching
branches"); the `skill-curate` pipeline folds them into skills. Process improvements
are written up as proposals in the bridge, in general terms, and carried into the
control plane in a separate session. Commits there pass the leak gates: author
identity, every bridge's denylist, no agent attribution, and a review by a model with
no project context.

## Getting started

Starship runs on Linux and Windows, and needs Claude Code and git. The `ensign` binary
(starship's resolver, hook driver and CLI) ships prebuilt with each release.

```
git clone --branch <release tag> https://github.com/joeriedel/starship.git
starship/bin/<linux-amd64|windows-amd64>/ensign setup        # installs ensign on PATH
ensign init ~/bridges/home --remote https://github.com/joeriedel/starship.git --tag <release tag>
cd ~/bridges/home && claude                                  # then type: help
```

On another machine, clone the bridge and run `claude` in it; Claude sets it up. To check
that the Claude Code harness still behaves the way starship relies on, run
`ensign selftest` after upgrading Claude Code.

Building `ensign` yourself needs Go: `go build ./cmd/ensign`. Committed binaries come only
from the release workflow.

## License

[MIT](LICENSE)
