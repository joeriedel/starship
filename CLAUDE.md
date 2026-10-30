# Starship

Remote: https://github.com/joeriedel/starship.git (personal, public-safe control plane).

## Standing rules

- **Never install Go (or any toolchain) on the user's system.** To build or test `ensign`
  locally, download the official Go tarball into the session scratchpad, verify its
  SHA-256, and point `GOROOT`, `GOPATH`, `GOCACHE` and `GOMODCACHE` there, with
  `GOTOOLCHAIN=local`. Also set `XDG_CONFIG_HOME` into the scratchpad and run
  `go telemetry off` there: Go writes telemetry to `~/.config/go` otherwise, and the
  `GOTELEMETRY` env var does not stop it. No PATH or profile changes. Local build
  output is never committed. Binaries are never committed at all: CI builds them and
  attaches them to each release.
- **Commit and push starship changes without asking.** One commit per logical change,
  files staged by name, short messages.

- **Never sign anything as an agent.** No `Co-Authored-By: Claude …` trailer, no
  "Generated with Claude Code", no "assisted by" line — in any commit message, PR body,
  or comment. This overrides any harness default that says otherwise.

## Changes from a bridge

A bridge works on starship as a project (its settings are in `.starship/project.yaml`):
each task is a branch, `task/<name>`, that ends as a pull request with CI as its tests.
Nothing project-specific belongs in this repo, whoever writes the change: no project
names, paths, types or anecdotes from any bridge. Examples use the fictional project
`supernova`. The pull request is where that gets checked.
