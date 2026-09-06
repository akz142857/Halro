#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: tools/release/build_deb.sh --version vX.Y.Z --arch amd64|arm64 \
  --archive PATH [--revision N] [--output DIRECTORY]

Builds halro and halro-deadman Debian packages from an already-built Halro Linux
release archive. It never recompiles a binary.
EOF
}

version=""
arch=""
archive=""
revision="1"
output="release"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) version="${2:-}"; shift 2 ;;
    --arch) arch="${2:-}"; shift 2 ;;
    --archive) archive="${2:-}"; shift 2 ;;
    --revision) revision="${2:-}"; shift 2 ;;
    --output) output="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([~-][0-9A-Za-z][0-9A-Za-z.+~-]*)?$ ]]; then
  echo "version must look like v1.2.3 or v1.2.3-rc.1, got '$version'" >&2
  exit 2
fi
case "$arch" in amd64|arm64) ;; *) echo "unsupported architecture: '$arch'" >&2; exit 2 ;; esac
if [[ ! "$revision" =~ ^[1-9][0-9]*$ ]]; then
  echo "revision must be a positive integer, got '$revision'" >&2
  exit 2
fi
if [ ! -f "$archive" ]; then
  echo "release archive not found: $archive" >&2
  exit 2
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
archive=$(cd "$(dirname "$archive")" && pwd)/$(basename "$archive")
mkdir -p "$output"
output=$(cd "$output" && pwd)

work=$(mktemp -d "${TMPDIR:-/tmp}/halro-deb.XXXXXX")
trap 'rm -rf "$work"' EXIT
tar -xzf "$archive" -C "$work"
archive_root="$work/halro-linux-$arch"
for required in halro halro-deadman LICENSE NOTICE THIRD_PARTY_NOTICES.md README.md; do
  if [ ! -f "$archive_root/$required" ]; then
    echo "release archive is missing halro-linux-$arch/$required" >&2
    exit 1
  fi
done

upstream_version="${version#v}"
# Debian sorts '~' before the final version, which preserves SemVer prerelease
# order (0.8.0~rc.1 < 0.8.0). A release revision changes packaging only.
debian_upstream="${upstream_version/-/~}"
package_version="${debian_upstream}-${revision}"
source_date_epoch="${SOURCE_DATE_EPOCH:-$(git -C "$repo_root" show -s --format=%ct HEAD)}"

write_control() {
  local root=$1 package=$2 description=$3
  local installed_size
  installed_size=$(du -sk "$root/usr" | awk '{print $1}')
  mkdir -p "$root/DEBIAN"
  cat >"$root/DEBIAN/control" <<EOF
Package: $package
Version: $package_version
Section: admin
Priority: optional
Architecture: $arch
Maintainer: Halro Release Engineering <security@halro.ai>
Installed-Size: $installed_size
Depends: adduser, ca-certificates
Homepage: https://halro.ai/
Description: $description
 Halro is a self-hosted LLM gateway for credentials, routing, policy,
 usage accounting, cost governance, and auditable provider attempts.
EOF
  (
    cd "$root"
    find usr -type f -print0 | LC_ALL=C sort -z | xargs -0 md5sum
  ) >"$root/DEBIAN/md5sums"
}

install_file() {
  local mode=$1 source=$2 destination=$3
  mkdir -p "$(dirname "$destination")"
  install -m "$mode" "$source" "$destination"
}

normalize_tree_mtime() {
  local root=$1 stamp
  if stamp=$(date -u -d "@$source_date_epoch" +%Y%m%d%H%M.%S 2>/dev/null); then
    :
  else
    stamp=$(date -u -r "$source_date_epoch" +%Y%m%d%H%M.%S)
  fi
  find "$root" -exec touch -h -t "$stamp" {} +
}

install_common_docs() {
  local package=$1 root=$2
  install_file 0644 "$archive_root/LICENSE" "$root/usr/share/doc/$package/LICENSE"
  install_file 0644 "$archive_root/NOTICE" "$root/usr/share/doc/$package/NOTICE"
  install_file 0644 "$archive_root/THIRD_PARTY_NOTICES.md" "$root/usr/share/doc/$package/THIRD_PARTY_NOTICES.md"
  install_file 0644 "$archive_root/README.md" "$root/usr/share/doc/$package/README.md"
}

halro_root="$work/pkg-halro"
install_file 0755 "$archive_root/halro" "$halro_root/usr/bin/halro"
install_file 0644 "$repo_root/configs/config.example.yaml" "$halro_root/usr/share/halro/config.example.yaml"
install_file 0644 "$repo_root/packaging/debian/halro.service" "$halro_root/usr/lib/systemd/system/halro.service"
install_file 0755 "$repo_root/packaging/debian/halro.postinst" "$halro_root/DEBIAN/postinst"
install_file 0755 "$repo_root/packaging/debian/package.postrm" "$halro_root/DEBIAN/postrm"
install_common_docs halro "$halro_root"
write_control "$halro_root" halro "Self-hosted LLM gateway"

deadman_root="$work/pkg-halro-deadman"
install_file 0755 "$archive_root/halro-deadman" "$deadman_root/usr/bin/halro-deadman"
for file in config.example.yaml config.schema.json event.schema.json RECEIVER-CONTRACT.md; do
  if [ ! -f "$archive_root/$file" ]; then
    echo "release archive is missing halro-linux-$arch/$file" >&2
    exit 1
  fi
  install_file 0644 "$archive_root/$file" "$deadman_root/usr/share/halro-deadman/$file"
done
install_file 0644 "$repo_root/packaging/debian/halro-deadman.service" \
  "$deadman_root/usr/lib/systemd/system/halro-deadman.service"
install_file 0755 "$repo_root/packaging/debian/halro-deadman.postinst" "$deadman_root/DEBIAN/postinst"
install_file 0755 "$repo_root/packaging/debian/package.postrm" "$deadman_root/DEBIAN/postrm"
install_common_docs halro-deadman "$deadman_root"
write_control "$deadman_root" halro-deadman "Independent monitor for a Halro gateway"

for root in "$halro_root" "$deadman_root"; do
  normalize_tree_mtime "$root"
done

dpkg-deb --build --root-owner-group -Zxz "$halro_root" \
  "$output/halro_${package_version}_${arch}.deb"
dpkg-deb --build --root-owner-group -Zxz "$deadman_root" \
  "$output/halro-deadman_${package_version}_${arch}.deb"
