#!/bin/sh
# Independent, read-only join after the original container namespace is gone.
set -eu
image=/halro-host-owned/state.ext4.img
test "$#" -eq 5
expected_device=$1
expected_inode=$2
expected_size=$3
original_loop=$4
original_loop_device=$5
exec 5< "$image"
identity="$expected_device:$expected_inode:$expected_size"
verify() {
  test "$(stat -L -c '%Hd:%Ld:%i:%s' /proc/self/fd/5)" = "$identity"
  test "$(stat -c '%Hd:%Ld:%i:%s' "$image")" = "$identity"
  test "$(stat -L -c %F /proc/self/fd/5)" = 'regular file'
  test "$(stat -c %h "$image")" -eq 1
  test "$(( $(stat -L -c %b /proc/self/fd/5) * 512 ))" -ge "$expected_size"
}
verify
tries=0
while :; do
  # BACK-FILE is a namespace-relative string. Other containers may have exactly
  # that string for a different image; only the actual backing dev/inode counts.
  rows=$(losetup -l -n --raw -O NAME,BACK-INO,BACK-MAJ:MIN,MAJ:MIN)
  matches=$(printf '%s\n' "$rows" | awk -v inode="$expected_inode" -v device="$expected_device" '$2 == inode && $3 == device { print $0 }')
  verify
  if [ -z "$matches" ]; then break; fi
  if [ "$tries" -ge 100 ]; then
    printf 'HALRO_HOST_JOIN_UNCONFIRMED original_loop=%s original_device=%s actual_owned_refs=%s\n' "$original_loop" "$original_loop_device" "$matches" >&2
    exit 2
  fi
  sleep 0.05
  tries=$((tries+1))
done
exec 5<&-
printf 'HALRO_HOST_LOOP_JOIN {"backing_device":"%s","backing_inode":%s,"backing_size":%s,"original_loop":"%s","original_loop_device":"%s","loop_absent":true,"observer_reader_closed":true,"polls":%s}\n' "$expected_device" "$expected_inode" "$expected_size" "$original_loop" "$original_loop_device" "$tries"
