#!/bin/sh
set -eu

usage() {
  echo "usage: $0 RUN_ID OUTPUT_DIRECTORY" >&2
  exit 2
}

[ "$#" -eq 2 ] || usage
run_id=$1
output=$2
case "$run_id" in
  *[!0-9]*|'') usage ;;
esac
[ ! -e "$output" ] || {
  echo "refusing to replace existing archive directory: $output" >&2
  exit 1
}

mkdir -p "$output/release" "$output/evidence"
gh run view "$run_id" --json databaseId,attempt,headSha,headBranch,event,status,conclusion,url,workflowName >"$output/run.json"
gh run view "$run_id" --log >"$output/run.log"
attempt=$(jq -r '.attempt' "$output/run.json")
gh run download "$run_id" --name release-assets --dir "$output/release"
gh run download "$run_id" --name "release-run-evidence-${run_id}-${attempt}" --dir "$output/evidence"

python3 tools/release/run_evidence.py verify \
  --release-dir "$output/release" \
  --manifest "$output/evidence/release-run-evidence.json"

manifest="$output/evidence/release-run-evidence.json"
test "$(jq -r '.run.id' "$manifest")" = "$run_id"
test "$(jq -r '.run.attempt' "$manifest")" = "$attempt"
test "$(jq -r '.run.commit' "$manifest")" = "$(jq -r '.headSha' "$output/run.json")"
workflow_ref=$(jq -r '.run.workflow_ref' "$manifest")
cosign verify-blob \
  --certificate-identity "https://github.com/${workflow_ref}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  --bundle "$output/evidence/release-run-evidence.json.sigstore.json" \
  "$manifest"

(
  cd "$output"
  find . -type f ! -name archive-sha256.txt -print | LC_ALL=C sort | xargs shasum -a 256 >archive-sha256.txt
)
echo "release run archived and verified at $output"
