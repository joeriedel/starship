# Where edits are allowed

A hook refuses Edit, Write and NotebookEdit outside the caller's folders; keep shell writes inside them.

- Session: the bridge, temp, `~/.claude`, and the `session-edit` folders in `bridge.yaml` (user-added; never code).
- Run coder, style reviewer: code folders, scratch, temp.
- Run documentation stage: the `@docs` paths, scratch, temp.
- Others: temp, scratch.
