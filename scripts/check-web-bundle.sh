#!/bin/sh
set -eu

repo_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
dist_dir="$repo_dir/internal/webui/dist"

node_major=$(node -p 'process.versions.node.split(".")[0]')
if [ "$node_major" != "22" ]; then
	echo "frontend production evidence requires Node 22 (found $(node --version)); match CI before rebuilding the committed bundle" >&2
	exit 2
fi

# A repeatable build with an old node_modules tree proves only that the old
# compiler/dependencies are repeatable. CI installs from the lockfile first.
# Refuse a stale direct dependency before rebuilding or replacing any assets.
if ! (cd "$repo_dir/web" && npm ls --depth=0 >/dev/null); then
	echo "frontend dependencies do not match package.json; run cd web && npm ci --ignore-scripts before rebuilding the committed bundle" >&2
	exit 2
fi

snapshot_dir=$(mktemp -d "${TMPDIR:-/tmp}/halro-web-dist.XXXXXX")

cleanup() {
	rm -rf -- "$snapshot_dir"
}
trap cleanup EXIT HUP INT TERM

cp -R "$dist_dir/." "$snapshot_dir/"

# The production build starts with `tsc -b`. Comparing the bundle to the
# pre-build snapshot works for both a clean checkout and a correctly rebuilt,
# uncommitted worktree; comparing to HEAD would reject the latter even when the
# source and committed bundle are in sync.
cd "$repo_dir/web"
npm run build

if ! diff -qr "$snapshot_dir" "$dist_dir"; then
	echo "embedded web bundle drifted; commit the rebuilt internal/webui/dist with web sources" >&2
	exit 1
fi
