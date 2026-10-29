# Starship

An agent-orchestration control plane for [Claude Code](https://claude.com/claude-code).

Starship holds no application. It holds the configuration used to work on *other*
repositories — roles, pipelines, and the rules that govern them. You open a session in an
overlay, name a project and a task, and say what you want done; which agents run, in what
order, on which model, under which project's rules, is resolved from configuration.

> **Status: design.** The design is in [`docs/design.md`](docs/design.md). Nothing is
> implemented yet.

## The idea in one picture

```
control plane   this repo: roles, pipelines, extension bundles, the hook driver
   ▼
overlay         one per context (home, work, ...), kept wherever that context requires:
   ▼            projects, tasks, project skills, integrations
project         bindings (@build, @test, ...), pipeline extensions, rules
   ▼
task            workspace paths, VCS state, per-task tweaks, the task journal
```

Each layer can extend or override the one above it. The control plane knows nothing
about any project, so it can be shared across contexts that must stay separate, and
improvements to the process can flow back from any of them.

## What makes it different

- **The process is enforced, not remembered.** Pipelines are small state machines.
  Claude Code hooks enforce stage order, budgets, gates and the approval word `go`;
  agents supply judgement, not bookkeeping.
- **Everything is extensible below the control plane.** Bindings say what `@build`
  means in a project; pipeline operations insert, remove or reshape stages; events attach
  behaviour to points in a run; bundles package all three.
- **Nothing is guessed.** A missing binding stops the run. "Not applicable" has to be
  declared.
- **The coding loop:** coder ⇄ reviewer (fanned out across review dimensions) → style →
  tester, with a pass budget per cycle and a cycle budget per run. Success means it
  compiles and the tests ran and passed.
- **Style that learns.** The style pass feeds a per-project guide, curated for
  consistency, that the coder reads on later runs. Style documents and exemplar code can
  be ingested to seed it.
- **Task journals are the memory.** Verbose, committed, and good enough for a fresh
  session to pick the work up.
- **Type `help`** in a session for a guide generated from the configuration you're in.

## License

[MIT](LICENSE)
