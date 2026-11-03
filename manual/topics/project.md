# Creating a project

Then fill its bindings (`@build`, `@test`, …): read the code, propose commands for the user to confirm. If the code ships `.starship/project.yaml`, `project new` copies it in; have the user confirm those, not derive new ones. If tests are one command and any services they need can be started and stopped by commands (`@services-up`, `@services-down`, below), propose `plain: true` on `@test` (ensign runs them, no tester agent).

If the test stage needs services, add `@services-up` (start them) and `@services-down` (stop them), per location when the project has locations. `@services-up` returns only once the services are ready: when the start returns early, write the readiness wait into the command; anything that must happen before the start, such as resetting data, goes into the same command in the order the project needs, e.g. `./services.sh reset && ./services.sh up && ./services.sh wait`. There are no separate readiness or reset bindings. Both run in the location's workspace, in the test stage only, and have no cross-location form; each may set `timeout:`. In a plain stage ensign runs every side's up before `@build`, then `@test`, then every side's down in reverse, whatever the outcome; a failing up or down makes the stage `error` (not `fail`), and a signal received meanwhile waits for the downs. If a `run-stage` dies, recording the stage as `error` also runs the downs. A services binding with guidance but no command makes the stage need a tester, who runs them by hand. A project with neither binding is unchanged.

`platforms:` is required in `project.yaml`: the platforms the project supports, which reviewers treat as real failure targets. `project new` takes `--platforms linux,macos,windows` unless the code ships them. Example: `platforms: [linux, macos, windows]`.

`rules:` lists standing run rules, one string per rule, under `bridge.yaml`, an extension, `project.yaml` or a location (`rules: ["Never touch the vault."]` for project `supernova`). They merge in layer order with duplicates dropped, skip the task's frontmatter, and reach every run's work item under `## Project rules` (a retry keeps the original run's) and the run summary with their source. Task-specific rules go in the task's Scope.

`task open` reports when the code's shipped `.starship/project.yaml` has bindings, pipelines, extensions (`use`) or rules that the project's `project.yaml` lacks or has with a different value; it only reports, and never changes settings. Review the differences with the user, change `project.yaml` if they want it, then run `ensign project reviewed <project>` to record the shipped values in `projects/<project>/shipped-reviewed.yaml` (commit it) so deliberate differences stay quiet until the shipped value changes again.

For a project with locations, `task open` makes the same comparison for each of the task's locations whose folder is set on this machine, between that folder's `.starship/project.yaml` and the bridge's settings for the location (the project's bindings with the location's own bindings laid over them, as a run resolves them, and the project's rules plus the location's), and names the location in its report. `ensign project reviewed <project>[/<task>]` then records every such location (the task's, if a task is given) that ships a file, and `--location <name>` records just that one; each location has its own entry under `locations:` in `shipped-reviewed.yaml`, and recording one keeps the others.

# Code locations

A project may keep its code in several repos (e.g. `client`, `server`, `models`). Declare them in `project.yaml`:

```yaml
locations:
  models:
    bindings: { build: { command: "make" }, test: { command: "make test" } }
    rules: ["Keep the schema backwards compatible."]
  client:
    depends-on: [models]
    bindings: { test: { command: "npm test" } }
cross-location:
  bindings: { test: { command: "./e2e.sh" } }
```

- Names: lower-case letters, digits, dashes. `depends-on` names declared locations only: no self-dependency, no cycle.
- Location bindings override the project's; the task's override both. `cross-location` `test` runs once after the sides' own tests when a run has several sides.
- A task names the locations it uses: `locations: [client, models]` in its frontmatter, or `ensign task new <p>/<t> --locations client,models`. Required once the project declares any.
- Each run works in one location: `ensign run <p>/<t> --location <name>`. Optional when the task names one; required when it names several. A sequence run step takes the same flags: `ensign sequence run <p>/<t> --location <name> [--sides <a,b>]`; see `ensign help sequence`.
- Landing commands (`ensign github-pr pull-request|merge|update`, `ensign git-catalog squash`) take `--location <name>`; without it they use the location of the task's most recently finished successful run, else its sole location. Base and tested-commit records are kept per location.
- A run tests its sides: the edited location plus every location that depends on it, transitively (`models` edited: `models`, `client`, `server`). Each side builds and tests with its own bindings in its own folder; other sides build from their checkout as it stands. A task's own binding overrides apply to the edited location only; the other sides use the bridge's, the project's and their own location's.
- Narrow only when you know the change affects fewer: `ensign run <p>/<t> --location models --sides models,server`. The set must include the edited location and only affected ones. The run summary shows the sides.
- A change that needs another location's unlanded branch (e.g. `server` against a `models` branch not merged yet): name it at run setup with `ensign run <p>/<t> --location server --dependency models=<branch>` (repeatable). The location must be one the edited location depends on, and the branch must exist in its local checkout and be checked out there. The branch applies on every side that depends on it, the edited location included. A checkout on another branch (or a detached HEAD) is refused, naming the folder and the branch it is on, at run setup and again before each apply; check the branch out first.
- A run for a subtask adds a dependency by itself, as if `--dependency <location>=task/<task>` were given, for each location its own location depends on where an earlier subtask of the task is done, when that location's `task/<task>` branch exists and has commits its base lacks. A published (missing) or not-ahead branch is not applied. A `--dependency` for the location wins. The run summary marks it automatic; the checkout rule above applies to it.
- `@apply-dependency` (per location binding) wires that branch into a side: ensign runs its command in the side's folder before `@build` and `@test`, then restores the tree. Env: `STARSHIP_DEPENDENCY` (location), `STARSHIP_DEPENDENCY_BRANCH`, `STARSHIP_DEPENDENCY_PATH` (its local checkout), `STARSHIP_APPLIED_FILES` (a file the binding writes every path it touched into, one per line, relative to the side's folder; `/` or `\`; no absolute or escaping paths). Restore sets each listed file back to what it was before the apply: HEAD, the content it already had when it was modified, or deleted when neither had it. A file edited since the apply is left alone and named in an error; once its wiring is out by hand, `ensign dependency restore --side <location> --done <file>` settles it (commits refuse until then).
- Restore never deletes or rewrites a git-ignored file (or one inside an ignored folder) that existed before the apply, such as a developer's own `go.work`: it is left in place, reported as a notice (not an error), and dropped from the record; ignored files cannot reach a commit, so remove the wiring from them by hand. `ensign dependency apply` refuses to run again while a kept file is neither at its applied content nor restored, naming the `--done` command for it.
- `ensign run` refuses when a side needs a dependency applied (named with `--dependency`, or the edited location on its dependents) and that location has no `@apply-dependency` binding: add it, or `not-applicable: <why>` when it already builds from the sibling checkout (the side is then tested as it is), or narrow `--sides`.
- When a run tests dependents of the edited location, each dependent gets the edited location's branch applied (the branch its workspace is on; a detached HEAD is an error).
- Agent stages (not plain): coder and tester prompts give the exact `ensign dependency apply --side <location>` and `ensign dependency restore [--side <location>]` commands. Run apply before `@build`/`@test` in that side and restore before finishing.
- Commits refuse while the wiring is in the tree (`dependency wiring from @apply-dependency is still in the tree`); run `ensign dependency restore`.
- Ensign never pushes the dependency branch. If a project's tooling can only fetch from the remote, its `@apply-dependency` command fails naming the branch to push (say so in the binding's `guidance` or a comment); the push needs the user's approval like any push.
- Record each location's folder on this machine: `ensign workspace <project> --location <name> <path>`. Ask the user; never guess.
- A new project with locations: `ensign project new <name> --locations client,server --platforms <list>` (instead of `--workspace`; `--platforms` is required). It writes the `locations:` block and records no folder; record each with the command above.
- A project that declares no locations works as before.
