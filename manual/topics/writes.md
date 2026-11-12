# Connector writes

Only when `bridge.yaml` has `connector_writes:` (exact `mcp__<server>__<tool>` names; the key alone, even empty, turns it on). Nothing reaches an external system through a connector tool until the user has approved that exact call with the word `go`. Only the session makes these calls; an agent never does.

## What counts as a write

Any connector tool not listed under `connectors:`. That includes:
- Posting to tickets or the wiki, sending email, Slack posts: they go out as the user, at once.
- Moving or deleting, for example moving email to an archive folder (`ensign help mail-cleanup`).
- Creating a mail draft: the user then sends it, since the connector can't.
- Creating or changing a meeting invite: it reaches others as soon as it is written.

A tool in neither list is refused as a write. When the user asks, you add it to `connector_writes:` in `bridge.yaml`; the tool's allow entry arrives at the next session start (`/compact`, `/clear` or restarting claude).

## Flow

1. Register each call: `ensign writes add --tool <name>`, with the complete tool input as one JSON object on stdin. Each add prints the listing of everything pending; several adds build one listing. When the shell is PowerShell, send non-ASCII characters in the input as `\uXXXX` JSON escapes: piped text is ASCII there and would turn into `?`. They decode to the same value, so the listing and the comparison stay right. Unverified: whether PowerShell 5.1 garbles non-ASCII in the output of `ensign writes` (Linux can't check it; the input rule above covers input only). If it does, the listing you reproduce won't match, so such a write can't be approved in that session: safe, but a dead end whose only sign is the "not shown" reply. If that reply persists for such a write, tell the user instead of retrying.
2. Reproduce the output of `ensign writes` verbatim in your reply. The turn can't end until the listing is in your last message.
3. The user types `go`. It approves exactly the listing they were shown; a change since (an add or drop) approves nothing, so show it again.
4. Make each call now, with exactly the registered input. One approval covers one call. A call that differs in any field, or whose saved input file was edited or removed, is refused and voids the approvals not yet used.

`ensign writes` shows the listing. `ensign writes drop <n>...` or `drop --all` removes pending writes.

## The listing and the saved files

Every write is saved whole as `<session>.writes.<token>.json` under the bridge's `.starship/sessions/`; the listing names that file and its sha256, and the approval is tied to that hash. Do not edit or move it. A text longer than about 500 characters is also saved as plain text (`.txt`) and named in the listing. The files go when the write is dropped, used or voided.

- Default: texts are shown in full. `--short` shows only the start of each long text, with its length, sha256 and file; the user can open the file. Pick `--short` when the text is long and the user does not need all of it to decide. Short texts are always shown in full.
- Never shortened, whatever you choose: destinations, recipients, IDs, and every URL or email address (listed in full under a shortened text).
- Input with a string `documentId` and a `requests` list (a Google Docs batch update, e.g. `mcp__<server>__update_doc`) gets a summary: the document, the inserted text as plain text, one line per kind of formatting (paragraph styles, links with each URL, bullets), the revision guard. Add `--title <text>` to name the document (shown as given by you, unchecked).
- Every deletion and replacement is stated. For the text that goes, read the document first (a reader), save its body as plain text and add `--read <file>`; add `--read-revision <id>` with the revision you read. Ensign applies the requests in order to that text (positions are UTF-16 units; the body starts at index 1) and shows what each delete removes and each replace matches. It is labelled exact only when the batch requires that same revision (`writeControl.requiredRevisionId`); otherwise it is your read, unverified. Without `--read`, each deletion shows its range and length only.
- A request kind or field the summary does not know is shown in full as JSON, never dropped. Other tools list their fields as is.

## `go` scope

An approval lasts only for the turn it was given in; the next prompt voids what is unused. A write added later needs a new `go`. Identical writes are approved once each.

## Runs and sequences

Writes, a direct edit request and a run or sequence step waiting for go never wait together. While writes are pending, `ensign run`, `ensign sequence` steps and `ensign direct ask` are refused: approve or drop the writes first. While a direct request waits, `ensign writes add` is refused. A `go` with both present approves neither. Approved writes, and an approved run in progress, don't block.

## After a denial

The reason says what is missing. Register the call, show the listing, wait for `go`, repeat the call unchanged. Never retry with other input to get past a refusal.
