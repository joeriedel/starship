# Creating a project

Then fill its bindings (`@build`, `@test`, …): read the code, propose commands for the user to confirm. If the code ships `.starship/project.yaml`, `project new` copies it in; have the user confirm those, not derive new ones. If tests are one command needing no services or setup, propose `plain: true` on `@test` (ensign runs them, no tester agent).

`platforms:` is required in `project.yaml`: the platforms the project supports, which reviewers treat as real failure targets. `project new` takes `--platforms linux,macos,windows` unless the code ships them. Example: `platforms: [linux, macos, windows]`.

# Code locations

A project may keep its code in several repos (e.g. `client`, `server`, `models`). Declare them in `project.yaml`:

```yaml
locations:
  models:
    bindings: { build: { command: "make" }, test: { command: "make test" } }
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
