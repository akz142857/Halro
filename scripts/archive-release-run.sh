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

output_parent=$(dirname "$output")
mkdir -p "$output_parent"
staging=$(mktemp -d "$output_parent/.halro-release-archive.XXXXXX")
cleanup() {
  [ -z "$staging" ] || rm -rf -- "$staging"
}
trap cleanup EXIT HUP INT TERM

gh run view "$run_id" --json databaseId,attempt,headSha,headBranch,event,status,conclusion,url,workflowName >"$staging/run.json"
attempt=$(jq -er '.attempt | select(type == "number") | tostring' "$staging/run.json")
artifact_json=$(gh api "repos/{owner}/{repo}/actions/runs/${run_id}/artifacts?per_page=100")
release_assets=$(printf '%s' "$artifact_json" | jq -er \
  '[.artifacts[] | select(.expired == false and .name == "release-assets") | .name] | unique | if length == 1 then .[0] else error("expected one live release-assets artifact") end')
formal="release-run-evidence-${run_id}-${attempt}"
dry_run="${formal}-dry-run"
evidence=$(printf '%s' "$artifact_json" | jq -er --arg formal "$formal" --arg dry_run "$dry_run" \
  '[.artifacts[] | select(.expired == false) | .name | select(. == $formal or . == $dry_run)] | unique | if length == 1 then .[0] else error("expected exactly one live formal or dry-run evidence artifact") end')

mkdir -p "$staging/release" "$staging/evidence"
gh run view "$run_id" --log >"$staging/run.log"
gh run download "$run_id" --name "$release_assets" --dir "$staging/release"
gh run download "$run_id" --name "$evidence" --dir "$staging/evidence"

python3 tools/release/run_evidence.py verify \
  --release-dir "$staging/release" \
  --manifest "$staging/evidence/release-run-evidence.json"

manifest="$staging/evidence/release-run-evidence.json"
test "$(jq -r '.run.id' "$manifest")" = "$run_id"
test "$(jq -r '.run.attempt' "$manifest")" = "$attempt"
test "$(jq -r '.run.commit' "$manifest")" = "$(jq -r '.headSha' "$staging/run.json")"
workflow_ref=$(jq -r '.run.workflow_ref' "$manifest")
cosign verify-blob \
  --certificate-identity "https://github.com/${workflow_ref}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  --bundle "$staging/evidence/release-run-evidence.json.sigstore.json" \
  "$manifest"

(
  cd "$staging"
  find . -type f ! -name archive-sha256.txt -print | LC_ALL=C sort | xargs shasum -a 256 >archive-sha256.txt
)
mv "$staging" "$output"
staging=""
echo "release run archived and verified at $output"
