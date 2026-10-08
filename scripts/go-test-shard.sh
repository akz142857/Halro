#!/bin/sh
# internal/app holds over a thousand tests and runs for about half an hour as
# one binary, past any reasonable per-package timeout. This splits it into
# fixed shards, each its own `go test` run with its own timeout, so CI can run
# them in parallel and `make test` in sequence. Every other package runs as
# before in the "rest" mode.
#
#   scripts/go-test-shard.sh rest [go test flags...]
#   scripts/go-test-shard.sh app INDEX TOTAL [go test flags...]
#   scripts/go-test-shard.sh check TOTAL
#
# Shards take every TOTAL-th top-level test of the sorted list, which spreads
# a run of similarly named heavy tests (TestHA..., TestUsageDate...) instead
# of putting them in one shard. Subtests follow their parent. `check` proves
# the shards are disjoint and together cover every listed test.
set -eu

pkg=./internal/app

list_app() {
	# -list prints matching top-level names, then the package's "ok" line.
	go test -list '.*' "$pkg" | grep -E '^(Test|Fuzz|Example)' | LC_ALL=C sort
}

shard_names() {
	list_app | awk -v i="$1" -v n="$2" '(NR - 1) % n == i'
}

mode=${1:-}
case "$mode" in
rest)
	shift
	# shellcheck disable=SC2046 # one argument per package is intended
	exec go test "$@" $(go list ./... | grep -v '/internal/app$')
	;;
app)
	index=$2
	total=$3
	shift 3
	names=$(shard_names "$index" "$total")
	if [ -z "$names" ]; then
		echo "shard $index/$total of $pkg is empty" >&2
		exit 1
	fi
	pattern="^($(printf '%s\n' "$names" | paste -sd '|' -))\$"
	exec go test "$@" -run "$pattern" "$pkg"
	;;
check)
	total=$2
	all=$(list_app)
	union=$(i=0; while [ "$i" -lt "$total" ]; do shard_names "$i" "$total"; i=$((i + 1)); done | LC_ALL=C sort)
	if [ "$all" != "$union" ]; then
		echo "shards of $pkg do not partition its tests" >&2
		exit 1
	fi
	if [ -n "$(printf '%s\n' "$union" | uniq -d)" ]; then
		echo "a test of $pkg falls in more than one shard" >&2
		exit 1
	fi
	echo "$(printf '%s\n' "$all" | wc -l | tr -d ' ') tests of $pkg in $total disjoint shards"
	;;
*)
	echo "usage: $0 rest|app INDEX TOTAL|check TOTAL [go test flags...]" >&2
	exit 2
	;;
esac
