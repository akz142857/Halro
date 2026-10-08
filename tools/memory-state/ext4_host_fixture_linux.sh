#!/bin/sh
set -eu
# Only the host driver's newly-created exclusive file is writable from the host.
# Do not create, adopt, truncate or delete a preexisting backing file here.
work=/halro-fixture
image=/halro-host-owned/state.ext4.img
mountpoint=$work/data
loop=
loop_device=
backing_device=
backing_inode=
loop_identity=
identity=
verify_loop() {
  current=$(losetup -n --raw -O NAME,BACK-INO,BACK-MAJ:MIN,MAJ:MIN "$loop") || return 1
  test "$current" = "$loop_identity"
}
cleanup() {
  original=$?
  trap - EXIT HUP INT TERM
  set +e
  good=1
  if [ -n "$identity" ]; then
    [ "$(stat -L -c '%d:%i:%s' /proc/self/fd/9)" = "$identity" ] || good=0
    [ "$(stat -c '%d:%i:%s' "$image")" = "$identity" ] || good=0
  else good=0; fi
  if mountpoint -q "$mountpoint"; then
    [ "$(findmnt -n -o SOURCE -M "$mountpoint")" = "$loop" ] || good=0
    [ "$(findmnt -n -o FSTYPE -M "$mountpoint")" = ext4 ] || good=0
    if [ "$good" = 1 ]; then umount "$mountpoint" || good=0; fi
  fi
  mountpoint -q "$mountpoint" && good=0
  if [ -n "$loop" ] && [ -n "$loop_identity" ]; then
    verify_loop || good=0
    [ "$(stat -L -c '%Hr:%Lr' /proc/self/fd/8)" = "$loop_device" ] || good=0
    if [ "$good" = 1 ]; then losetup -d "$loop" || good=0; fi
  else good=0; fi
  # Detach may mark AUTOCLEAR while the original container namespace still
  # owns a VFS reference. Transfer only a pending join; no bytes/fee are retired
  # here. The host creator remains open until rm + independent kernel proof.
  exec 8<&- || good=0
  exec 9>&- || good=0
  if [ "$good" = 1 ]; then
    printf 'HALRO_EXT4_HOST_JOIN_PREPARED {"backing":"/halro-host-owned/state.ext4.img","unmounted":true,"detach_requested":true,"reader_closed":true,"backing_device":"%s","backing_inode":%s,"backing_size":%s,"original_loop":"%s","original_loop_device":"%s","namespace_join_pending":true}\n' "$backing_device" "$backing_inode" "$((mib*1048576))" "$loop" "$loop_device"
  else
    printf 'HALRO_EXT4_HOST_CLEANUP_UNCONFIRMED owner_retained=1\n' >&2
    [ "$original" != 0 ] || original=2
  fi
  exit "$original"
}
trap cleanup EXIT HUP INT TERM
mib=${HALRO_EXT4_IMAGE_MIB:-128}
case "$mib" in ''|*[!0-9]*) exit 2;; esac
test "$mib" -ge 128
test "$mib" -le 32768
test ! -e "$work"
mkdir -m 700 "$work"
test -f "$image"
test ! -L "$image"
exec 9<> "$image"
identity=$(stat -L -c '%d:%i:%s' /proc/self/fd/9)
backing_device=$(stat -L -c '%Hd:%Ld' /proc/self/fd/9)
backing_inode=$(stat -L -c %i /proc/self/fd/9)
test "$(stat -c '%d:%i:%s' "$image")" = "$identity"
test "$(stat -c %s "$image")" -eq "$((mib*1048576))"
test "$(( $(stat -c %b "$image") * 512 ))" -ge "$((mib*1048576))"
printf 'HALRO_EXT4_HOST_BACKING bytes=%s st_blocks=%s identity=%s\n' "$(stat -c %s "$image")" "$(stat -c %b "$image")" "$identity"
df -P -B1 "$image"
# Format through the qualified block device: mkfs against a host shared file
# can turn zero-range writes into holes even when discard is disabled.
mkdir -m 700 "$mountpoint"
loop=$(losetup --find)
case "$loop" in /dev/loop*) minor=${loop#/dev/loop};; *) exit 2;; esac
case "$minor" in ''|*[!0-9]*) exit 2;; esac
# Docker snapshots /dev at container creation. A newly allocated free kernel
# loop can lack its node in this private /dev tmpfs; create only that exact
# kernel-returned node, never overwrite a name or detach an existing binding.
test ! -L "$loop"
if [ ! -e "$loop" ]; then mknod -m 600 "$loop" b 7 "$minor"; fi
test -b "$loop"
test "$(stat -c '%Hr:%Lr' "$loop")" = "7:$minor"
# LOOP_SET_FD is exclusive. A competing bind fails closed before any mkfs IO.
losetup --direct-io=on "$loop" "$image"
test "$(losetup -n --raw -O BACK-FILE "$loop")" = "$image"
exec 8< "$loop"
loop_device=$(stat -L -c '%Hr:%Lr' /proc/self/fd/8)
loop_identity=$(losetup -n --raw -O NAME,BACK-INO,BACK-MAJ:MIN,MAJ:MIN "$loop")
test "$loop_identity" = "$loop $backing_inode $backing_device $loop_device"
printf 'HALRO_EXT4_LOOP_IDENTITY %s\n' "$loop_identity"
dio=$(losetup -n --raw -O DIO "$loop")
if [ "$dio" != 1 ]; then
  printf 'HALRO_EXT4_LOOP qualification_refused actual_direct_io=%s\n' "$dio" >&2
  exit 2
fi
printf 'HALRO_EXT4_LOOP direct_io=1 owned_backing=%s loop=%s\n' "$image" "$loop"
# Native ext4 initialization IO and all later writes are charged here.
mkfs.ext4 -q -F -b 4096 -I 256 -m 0 -E nodiscard,lazy_itable_init=0,lazy_journal_init=0 "$loop"
printf 'HALRO_EXT4_HOST_FORMATTED bytes=%s st_blocks=%s\n' "$(stat -c %s "$image")" "$(stat -c %b "$image")"
test "$(( $(stat -c %b "$image") * 512 ))" -ge "$((mib*1048576))"
mount -t ext4 -o nosuid,nodev,noexec "$loop" "$mountpoint"
printf 'HALRO_EXT4_TARGET '
findmnt -n -o FSTYPE,SOURCE,OPTIONS -M "$mountpoint"
df -P -B1 "$mountpoint"
export TMPDIR="$mountpoint" HALRO_EXT4_ROOT="$mountpoint"
if [ "$mib" -eq 128 ]; then
  export HALRO_TERMINAL_ENOSPC_ROOT="$mountpoint" HALRO_TERMINAL_ENOSPC_FS=ext4
fi
"$@"
