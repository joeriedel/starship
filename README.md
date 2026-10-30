# Starship

> **Claude:** to set up a starship bridge, follow [`SETUP.md`](SETUP.md). Don't clone
> this repository into the user's folder.

**Use Claude Code for real, ongoing work. Every change is reviewed and tested, and every
task's history is kept.**

Starship is a control plane for Claude Code. You keep talking to Claude the way you
already do, but each piece of work runs as a task. It's planned before anything changes,
carried out by separate agents that write, review, tidy and test the code, and written
down as it goes.

What that gets you:

- **The full history of every task.** Weeks later, when an issue turns up in something
  you shipped, the task is still there: what was asked for, the plan, each decision and
  why, every review finding and what was done about it, the test runs, and a commit for
  each pass. Keeping a record like that by hand is something nobody actually manages.
  Starship keeps it without being asked.
- **Changes that were checked before you see them.** Code is reviewed by agents that
  didn't write it, and only counts as done once it builds and the full test suite has run
  and passed. Hooks enforce this, so no agent can skip it.
- **Pick up where you left off, anywhere.** Tasks and their journals live in a git repo of
  your own, so a new session tomorrow, or a different machine, carries on with nothing
  lost.
- **It learns your codebase.** Style corrections become a per-project guide, and facts
  agents discover become skills, so later runs need less correcting.

## Getting started

Starship needs nothing but **Claude Code and git**, on Linux, Windows or macOS. There's
nothing else to install or build; Claude sets everything up.

**1. Make a bridge.** A bridge is a folder, and a git repo of your own, where starship
keeps your projects and tasks. Make an empty folder, start `claude` in it, and ask:

```
> make a starship bridge here, following https://github.com/joeriedel/starship/blob/master/SETUP.md
```

When it's done, restart `claude` in the same folder. To use the bridge on other machines
too, push it to a private repo; on another machine, clone it and start `claude` in it.

**2. Add a project.** Tell Claude about a codebase you work on:

```
> add a project called foobar, its code is in ~/projects/foobar
```

Claude reads the code and suggests how the project builds and runs its tests, for you to
confirm. If you don't say where the code is, it asks.

**3. Start a task and describe the work.**

```
> new task fix-login in foobar
> logins fail when the password has a trailing space; fix it and add a test
```

A task works in the project's code unless you name a different location, such as a
second checkout.

Claude reads the code and comes back with a plan. Type `go` to start the run. A coder
makes the change, reviewers who didn't write it check it, a style pass tidies it, and a
tester runs the full suite; anything they find goes back to the coder. You get a report
when it's done: what changed, every finding and what was decided about it.

**4. Come back to it any time.** `open foobar / fix-login` picks the task up where it left
off, in a new session or on another machine. Type `help` to see everything you can ask
for, and `status` for where things stand.

## The cast

**The starship** is this repo, the control plane. It holds the roles, the pipelines and
the rules. It knows nothing about any of your projects, which is deliberate: it's public,
it's shared, and it can be improved from anywhere.

**A bridge** is where you stand and give orders. You keep one for each part of your life,
such as `~/bridges/work` and `~/bridges/home`. A bridge is a git repo of your own, hosted
wherever that part of your life requires. It holds your projects, your tasks and their
journals, your project-specific skills, and anything else that must never leave it. Your
work bridge stays on work infrastructure, and nothing in it reaches the others.

**Projects and tasks** live in the bridge. A project fills in what the pipeline's named
steps mean there: `@build` and `@test` are placeholders, and the project says which
command each one runs. It also says which review dimensions matter. A task points at a
workspace (your actual code repo, in any version control) and keeps the journal.
Starship never writes its own files into your code; the only thing that lands there is
the change itself.

**The ensign** is `ensign`, a small binary that does the paperwork. It's wired into
Claude Code's hooks, so it sees every agent dispatched and every report filed. It
enforces the pipeline in code, so the rules hold no matter what an agent says.

**The crew** are role definitions: an **orchestrator** that leads a run, plus a coder,
reviewers, a style reviewer, a tester, an explorer for reading code, and a curator that
keeps skills and style guides tidy. Each has a narrow job and a Claude model sized to it.

## How it works

- **The agents bring judgement; the ensign brings the clipboard.** The process lives in
  config and is enforced by hooks, not by instructions in a prompt.
- **Every run starts from a plan.** Claude follows a session manual: understand first,
  plan, never edit the code directly. Code changes only happen inside a run.
- **Only `go` starts a run.** Not "yes" or "sounds good": a hook checks for
  the exact word before anything happens.
- **Runs are small state machines.** A pipeline is a YAML file. Code goes to review;
  findings go back to code; a failed test goes back to code; budgets stop it looping
  forever. Success means it compiled and the
  tests actually ran and passed.
- **Agents can't cut corners.** If one tries to skip a stage, the dispatch is refused. A
  report missing its outcome gets sent back to be redone. An orchestrator that tries to
  stop early is told the run isn't finished.
- **Memory lives in files.** Task journals are committed and pushed in the bridge,
  so any session on any machine can pick the work up.
- **Some words don't need the model.** `help`, `status` and `go` are answered by a hook without
  calling the model at all. `help` tells you what you can ask for in the bridge you're
  in, in plain language.

## Make it yours

You customise starship without forking it. Whatever is specific to you goes in your
bridge, project or task, never in the control plane. There's one config file at each
level, and each level can add to or override the one above it.

| You want to | Use |
|---|---|
| say what `@build` or `@test` means for a project | **bindings** |
| add a review dimension, insert a stage, drop one, change budgets | **pipeline ops** |
| run something at a point in a run: before code, after test, on success | **events** |
| teach agents your project's conventions | **skills** |
| invent a whole new process | **pipelines** |
| package all of the above to reuse | **extensions** |

```yaml
# projects/supernova/project.yaml
use: [git-catalog]                       # commit every coder pass

bindings:
  build: { command: "./build.sh --fast" }
  test:  { command: "./test.sh" }

pipelines:
  coding:
    ops:
      - append: { stage: review, dimensions: [security] }   # one more reviewer
      - set:    { budgets.test: 1 }                         # one test cycle

events:
  stage.after:test: [{ observe: scripts/post-summary.sh }]
```

Two extensions ship in the box:

- **[`git-catalog`](extensions/git-catalog)** commits each coder pass, so the
  orchestrator can see how the work evolved. It squashes everything into one commit when
  you say so.
- **[`style-learning`](extensions/style-learning)** turns what the style pass finds into
  a per-project style guide. The coder reads it on later runs, so the same style
  issues don't keep coming up. You can seed it with style docs or exemplar code.

Agents also report facts they discover along the way ("this build needs a clean after
switching branches"), and a curator folds them into skills. Over time the crew stops
rediscovering the same things.

## Things it won't do

- **Guess.** If a pipeline needs `@test` and the project hasn't said what that means,
  the run doesn't start. If there really is no test suite, you have to say so, with a
  reason.
- **Start without your `go`.**
- **Let your work leak into public.** Improvements discovered in a bridge come back as
  proposals written in general terms. Commits to the control plane then pass the leak
  gates: the right author identity, every bridge's list of banned terms, and a review by
  a model that has never seen your projects.
- **Sign anything as an AI.** No co-author trailers, no "generated with".

## Further reading

[`docs/design.md`](docs/design.md) has the full design: layers, pipelines, the hook
driver, the customization points, what has been verified against Claude Code, and what is
still open.

## License

[MIT](LICENSE)
