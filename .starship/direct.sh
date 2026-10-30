#!/bin/sh
# Copyright (c) 2026 Joseph Riedel
# Licensed under the MIT License. See LICENSE.

# Direct tasks on this repository: no task branch and no pull request; the work goes
# straight to master. A task opts in by replacing two bindings in its own settings:
#
#   bindings:
#     branch: { command: 'sh "$STARSHIP_WORKSPACE/.starship/direct.sh" start' }
#     test:   { command: 'sh "$STARSHIP_WORKSPACE/.starship/direct.sh" test', plain: true }
#
# start: put the checkout on master, up to date with GitHub.
# test:  commit, push master, and wait for CI on that commit. The exit code is CI's
#        verdict; 125 means it could not get one.
set -e
case "$1" in
start)
	if [ -n "$(git status --porcelain)" ]; then
		echo "the checkout has uncommitted changes; commit or stash them, then start the run again" >&2
		exit 1
	fi
	git switch -q master
	git pull -q --ff-only origin master
	echo "on master at $(git rev-parse --short HEAD)"
	;;
test)
	branch=$(git symbolic-ref --short -q HEAD || true)
	if [ "$branch" != master ]; then
		echo "the checkout is on ${branch:-a detached HEAD}, not master" >&2
		exit 125
	fi
	if [ -n "$(git status --porcelain)" ]; then
		git add -A
		git commit -q -m "${STARSHIP_TASK#*/}" || exit 125
	fi
	git push -q origin master || exit 125
	sha=$(git rev-parse HEAD)

	# The CI run takes a few seconds to appear after a push.
	i=0
	run=""
	while [ -z "$run" ]; do
		i=$((i + 1))
		if [ $i -gt 30 ]; then
			echo "no CI run appeared for $sha" >&2
			exit 125
		fi
		sleep 5
		run=$(gh run list --workflow ci --commit "$sha" --json databaseId --jq '.[0].databaseId // empty' || true)
	done

	if gh run watch "$run" --exit-status >/dev/null; then
		echo "CI passed for $sha"
		exit 0
	fi
	gh run view "$run" --log-failed | tail -n 80
	exit 1
	;;
*)
	echo "usage: direct.sh start|test" >&2
	exit 125
	;;
esac
