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
- Location bindings override the project's; the task's override both. `cross-location` bindings are kept; no run uses them yet.
- A task names the locations it uses: `locations: [client, models]` in its frontmatter, or `ensign task new <p>/<t> --locations client,models`. Required once the project declares any.
- Each run works in one location: `ensign run <p>/<t> --location <name>`. Optional when the task names one; required when it names several. Sequence runs need a task with one location.
- Landing commands (`ensign github-pr pull-request|merge|update`, `ensign git-catalog squash`) take `--location <name>`; without it they use the location of the task's most recently finished successful run, else its sole location. Base and tested-commit records are kept per location.
- Record each location's folder on this machine: `ensign workspace <project> --location <name> <path>`. Ask the user; never guess.
- A project that declares no locations works as before.
