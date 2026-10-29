# Starship

**Give Claude Code a crew, a process, and a memory.**

There is a particular kind of silence that follows the words "I've fixed it." It is the
silence of someone who has just been told the tests pass, and who knows, in the deep
and unreasonable way that experience teaches, that nobody has actually run them.

You asked Claude to make a small change. It made the change. It also improved three
things you hadn't mentioned, reviewed its own work and found it excellent,[^1] and
decided the slow tests were probably fine because they had been fine last time. By
tomorrow it will remember none of this, which in some ways is a mercy.

Starship exists because that is not a way to run anything.

You still just talk to Claude. But somewhere beneath the conversation there is now a
crew of agents who each do one job, a process none of them can wriggle out of, and a
written record that is still there in the morning.

> **Status: working, not yet released.** Everything below is built and tested,
> including live runs against real Claude Code. No release has been cut. The full design
> is in [`docs/design.md`](docs/design.md), for people who like that sort of thing.[^2]

## A day on the bridge

It is Monday, which is never a promising start. You open a terminal in your work bridge
and start Claude.

```
you      open supernova / rate-limit
claude   Task rate-limit: workspace ~/src/supernova, branch rate-limit, clean.
you      add per-account rate limiting to the login endpoint, limits from config
```

Claude goes and reads the code. Reading is always allowed; it is the *doing* that
requires paperwork. It comes back with a plan: what will change, what "done" looks like,
and what it will very carefully not touch. It shows you the run it has prepared. Then it
waits, because nothing happens on this bridge until someone says the word.

```
you      go
```

And the crew goes to work:

- A **coder** makes the change and confirms it compiles. This is the coder's whole
  job, and it is not invited to have opinions about the rest of the codebase.
- **Reviewers** read the result, one for each concern your project cares about. They
  have not been told what the coder thinks of its own work, and would not care if they
  had. They find something. Back it goes.
- A **style reviewer** makes it look like the code around it, rather than like code
  that wandered in from another project and is hoping nobody notices.
- A **tester** builds it and runs the actual suite, the whole suite, the slow parts too.
  A test fails. Back it goes again.
- The second time round, it passes. Everyone is mildly surprised, as is traditional.

You get a report: what changed, every finding and what was decided about it, and
anything set aside for another day.

On Tuesday you are on a different machine. You open the same task and Claude knows
exactly where things stand, because last night the task journal was committed and
pushed. A hook would not let the session end until it was. Hooks are like that.

At no point did you type a command. You had a conversation, and some things happened
that you can check.

## The cast

**The starship** is this repository: the control plane. It holds the roles, the
pipelines and the rules, and it knows nothing whatsoever about your projects. This is
not ignorance but policy. The starship is public, it is shared between every part of
your life, and things that are shared between every part of your life should not know
what any of them are called.

**A bridge** is where you stand and give orders. You keep one for each part of your
life that ought to stay separate: `~/bridges/work`, say, and `~/bridges/home`. A bridge
is a git repository of your own, kept wherever that part of your life insists it be
kept. It holds your projects, your tasks and their journals, the skills specific to
your code, and a list of words that must never leave it. The work bridge stays at
work. It does not come home for the weekend.

**Projects and tasks** live on the bridge. A project knows what things *mean* there:
what `@build` runs, what `@test` runs, which kinds of review matter. A task points at a
workspace (your actual code, in whatever version control it happens to live) and keeps
the journal. Starship never leaves its own belongings in your code. The only thing that
arrives in your workspace is the change you asked for.

**The ensign** is `ensign`, a small binary that does the paperwork. It sits inside
Claude Code's hooks, where it sees every agent sent out and every report that comes
back. It does not get tired. It cannot be reasoned with, flattered, or told that the
tests are *basically* passing. It has a clipboard, and on the clipboard is the
pipeline, and that is the end of the discussion.

**The crew** are the roles. An **orchestrator** leads each run and does the thinking
about what to ask for. Under it are a coder, reviewers, a style reviewer and a tester.
An explorer reads code so that nobody else has to hold all of it in their head. A
curator keeps the skills and style guides in order, like a librarian who has been
given permission to throw things away.

## How it works, without the jargon

- **The agents bring judgement. The ensign brings the clipboard.** The process lives in
  configuration and is enforced by hooks, not in a prompt that says "please" and hopes.
- **Every run starts from a plan.** Claude follows a session manual, which says,
  among other things: understand first, plan second, and never edit the code yourself.
  Code changes happen inside a run or not at all.
- **Only `go` starts a run.** Not "yes". Not "sounds good". Not "I suppose so". A hook
  waits for the exact word, and no amount of enthusiasm will substitute for it.
- **A run is a small state machine,** written down in a YAML file. Code goes to review.
  Findings go back to code. A failed test goes back to code. Budgets stop the whole
  thing going round forever, which it otherwise would, quite happily. Success means it
  compiled and the tests actually ran and actually passed.
- **Agents cannot cut corners.** Try to skip a stage and the dispatch is refused. File
  a report without an outcome and it is sent back to be done properly. An orchestrator
  that tries to slip off early is reminded, firmly, that the run is not over.
- **Memory is kept in files,** which have the great advantage of still existing
  tomorrow. Task journals are committed and pushed with the bridge, so any session on
  any machine can pick up where the last one left off.
- **Some words cost nothing.** `help`, `status` and `go` are answered by a hook before
  the model is consulted at all. `help` describes what you can ask for on the bridge
  you're standing on, in plain words. It never mentions the ensign, which prefers to go
  unnoticed.

## Make it yours

Starship is built to be extended from below, by the people who know what they're
talking about. Anything specific to you goes in your bridge, project or task, never in
the starship. There is one configuration file at each level, and each level can add to,
or quietly overrule, the one above.

| You want to | Use |
|---|---|
| say what "build" or "test" means for a project | **bindings** |
| add a review concern, insert a stage, remove one, adjust budgets | **pipeline ops** |
| make something happen before code, after test, on success | **events** |
| teach the crew your project's habits | **skills** |
| invent an entirely new way of doing things | **pipelines** |
| bundle all of the above so you only explain it once | **bundles** |

```yaml
# projects/supernova/project.yaml
use: [git-catalog]                       # commit every coder pass

bindings:
  build: { command: "./build.sh --fast" }
  test:  { command: "./test.sh" }

pipelines:
  coding:
    ops:
      - append: { stage: review, dimensions: [security] }   # one more pair of eyes
      - set:    { budgets.test: 1 }                         # patience is finite

events:
  stage.after:test: [{ observe: scripts/post-summary.sh }]
```

Two bundles come in the box:

- **[`git-catalog`](extensions/git-catalog)** commits each of the coder's attempts as
  it goes, so the orchestrator can see how the work got where it is. When you say so,
  it squashes them into a single commit and pretends it was always like that.
- **[`style-learning`](extensions/style-learning)** keeps a style guide for each
  project, written from what the style reviewer keeps having to say. The coder reads it
  next time, so with any luck the same thing need not be said twice. You can seed it
  with your existing style documents, or with code you'd like more code to look like.

The crew also notes down useful facts as it works ("this build needs a clean after
switching branches"), and the curator files them into skills. Over time the crew stops
rediscovering the same things, which is more than can be said for most of us.

## Things it flatly refuses to do

- **Guess.** If a pipeline needs `@test` and nobody has said what that means, the run
  does not start. If there truly is no test suite, you may say so, but you must give a
  reason, and the reason is written down.
- **Start without your `go`.**
- **Let your work wander into public.** Improvements discovered on a bridge are written
  up as proposals, in general terms, and carried to the starship separately. On the way
  in, commits pass the leak gates: the right author, every bridge's list of forbidden
  words, and a review by a model that has never met your projects and can therefore
  notice when something sounds like it came from one.
- **Sign anything as an AI.** No co-author lines, no "generated with". The work is
  yours. So, admittedly, is the blame.

## Getting started

You will need Claude Code and git, on Linux or Windows. Each release carries `ensign`
already built, so there is nothing to compile.

```
git clone --branch <release tag> https://github.com/joeriedel/starship.git
starship/bin/<linux-amd64|windows-amd64>/ensign setup        # puts ensign on PATH
ensign init ~/bridges/home --remote https://github.com/joeriedel/starship.git --tag <release tag>
cd ~/bridges/home && claude                                  # then type: help
```

On another machine, clone your bridge and run `claude` inside it, and Claude sets up the
rest. After upgrading Claude Code, run `ensign selftest`. It sends a few cents' worth of
probe agents out to check that Claude Code still behaves the way starship depends on.
Software changes. It is wise to ask.

If you would rather build `ensign` yourself, you will need Go: `go build ./cmd/ensign`.

## Further reading

[`docs/design.md`](docs/design.md) has everything: layers, pipelines, the hook driver,
the extension points, what has been checked against the real Claude Code, and what is
still an open question.

## License

[MIT](LICENSE)

[^1]: This is less surprising than it sounds. Very few people, and no language models,
    have ever reviewed their own work and found it wanting.

[^2]: There are such people. Some of them are quite nice.
