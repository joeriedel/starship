#!/bin/sh
# Run start for a bridge working on this repository: put the checkout on the task's
# branch, task/<name>, creating it from origin/master the first time.
set -e
branch="task/${STARSHIP_TASK#*/}"
current=$(git symbolic-ref --short -q HEAD || true)
if [ "$current" = "$branch" ]; then
	exit 0
fi
if [ -n "$(git status --porcelain)" ]; then
	echo "the checkout has uncommitted changes on ${current:-a detached HEAD}; commit or stash them, then start the run again" >&2
	exit 1
fi
if git rev-parse -q --verify "refs/heads/$branch" >/dev/null; then
	git switch -q "$branch"
else
	git fetch -q origin master
	git switch -q -c "$branch" origin/master
fi
echo "on $branch"
