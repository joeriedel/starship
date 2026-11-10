# Briefing

A briefing gathers everything new from every declared source, one reader per source, into intake files that lose nothing. The secretary then composes the day's briefing from them, its own files and the bridge's tasks, and keeps the user's goals, action items and people current under `notes/secretary/`.

## Trigger

Any request for a briefing in plain words ("brief me", "what's going on", "what did I miss"). There is no fixed phrase.

## Steps

1. Run `ensign brief`, with `--also <source>=<item>` for an item the user names for this pass only (no cursor, not saved). On Windows, prefix `MSYS_NO_PATHCONV=1` when an item starts with `/`. It refuses, running nothing, if `notes/secretary/goals.md` or `items.md` doesn't parse: the error names the file and line, so fix that block (see Corrections) and run it again. It removes intake folders older than 30 days, runs the command sources, then prints a `Problem:` line for each item `tasks:` entry that names a task that doesn't exist (the briefing still runs: fix the entry in `items.md` or create the task), then prints one brief per declared source, each ending with `End of brief.`. The reader must be declared under `connectors:`.
2. Dispatch one `reader` agent per brief, all of them at once in one message, each with its brief's text up to its end line. Read nothing yourself.
3. When all have finished, dispatch one `secretary` agent with a brief naming the date and its intake folder `reader/intake/<YYYY-MM-DD>/`. When `ensign brief` closes with a focal (period, starts date when set, ends date, brag file path, rubric), put those facts in the secretary's brief too; a `Problem:` line for `focal:` means the pass runs without focal work (fix `focal:` in `bridge.yaml`, `ensign help impact`). It writes under `notes/secretary/` (and `notes/focal/` for a focal) and replies with the briefing.
4. Show the user that briefing in full, in the chat, as the secretary wrote it. Do not summarise it or add to it.
5. Run `ensign journal commit`.

## Corrections

The files are the user's to correct, and you make the correction directly, with no agent:

- "I23 is done", "drop I7", "move I9 to Friday": edit that item in `notes/secretary/items.md` (`status`, `due`) and add a log entry with today's date and `source: the user`.
- "Add a goal": add it to `notes/secretary/goals.md` with the next free id (`G1`, `G2`, ...), `title`, optional `target` and `status: active`. A goal the secretary proposed in its briefing is added only when the user says yes.
- "Make a task for I23": create the task with `ensign task new <project>/<task>` (`ensign help project`), then add `<project>/<task>` to the item's `tasks:` list in `items.md` and a log entry with `source: the user`. "Link I23 to project/task": add it to `tasks:`. `tasks:` is the only stored link: `ensign task open` and `ensign task close` list an item's tasks from `items.md`, and the secretary tracks the item's progress from those tasks.
- "X isn't urgent", "always flag Y": fix the item, and add a dated rule quoting the user to `notes/secretary/preferences.md` (plain language, e.g. `2026-11-09: "always flag anything from the supernova launch channel"`). A rule the secretary suggested in Suggestions is added only when the user says yes. Priority rules live in `preferences.md`, not `bridge.yaml`.
- Keep each file's single fenced block valid: `starship-goals` in `goals.md`, `starship-items` in `items.md`. Item `status` is `open`, `waiting`, `done` or `dropped`; dates are `YYYY-MM-DD`; an item's `goal` must be an id in `goals.md`.
- Run `ensign journal commit` afterwards. The secretary keeps corrections made in its files.

## Files

- `reader/intake/<YYYY-MM-DD>/<source>.md`: one per source per day, written by the readers. Each entry has a `summary:` line; unreadable items are listed as `unreadable:` with the reason and keep their cursor. A second pass on the same day appends under a heading with its pass time.
- `notes/focal/<period>.brag.md`: the living brag file, kept by the secretary when `bridge.yaml` declares `focal:` (`ensign help impact`).
- `notes/secretary/`: `goals.md`, `items.md`, `items-archive.md`, `people.md`, `preferences.md` (the user's priority rules, read-only for the secretary) and `briefings/<YYYY-MM-DD>.md`. The briefing ends with a Suggestions section: proposed goals, proposed preference rules and channels to start watching. A second briefing the same day rewrites that day's file and marks what is new.
- `reader/cursors/<source>.md`, `reader/mail-read/<source>.md`, `reader/command/`: the readers' state. All of it is Markdown under `reader/` or `notes/`, so `ensign journal commit` commits it. A briefing never touches mail and never offers to move it; when the user asks for cleanup, see `ensign help mail-cleanup`. Lines the migration of an old shared `reader/cursors.md` or `reader/mail-read.md` could not read are kept in `reader/cursors/unparsed.md`; every brief reports them as a Problem until you move each into its source's file or delete it and remove the file.

## Declaring

You edit `bridge.yaml` when the user asks, and tell them what you changed; the reader must be declared under `connectors:`:

```yaml
sources:
  supernova-slack:
    kind: slack
    watch:
      - "#supernova-launch"
      - "@nova-lead"
  work-mail:
    kind: mail
    watch: [inbox]
  work-calendar:
    kind: calendar
    watch: [Work]
    lookahead: 7
  supernova-wiki:
    kind: wiki
    watch:
      - https://wiki.supernova.example/pages/launch-plan
      - "tree: https://wiki.supernova.example/pages/launch"
      - "@mine"
      - "@mentions"
  supernova-tickets:
    kind: ticket
    watch:
      - SUPERNOVA
      - "query: project = SUPERNOVA AND status = Open"
      - "@assigned"
      - "@reported"
      - "@mentions"
  supernova-review:
    kind: command
    command: [gh, pr, list, --state, open, --json, number,title,updatedAt]
    timeout: 60
    env:
      GH_HOST: git.supernova.example
```

A slack item is `#channel` (no spaces) or `@person` (a DM; spaces allowed), or the reserved `@dms` (any case): every DM and group DM the user is in, never channels, in `watch` and in `--also`; a person whose handle is literally `dms` can't be watched by name. Never a pattern (`*`, `?`) or `@here`, `@channel`, `@everyone`. A slack source also covers the threads the user is in or mentioned in, and never the rest of the workspace. Extra slack items given with `--also` follow the same forms. A calendar item is a calendar's name; the reader reads who met and which orgs they come from. A calendar source also takes `lookahead`: whole days of upcoming meetings to read, 0 to 31, default 7, 0 is off; no other kind takes it. Upcoming meetings are re-read whole each pass with no cursor.

A wiki item is a page URL (that page and its comments), `tree: <page URL>` (the page and every page under it), or the reserved `@mine` (pages the user created or owns, and new comments on them) or `@mentions` (pages and comments that mention the user). A ticket item is a project key, `query: <query>` (a saved query in the tracker's language, run as given), or the reserved `@assigned` (open tickets assigned to the user), `@reported` or `@mentions`. Reserved items and the `tree:`/`query:` prefix are accepted in any case and shown in lower case, in `watch` and in `--also`; any other `@` item is refused, and a `tree:` or `query:` with nothing after it too. A pass reads only what an item names, never a whole space or tracker: if the connector can't run the search an item needs, the item is unreadable. The cursor is a last-modified time, and a first look goes back 14 days.

A command source is for a system with no connector, such as a code review server read with `gh`. It has `command` instead of `watch`: a list of strings, the program first, then its arguments. `ensign brief` runs it with no shell (so no pipes, quoting or expansion), from the bridge folder, with the user's environment plus any `env:` entries, which override variables of the same name. `timeout` is whole seconds, 1 to 600, default 60. Only a command source takes `command`, `timeout` and `env`, and it takes no `watch` and no `--also` items. `bridge.yaml` is committed and pushed: `env:` holds server names and usernames, never tokens or passwords (the program finds those in the user's own environment or login). The owner is trusted to declare read-only commands; ensign runs them as given and does not check what they do.

On success the output becomes `reader/command/<name>.out`, and the one before it becomes `reader/command/<name>.prev`, so the reader can compare what changed; the output is a snapshot of current state with no cursor, and anything past 1 MB is cut with a marker line. On failure (non-zero exit, timeout, program not found) neither file changes, the brief lists the source as unreadable with the reason and the last lines of the program's stderr, and the other sources still run. These files are committed by `ensign journal commit` with the other reader files.

Kinds: mail, slack, calendar, command, ticket, wiki. A name is lower-case letters, digits and hyphens; `unparsed` is reserved. An entry starting with `#` or `@` must be quoted, in the block form and in `[...]`: unquoted, `#` reads as a comment and `@` is a YAML syntax error, and a syntax error breaks all of `bridge.yaml` for every command, not only `brief`.

## Conflicts

A rebase conflict on a `reader/cursors/<source>.md` file: keep the later position for each line. On a `reader/mail-read/<source>.md` file: keep lines added on either side and keep removed lines removed, because a removed line means the mail has moved or gone. On `notes/secretary/items.md` or `goals.md`: keep both sides' changes in one valid block.
