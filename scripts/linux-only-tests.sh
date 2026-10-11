#!/bin/sh
# Tests that only build for Linux (`_linux_test.go`, `//go:build linux`) are
# never compiled by `make test` on a macOS host, and CI runs only for `main`
# and pull requests. A branch checked on macOS alone therefore never runs
# them; a regression in the physical Usage catch-up stayed hidden for two
# days that way (board §36). Portable tests that skip without Linux or ext4
# run nothing there either. This runs exactly those tests in a Linux
# container whose TMPDIR is a fresh ext4 filesystem, as an unprivileged user.
#
#   scripts/linux-only-tests.sh list    # print PKG<TAB>TEST for every Linux-only test
#   scripts/linux-only-tests.sh run     # build and run them in Docker (ubuntu:24.04)
#
# Tests gated on an explicit fixture variable (HALRO_*_ENOSPC_ROOT,
# HALRO_EXT4_ROOT and the like) still skip here, as they do in CI: they need
# a dedicated small filesystem and are run as recorded evidence instead.
set -eu

image=ubuntu:24.04
ext4_mib=${HALRO_LINUX_TESTS_EXT4_MIB:-8192}

# Test files that build for GOOS=linux but not for GOOS=darwin. Comparing the build lists honours build tags as well as file names.
linux_only_files() {
	pkg=$1
	files='{{range .TestGoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}'
	GOOS=linux go list -f "$files" "$pkg" | LC_ALL=C sort >"$work/linux"
	GOOS=darwin go list -f "$files" "$pkg" | LC_ALL=C sort >"$work/darwin"
	LC_ALL=C comm -23 "$work/linux" "$work/darwin"
	# Portable files whose tests skip unless they find Linux or ext4 run
	# nothing on macOS either.
	xargs -r grep -l -E 't\.Skip\("[^"]*(ext4|Linux|inode)|runtime\.GOOS != "linux"' <"$work/darwin" || true
}

list() {
	for pkg in $(GOOS=linux go list ./...); do
		names=$(linux_only_files "$pkg" | LC_ALL=C sort -u | xargs -r sed -n 's/^func \(Test[A-Za-z0-9_]*\)(t \*testing\.T) {$/\1/p' | LC_ALL=C sort -u)
		for name in $names; do
			printf '%s\t%s\n' "$pkg" "$name"
		done
	done
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM

case "${1:-}" in
list)
	list
	;;
run)
	command -v docker >/dev/null || { echo "linux-only-tests: docker is required" >&2; exit 2; }
	docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image" >/dev/null
	list >"$work/tests"
	test -s "$work/tests" || { echo "linux-only-tests: no Linux-only tests found" >&2; exit 2; }
	module=$(go list -m)
	arch=$(go env GOARCH)
	mkdir "$work/bin"
	: >"$work/plan"
	for pkg in $(cut -f1 "$work/tests" | uniq); do
		rel=${pkg#"$module"/}
		bin=$(printf '%s' "$rel" | tr / _).test
		CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -o "$work/bin/$bin" "$pkg"
		pattern=$(awk -F '\t' -v p="$pkg" '$1 == p { printf "%s%s", sep, $2; sep = "|" }' "$work/tests")
		printf '%s %s ^(%s)$\n' "$rel" "$bin" "$pattern" >>"$work/plan"
	done
	echo "linux-only-tests: $(wc -l <"$work/tests" | tr -d ' ') tests in $(wc -l <"$work/plan" | tr -d ' ') packages"
	# Root only mounts the filesystem; the tests run as nobody, like CI's
	# unprivileged runner, from their package directory (read-only source).
	docker run --rm --pull=never --privileged --network=none \
		-e HALRO_LINUX_TESTS_EXT4_MIB="$ext4_mib" \
		--mount "type=bind,src=$(pwd),dst=/src,readonly" \
		--mount "type=bind,src=$work,dst=/work,readonly" \
		"$image" /bin/sh -eu -c '
truncate -s "${HALRO_LINUX_TESTS_EXT4_MIB}M" /ext4.img
mkfs.ext4 -q -F /ext4.img
mkdir -m 700 /ext4
mount -o loop,nosuid,nodev /ext4.img /ext4
test "$(stat -f -c %t /ext4)" = ef53
chown 65534:65534 /ext4
status=0
while read -r rel bin pattern; do
	echo "=== $rel"
	cd "/src/$rel"
	if ! setpriv --reuid=65534 --regid=65534 --clear-groups env HOME=/ext4 TMPDIR=/ext4 \
		"/work/bin/$bin" -test.run "$pattern" -test.count=1 -test.timeout=20m -test.v >/ext4/out 2>&1; then
		status=1
		grep -v -E "^(=== (RUN|PAUSE|CONT)|--- (PASS|SKIP)|    --- (PASS|SKIP))" /ext4/out || true
	fi
	# A skipped test is not a passed one: report both, top-level only.
	printf "passed %s, skipped %s, failed %s\n" "$(grep -c "^--- PASS" /ext4/out || true)" "$(grep -c "^--- SKIP" /ext4/out || true)" "$(grep -c "^--- FAIL" /ext4/out || true)"
	grep -B1 "^--- SKIP" /ext4/out | sed -n "s/^ *[A-Za-z_0-9]*\.go:[0-9]*: /skip: /p" | sort | uniq -c || true
done </work/plan
exit "$status"
'
	;;
*)
	echo "usage: $0 list|run" >&2
	exit 2
	;;
esac
