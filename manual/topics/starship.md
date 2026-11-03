# Improving starship

The bridge's copy of starship is pinned, never edited. Improve starship as a task in a project whose workspace is a starship checkout. If the bridge has none, offer to add one, with a checkout of its own (runs switch its branch); `ensign project new` needs nothing filled in. Each task has its own branch; its test stage waits for CI on it. When the run passes, tell the user and wait for them to ask for the pull request, or for the task to go straight to master.

Starship is public: nothing from this bridge's other projects goes into it. Describe changes in general terms; use the fictional project `supernova` in examples.
