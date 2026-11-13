# Runs

`bridge.yaml` `keep-runs` (default 500) is how many finished runs the bridge keeps in all. SessionStart deletes older finished runs beyond it, 50 delete attempts at most per start, shared with folders that have `pipeline.json` and no `state.json`, which are deleted first once 5 minutes old and don't count toward `keep-runs`; `ensign gc [<keep>]` does it now. Finished runs lose their `scratch/` contents; `reports/evidence/`, where run agents leave evidence that must outlive the run, stays. Finished runs with a bad pipeline.json are deleted the same way beyond `keep-runs`, and a run ended by `ensign run clear` is held while a `.tested` marker needs it. Pending, running and ask-user runs are never deleted.

## Uncommitted edits before a run

A run is refused while its work folder has uncommitted git changes (a p4 task is never refused for them), and the refusal names the paths (up to 5). Ask the user to commit, stash or discard them, then run again; a direct edit leaves such changes. This applies to `ensign run` and again at `go`, which is refused the same way and can be typed again once the folder is clean. A sequence step is not refused: it waits like a step on a busy folder (`waiting: …` in the listing and the Stop message) and starts by itself once the folder is clean. A retry is exempt: its dirty tree is the failed attempt's work, carried into the new run. A run's commit sweeps in everything dirty, which is why a run never starts on top of someone's work.

## The closing gate

When the last stage passes, the run is not finished: `ensign next` says the pipeline has passed and gives the lead the command to record its judgment. `ensign run finish --run <run> --goal met` finishes it as a success; `--goal not-met "<why>"` finishes it as the error `goal-not-met`. Only then does a sequence publish or move on. A lead that stops without finishing is told the same, once for free and then counted as an early stop. A run that is not at the gate refuses `run finish`. Without a lead (a pipeline the session drives), run it yourself.

## Recording tested work

The `record:` setting (`project.yaml`, overridable in a task's frontmatter) says what a run does with work that passed its tests:

- git `commit` (default): the github-pr and git-catalog steps commit it, and github-pr records the tested commit. git `none`: those steps commit nothing and no tested marker is written, so the work stays uncommitted in the workspace for the user to land by hand. A git task can't use `shelve`.
- Perforce `shelve` (default): at the successful end of a run that passed its test stage and went through the whole pipeline (docs edits included), `p4 shelve -f -c <changelist>`; `commit`: `p4 submit -c <changelist>`; `none`: nothing, the changelist stays open. A changelist with no open files records nothing and the run says so; a failing `p4` fails the run. A project `@record` binding replaces the submit or shelve, run with the handler environment plus `STARSHIP_RECORD` (the mode), `STARSHIP_CHANGELIST` and `STARSHIP_CLIENT`; a not-applicable `@record` does nothing. The git-catalog and github-pr steps do nothing for a Perforce task.
- A publish step (and `ensign github-pr merge` or `pull-request`) for a task with `record: none` is refused: nothing was recorded, so the user lands the work by hand.
- A review stage's dispatch ends with the run's change: for git the `git diff <base>` command plus the untracked files it does not show, for Perforce a file in the run's `reports/` folder (`change-<dispatch>.diff`) with the diff of the files open in the task's changelist, or of the shelf when none are open; a change no one opened (`ensign p4 open`) is not in it.

## Folders held by another session's run

- A run that works in a code folder (its workspace and every side's) holds it until it finishes.
- A run that only applies a dependency branch from a folder shares it with other such runs.
- A run that works in a folder is refused while another live run holds it, working or only reading.
- A run that only reads a folder is refused while another live run works in it.
- A run is also refused while a landing (a publish step or `github-pr merge`) is under way in a folder it works in or reads, a folder around it or a folder inside it: `a landing is under way in the code folder <folder>; start this once it has finished`. It names no run; wait for the landing, then start the run again. Landings in one folder do not refuse each other.
- An update (`github-pr update`) under way in a folder refuses a run the same way: `an update is under way in the code folder <folder>; start this once it has finished`. Wait for it, then start the run again.
- A landing is refused while an approved run of another task holds the folder (works or reads) and is not on its own branch there yet; the refusal names the run: wait for that run to finish. An update is refused while any approved run holds the folder, unless the branch is already up to date.
- When a run is refused because a live run of another session holds the folder, tell the user another session is doing work, name the run and the folder, and stop. Never suggest retiring that run or speculate that it is dead.
- Run `ensign run retire <run>` only when the user tells you to: only the user knows a run is hung or stuck.
- Retire refuses a run of your own session (end that with `ensign run stop <run>`), a finished run and an unknown run. Retire is only for another session's run.
- Retiring marks the run as error and frees its folders. It leaves the work in the code folder as it is: look at that work before you start work there.
- A run whose files can't be read blocks every run, link and update, naming the run. `ensign run clear <run>` ends it as a failed run (the folder is kept, the code folders and anything the run started are untouched); its task must pass again before it can publish. It works with no session. It also takes an ask-user run whose pipeline.json can't be read, is missing or fails budget canonicalization (it can't resume; the run keeps its finish time, so a pass since still lifts its hold). A pipeline.json that fails budget canonicalization counts as unreadable. It refuses a readable run, a run still open, a success or error run with a bad pipeline.json (it blocks nothing bridge-wide; a cleanup removes it past `keep-runs`, and only a faulted error run is held on a publish until a new passing run of its task) and a folder with no `state.json` (it blocks nothing; a cleanup removes it only once its pipeline.json is 5 minutes old, and a folder with neither file is left for you to delete by hand). gc keeps a cleared run while a `.tested` marker counts it, or while the markers can't be read. Offer it to the user, don't run it unasked.

## Stopping your own run

- When the user says stop during a run of your session, run `ensign run stop <run>` at once. Do not wait for, resume or message the lead or its agents.
- In a sequence, run `ensign sequence halt --reason <why>` first, then stop each running run of the step, so the step ends cancelled. Without the halt the step waits for a decision in stop mode and ends failed at once in keep-going mode.
- Stop ends the run as error and frees its folders. It leaves the work in the code folder as it is. Agents still working are ignored and their reports dropped.
- A stray agent may touch files briefly after the stop: check the workspace before the next run there.
