# Creating a project

Then fill its bindings (`@build`, `@test`, …): read the code, propose commands for the user to confirm. If the code ships `.starship/project.yaml`, `project new` copies it in; have the user confirm those, not derive new ones. If tests are one command needing no services or setup, propose `plain: true` on `@test` (ensign runs them, no tester agent).
