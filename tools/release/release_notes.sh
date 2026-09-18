#!/usr/bin/env bash
# Render a release's notes from CHANGELOG.md.
#
# `gh release create --generate-notes` writes the merged-pull-request list, which
# describes how the work arrived rather than what the release is. v0.8.4 is the
# case that made it untenable: its substance was one merge, and the eleven
# dependency bumps around it took the top eleven lines. prepare has already
# refused any version whose CHANGELOG section is missing, so the section is the
# release's own description and is what the notes are built from.
set -euo pipefail

version=${1:?usage: release_notes.sh vX.Y.Z [changelog]}
changelog=${2:-CHANGELOG.md}
repository=${GITHUB_REPOSITORY:-akz142857/Halro}
upstream=${version#v}

section=$(awk -v want="## [${upstream}]" '
  index($0, want) == 1 { capture = 1; next }
  capture && /^## \[/ { exit }
  capture { print }
' "${changelog}")

# Fail closed: notes that silently came out empty would publish an immutable
# release describing nothing, and the tag already exists by the time this runs.
if [ -z "$(printf '%s' "${section}" | tr -d '[:space:]')" ]; then
  echo "release_notes: ${changelog} has no content under '## [${upstream}]'" >&2
  exit 1
fi

# Trim the blank lines the section boundary leaves at either end.
section=$(printf '%s\n' "${section}" | sed -e '/./,$!d' | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}')

printf '%s\n' "${section}"

cat <<NOTES

## Install

\`\`\`bash
docker pull ghcr.io/${repository%%/*}/halro:${version}
docker pull ghcr.io/${repository%%/*}/halro-deadman:${version}   # the independent watchdog, deployed separately
\`\`\`

Binary archives for Linux and macOS on amd64 and arm64, and Debian packages for
both Linux architectures, are attached below. Every artifact is listed in
\`checksums.txt\` and carries a Sigstore bundle; the verification steps are in the
[README](https://github.com/${repository}/blob/${version}/README.md#verify-release-downloads).

Full notes: [CHANGELOG.md](https://github.com/${repository}/blob/${version}/CHANGELOG.md).
NOTES

# The compare link is a convenience, not a gate: a run that cannot resolve the
# previous tag still publishes complete notes rather than failing the release.
previous=$(git describe --tags --abbrev=0 "${version}^" 2>/dev/null || true)
if [ -n "${previous}" ]; then
  printf '\n**Full Changelog**: https://github.com/%s/compare/%s...%s\n' \
    "${repository}" "${previous}" "${version}"
fi
