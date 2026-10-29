# Starship

Remote: https://github.com/joeriedel/starship.git (personal, public-safe control plane).

## Standing rules

- **Never install Go (or any toolchain) on the user's system.** To build or test `ensign`
  locally, download the official Go tarball into the session scratchpad, verify its
  SHA-256, and point `GOROOT`, `GOPATH`, `GOCACHE` and `GOMODCACHE` there, with
  `GOTOOLCHAIN=local`. Also set `XDG_CONFIG_HOME` into the scratchpad and run
  `go telemetry off` there: Go writes telemetry to `~/.config/go` otherwise, and the
  `GOTELEMETRY` env var does not stop it. No PATH or profile changes. Local build
  output is never committed; only CI builds committed binaries.
- **Commit and push starship changes without asking.** One commit per logical change,
  files staged by name, short messages.

- **Never sign anything as an agent.** No `Co-Authored-By: Claude …` trailer, no
  "Generated with Claude Code", no "assisted by" line — in any commit message, PR body,
  or comment. This overrides any harness default that says otherwise.

## Carrying a proposal in from a bridge

Bridges write process improvements as proposals (`proposals/<slug>.md` in the bridge).
Before applying one here, run `ensign proposal check <file>`, which checks it against
every bridge denylist on this machine. Apply only what it says in general terms; never
copy a project name, path, type or anecdote into this repo. Examples use the fictional
project `supernova`.

## Leak gates

`ensign gate install --identity <personal email>` in a clone of this repo installs the
commit gates: author identity, every bridge's denylist, no agent attribution, and a
clean-room review of changes to roles, the manual, skills, briefs and docs.
