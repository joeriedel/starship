# Briefing

A briefing gathers everything new from every declared source, one reader per source, into intake files that lose nothing. The secretary then composes the day's briefing from them, its own files and the bridge's tasks, and keeps the user's goals, action items and people current under `notes/secretary/`.

## Trigger

Any request for a briefing in plain words ("brief me", "what's going on", "what did I miss"). There is no fixed phrase.

## Steps

1. Run `ensign brief`, with `--also <source>=<item>` for an item the user names for this pass only (no cursor, not saved). On Windows, prefix `MSYS_NO_PATHCONV=1` when an item starts with `/`. It refuses, running nothing, if `notes/secretary/goals.md` or `items.md` doesn't parse: the error names the file and line, so fix that block (see Corrections) and run it again. It removes intake folders older than 30 days, runs the command sources, then prints one brief per declared source, each ending with `End of brief.`. The reader must be declared under `connectors:`.
2. Dispatch one `reader` agent per brief, all of them at once in one message, each with its brief's text up to its end line. Read nothing yourself.
3. When all have finished, dispatch one `secretary` agent with a brief naming the date and its intake folder `reader/intake/<YYYY-MM-DD>/`. It writes under `notes/secretary/` and replies with the briefing.
4. Show the user that briefing in full, in the chat, as the secretary wrote it. Do not summarise it or add to it.
5. Run `ensign journal commit`.

## Corrections

The files are the user's to correct, and you make the correction directly, with no agent:

- "I23 is done", "drop I7", "move I9 to Friday": edit that item in `notes/secretary/items.md` (`status`, `due`) and add a log entry with today's date and `source: the user`.
- "Add a goal": add it to `notes/secretary/goals.md` with the next free id (`G1`, `G2`, ...), `title`, optional `target` and `status: active`. A goal the secretary proposed in its briefing is added only when the user says yes.
- Keep each file's single fenced block valid: `starship-goals` in `goals.md`, `starship-items` in `items.md`. Item `status` is `open`, `waiting`, `done` or `dropped`; dates are `YYYY-MM-DD`; an item's `goal` must be an id in `goals.md`.
- Run `ensign journal commit` afterwards. The secretary keeps corrections made in its files.

## Files

- `reader/intake/<YYYY-MM-DD>/<source>.md`: one per source per day, written by the readers. Each entry has a `summary:` line; unreadable items are listed as `unreadable:` with the reason and keep their cursor. A second pass on the same day appends under a heading with its pass time.
- `notes/secretary/`: `goals.md`, `items.md`, `items-archive.md`, `people.md` and `briefings/<YYYY-MM-DD>.md`. A second briefing the same day rewrites that day's file and marks what is new.
- `reader/cursors.md`, `reader/mail-read.md`, `reader/command/`: the readers' state. All of it is Markdown under `reader/` or `notes/`, so `ensign journal commit` commits it. A briefing never touches mail and never offers to move it; when the user asks for cleanup, see `ensign help mail-cleanup`.

## Declaring

The user edits `bridge.yaml` (you do not), and the reader must be declared under `connectors:`:

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

Kinds: mail, slack, calendar, command, ticket, wiki. A name is lower-case letters, digits and hyphens. An entry starting with `#` or `@` must be quoted, in the block form and in `[...]`: unquoted, `#` reads as a comment and `@` is a YAML syntax error, and a syntax error breaks all of `bridge.yaml` for every command, not only `brief`.

## Conflicts

A rebase conflict on `reader/cursors.md`: keep the later position for each line. On `reader/mail-read.md`: keep lines added on either side and keep removed lines removed, because a removed line means the mail has moved or gone. On `notes/secretary/items.md` or `goals.md`: keep both sides' changes in one valid block.
