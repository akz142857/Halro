#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/backup.sh [options]

Create and immediately verify an offline encrypted Halro backup.

Options:
  --binary PATH       Halro binary (default: ./bin/halro)
  --config PATH       Halro config (default: ./config.yaml)
  --output-dir PATH   Backup archive directory (default: ./backups)
  --key-file PATH     Dedicated 32-byte Backup Key (default: ./backup.key)
  --name NAME         Archive filename without .hmbk (default: UTC timestamp)
  -h, --help          Show this help

The server must be stopped. If --key-file does not exist, this script creates
it with mode 0600. Store that key independently from the archive and Master Key.
EOF
}

binary=./bin/halro
config=./config.yaml
output_dir=./backups
key_file=./backup.key
backup_name=

while (($# > 0)); do
  case "$1" in
    --binary|--config|--output-dir|--key-file|--name)
      if (($# < 2)) || [[ -z "$2" ]]; then
        echo "missing value for $1" >&2
        usage >&2
        exit 2
      fi
      case "$1" in
        --binary) binary=$2 ;;
        --config) config=$2 ;;
        --output-dir) output_dir=$2 ;;
        --key-file) key_file=$2 ;;
        --name) backup_name=$2 ;;
      esac
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ ! -x "$binary" ]]; then
  echo "Halro binary is not executable: $binary" >&2
  exit 1
fi
if [[ ! -f "$config" ]]; then
  echo "Halro config does not exist: $config" >&2
  exit 1
fi
if [[ -d "$key_file" ]]; then
  echo "Backup Key path is a directory: $key_file" >&2
  exit 1
fi

mkdir -p "$output_dir"
if [[ ! -e "$key_file" ]]; then
  if ! command -v openssl >/dev/null 2>&1; then
    echo "openssl is required to generate a Backup Key" >&2
    exit 1
  fi
  key_parent=$(dirname "$key_file")
  mkdir -p "$key_parent"
  old_umask=$(umask)
  umask 077
  openssl rand 32 >"$key_file"
  umask "$old_umask"
  chmod 600 "$key_file"
  echo "Generated Backup Key: $key_file" >&2
  echo "Move or copy it to an independent Secret Manager or encrypted offline store." >&2
fi

if [[ -z "$backup_name" ]]; then
  backup_name="halro-$(date -u +%Y%m%dT%H%M%SZ)"
fi
if [[ "$backup_name" == */* || "$backup_name" == "." || "$backup_name" == ".." ]]; then
  echo "Backup name must be a filename without path components" >&2
  exit 1
fi
backup_name=${backup_name%.hmbk}
if [[ -z "$backup_name" ]]; then
  echo "Backup name must not be empty" >&2
  exit 1
fi
archive="$output_dir/$backup_name.hmbk"

if [[ -e "$archive" ]]; then
  echo "Backup archive already exists: $archive" >&2
  exit 1
fi

echo "Running offline diagnostics..." >&2
"$binary" doctor --config "$config" >/dev/null

echo "Creating encrypted backup: $archive" >&2
"$binary" backup create \
  --config "$config" \
  --output "$archive" \
  --key-file "$key_file"
if [[ ! -f "$archive" ]]; then
  echo "Backup command returned without publishing the archive: $archive" >&2
  exit 1
fi

echo "Verifying encrypted backup..." >&2
"$binary" backup verify \
  --file "$archive" \
  --key-file "$key_file"

echo "Backup created and verified: $archive" >&2
echo "Backup Key: $key_file (store independently; it is not embedded in the archive)" >&2
