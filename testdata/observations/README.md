Evidence about Claude Code behaviour, kept so the docs can cite it.

`observe.py` runs sessions against the scripted stand-in for the Messages API that `ensign selftest` uses (`ensign selftest-mock [<log file>]` prints its URL and logs every request), in a throwaway home and config folder, and writes the raw hook records and an analysis. `hook.py` is its hook. Run it with `python3 -I observe.py --ensign <path to ensign> --out <folder>`; it needs `claude` on PATH. Each result folder is named by date and Claude Code version and holds a `SUMMARY.md`.
