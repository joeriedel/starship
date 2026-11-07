# Runs

`bridge.yaml` `keep-runs` (default 500) is how many finished runs the bridge keeps in all. SessionStart deletes older finished runs beyond it, 50 delete attempts at most per start, shared with folders that have `pipeline.json` and no `state.json`, which are deleted first once 5 minutes old and don't count toward `keep-runs`; `ensign gc [<keep>]` does it now. Finished runs lose their `scratch/` contents. Finished runs with a bad pipeline.json are deleted the same way beyond `keep-runs`, and a run ended by `ensign run clear` is held while a `.tested` marker needs it. Pending, running and ask-user runs are never deleted.

## Folders held by another session's run

- A run that works in a code folder (its workspace and every side's) holds it until it finishes.
- A run that only applies a dependency branch from a folder shares it with other such runs.
- A run that works in a folder is refused while another live run holds it, working or only reading.
- A run that only reads a folder is refused while another live run works in it.
- When a run is refused because a live run of another session holds the folder, tell the user another session is doing work, name the run and the folder, and stop. Never suggest retiring that run or speculate that it is dead.
- Run `ensign run retire <run>` only when the user tells you to: only the user knows a run is hung or stuck.
- Retire refuses a run of your own session (end that with `ensign run stop <run>`), a finished run and an unknown run. Retire is only for another session's run.
- Retiring marks the run as error and frees its folders. It leaves the work in the code folder as it is: look at that work before you start work there.
- A run whose files can't be read blocks every run, link and update, naming the run. `ensign run clear <run>` ends it as a failed run (the folder is kept, the code folders and anything the run started are untouched); its task must pass again before it can publish. It works with no session. It refuses a readable run, a run still open, a finished run with a bad pipeline.json (it blocks nothing bridge-wide, and a new passing run of its task lifts its hold on a publish) and a folder with no `state.json`. gc keeps a cleared run while a `.tested` marker counts it, or while the markers can't be read. Offer it to the user, don't run it unasked.

## Stopping your own run

- When the user says stop during a run of your session, run `ensign run stop <run>` at once. Do not wait for, resume or message the lead or its agents.
- In a sequence, run `ensign sequence halt --reason <why>` first, then stop each running run of the step, so the step ends cancelled. Without the halt the step waits for a decision in stop mode and ends failed at once in keep-going mode.
- Stop ends the run as error and frees its folders. It leaves the work in the code folder as it is. Agents still working are ignored and their reports dropped.
- A stray agent may touch files briefly after the stop: check the workspace before the next run there.
