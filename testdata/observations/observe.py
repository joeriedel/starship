#!/usr/bin/env python3
# Copyright (c) 2026 Joseph Riedel
# Licensed under the MIT License. See LICENSE.
"""Observes how CLAUDE_ENV_FILE and the lines written to it behave across the routes that start a Claude Code session.

For each route (print mode or an interactive session under a pty; `--permission-mode auto` or not) it runs three sessions in one project: a startup, then `--continue`, then `--resume <id>`. A SessionStart hook (hook.py) logs the payload and the CLAUDE_ENV_FILE it was given, and exports SUPERNOVA_ENV to that file. The model is a scripted stand-in (`ensign selftest-mock`) that makes one Bash call printing $SUPERNOVA_ENV; a PostToolUse hook logs the output. Everything runs in a throwaway home and Claude Code config folder, seeded with nothing from the real ones; no real credential or provider is used.

Usage: observe.py --ensign <path to ensign> --out <folder> [--routes p-plain,p-auto,tui-plain,tui-auto]

Writes <out>/<route>-<step>.jsonl (the raw hook records, paths made generic) and <out>/results.json. The folder names the project `supernova`; the temp root shows as `<root>`.
"""
import argparse
import fcntl
import json
import os
import pty
import select
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time

HERE = os.path.dirname(os.path.abspath(__file__))
KEY = "sk-ant-observe-dummy-key-0000000000"
PROMPT = "run it selftest-script: observe-bash"


def isolated_env(root, base_url):
    home = os.path.join(root, "home")
    env = {k: v for k, v in os.environ.items() if not (k.startswith("ANTHROPIC_") or k.startswith("CLAUDE") or k.upper() in ("NO_PROXY", "HOME", "TERM"))}
    env.update({
        "HOME": home, "XDG_CONFIG_HOME": home + "/.config", "XDG_DATA_HOME": home + "/.local/share", "XDG_STATE_HOME": home + "/.local/state", "XDG_CACHE_HOME": home + "/.cache",
        "CLAUDE_CONFIG_DIR": home + "/.claude", "ANTHROPIC_BASE_URL": base_url, "ANTHROPIC_API_KEY": KEY,
        "CLAUDE_CODE_AUTO_MODE_SERVER": "0", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1",
        "NO_PROXY": "127.0.0.1,localhost", "TERM": "xterm-256color",
    })
    return env


def setup(root):
    proj = os.path.join(root, "supernova")
    cfg = os.path.join(root, "home", ".claude")
    os.makedirs(os.path.join(proj, ".claude"))
    os.makedirs(cfg)
    log = os.path.join(root, "hooks.jsonl")
    hook = lambda ev: [{"matcher": "Bash", "hooks": [{"type": "command", "command": "python3 %s %s %s" % (os.path.join(HERE, "hook.py"), log, ev)}]}] if ev == "PostToolUse" else [{"hooks": [{"type": "command", "command": "python3 %s %s %s" % (os.path.join(HERE, "hook.py"), log, ev)}]}]
    with open(os.path.join(proj, ".claude", "settings.json"), "w") as f:
        json.dump({"hooks": {"SessionStart": hook("SessionStart"), "PostToolUse": hook("PostToolUse")}}, f)
    # An interactive session asks about onboarding, the API key and trusting the folder; these answers are the only seeding.
    with open(os.path.join(cfg, ".claude.json"), "w") as f:
        json.dump({"hasCompletedOnboarding": True, "theme": "dark", "customApiKeyResponses": {"approved": [KEY[-20:]], "rejected": []},
                   "projects": {proj: {"hasTrustDialogAccepted": True, "allowedTools": [], "hasCompletedProjectOnboarding": True}}}, f)
    subprocess.run(["git", "init", "-q"], cwd=proj, check=True)
    return proj, log


def records(log):
    if not os.path.exists(log):
        return []
    return [json.loads(l) for l in open(log) if l.strip()]


def run_print(args, env, cwd, timeout=120):
    p = subprocess.run(["claude", "-p", "--model", "haiku", "--output-format", "json"] + args, input=PROMPT, text=True, capture_output=True, env=env, cwd=cwd, timeout=timeout)
    return p.returncode


def run_tui(args, env, cwd, log, before, timeout=120):
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(cwd)
        os.execvpe("claude", ["claude", "--model", "haiku"] + args, env)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 50, 160, 0, 0))
    out = bytearray()

    def drain(seconds):
        end = time.time() + seconds
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.2)
            if r:
                try:
                    chunk = os.read(fd, 65536)
                except OSError:
                    return False
                if not chunk:
                    return False
                out.extend(chunk)
        return True

    drain(8)
    for ch in PROMPT:
        os.write(fd, ch.encode())
        time.sleep(0.01)
    time.sleep(0.5)
    os.write(fd, b"\r")
    start = time.time()
    while time.time() - start < timeout:
        drain(1)
        if any(r["hook_event_name"] == "PostToolUse" for r in records(log)[before:]):
            drain(4)
            break
    os.write(fd, b"/exit\r")
    drain(5)
    try:
        os.killpg(os.getpgid(pid), signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        pass
    try:
        os.waitpid(pid, 0)
    except ChildProcessError:
        pass
    return 0


def analyse(recs):
    """What the step's hook records show: each firing and whether Bash got the line from the firing that sits under the session's own id."""
    starts = [r for r in recs if r["hook_event_name"] == "SessionStart"]
    posts = [r for r in recs if r["hook_event_name"] == "PostToolUse"]
    res = {
        "firings": [{"log_line": r["firing"], "source": r.get("source"), "session_id": r["session_id"], "env_file_set": bool(r["env_file"]), "env_file_matches_session_id": r["env_file_has_session_id"]} for r in starts],
        "bash_runs": len(posts),
    }
    if posts:
        out = json.dumps(posts[0].get("tool_response", ""))
        res["bash_output"] = out
        session = posts[0]["session_id"]
        marker = "supernova-env=firing-"
        if marker in out:
            res["delivered"] = True
            res["delivered_line_stamped_with_own_session_id"] = ("session-" + session) in out
            n = out.split(marker, 1)[1].split("-", 1)[0]
            res["delivered_line_written_by_this_step_firing"] = n in [r["firing"] for r in starts]
        else:
            res["delivered"] = False
    return res


def sanitize(text, root):
    return text.replace(root, "<root>")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ensign", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--routes", default="p-plain,p-auto,tui-plain,tui-auto")
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    results = {"claude_version": subprocess.run(["claude", "--version"], capture_output=True, text=True).stdout.strip(), "date": time.strftime("%Y-%m-%d"), "routes": {}}
    for route in a.routes.split(","):
        mode, perm = route.split("-")
        root = tempfile.mkdtemp(prefix="supernova-observe-")
        mocklog = os.path.join(root, "mock.jsonl")
        mock = subprocess.Popen([a.ensign, "selftest-mock", mocklog], stdout=subprocess.PIPE, text=True)
        try:
            url = mock.stdout.readline().strip()
            env = isolated_env(root, url)
            proj, log = setup(root)
            flags = ["--allowedTools", "Bash"] + (["--permission-mode", "auto"] if perm == "auto" else [])
            session = None
            steps = {}
            for step in ("startup", "continue", "resume"):
                extra = {"startup": [], "continue": ["--continue"], "resume": ["--resume", session or "missing"]}[step]
                before = len(records(log))
                (run_print if mode == "p" else run_tui)(*((extra + flags, env, proj) if mode == "p" else (extra + flags, env, proj, log, before)))
                recs = records(log)[before:]
                if step == "startup" and recs:
                    session = recs[0]["session_id"]
                steps[step] = analyse(recs)
                with open(os.path.join(a.out, "%s-%s.jsonl" % (route, step)), "w") as f:
                    for r in recs:
                        f.write(sanitize(json.dumps(r, sort_keys=True), root) + "\n")
            results["routes"][route] = steps
        finally:
            mock.kill()
            shutil.rmtree(root, ignore_errors=True)
    with open(os.path.join(a.out, "results.json"), "w") as f:
        f.write(json.dumps(results, indent=2, sort_keys=True) + "\n")
    print(json.dumps(results, indent=2, sort_keys=True))


if __name__ == "__main__":
    sys.exit(main())
