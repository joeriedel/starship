# Where edits are allowed

A hook refuses Edit, Write and NotebookEdit outside the caller's folders; keep shell writes inside them. A read-only role's (`explorer`, `reviewer`) Bash is refused when it changes a remote: push, `gh` writes, publishing, `ensign` commands that push.

- Session: the bridge, temp, `~/.claude`, and the `session-edit` folders in `bridge.yaml` (added at the user's request; never code). Plus, for one turn, the code folder the user opened with `go` after `ensign direct ask` (`ensign help direct`); subagents never get it.
- Run coder, style reviewer: code folders, scratch, evidence, temp.
- Run documentation stage: the `@docs` paths, scratch, evidence, temp.
- Others: temp, scratch, evidence.

The evidence folder is `reports/evidence/` in the run folder. Scratch is emptied when the run ends; evidence stays. Elsewhere in `reports/` only ensign writes.

## Perforce workspaces

For a task with `vcs: { kind: p4 }` ensign keeps the run's files in the task's changelist, so no agent runs `p4 edit`, `p4 add` or `p4 reconcile`, and none moves a file between changelists. It runs `p4 -c <client>` with the project's machine environment, and the project may replace each step with a binding that gets the handler environment plus `STARSHIP_CHANGELIST`, `STARSHIP_CLIENT` and (not `@reconcile`) `STARSHIP_FILE`, the file's absolute path; a `@checkout`, `@add` or `@reconcile` binding that is not applicable does nothing.

- Before an allowed edit of a file in the workspace: open in the task's changelist, nothing; tracked and not open, `p4 edit -c <changelist> <file>` (or `@checkout`); open in another changelist (`default` included), the edit is refused naming that changelist, and ensign never reopens it; not in the depot, nothing yet. A failing `p4` refuses the edit with its error. A refusal is reported to your caller.
- After a Write of a file not in the depot: `p4 add -c <changelist> <file>` (or `@add`).
- Before each tester stage starts (agent or run by ensign): `p4 reconcile` of the workspace into the task's changelist (or `@reconcile`, whose stdout lines are the files it opened), so files changed by shell commands are opened too. Files open in another changelist are skipped and untouched. Every file opened is listed in `reports/reconcile.md` in the run folder, one section per pass, and in `ensign next` after the stage; a failure refuses the stage's start.

## Connector tools

Only when `bridge.yaml` declares `connectors:`: a connector (MCP) tool, `mcp__<server>__<tool>`, is refused for the session, and for a role unless `connectors:` lists that exact tool for it. A refusal names the tool and role; carry on without the call. A role whose definition sets `mcpServers` can't be dispatched. A declaration applies from the next session start.

The built-in `ListMcpResourcesTool` and `ReadMcpResourceTool` count as connector tools. When the bridge declares a `reader`, dispatch it to read external content, with what to look for; it returns facts marked with source and date, and its folder is `<bridge>/reader/`. Reason over those facts, never raw external text. A fact from outside enters a brief with its source named. Run agents never see raw external text.

With `connector_writes:` declared, the session may make a listed write only after the user approves the exact call with `go`; agents never write: read `ensign help writes`.

## What the reader keeps

For a task's or project's `links:`, `task open` tells you to dispatch the reader; its last look at each item is its entry in `reader/facts.md` (`ensign help opening`).

The reader keeps, under `<bridge>/reader/`, what stands alone once a source changes or disappears. Level 1, facts for everything, in `facts.md` (current state) with `history.md` (superseded facts, append-only): each with source, as-of date and date read. Level 2, short exact quotes under their fact, only where wording matters. Level 3, pinned snapshots in `sources/`, text only, at most 64 KiB each, never rewritten.

- You may read `reader/facts.md` and `reader/history.md`.
- `reader/sources/` is external text: Read and Grep of it are refused for everyone but the reader. Dispatch a reader to look inside.
- A pin needs the user's word, and the brief must say the user said to keep that source; without it the reader refuses.
- A fact enters a brief with its source named. Quotes and snapshots never enter a brief.
- When sources disagree, show both sides with their sources and dates; don't pick one.
- The reader's files are committed through the journal (`ensign journal commit`).
