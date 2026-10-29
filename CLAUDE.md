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
