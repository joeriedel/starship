# Briefing

A briefing gathers everything new from every declared source, one reader per source, into intake files that lose nothing. Composing the briefing from them is a separate step that is not available yet.

## Trigger

Any request for a briefing in plain words ("brief me", "what's going on", "what did I miss"). There is no fixed phrase. Catch-up (`ensign help catch-up`) still exists separately and proposes notes; a briefing writes no proposals.

## Steps

1. Run `ensign brief`, with `--also <source>=<item>` for an item the user names, as in catch-up. On Windows, prefix `MSYS_NO_PATHCONV=1` when an item starts with `/`. It runs the command sources first, then prints one brief per declared source, each ending with `End of brief.`. Sources are declared in `bridge.yaml` as in catch-up, and the reader must be declared under `connectors:`.
2. Dispatch one `reader` agent per brief, all of them at once in one message, each with its brief's text up to its end line. Read nothing yourself.
3. When all have finished, follow the closing instruction. The secretary is not available yet, so show the user the summary lines from this pass's intake files in `reader/intake/<YYYY-MM-DD>/`, then run `ensign journal commit`.

## Intake files

`reader/intake/<YYYY-MM-DD>/<source>.md`, one per source per day. They are Markdown under `reader/`, so the journal commits them. A second pass on the same day appends under a heading with its pass time. Each entry has a `summary:` line; unreadable items are listed as `unreadable:` with the reason and keep their cursor.
