#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: verify-release.sh VERSION EXPECTED_COMMIT DOWNLOAD_DIRECTORY" >&2
  exit 2
fi
version=$1
expected_commit=$2
download_dir=$3
repository=akz142857/Halro
identity="https://github.com/${repository}/.github/workflows/release.yml@refs/heads/main"
issuer="https://token.actions.githubusercontent.com"

if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
  echo "invalid release version: $version" >&2
  exit 2
fi
if [[ ! "$expected_commit" =~ ^[0-9a-f]{40}$ ]]; then
  echo "invalid release commit: $expected_commit" >&2
  exit 2
fi
actual_commit=$(gh api "repos/$repository/commits/$version" --jq .sha)
if [ "$actual_commit" != "$expected_commit" ]; then
  echo "tag $version resolves to $actual_commit, not $expected_commit" >&2
  exit 1
fi
# Debian sorts '~' before the final version, which preserves SemVer prerelease
# order (0.8.0~rc.1 < 0.8.0). Do not derive it with a pattern replacement whose
# replacement is a bare tilde: Bash 5 tilde-expands that to $HOME even inside
# double quotes, so v1.2.3-rc.1 derived 1.2.3/home/runnerrc.1 and every package
# in a prerelease was then rejected for having the wrong version.
expected_package_version=${version#v}
if [[ "$expected_package_version" == *-* ]]; then
  expected_package_version="${expected_package_version%%-*}~${expected_package_version#*-}"
fi
expected_package_version="${expected_package_version}-1"
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
  # --ignore-missing above exits zero as long as *some* listed file verified, so
  # a package absent from checksums.txt is skipped rather than refused. That is
  # how akz142857/Halro published halro_<version>-1_<arch>.deb with no checksum
  # entry and no Sigstore bundle from v0.7.1 through v0.8.3 while this script
  # reported success. Membership is asserted per package, before anything here
  # trusts the file.
  package_basename=$(basename "$package")
  if ! awk '{print $2}' "$download_dir/checksums.txt" | grep -Fxq -- "$package_basename"; then
    echo "release $version: $package_basename is not listed in checksums.txt" >&2
    exit 1
  fi
  gh attestation verify "$package" --repo "$repository" \
    --source-digest "$expected_commit" \
    --source-ref refs/heads/main \
    --cert-identity "$identity"
  cosign verify-blob --certificate-identity "$identity" \
    --certificate-oidc-issuer "$issuer" \
    --bundle "${package}.sigstore.json" "$package"
done
if [ "$found" -ne 1 ]; then
  echo "release $version contains no Debian packages" >&2
  exit 1
fi

declare -A package_arches=()
for package in "$download_dir"/*.deb; do
  package_name=$(dpkg-deb --field "$package" Package)
  package_arch=$(dpkg-deb --field "$package" Architecture)
  package_version=$(dpkg-deb --field "$package" Version)
  if [ "$package_version" != "$expected_package_version" ]; then
    echo "$package_name ($package_arch) has version $package_version, expected $expected_package_version" >&2
    exit 1
  fi
  package_key="$package_name:$package_arch"
  case "$package_key" in
    halro:amd64|halro:arm64|halro-deadman:amd64|halro-deadman:arm64) ;;
    *) echo "unexpected Debian package: $package_name ($package_arch)" >&2; exit 1 ;;
  esac
  if [ -n "${package_arches[$package_key]:-}" ]; then
    echo "duplicate Debian package for $package_name ($package_arch)" >&2
    exit 1
  fi
  package_arches[$package_key]=$package_version
done
if [ "${#package_arches[@]}" -ne 4 ]; then
  echo "release must contain halro and halro-deadman for amd64 and arm64" >&2
  exit 1
fi
