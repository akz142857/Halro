#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: tools/release/test_deb.sh HALRO_DEB HALRO_DEADMAN_DEB" >&2
  exit 2
fi

halro_deb=$1
deadman_deb=$2
work=$(mktemp -d "${TMPDIR:-/tmp}/halro-deb-test.XXXXXX")
trap 'rm -rf "$work"' EXIT

assert_contains() {
  local haystack=$1 needle=$2
  if ! grep -Fq -- "$needle" <<<"$haystack"; then
    echo "expected package metadata to contain: $needle" >&2
    exit 1
  fi
}

halro_contents=$(dpkg-deb --contents "$halro_deb")
deadman_contents=$(dpkg-deb --contents "$deadman_deb")
assert_contains "$halro_contents" "./usr/bin/halro"
assert_contains "$halro_contents" "./usr/share/halro/config.example.yaml"
assert_contains "$halro_contents" "./usr/lib/systemd/system/halro.service"
assert_contains "$deadman_contents" "./usr/bin/halro-deadman"
assert_contains "$deadman_contents" "./usr/share/halro-deadman/config.example.yaml"
assert_contains "$deadman_contents" "./usr/lib/systemd/system/halro-deadman.service"

if grep -Eq '\./etc/.*/config\.ya?ml' <<<"$halro_contents$deadman_contents"; then
  echo "packages must not install a live configuration" >&2
  exit 1
fi

for package in "$halro_deb" "$deadman_deb"; do
  control_dir="$work/$(basename "$package" .deb)-control"
  dpkg-deb --control "$package" "$control_dir"
  if grep -REn '(systemctl|service)[[:space:]]+(enable|start|restart)|deb-systemd-invoke[[:space:]]+start|halro([[:space:]]|.*\/)(init|start)' "$control_dir"; then
    echo "maintainer scripts must not initialize Halro or enable/start services: $package" >&2
    exit 1
  fi
done

dpkg-deb --extract "$halro_deb" "$work/halro"
host_arch=$(uname -m)
case "$host_arch" in x86_64) host_arch=amd64 ;; aarch64|arm64) host_arch=arm64 ;; esac
package_arch=$(dpkg-deb --field "$halro_deb" Architecture)
if [ "$(uname -s)" = "Linux" ] && [ "$host_arch" = "$package_arch" ]; then
  "$work/halro/usr/bin/halro" version >/dev/null
else
  echo "Skipping binary execution on $(uname -s)/$host_arch for Linux/$package_arch"
fi
echo "Debian package checks passed"
