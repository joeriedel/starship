# Setting up a starship bridge

These steps are for Claude, when a user asks for a starship bridge in a folder.

**Don't clone this repository.** A bridge isn't a copy of starship: it's the user's own
folder, and starship fetches what it needs into it. Run everything below from the folder
the user named, which should be empty. If it already holds files, including a clone of
this repository, stop and ask the user what to do with them; don't delete or move them.

1. Download the newest release's `ensign` for this machine, and let it set up the bridge:

   ```
   TAG=$(git ls-remote --tags --refs --sort=-v:refname https://github.com/joeriedel/starship.git 'v*' | head -n 1 | sed 's#.*/##')
   mkdir -p .starship/bin
   curl -fsSL -o .starship/bin/ensign "https://github.com/joeriedel/starship/releases/download/$TAG/ensign-<platform>"
   chmod +x .starship/bin/ensign
   .starship/bin/ensign init .
   ```

   `<platform>` is `linux-amd64`, `darwin-arm64` (a Mac with Apple silicon),
   `darwin-amd64` (an Intel Mac) or `windows-amd64.exe` (save it as
   `.starship/bin/ensign.exe`, and skip `chmod`).

   `init` pins the bridge to that release, fetches starship's files into
   `.starship/control-plane/`, checks the binary against the release's checksums, and
   commits the bridge's files. Nothing is installed outside the folder. It refuses to
   overwrite a file already in the folder; if it does, tell the user which ones and let
   them decide.

2. Tell the user to restart `claude` in the folder and type `help`. Suggest they push the
   bridge to a private repo of their own, never to starship.
