# Creating a project

Then fill its bindings (`@build`, `@test`, …): read the code, propose commands for the user to confirm. If the code ships `.starship/project.yaml`, `project new` copies it in; have the user confirm those, not derive new ones. If tests are one command needing no services or setup, propose `plain: true` on `@test` (ensign runs them, no tester agent).

`platforms:` is required in `project.yaml`: the platforms the project supports, which reviewers treat as real failure targets. `project new` takes `--platforms linux,macos,windows` unless the code ships them. Example: `platforms: [linux, macos, windows]`.
