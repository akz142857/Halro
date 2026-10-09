#!/bin/sh
# A few packages run their tests in sequence for many minutes as one binary,
# and fsync-heavy ones slow down severalfold beside other packages, past any
# reasonable per-package timeout. This splits each into fixed shards, each its
# own `go test` run with its own timeout, so CI can run them in parallel and
# `make test` in sequence. Every other package runs as before in "rest" mode.
#
#   scripts/go-test-shard.sh rest [go test flags...]
#   scripts/go-test-shard.sh shard PKG INDEX TOTAL [go test flags...]
#   scripts/go-test-shard.sh check PKG TOTAL
#   scripts/go-test-shard.sh check-all
#   scripts/go-test-shard.sh all [go test flags...]   # what `make test` runs
#
# Shards take every TOTAL-th top-level test of the sorted list, which spreads
# a run of similarly named heavy tests (TestHA..., TestUsageDate...) instead
# of putting them in one shard. Subtests follow their parent. `check` proves
# the shards are disjoint and together cover every listed test.
set -eu

# The one list of sharded packages: PKG:TOTAL for ordinary runs. Race runs use
# twice as many shards. `rest` excludes exactly these packages.
sharded="./internal/app:4 ./internal/budget:2 ./internal/ledger:2 ./internal/usage:3"

list_tests() {
	# -list prints matching top-level names, then the package's "ok" line.
	go test -list '.*' "$1" | grep -E '^(Test|Fuzz|Example)' | LC_ALL=C sort
}

shard_names() {
	list_tests "$1" | awk -v i="$2" -v n="$3" '(NR - 1) % n == i'
}

require_sharded() {
	for entry in $sharded; do
		if [ "${entry%:*}" = "$1" ]; then
			return 0
		fi
	done
	echo "$1 is not a sharded package" >&2
	exit 2
}

check_package() {
	pkg=$1
	total=$2
	all=$(list_tests "$pkg")
	union=$(i=0; while [ "$i" -lt "$total" ]; do shard_names "$pkg" "$i" "$total"; i=$((i + 1)); done | LC_ALL=C sort)
	if [ "$all" != "$union" ]; then
		echo "shards of $pkg do not partition its tests" >&2
		exit 1
	fi
	if [ -n "$(printf '%s\n' "$union" | uniq -d)" ]; then
		echo "a test of $pkg falls in more than one shard" >&2
		exit 1
	fi
	echo "$(printf '%s\n' "$all" | wc -l | tr -d ' ') tests of $pkg in $total disjoint shards"
}

mode=${1:-}
case "$mode" in
rest)
	shift
	excluded=$(for entry in $sharded; do printf '%s\n' "github.com/akz142857/Halro/${entry%:*}" | sed 's|/\./|/|'; done)
	# shellcheck disable=SC2046 # one argument per package is intended
	exec go test "$@" $(go list ./... | grep -vxF "$excluded")
	;;
shard)
	pkg=$2
	index=$3
	total=$4
	shift 4
	require_sharded "$pkg"
	names=$(shard_names "$pkg" "$index" "$total")
	if [ -z "$names" ]; then
		echo "shard $index/$total of $pkg is empty" >&2
		exit 1
	fi
	pattern="^($(printf '%s\n' "$names" | paste -sd '|' -))\$"
	exec go test "$@" -run "$pattern" "$pkg"
	;;
check)
	require_sharded "$2"
	check_package "$2" "$3"
	;;
check-all)
	for entry in $sharded; do
		check_package "${entry%:*}" "${entry#*:}"
		check_package "${entry%:*}" $((2 * ${entry#*:}))
	done
	;;
all)
	shift
	"$0" check-all
	"$0" rest "$@"
	factor=1
	for flag in "$@"; do
		if [ "$flag" = "-race" ]; then
			factor=2
		fi
	done
	for entry in $sharded; do
		total=$((factor * ${entry#*:}))
		i=0
		while [ "$i" -lt "$total" ]; do
			"$0" shard "${entry%:*}" "$i" "$total" "$@"
			i=$((i + 1))
		done
	done
	;;
*)
	echo "usage: $0 rest|shard PKG INDEX TOTAL|check PKG TOTAL|check-all|all [go test flags...]" >&2
	exit 2
	;;
esac
