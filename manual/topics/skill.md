# Facts with no skill

A fact naming a nonexistent skill is held; `status` lists it. If asked for the skill, read `skills/proposed/<name>/`, write a one-line description of the technology and run `ensign skill new <name> --description "<description>"`. The facts become pending; `skill-curate` folds them in. If told to discard them, run `ensign skill discard <name>`. Commit either way: `ensign journal commit`.
