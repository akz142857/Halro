#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: verify-release.sh VERSION DOWNLOAD_DIRECTORY" >&2
  exit 2
fi
version=$1
download_dir=$2
repository=akz142857/Halro
identity="https://github.com/${repository}/.github/workflows/release.yml@refs/heads/main"
issuer="https://token.actions.githubusercontent.com"

if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
  echo "invalid release version: $version" >&2
  exit 2
fi
mkdir -p "$download_dir"
gh release download "$version" --repo "$repository" --dir "$download_dir" \
  --pattern '*.deb' --pattern '*.deb.sigstore.json' \
  --pattern checksums.txt --pattern checksums.txt.sigstore.json

cosign verify-blob --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer" \
  --bundle "$download_dir/checksums.txt.sigstore.json" \
  "$download_dir/checksums.txt"
(
  cd "$download_dir"
  sha256sum --check --ignore-missing checksums.txt
)
found=0
for package in "$download_dir"/*.deb; do
  [ -e "$package" ] || continue
  found=1
  gh attestation verify "$package" --repo "$repository"
  cosign verify-blob --certificate-identity "$identity" \
    --certificate-oidc-issuer "$issuer" \
    --bundle "${package}.sigstore.json" "$package"
done
if [ "$found" -ne 1 ]; then
  echo "release $version contains no Debian packages" >&2
  exit 1
fi
