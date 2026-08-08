#!/bin/sh
set -eu

CHECK_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/halro-kms-boundaries.XXXXXX")
cleanup() {
	rm -rf "$CHECK_ROOT"
}
trap cleanup EXIT INT TERM

for package in ./internal/gateway ./internal/masterkey ./internal/kms; do
	output="$CHECK_ROOT/dependencies.txt"
	go list -deps "$package" >"$output"
	if grep -E 'github.com/aws/aws-sdk-go|internal/kms/awskms' "$output"; then
		echo "AWS SDK dependency crossed provider-neutral boundary: $package" >&2
		exit 1
	fi
done

output="$CHECK_ROOT/aws-adapter-dependencies.txt"
go list -deps ./internal/kms/awskms >"$output"
if grep -E 'go\.etcd\.io/bbolt|github\.com/akz142857/Halro/internal/(app|audit|backup|domain|gateway|store|vault)' "$output"; then
	echo "AWS KMS adapter crossed persistence, Vault, Admin, Audit, or request-path boundary" >&2
	exit 1
fi
