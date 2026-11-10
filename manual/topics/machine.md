# Toolchain setup per machine

A binding is a plain command (`go build ./...`); where its tools are differs per machine, so that is recorded in this machine's `machine.yaml`, per project.

On Windows bindings always run through Git Bash (Git for Windows is required; `CLAUDE_CODE_GIT_BASH_PATH` overrides the lookup), so write them for a POSIX shell, `$STARSHIP_*` variables included. Without Git Bash a binding fails saying so.

- When a binding fails because a tool is missing or the wrong version, ask the user for the toolchain's folders and variables. Never guess them.
- `ensign env <project>`: show what is recorded.
- `ensign env <project> NAME=value`: set a variable. `NAME=` removes it. `PATH` is not set this way.
- `ensign env <project> --path <folder>`: put an existing folder first on `PATH`. Repeat for more; earlier ones win.
- `ensign env <project> --drop-path <folder>`: remove one.
- Code locations of a project that declares them are recorded here too: `ensign workspace <project> --location <name> <path>`.
- Ensign applies it to every command it runs for the project, as soon as it is recorded.
- Agents' shells get it from the next run on.
- Installing or downloading a toolchain is not part of this; ask the user to install it.
