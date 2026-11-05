# Notes

The bridge keeps general context above any project in plain Markdown under `notes/`. Session start lists the files; read one when it bears on the request. Nothing here exists until used, and nothing reads external sources except a catch-up pass the user asks for (`ensign help catch-up`). Mail cleanup is a separate thing the user must ask for (`ensign help mail-cleanup`).

## Where things go

- The user's roles, goals, project involvement: `notes/profile.md`.
- A person: `notes/people.md`.
- A project: `projects/<project>/project.md`.
- A task: the `## Notes` section of that task's `task.md`, never Goal or Scope, which reach run agents.
- Anything dated or otherwise general: `notes/log/YYYY-MM.md`, as a dated entry.

Create a file when first needed. Keep notes short and factual. Long material stays at its source with a link; never link a run's folder.

## Adding notes

The user adds notes by telling you. File each in the place above, commit with `ensign journal commit -m "<message>"`, and ask one short question if the place is unclear.

When you move a note out of a journal-off project (`projects/supernova/...` into `notes/`), the no-path commit leaves the source's deletion uncommitted; follow the section "A journal-off project's files" of `ensign help journal`.

## Inbox

`notes/inbox.md` holds unsorted notes the user edits from anywhere, such as a phone. When session start says it holds something, sort each item into its place by the rules above first, leave the file blank (keep it), and commit. Ask one short question about an unclear item and leave that item in the inbox until answered.

## Rules

- Markdown files edited by hand in `notes/` are committed at the next session start, except after a compaction; other files and dotfiles are never committed.
- Run agents never see notes: never point a stage at them or paste them in wholesale. Copy a fact a run needs into that run's Scope, as with project notes.
