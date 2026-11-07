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
- The step's pending subtasks, in listed order, split into groups. A group is consecutive subtasks whose locations are unrelated (neither depends on the other, directly or through others) and whose code folders differ. A subtask that conflicts with one in the group starts the next group. A subtask without a location is a group of its own.
- The step creates the runs of the first group, all queued; `go` starts every one of them and returns a dispatch instruction for each. Dispatch every lead; the runs work side by side.
- A run whose folder is busy (another run holds it, or a landing is under way there) waits, with its own reason in the listing, and starts when the folder is free.
- When every run of the group has succeeded, the step creates the next group's runs and starts them. The step is done when none are pending.
- When one run of a group fails, the others continue to their end; then in stop mode the step waits for a decision, and in keep-going mode it ends failed.
- `ensign sequence retry` reruns only the failed subtasks of the group; the runs that succeeded stay. In stop mode `accept` and `skip` end the step; in keep-going mode a failed group ends the step at once (no retry). Either way later groups do not run and their subtasks stay pending.
- The listing shows each group, e.g. `group 2: 02-client-api (client), 03-server-api (server)`, with the status of the runs that exist.
- A run for a subtask applies the task branch of earlier done subtasks in the locations it depends on, by itself: see `ensign help project`, dependencies.
- Each lead is dispatched with the prompt its approval text gives, first line included; that line ties the spawn to its run.

Publishing:

- Pull requests and landing stay at the task level, on the task's branch.
- Default: publish once, after the last subtask. Publish earlier only when the user asks.
- A publish step after a run step lands every location its groups worked in, dependencies first. After a partial failure a new `ensign sequence publish` lands only what is left.
- A publish refused with `a publish of <task> is already under way` applies here too: retry later.
