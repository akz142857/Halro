#!/bin/sh
# Paired price-pin benchmark: base (51586ee7) against main with the group
# commit, interleaved, six rounds, temporary files on the container's own fs.
set -u
mkdir -p /work/tmp
export TMPDIR=/work/tmp
cd /b
for round in 1 2 3 4 5 6; do
  for version in base fixed; do
    ./$version.test -test.run '^$' -test.bench '^BenchmarkDeploymentPricePinCeiling$' -test.cpu 4 -test.count 1 >> r4-$version.txt 2>&1
    rm -rf /work/tmp/*
  done
done
date > DONE4
