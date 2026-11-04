# Starship

> **Claude:** to set up a starship bridge, follow [`SETUP.md`](SETUP.md). Don't clone this repository into the user's folder.

A customizable control plane for Claude Code.

Starship adds tasks and pipelines on top of Claude Code. You still talk to Claude in a normal session, but code changes happen inside a pipeline run: a sequence of stages where separate agents write, review, tidy and test the change. A small Go binary, `ensign`, runs from Claude Code's hooks and checks each step against the pipeline definition, so the order of work doesn't depend on an agent following a prompt.

Each task keeps a journal in a git repo you own, so you can read back what happened on it or continue it from another machine.

## Getting started

You need Claude Code and git, on Linux, Windows or macOS. Claude handles the rest of the setup.

### 1. Make a bridge

A bridge is a folder, and a git repo of your own, where starship keeps your projects and tasks. Make an empty folder, start `claude` in it, and ask:

```
> make a starship bridge here, following https://github.com/joeriedel/starship/blob/master/SETUP.md
```

When it finishes, restart `claude` in the same folder. To use the bridge on other machines, push it to a private repo, then clone it on the other machine and start `claude` there.

### 2. Add a project

Tell Claude about a codebase you work on:

```
> add a project called supernova, its code is in ~/projects/supernova
```

Claude reads the code, suggests how the project builds and runs its tests, and asks you to confirm. If you don't say where the code is, it asks.

### 3. Start a task and describe the work

```
> new task fix-login in supernova
> logins fail when the password has a trailing space; fix it and add a test
```

The task uses the project's code unless you name a different location, such as a second checkout.

Claude reads the code and replies with a plan. Type `go` to start the run. When the run ends you get a report of what changed, what each stage found, and what the lead decided about it.

### 4. Come back to it later

`open supernova / fix-login` reopens the task in a new session or on another machine. `help` lists what you can ask for, and `status` shows where things stand.

## Layout

The starship is this repo: the control plane. It holds the roles, pipelines and rules. It contains nothing about your projects.

A bridge is your own git repo, hosted wherever you like. It holds your projects, your tasks and their journals, and your project-specific skills. You can keep several, such as `~/bridges/work` and `~/bridges/home`, and they share nothing with each other.

A project lives in a bridge and fills in what the pipeline's named steps mean there: `@build` and `@test` are placeholders, and the project sets the command for each. It also picks its review dimensions.

A task lives in a project and points at a workspace, which is your code repo in any version control. The task's journal stays in the bridge. Starship doesn't write its own files into the workspace, so the only thing that lands there is the change.

`ensign` is the binary that does the bookkeeping. Claude Code calls it from its hooks, so it sees each agent dispatch and each report.

The agents are role definitions in [`roles/`](roles): an orchestrator that leads a run, a coder, reviewers, a style reviewer, a tester, a documentation writer, an explorer for reading code, a style generator that proposes style rules, and a curator that maintains skills and style guides. Each role file sets the agent's job and its Claude model.

## How a run works

The session agent plans the work with you and doesn't edit code itself. When you type `go`, a UserPromptSubmit hook checks that the message is exactly that word and marks the pending run as approved. "Yes" or "sounds good" won't start it. The session then dispatches the orchestrator, which leads the run.

A pipeline is a YAML file. The default one, [`pipelines/coding.yaml`](pipelines/coding.yaml), defines this loop:

```
code -> review -> style -> test -> docs -> success
  ^        |                 |
  +--------+  findings       |
  +--------------------------+  fail
```

Each stage declares its outcomes and where each one goes. A review with findings returns to code, and a failed test returns to code. The docs stage updates the files a project declares in `@docs` and is skipped when it declares none. Two budgets bound the loop: `coder-loop` counts passes through the code stage and `test` counts test cycles, each 3 by default. A run that exhausts a budget ends in an error instead of retrying.

Test results come from the project's `@test` command. If the binding is marked `plain`, `ensign` runs the command itself and uses the exit code, with no tester agent involved. Otherwise a tester agent runs the suite and reports.

## What the hooks enforce

A PreToolUse hook checks each agent dispatch against the pipeline, and refuses one that's out of order or that arrives before `go`.

When an agent finishes, a SubagentStop hook reads its report. If the outcome is missing or isn't one the stage declares, the hook sends the agent back to fix it. An orchestrator that tries to hand back early gets refused until the pipeline has finished.

The same PreToolUse hook checks every Edit, Write and NotebookEdit against the folders the caller may edit. The session gets the bridge, coders and style reviewers get their run's code and scratch folders, and other agents get temp and their run's scratch folder. This check doesn't cover shell commands, so a Bash write isn't blocked.

`help` and `status` never reach the model. The UserPromptSubmit hook answers them directly and stops the turn.

## Task records

Each task has two files in the bridge. `task.md` holds the current state: goal, scope, status, outstanding work and decisions. `history.md` is append-only, with an account of each run. `ensign` adds a line of facts when a run ends, the session writes the account under it, and a hook stops the session ending a turn with uncommitted journal changes.

Because the journal is plain files in git, a new session or another machine picks up the task by reading them.

## Skills and style

Agents can report facts as they work, such as "this build needs a clean after switching branches". The facts wait in the bridge until you say "curate the skills", which runs a pipeline where a curator folds them into the matching skills. A fact for a skill that doesn't exist yet stays on hold until you create the skill or discard it.

A skill reaches agents in one of two ways. An `inject` skill goes into the brief of every stage whose role it lists. An `on-demand` skill exposes only its description, and the agent loads the body if it decides it needs it.

Per-project style guides need the `style-learning` extension, described below.

## Customization

You customize starship from your bridge, project or task, without forking the control plane. Each level has one config file, and each can add to or override the level above it.

| You want to | Use |
|---|---|
| say what `@build` or `@test` means for a project | bindings |
| add a review dimension, insert a stage, drop one, change budgets | pipeline ops |
| run something at a point in a run: before code, after test, on success | events |
| teach agents your project's conventions | skills |
| define a new process | pipelines |
| package any of the above for reuse | extensions |

```yaml
# projects/supernova/project.yaml
use: [git-catalog]                       # commit every coder pass

platforms: [linux, macos]                # required: what the project supports

bindings:
  build: { command: "./build.sh --fast" }
  test:  { command: "./test.sh" }

rules:                                   # standing rules, added to every work item
  - Never touch the vault.

pipelines:
  coding:
    ops:
      - append: { stage: review, dimensions: [security] }   # one more reviewer
      - set:    { budgets.test: 1 }                         # one test cycle

events:
  stage.after:test: [{ observe: scripts/post-summary.sh }]
```

`rules:` is a list of strings in `bridge.yaml`, an extension, a project or a location. Together, without duplicates, they are appended to every work item under `## Project rules`, and the run summary shows each rule with where it came from. A task's own frontmatter `rules:` is ignored; put task-specific rules in the task's Scope.

Three extensions ship with starship, and a project turns each one on through `use:`:

- [`git-catalog`](extensions/git-catalog) commits each coder pass and squashes the commits into one when you ask.
- [`style-learning`](extensions/style-learning) records what the style pass finds in a per-project style guide and adds the guide's active rules to the coder's brief. You can seed it with style docs or example code.
- [`github-pr`](extensions/github-pr) gives each task its own branch and can use your own tests or the repository's CI. After a task passes, you ask for a pull request or for it to go straight to the main branch.

A repository can also ship its own project settings in `.starship/project.yaml`, so adding it as a project needs nothing filled in. Starship ships one. To work on starship from your own bridge, say "improve starship": each change becomes a task on its own branch, with CI as its tests.

## Further reading

[`docs/design.md`](docs/design.md) covers the full design: layers, pipelines, the hook driver, customization points, what has been tested against Claude Code, and what's still open.

## License

[MIT](LICENSE)
