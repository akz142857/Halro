#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
review="$root/docs/verification/dependency-license-review.md"

for relative in go.mod go.sum web/package.json web/package-lock.json; do
  actual=$(git -C "$root" hash-object "$relative")
  if ! grep -Fq -- "- \`$relative\`: \`$actual\`" "$review"; then
    echo "dependency license review is stale for $relative (current Git blob $actual)" >&2
    echo "refresh docs/verification/dependency-license-review.md and its drift-gate hashes" >&2
    exit 1
  fi
done
