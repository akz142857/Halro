#!/bin/sh
set -eu
# All writable objects live in this disposable container's own writable layer.
# The caller mounts only binaries and repository input, both read-only.
work=/halro-fixture
image=$work/state.ext4.img
mountpoint=$work/data
loop=
cleanup() {
  if mountpoint -q "$mountpoint"; then umount "$mountpoint"; fi
  if [ -n "$loop" ] && [ "$(losetup -n -O BACK-FILE "$loop")" = "$image" ]; then losetup -d "$loop"; fi
}
trap cleanup EXIT HUP INT TERM
mib=${HALRO_EXT4_IMAGE_MIB:-128}
case "$mib" in ''|*[!0-9]*) exit 2;; esac
test "$mib" -ge 128
test "$mib" -le 32768
test ! -e "$work"
mkdir -m 700 "$work"
test ! -e "$image"
truncate -s "${mib}M" "$image"
test -f "$image"
mkfs.ext4 -q -F -b 4096 -I 256 -m 0 "$image"
mkdir -m 700 "$mountpoint"
# A real physical filesystem has one file-cache layer. A buffered loop also
# charges the private backing-image cache to this cgroup. Keep the ext4 file
# cache in the measurement and require the actual owned loop to bypass only
# that artificial second layer. Unsupported direct IO refuses the experiment.
loop=$(losetup --find --show --direct-io=on "$image")
dio=$(losetup -n --raw -O DIO "$loop")
if [ "$dio" != 1 ]; then
  printf 'HALRO_EXT4_LOOP qualification_refused actual_direct_io=%s\n' "$dio" >&2
  exit 2
fi
printf 'HALRO_EXT4_LOOP direct_io=1 owned_backing=%s loop=%s\n' "$image" "$loop"
test "$(losetup -n -O BACK-FILE "$loop")" = "$image"
mount -t ext4 -o nosuid,nodev,noexec "$loop" "$mountpoint"
export TMPDIR="$mountpoint" HALRO_EXT4_ROOT="$mountpoint"
if [ "$mib" -eq 128 ]; then
  export HALRO_TERMINAL_ENOSPC_ROOT="$mountpoint" HALRO_TERMINAL_ENOSPC_FS=ext4
fi
"$@"
