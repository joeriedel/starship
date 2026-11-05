# Code locations and machines

## Several code locations

Several code locations (`ensign help project`): `task new --locations <a,b>`; each run edits one, `--location <name>`, and tests it plus its dependents; `--sides <a,b>` narrows only when you know fewer are affected (include the edited one).

## Tasks without code

A task with `pipeline: none` needs no code folder: `task open` prints no workspace or VCS lines for it. Read `ensign help project`.

## On this machine

**Code location differs per machine**: it lives in this machine's `machine.yaml`, never the bridge's files. If a task stops for lack of a folder here, ask the user where the code is and run `ensign workspace <project> <path>` (or `<project>/<task>` for a task's own checkout). Toolchain (PATH folders, variables): `ensign env <project>`. Binding fails for a missing tool: read `ensign help machine`.

## Linked items

`task open` lists the task's `links:`, also for a task without code (`ensign help project`). When the bridge declares the `reader`, dispatch it with those links before you start work: it compares each item with what it kept, and you tell the user what it says changed.
