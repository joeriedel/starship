# Copyright (c) 2026 Joseph Riedel
# Licensed under the MIT License. See LICENSE.
"""Hook for observe.py. Usage: hook.py <log file> <event>

SessionStart: logs the payload and what the hook was given, then exports SUPERNOVA_ENV to CLAUDE_ENV_FILE so a later Bash command shows which firing's line reached it.
PostToolUse: logs the payload, which holds the Bash command's output.
"""
import json
import os
import sys

log, event = sys.argv[1], sys.argv[2]
payload = json.load(sys.stdin)
record = dict(payload)
record["hook_event_name"] = event
if event == "SessionStart":
    env_file = os.environ.get("CLAUDE_ENV_FILE", "")
    record["env_file"] = env_file
    record["env_file_has_session_id"] = bool(env_file) and payload.get("session_id", "") in env_file
    record["claude_pid_set"] = bool(os.environ.get("CLAUDE_PID"))
    firing = str(len(open(log).readlines()) if os.path.exists(log) else 0)
    record["firing"] = firing
    if env_file:
        with open(env_file, "a") as f:
            f.write("export SUPERNOVA_ENV=firing-%s-source-%s-session-%s\n" % (firing, payload.get("source", ""), payload.get("session_id", "")))
with open(log, "a") as f:
    f.write(json.dumps(record) + "\n")
