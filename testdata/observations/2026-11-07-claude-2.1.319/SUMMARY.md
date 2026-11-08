# CLAUDE_ENV_FILE across the session routes, Claude Code 2.1.319, 2026-11-07

Produced by `testdata/observations/observe.py` (with `hook.py`), run on Linux with Claude Code 2.1.319 (`claude --version` printed `2.1.319 (Claude Code)`). Each session ran in a throwaway home and Claude Code config folder with a dummy API key, against the scripted stand-in for the Messages API (`ensign selftest-mock`), so no real account, credential, provider or model was involved. The project is the fictional `supernova`; paths show as `<root>`. The raw hook records are in the `*.jsonl` files here and the per-step analysis is in `results.json`.

Each route ran three sessions in one project, in order: a startup, `--continue`, `--resume <id>`. A SessionStart hook logged its payload and the `CLAUDE_ENV_FILE` it was given and wrote `export SUPERNOVA_ENV=firing-<n>-source-<source>-session-<id>` to that file. The scripted model then made one Bash call, `echo "supernova-env=$SUPERNOVA_ENV"`, and a PostToolUse hook logged the output. Routes: `p-plain` is `claude -p --allowedTools Bash`; `p-auto` adds `--permission-mode auto`; `tui-plain` and `tui-auto` are the same without `-p`, driven under a pty (Python `pty`, the prompt typed and Enter sent). One run of each step per route, so these are single observations, not counts.

Auto mode ran with `CLAUDE_CODE_AUTO_MODE_SERVER=0`, so Claude Code sent its own classifier requests (ordinary Messages requests to a Sonnet model, asking for `<severity>N</severity>`) and the stand-in answered `<severity>0</severity>`. With the default, a base URL other than Anthropic's makes Claude Code expect the server to review actions, the stand-in cannot, and every action that needs a check is refused (observed: `Auto mode can't check actions in this session`, exit 1, an Agent call denied). A Bash call that `--allowedTools` allows (an `echo`, seen once) ran in that mode with the default too. What is not established is the server-side review path, and the verdicts of the real classifier.

| route | step | SessionStart source | env file under the session's id | Bash ran | line delivered to Bash | delivered line written by this step's firing |
|---|---|---|---|---|---|---|
| p-plain | startup | startup | yes | yes | yes | yes |
| p-plain | continue | resume | no (throwaway id) | yes | yes, from the startup's firing | no |
| p-plain | resume | resume | yes | yes | yes | yes |
| p-auto | startup | startup | yes | yes | yes | yes |
| p-auto | continue | resume | no (throwaway id) | yes | yes, from the startup's firing | no |
| p-auto | resume | resume | yes | yes | yes | yes |
| tui-plain | startup | startup | yes | yes | yes | yes |
| tui-plain | continue | resume | yes | yes | yes | yes |
| tui-plain | resume | resume | yes | yes | yes | yes |
| tui-auto | startup | startup | yes | yes | yes | yes |
| tui-auto | continue | resume | yes | yes | yes | yes |
| tui-auto | resume | resume | yes | yes | yes | yes |

In every step `session_id` was the first session's id (startup, continue and resume kept it), and the line that reached Bash carried that id.

What this shows. Interactive `--continue` matches: the env file Claude Code gave the SessionStart hook sat under the session's own id, and a line written to it reached Bash (tui-plain and tui-auto). This differs from `-p --continue`, which gave a throwaway id, whose file did not reach Bash. Interactive `--resume <id>` delivered to Bash (tui-plain and tui-auto). Delivery to Bash under `--permission-mode auto` was observed on `-p` startup and `--resume` and on all three interactive steps; `-p --continue` under auto behaved as without it (throwaway id, line from the earlier firing).

Limits. One run per step, on one machine, Linux only; macOS and Windows were not observed. The interactive sessions were driven by typed keystrokes into a pty and ended with `/exit`, then killed. The session model was the scripted stand-in, so nothing here says how a real model behaves. The onboarding, API key and folder-trust answers an interactive session asks for were seeded in the throwaway config folder (`.claude.json`); nothing was copied from a real one.
