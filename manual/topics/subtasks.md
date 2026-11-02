# Subtasks

A task can be split into subtasks, each run on its own, one after another, on the task's branch. A task with no `subtasks/` folder works as always.

- Layout: one file per subtask, `projects/<project>/tasks/<task>/subtasks/<NN>-<name>.md`, e.g. `01-models-schema.md`. The name is the file name without `.md`.
- Order: by the number the name starts with, then by name. Number them in the order they must run.
- Content: a `## Goal` and/or a `## Scope` section, as in a task. A subtask with neither is refused.
- Frontmatter is optional. `location: <name>` sets the code location its run works in. When the task uses several locations every subtask must name one of them; otherwise it is optional.
- Never edit the task's own files to record progress; ensign keeps it in `.starship/tasks/`.

Running:

- `ensign run <project>/<task>` runs the first pending subtask. `--subtask <name>` runs a named pending one.
- The run's work item is the task's goal and scope, the subtask's goal and scope, and a list of earlier subtasks already on the branch. Later subtasks are not shown to it.
- `--location` must match the subtask's location, if it names one.
- A run that ends in success marks its subtask done. A failed run leaves it pending.
- When none are pending, `ensign run` refuses and says so.

Progress:

- `ensign task open` and the task listings show `subtasks <done or dropped>/<total>` and the next pending name.
- `ensign task subtask done <project>/<task> <name>`: mark one done, for work finished outside a run.
- `ensign task subtask drop <project>/<task> <name>`: mark one dropped; it is skipped and not on the branch.
- Ask the user before marking a subtask by hand.

Sequences:

- A sequence step names a task, never a subtask: `ensign sequence run` refuses `--subtask`.
- The step runs the next pending subtask; when it succeeds and more are pending, the step queues the next one and carries on. The step is done when none are pending.
- A failed subtask run waits for a decision as any run does; `ensign sequence retry` reruns the same subtask, and `accept` ends the step so later subtasks do not run.

Publishing:

- Pull requests and landing stay at the task level, on the task's branch.
- Default: publish once, after the last subtask. Publish earlier only when the user asks.
