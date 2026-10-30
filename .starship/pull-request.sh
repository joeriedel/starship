#!/bin/sh
# The test stage for a bridge working on this repository: CI is the test suite. Commits
# the task's changes, pushes its branch, opens the pull request if there isn't one, and
# waits for the checks. The exit code is CI's verdict; 125 means it could not get one.
set -e
branch=$(git symbolic-ref --short -q HEAD || true)
case "$branch" in
task/*) ;;
*)
	echo "the checkout is on ${branch:-a detached HEAD}, not a task branch" >&2
	exit 125
	;;
esac
name=${branch#task/}

if [ -n "$(git status --porcelain)" ]; then
	git add -A
	git commit -q -m "$name" || exit 125
fi
git push -q --force-with-lease -u origin "$branch" || exit 125

if ! gh pr view "$branch" --json number >/dev/null 2>&1; then
	body=$(printf 'Opened from a starship task.\n\n%s\n' "$(git log --format='- %s' origin/master..HEAD)")
	gh pr create --head "$branch" --base master --title "$name" --body "$body" >/dev/null || exit 125
fi
gh pr view "$branch" --json url --jq .url

# Checks take a few seconds to register after a push.
sha=$(git rev-parse HEAD)
i=0
until [ "$(gh pr view "$branch" --json headRefOid --jq .headRefOid)" = "$sha" ] &&
	[ "$(gh pr checks "$branch" --json name --jq length 2>/dev/null || true)" -gt 0 ] 2>/dev/null; do
	i=$((i + 1))
	if [ $i -gt 30 ]; then
		echo "no CI checks appeared for $sha" >&2
		exit 125
	fi
	sleep 5
done

if gh pr checks "$branch" --watch --interval 15 >/dev/null; then
	exit 0
fi
gh pr checks "$branch" || true
run=$(gh run list --branch "$branch" --commit "$sha" --json databaseId,conclusion \
	--jq '[.[] | select(.conclusion == "failure")][0].databaseId')
if [ -n "$run" ]; then
	gh run view "$run" --log-failed | tail -n 80
fi
exit 1
