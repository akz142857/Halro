#!/bin/sh
set -eu

for policy in deploy/aws-kms/*.json; do
	jq -e . "$policy" >/dev/null
done

kubernetes=deploy/kubernetes/halro-aws-kms.yaml
grep -Eq '^[[:space:]]+replicas: 1$' "$kubernetes"
grep -Eq '^[[:space:]]+type: Recreate$' "$kubernetes"
grep -Eq '^[[:space:]]+allowPrivilegeEscalation: false$' "$kubernetes"
grep -Eq '^[[:space:]]+readOnlyRootFilesystem: true$' "$kubernetes"
grep -Eq '^[[:space:]]+seccompProfile: \{type: RuntimeDefault\}$' "$kubernetes"
grep -Eq '^[[:space:]]+capabilities: \{drop: \["ALL"\]\}$' "$kubernetes"
! grep -Eq 'AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)' "$kubernetes"

systemd=deploy/systemd/halro-aws-kms.service
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
grep -q 'tools/release/verify_environment.py' .github/workflows/release.yml
grep -q 'tools/release/run_evidence.py create' .github/workflows/release.yml
grep -q 'release-run-evidence.json.sigstore.json' .github/workflows/release.yml

# Every action reference in both workflows must be a 40-hex commit, not a
# movable tag. A tag can be repointed at new code after review, which is the
# supply-chain hole this release is signed to close; the check lives here so
# adding an unpinned step fails the same gate as removing a signing step.
unpinned=$(grep -hoE '^\s*(- )?uses: [^ ]+' .github/workflows/ci.yml .github/workflows/release.yml \
  | grep -vE '@[0-9a-f]{40}$' || true)
if [ -n "$unpinned" ]; then
  echo "unpinned GitHub Action references:" >&2
  echo "$unpinned" >&2
  exit 1
fi

! grep -R --include='*.go' --exclude='*_test.go' -q '"net/http/pprof"' cmd internal

python3 -B -m unittest tools/m11/release-evidence/test_verify.py
python3 -B -m unittest tools/release/test_verify_environment.py tools/release/test_run_evidence.py
