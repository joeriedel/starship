# Sequences

When the user asks for several steps, build a sequence: `ensign sequence run supernova/<task>`, `ensign sequence publish supernova/<task>`, `ensign sequence release supernova <version>`, `ensign sequence do <instruction>`. Give the sequence a short name saying what it is for with `--name` on its first step, for example `ensign sequence do <instruction> --name "hygiene fixes"`; later steps join it and take no name. Steps are numbered from 1 in each sequence and addressed as `<seq>.<step>`, for example `5.3`; a plain `<step>` works only while exactly one sequence is open. Show the listing it prints; one `go` approves it as shown and the steps then run one after another. Set `ensign sequence mode keep-going` only when asked. After each run write its journal account, and report each step as it finishes. A release step runs as a background command when the Stop hook names it. A `do` step is yours to carry out, then `ensign sequence done <seq>.<step>` or `fail <seq>.<step> --reason`; never edit a workspace for it (the edit guard still applies). Publish only as an approved publish step; any other push still needs the user's ask. When a run step fails, the sequence waits. Retry with `ensign sequence retry <seq>.<step> --diagnosis "<why>" --state "<what is left>"` only for a diagnosed, small, fixable cause; never for a design question, a scope change, or the same finding coming back; otherwise `ensign sequence accept <seq>.<step>`; in stop mode accept ends the step, halts the sequence and cancels every later step (in keep-going it skips later steps of that task). Only when the user explicitly asks to carry on past the failure, like `drop`: `ensign sequence skip <seq>.<step>` (stop mode only) ends the step as failed, cancels its task's publish step and lets the rest of the sequence run. Its output names later steps it may affect, in this sequence and in later ones: release steps, `do` steps naming the task, run steps of the task, and publish steps of the task in later sequences. Run steps of other tasks aren't flagged because each starts from master, but the failed run's uncommitted work stays in its workspace, and a later run in that folder won't start until it is committed or stashed. Skip is available only while the step waits, since a failure after a retry halts the sequence at once. Report every retry and write its diagnosis in the journal. In keep-going mode, if continuing is wasted, run `ensign sequence halt --reason <why>` and say why.

When several runs of the session are live:

- Pass `--run <id>` to `ensign next`, `ensign resume` and `ensign run-stage`; without it they refuse and name the live runs.
- One run per code folder: start a run or a step there only when no other live run of any session holds the folder. Ensign refuses it otherwise; a step left waiting starts once the folder is free.
- Each run finishes on its own: write each one's journal account as it ends.

When a run step names a task with subtasks:

- The step holds a group of runs, one per subtask, side by side; `go` and each later group's start return one dispatch instruction per run. Dispatch every lead, passing the prompt exactly as given: its first line names the run.
- The Stop hook names every lead still waiting to be dispatched.
- A failed run does not stop the others of its group. When all are over the step waits; `retry` reruns only the failed ones, `accept` ends the step and later groups do not run; `skip` likewise.
- Details: `ensign help subtasks`.

When the project declares locations:

- A run step for a task with several locations needs `--location <name>`; `--sides <a,b>` narrows its sides. Both go on `ensign sequence run`.
- Retry keeps the run's location. It keeps the sides only if the old run was narrowed; otherwise the retry takes the default sides.
- A release step takes `--location <name>`: `ensign sequence release <project> <version> --location <name>`. Its `@release` is the project's overridden by that location's, and it runs in that location's folder. Without `--location` the project's only location is used; with several, the step is refused.

Drop pending steps of an approved sequence with `ensign sequence drop <seq>.<step>... --reason <why>` only when the user asks; it keeps the rest running, and dropping a run step drops its task's publish step.

Before `go` the sequence is yours to rebuild: `ensign sequence drop <seq>.<step>...` removes steps (the rest renumber; publish rule as above), `ensign sequence discard` removes the whole sequence so the next step starts a fresh one. To reorder or insert a step (say a `supernova` publish step between two runs), drop the later steps and add them again in the new order. `halt` stops work that has begun; don't use it to rebuild.

- `ensign sequence` shows the open sequences; `ensign sequence --all` shows every sequence in the session, finished ones included.
