#!/bin/sh
# Paired price-pin benchmark on Linux: base (51586ee7, before the metadata
# journal) against main (871d5f3f), interleaved, six rounds. The benchmark's
# temporary bbolt/journal files live on the container's own filesystem, not on
# the bind mount, so fsync is the Linux VM's and not virtiofs'.
set -u
mkdir -p /work/tmp
export TMPDIR=/work/tmp
cd /b
{ uname -a; nproc; df -h /work/tmp; mount | grep -E " / | /work" ; date; } > env.txt 2>&1
for round in 1 2 3 4 5 6; do
  for version in base main; do
    ./$version.test -test.run '^$' -test.bench '^BenchmarkDeploymentPricePinCeiling$' \
      -test.cpu 4 -test.count 1 >> $version.txt 2>&1
    rm -rf /work/tmp/*
  done
  echo "round $round done $(date)" >> progress.txt
done
date > DONE
