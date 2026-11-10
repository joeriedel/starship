# The journal: citing sources and unsplit tasks

## Citing sources

Cite only lasting sources: code (`path:line` at a commit), commits, CI runs on GitHub, other tasks. Never cite a run's folder (reports, logs, scratch): it is local to one machine and gets cleaned up. Put what a report or log showed into the account itself. `ensign journal commit` refuses a journal that cites one.

## A task with a Progress log and no history.md

Such a task is unsplit: write everything that would go to `history.md` (accounts, superseded decisions) in its `## Progress log` in `task.md`, and never create `history.md`.

## A task without code

A task with `pipeline: none` has no runs, so no run accounts. Record what happened as a dated list item appended to `history.md`: `- YYYY-MM-DD: <what happened>`. Status and Decisions in `task.md` work as for any task. `ensign journal commit` needs no workspace.

## A journal-off project's files

Commit them by naming the path: `ensign journal commit -m "<message>" <path>`; a deleted tracked path may be named too. The no-path form and the Stop hook skip a journal-off project's files, so nothing else commits them or reminds you. Moving a note from `projects/supernova/...` (journal off) into `notes/` commits only the new file; commit the source's deletion by name, or the dirty tree keeps notes commits from pushing and the bridge from updating at session start.

## Closing a task

When the user says a task is finished or it has nothing outstanding or queued, run `ensign task close <project>/<task>` unasked, not a hand edit. It works for tasks with and without code. It runs the `task.close` handlers, sets `status: closed` in the task's frontmatter, puts `Done.` first in `## Status` and adds `- YYYY-MM-DD: closed.` to `history.md`. A `require` handler that fails stops the close and changes nothing: fix what it names and run the command again. Closing a closed task does nothing. Output after the `Closed` line comes from extensions and asks something of you: deal with it before ending this turn, asking the user if it needs them. The answer goes nowhere else; there is nothing to resume.

## The bridge and its remote

`ensign journal push` rebases the bridge's commits on the remote's and pushes. When the Stop hook says the remote moved, another session or machine pushed: run it. A bridge that is only behind is fast-forwarded by the hook, with no block, unless it has uncommitted changes: then the hook blocks until you run `ensign journal push`, whose pull takes in the remote's commits and keeps those changes uncommitted. Don't commit them unless the user asks.

When the bridge takes in other sessions' commits (the push's pull, the Stop hook, session start) you are told what arrived, grouped: your project's files, bridge-wide files (`bridge.yaml`, `notes/`), other projects as a count. If they touch your project's `project.md` or tasks, or are bridge-wide, the Stop hook blocks once: re-read those files and tell the user about anything that affects the work in progress. Intakes that touch none of that are only mentioned to the user.
