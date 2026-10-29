# Starship

**Give Claude Code a crew, a process, and a memory.**

You've probably had this experience. You ask Claude to "just quickly fix" something. It
does. Along the way it tweaked three things you didn't ask about, reviewed its own work
(glowing review, naturally),[^1] skipped the slow tests because they were slow, and by
tomorrow has no idea any of it happened.

Starship is the fix for that. You still just talk to Claude. But behind the
conversation there's a crew of specialist agents, a process they can't wander off, and a
written record that outlives the session.

## A day on the bridge

It's Monday. You open a terminal in your work bridge and start Claude.

```
you      open supernova / rate-limit
claude   Task rate-limit: workspace ~/src/supernova, branch rate-limit, clean.
you      add per-account rate limiting to the login endpoint, limits from config
```

Claude reads the code (looking doesn't need permission; changing does) and comes back
with a plan. It says what
it will change, what done looks like, and what it deliberately won't touch. It shows you
the run it has set up: code, review, style, test. Then it waits.

```
you      go
```

Now the crew goes to work:

- A **coder** makes the change and confirms it compiles.
- **Reviewers** read it, one per concern (correctness, security, whatever your project
  asks for), and they don't trust the coder's opinion of itself.
- They find something, so it goes back to the coder. That's the loop doing its job.
- A **style reviewer** tidies it to match how your project actually writes code.
- A **tester** builds it and runs the real suite, slow parts included. A test fails, so
  it goes back to the coder again.
- Second time round it passes. Everyone is mildly surprised, as is traditional.

You get a report: what changed, every finding and what was decided about it, and anything
deferred.

On Tuesday you're on a different machine. You open the same task, and Claude knows
exactly where things stand. The task journal was committed and pushed last night, and a
hook wouldn't let the session end until it was. Hooks are like that.

At no point did you type a command. You just had a conversation.

## The cast

**The starship** is this repo, the control plane. It holds the roles, the pipelines and
the rules. It knows nothing about any of your projects, which is deliberate: it's public,
it's shared, and it can be improved from anywhere.

**A bridge** is where you stand and give orders. You keep one for each part of your life,
such as `~/bridges/work` and `~/bridges/home`. A bridge is a git repo of your own, hosted
wherever that part of your life requires. It holds your projects, your tasks and their
journals, your project-specific skills, and anything else that must never leave it. Your
work bridge stays at work; it doesn't come home for the weekend.

**Projects and tasks** live in the bridge. A project says what things mean there: what
`@build` runs, what `@test` runs, which review concerns matter. A task points at a
workspace (your actual code repo, in any version control) and keeps the journal.
Starship never writes its own files into your code; the only thing that lands there is
the change itself.

**The ensign** is `ensign`, a small binary that does the paperwork. It's wired into
Claude Code's hooks, so it sees every agent dispatched and every report filed. It
doesn't get tired, it doesn't get talked round, and it doesn't take "I'm sure the tests
pass" for an answer.

**The crew** are role definitions: an **orchestrator** that leads a run, plus a coder,
reviewers, a style reviewer, a tester, an explorer for reading code, and a curator that
keeps skills and style guides tidy, like a librarian with permission to throw things
away. Each has a narrow job and a model to match.

## How it works, without the jargon

- **The agents bring judgement; the ensign brings the clipboard.** The process lives in
  config and is enforced by hooks, not in a prompt that hopes for the best.
- **Every run starts from a plan.** Claude follows a session manual: understand first,
  plan, never edit the code directly. Code changes only happen inside a run.
- **Only `go` starts a run.** Not "yes", not "sounds good", not "I suppose
  so". A hook checks for the exact word before anything happens.
- **Runs are small state machines.** A pipeline is a YAML file. Code goes to review;
  findings go back to code; a failed test goes back to code; budgets stop it looping
  forever, which it otherwise would, quite happily. Success means it compiled and the tests actually ran and passed.
- **Agents can't cut corners.** If one tries to skip a stage, the dispatch is refused. A
  report missing its outcome gets sent back to be redone. A lead that tries to clock off
  early gets told the run isn't finished.
- **Memory is files, not vibes.** Task journals are committed and pushed in the bridge,
  so any session on any machine can pick the work up.
- **Some words are free.** `help`, `status` and `go` are answered by a hook without
  calling the model at all. `help` tells you what you can ask for in the bridge you're
  in, in plain language.

## Make it yours

Starship is built to be extended from below. Whatever is specific to you goes in your
bridge, project or task, never in the control plane. There's one config file at each
level, and each level can add to or override the one above it.

| You want to | Use |
|---|---|
| say what "build" or "test" means for a project | **bindings** |
| add a review concern, insert a stage, drop one, change budgets | **pipeline ops** |
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
      - set:    { budgets.test: 1 }                         # patience is finite

events:
  stage.after:test: [{ observe: scripts/post-summary.sh }]
```

Two bundles ship in the box:

- **[`git-catalog`](extensions/git-catalog)** commits each coder pass, so the lead can
  see how the work evolved. It squashes everything into one commit when you say so.
- **[`style-learning`](extensions/style-learning)** turns what the style pass finds into
  a per-project style guide. The coder reads it on later runs, so the same comment
  shouldn't come up twice. You can seed it with style docs or exemplar code.

Agents also report facts they discover along the way ("this build needs a clean after
switching branches"), and a curator folds them into skills. The crew gets better at
your projects the more it works on them.

## Things it flatly refuses to do

- **Guess.** If a pipeline needs `@test` and the project hasn't said what that means,
  the run doesn't start. If there really is no test suite, you have to say so, with a
  reason.
- **Start without your `go`.**
- **Let your work leak into public.** Improvements discovered in a bridge come back as
  proposals written in general terms. Commits to the control plane then pass the leak
  gates: the right author identity, every bridge's list of banned terms, and a review by
  a model that has never seen your projects.
- **Sign anything as an AI.** No co-author trailers, no "generated with". The work
  is yours. So, admittedly, is the blame.

## Getting started

You need Claude Code and git, on Linux or Windows. Each release ships `ensign` prebuilt,
so there's nothing to compile.

```
git clone --branch <release tag> https://github.com/joeriedel/starship.git
starship/bin/<linux-amd64|windows-amd64>/ensign setup        # puts ensign on PATH
ensign init ~/bridges/home --remote https://github.com/joeriedel/starship.git --tag <release tag>
cd ~/bridges/home && claude                                  # then type: help
```

On another machine, clone your bridge and run `claude` in it, and Claude sets the rest
up. After upgrading Claude Code, run `ensign selftest`. It runs a few cents' worth of
probe agents to check that Claude Code still behaves the way starship relies on.
Software changes; it's wise to ask.

If you'd rather build `ensign` yourself, you need Go: `go build ./cmd/ensign`.

## Further reading

[`docs/design.md`](docs/design.md) has the full design: layers, pipelines, the hook
driver, the extension points, what has been verified against Claude Code, and what is
still open.

## License

[MIT](LICENSE)

[^1]: Very few people, and no language models, have ever reviewed their own work and
    found it wanting.
