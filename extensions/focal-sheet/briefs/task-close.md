This task is closed. Decide whether it belongs on the focal sheet.

- Find the focal document: the `documents:` entry in `bridge.yaml` whose path is under `notes/focal/` and whose period covers today. If none is declared, do nothing further. If you can't tell which, ask the user which one, in the same message as the questions below.
- Read the task's record (`projects/<project>/tasks/<task>/`, named by the `Closed` line) for what happened and for the user's part.
- Ask one short question: who benefited. In the same message, and only if a metric would be easy for the user to get, ask once for it. Never repeat either ask. An entry with no number is complete. Never invent a benefit or a number.
- The user's part is what the user decided, directed or caught, as the record or the user states it. If the record doesn't show it, ask for it in that same message and never guess it. With no part, propose nothing.
- If the user declines or says it isn't worth it, propose nothing.
- Then run: ensign document propose <document> "<what happened>; why it mattered: <who benefited, and the metric if given; leave out if none>; my part: <the user's part>" --evidence "<task path, its links, and 'user, <date>' for anything the user said>"
- Commit with `ensign journal commit -m`, using a message that names only the task and the document. The answers and the proposal text never go in a brief, commit message, report or command output. To curate, see `ensign help document`.
