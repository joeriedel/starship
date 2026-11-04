# Connector writes

Only when `bridge.yaml` has `connector_writes:` (exact `mcp__<server>__<tool>` names; the key alone, even empty, turns it on). Nothing reaches an external system through a connector tool until the user has approved that exact call with the word `go`. Only the session makes these calls; an agent never does.

## What counts as a write

Any connector tool not listed under `connectors:`. That includes:
- Posting to tickets or the wiki, sending email, Slack posts: they go out as the user, at once.
- Moving or deleting, for example moving email to an archive folder.
- Creating a mail draft: the user then sends it, since the connector can't.
- Creating or changing a meeting invite: it reaches others as soon as it is written.

A tool in neither list is refused as a write. The user adds it to `connector_writes:` and restarts claude.

## Flow

1. Register each call: `ensign writes add --tool <name>`, with the complete tool input as one JSON object on stdin. Each add prints the listing of everything pending; several adds build one listing. When the shell is PowerShell, send non-ASCII characters in the input as `\uXXXX` JSON escapes: piped text is ASCII there and would turn into `?`. They decode to the same value, so the listing and the comparison stay right.
2. Reproduce the output of `ensign writes` verbatim in your reply. Every field is listed in full, so the user sees the destination and the text. The turn can't end until the listing is in your last message.
3. The user types `go`. It approves exactly the listing they were shown; a change since (an add or drop) approves nothing, so show it again.
4. Make each call now, with exactly the registered input. One approval covers one call. A call that differs in any field is refused and voids the approvals not yet used.

`ensign writes` shows the listing. `ensign writes drop <n>...` or `drop --all` removes pending writes.

## `go` scope

An approval lasts only for the turn it was given in; the next prompt voids what is unused. A write added later needs a new `go`. Identical writes are approved once each.

## Runs and sequences

Writes and a run or sequence step waiting for go never wait together. While writes are pending, `ensign run` and `ensign sequence` steps are refused: approve or drop the writes first. A `go` with both present approves neither. Approved writes, and an approved run in progress, don't block.

## After a denial

The reason says what is missing. Register the call, show the listing, wait for `go`, repeat the call unchanged. Never retry with other input to get past a refusal.
