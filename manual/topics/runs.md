# Runs

`bridge.yaml` `keep-runs` (default 500) is how many finished runs the bridge keeps in all. SessionStart deletes older finished runs beyond it, 50 at most per start; `ensign gc [<keep>]` does it now. Finished runs lose their `scratch/` contents. Pending, running and ask-user runs are never deleted.

## Folders held by another session's run

- A run that works in a code folder (its workspace and every side's) holds it until it finishes.
- A run that only applies a dependency branch from a folder shares it with other such runs.
- A run that works in a folder is refused while another live run holds it, working or only reading.
- A run that only reads a folder is refused while another live run works in it.
- When a run is refused because a live run of another session holds the folder, tell the user which run and which folder. That run's session may have ended without finishing it.
- Run `ensign run retire <run>` only with the user's explicit OK, never on your own.
- Retire refuses a run of your own session, a finished run and an unknown run.
- Retiring marks the run as error and frees its folders. It leaves the work in the code folder as it is: look at that work before you start work there.
