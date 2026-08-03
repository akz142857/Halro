#!/bin/sh
set -eu

for policy in deploy/aws-kms/*.json; do
	jq -e . "$policy" >/dev/null
done

kubernetes=deploy/kubernetes/heimdall-aws-kms.yaml
grep -Eq '^[[:space:]]+replicas: 1$' "$kubernetes"
grep -Eq '^[[:space:]]+type: Recreate$' "$kubernetes"
grep -Eq '^[[:space:]]+allowPrivilegeEscalation: false$' "$kubernetes"
grep -Eq '^[[:space:]]+readOnlyRootFilesystem: true$' "$kubernetes"
grep -Eq '^[[:space:]]+seccompProfile: \{type: RuntimeDefault\}$' "$kubernetes"
grep -Eq '^[[:space:]]+capabilities: \{drop: \["ALL"\]\}$' "$kubernetes"
! grep -Eq 'AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)' "$kubernetes"

systemd=deploy/systemd/heimdall-aws-kms.service
grep -Fxq 'LimitCORE=0' "$systemd"
grep -Fxq 'NoNewPrivileges=yes' "$systemd"
grep -Fxq 'CapabilityBoundingSet=' "$systemd"
grep -Fxq 'ProtectProc=invisible' "$systemd"

grep -q 'Generate SPDX SBOM' .github/workflows/release.yml
grep -q 'Generate checksums' .github/workflows/release.yml
grep -q 'Keyless sign release blobs' .github/workflows/release.yml
grep -q 'cosign sign-blob' .github/workflows/release.yml
grep -q 'M11_RELEASE_EVIDENCE_JSON' .github/workflows/release.yml
grep -q 'release-evidence/verify.py' .github/workflows/release.yml
grep -q 'cosign verify-blob' .github/workflows/release.yml

! grep -R --include='*.go' --exclude='*_test.go' -q '"net/http/pprof"' cmd internal

python3 -B -m unittest tools/m11/release-evidence/test_verify.py
