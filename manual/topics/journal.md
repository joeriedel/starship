# The journal: citing sources and unsplit tasks

## Citing sources

Cite only lasting sources: code (`path:line` at a commit), commits, CI runs on GitHub, other tasks. Never cite a run's folder (reports, logs, scratch): it is local to one machine and gets cleaned up. Put what a report or log showed into the account itself. `ensign journal commit` refuses a journal that cites one.

## A task with a Progress log and no history.md

Such a task is unsplit: write everything that would go to `history.md` (accounts, superseded decisions) in its `## Progress log` in `task.md`, and never create `history.md`.

## A task without code

A task with `pipeline: none` has no runs, so no run accounts. Record what happened as a dated list item appended to `history.md`: `- YYYY-MM-DD: <what happened>`. Status and Decisions in `task.md` work as for any task. `ensign journal commit` needs no workspace.

## Closing a task

When the user says a task is finished, or you would record it as Done, run `ensign task close <project>/<task>` instead of editing Status by hand. It works for tasks with and without code. It runs the `task.close` handlers, sets `status: closed` in the task's frontmatter, puts `Done.` first in `## Status` and adds `- YYYY-MM-DD: closed.` to `history.md`. A `require` handler that fails stops the close and changes nothing: fix what it names and run the command again. Closing a closed task does nothing. Output after the `Closed` line comes from extensions and asks something of you: deal with it before ending this turn, asking the user if it needs them. The answer goes nowhere else; there is nothing to resume.
