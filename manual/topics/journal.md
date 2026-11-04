# The journal: citing sources and unsplit tasks

## Citing sources

Cite only lasting sources: code (`path:line` at a commit), commits, CI runs on GitHub, other tasks. Never cite a run's folder (reports, logs, scratch): it is local to one machine and gets cleaned up. Put what a report or log showed into the account itself. `ensign journal commit` refuses a journal that cites one.

## A task with a Progress log and no history.md

Such a task is unsplit: write everything that would go to `history.md` (accounts, superseded decisions) in its `## Progress log` in `task.md`, and never create `history.md`.

## A task without code

A task with `pipeline: none` has no runs, so no run accounts. Record what happened as a dated list item appended to `history.md`: `- YYYY-MM-DD: <what happened>`. Status and Decisions in `task.md` work as for any task. `ensign journal commit` needs no workspace.
