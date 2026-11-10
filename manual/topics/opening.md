# Code locations and machines

## Several code locations

Several code locations (`ensign help project`): `task new --locations <a,b>`; each run edits one, `--location <name>`, and tests it plus its dependents; `--sides <a,b>` narrows only when you know fewer are affected (include the edited one).

## Tasks without code

A task with `pipeline: none` needs no code folder: `task open` prints no workspace or VCS lines for it. Read `ensign help project`.

## On this machine

**Code location differs per machine**: it lives in this machine's `machine.yaml`, never the bridge's files. If a task stops for lack of a folder here, ask the user where the code is and run `ensign workspace <project> <path>` (or `<project>/<task>` for a task's own checkout). Toolchain (PATH folders, variables): `ensign env <project>`. Binding fails for a missing tool: read `ensign help machine`.

## Linked items

`task open` lists the task's `links:`, also for a task without code (`ensign help project`). When the bridge declares the `reader`, dispatch it with those links before you start work: it compares each item with what it kept, and you tell the user what it says changed.

## Linked secretary items

`task open` also prints an `item` line for each secretary item in `notes/secretary/items.md` whose `tasks:` names the task (id, title, status, due date and goal). `task close` lists them again and you ask the user whether each is done too. If the secretary files don't parse, one line says so and the command still succeeds (`ensign help briefing`).

## Perforce workspaces

A task with `vcs: { kind: p4, client: <client> }` (optional `changelist: <pending changelist number>`, read only) gets a real `vcs` line from `task open`: the client, the task's changelist with its opened-file count (or that it is not among the client's pending changelists), and the other pending changelists with their opened-file counts (`default` when it has files), or "no other pending changelists". Files in other changelists are normal and never refuse a run (the git dirty-tree refusal does not apply to p4). It runs `p4 -c <client>` with `login -s`, `clients`, `changes -s pending` and `opened -C` in the workspace, with the project's machine environment, so `p4` must be on its PATH (`ensign help machine`). A project `@state` binding replaces this and its output is shown as is. A problem is one `vcs` line and `task open` still succeeds: no client in `vcs:`, `p4` not on PATH, not logged in (run `p4 login`), the client unknown to the server, a failing `@state`, or another failed `p4` command. The same check runs when a run starts, after `@sync` (any `--sync`), and its result is recorded with the run: a problem fails the run with "the Perforce workspace is not ready: ..." and you fix it first.
