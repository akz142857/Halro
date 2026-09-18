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

# A verifier that checks only what happened to be downloaded can certify a
# release after one package or architecture was accidentally omitted. The
# release workflow promises both products for both supported architectures, so
# make completeness part of the trust boundary before checking signatures.
debian_upstream=${version#v}
debian_upstream="${debian_upstream/-/~}"
package_version=${debian_upstream}-1
expected_packages=()
for package_name in halro halro-deadman; do
  for architecture in amd64 arm64; do
    expected_packages+=("${package_name}_${package_version}_${architecture}.deb")
  done
done
for package_name in "${expected_packages[@]}"; do
  if [ ! -f "$download_dir/$package_name" ]; then
    echo "release $version is incomplete: missing $package_name" >&2
    exit 1
  fi
  if [ ! -f "$download_dir/$package_name.sigstore.json" ]; then
    echo "release $version is incomplete: missing $package_name.sigstore.json" >&2
    exit 1
  fi
done

cosign verify-blob --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer" \
  --bundle "$download_dir/checksums.txt.sigstore.json" \
  "$download_dir/checksums.txt"
(
  cd "$download_dir"
  sha256sum --check --ignore-missing checksums.txt
)
for package in "$download_dir"/*.deb; do
  [ -e "$package" ] || continue
  # --ignore-missing above exits zero as long as *some* listed file verified, so
  # a package absent from checksums.txt is skipped rather than refused. That is
  # how the main .deb went unchecksummed from v0.7.1 to v0.8.3 without this
  # script failing. Membership is asserted here, per package, before anything
  # trusts it.
  if ! awk '{print $2}' "$download_dir/checksums.txt" | grep -Fx -- "$(basename "$package")" >/dev/null; then
    echo "release $version: $(basename "$package") is not listed in checksums.txt" >&2
    exit 1
  fi
  gh attestation verify "$package" --repo "$repository"
  cosign verify-blob --certificate-identity "$identity" \
    --certificate-oidc-issuer "$issuer" \
    --bundle "${package}.sigstore.json" "$package"
done
