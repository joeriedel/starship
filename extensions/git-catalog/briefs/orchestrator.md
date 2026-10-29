## The change catalog

Every coder pass in this run is committed locally by the harness — `wip(c<cycle> p<pass>)`,
with `BROKEN` in the subject when it didn't compile and the summary in the body. The run
started at a clean tree. Between passes, read the history with `git -C <workspace> log
--oneline <first wip commit>^..HEAD` and `git show --stat <sha>`:

| Signal | Usually means |
|---|---|
| the same file rewritten every pass | the brief is ambiguous; fix the brief |
| a diff growing instead of shrinking | the item is bigger than it looked; say so |
| a pass undoing an earlier one | the coder and reviewer disagree on intent; settle it |
| a run of `BROKEN` passes | the coder is fighting the build; the errors are in the bodies |

Never commit, rewrite or push these yourself.
