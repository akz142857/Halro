#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: import-release.sh VERIFIED_DOWNLOAD_DIRECTORY" >&2
  exit 2
fi
download_dir=$1
found=0
for package in "$download_dir"/*.deb; do
  [ -e "$package" ] || continue
  found=1
  reprepro includedeb stable "$package"
done
if [ "$found" -ne 1 ]; then
  echo "no verified Debian packages found in $download_dir" >&2
  exit 1
fi
reprepro check stable
