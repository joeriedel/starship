# Starship

**Give Claude Code a crew, a process, and a memory.**

Ask a single Claude session to make a change and it will usually do it, but it is also
the one deciding what "done" means. It writes the code, judges its own work, decides
which tests are worth running, and forgets all of it when the session ends.

Starship splits that up. You still just talk to Claude. Behind the conversation, a crew
of specialist agents does the work in stages, a process enforced by hooks decides what
happens next, and a written record carries the work from one session to the next.

## Getting started

You need Claude Code and git, on Linux or Windows; macOS isn't supported yet.

Make a folder for your bridge, start `claude` in it, and ask:

```
> make a starship bridge here, from https://github.com/joeriedel/starship
```

Claude asks where your code lives on this machine, sets the bridge up, and asks you to
restart `claude`. Then type `help`.

The bridge is a git repo of your own. Push it somewhere private, and on another machine
clone it and run `claude` in it; Claude sets that machine up the same way.

After upgrading Claude Code, run `ensign selftest`. It runs a few cents' worth of probe
agents to check that Claude Code still behaves the way starship relies on.

If you'd rather build `ensign` yourself, you need Go: `go build ./cmd/ensign`.

## A day on the bridge

It's Monday. You open a terminal in your work bridge (a private repo that holds your
work projects; more on bridges below) and start Claude.

```
> open supernova / rate-limit

⏺ Task rate-limit: workspace ~/src/supernova, branch rate-limit, clean.

> add per-account rate limiting to the login endpoint, limits from config
```

That's project `supernova`, task `rate-limit`. Claude reads the code (looking doesn't
need permission; changing does) and comes back with a plan. It says what it will change,
what done looks like, and what it deliberately won't touch. It shows you the run it has
set up: code, review, style, test. Then it waits.

```
> go
```

Now the crew goes to work:

- A **coder** makes the change and confirms it compiles.
- **Reviewers** read it, one per review dimension (correctness, security, whatever your
  project asks for), independently of the coder.
- They find something, so it goes back to the coder for another pass.
- A **style reviewer** tidies it to match how your project actually writes code.
- A **tester** builds it and runs the full suite. A test fails, so
  it goes back to the coder again.
- Second time round it passes.

You get a report: what changed, every finding and what was decided about it, and anything
deferred.

On Tuesday you're on a different machine. You open the same task, and Claude knows
exactly where things stand. The task journal was committed and pushed last night, and a
hook wouldn't let the session end until it was.

Apart from `go`, you never typed a command. You just had a conversation.

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
| package all of the above to reuse | **bundles** |

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

Two bundles ship in the box:

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

## For Claude: setting up a bridge

When asked to make a starship bridge in a folder:

1. Ask the user where their code checkouts live on this machine (for example `~/src`).
   Never guess it.
2. From the folder, run:

   ```
   TAG=$(git ls-remote --tags --refs --sort=-v:refname https://github.com/joeriedel/starship.git 'v*' | head -n 1 | sed 's#.*/##')
   git clone -q --depth 1 --branch "$TAG" https://github.com/joeriedel/starship.git .starship/control-plane
   .starship/control-plane/bin/linux-amd64/ensign init . --projects <their code folder>
   ```

   On Windows the binary is `.starship/control-plane/bin/windows-amd64/ensign.exe`.
   `init` pins the bridge to that release, puts `ensign` on PATH, and commits the
   bridge's files. It refuses to overwrite files already in the folder; if it does,
   tell the user which ones and let them decide.
3. Tell the user to restart `claude` in the folder and type `help`. Suggest they push the
   bridge to a private repo of their own, never to starship.

## Further reading

[`docs/design.md`](docs/design.md) has the full design: layers, pipelines, the hook
driver, the extension points, what has been verified against Claude Code, and what is
still open.

## License

[MIT](LICENSE)
