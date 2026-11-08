# Direct edits

A small edit made by you in a code folder, without a pipeline run, when the user chose that over a task. You ask whether to make a task or just do the edits before doing any work, and never pick on your own.

## Flow

1. `ensign direct ask <folder> "<what the user asked for>"`: folder is an existing directory; the work is one line. Nothing in ensign approves it.
2. Tell the user the folder and the work, ask them to type `go`, and end the turn. The request counts as shown once that turn has ended.
3. The next prompt must be exactly `go`. Any other reply drops the request, except `status` and `help`, which leave it waiting. A `go` before the turn ended, or with a run or sequence also waiting for go, opens nothing and drops it.
4. The guard is now open for that folder (Edit, Write, NotebookEdit) for that turn only. Do only what the user asked; run no builds or tests unless they asked; commits and pushes still need their explicit approval every time. Shell writes are not covered: they stay refused as ever. Subagents never get the folder.
5. When done run `ensign direct close`. The turn ending, or any next prompt, closes it anyway; the next piece of work needs its own `ask` and `go`.

An `ask` while one is pending or open replaces it. `ensign direct close` also drops a pending request.

## Refusals

- A folder a live run holds, whether it works there or only reads it, or a landing or an update is under way in it or in a folder around it: refused at `ask`, at `go`, and on every edit (a run in another session can start while the guard is open). A folder around a held one stays open for the rest of it.
- A folder a queued run of any session will start in, whether it works there or only reads it, approved or not yet: refused at `ask` and `go`.
- A run or sequence waiting for go in this session, or pending connector writes: finish or discard them first.
- The state file is not editable by file tools.

Direct edits leave the workspace dirty. A later run is refused while they stay uncommitted: tell the user to commit, stash or discard them first (`ensign help runs`).
