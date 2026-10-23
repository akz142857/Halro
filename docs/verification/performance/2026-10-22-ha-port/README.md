# HA port: 24-hour Replica soak and the price-pin cost on Linux, 2026-10-22

Two items `docs/todo/halro-ha-port-to-main.zh-CN.md` §6 owed after the port
(#445–#447): the Replica's heap over 24 hours (HA design §4.1: a Replica's
in-memory state must be bounded), and what phase 0a's metadata journal costs
the price-pin path on Linux rather than darwin.

## 1. 24-hour soak

Code: `main` at `871d5f3f` (after #447). Three members on one host (Apple M4,
macOS 27.2): `halro-0` Primary, `halro-1` and `halro-2` Replicas. Ledger seal
on at the 16 MiB floor with compression. Load: batches of 300 Chat requests,
4 workers, against the Primary, back to back, routed to an upstream that fails
so every request settles without provider I/O (mostly
`all_candidates_suspended`); a Gateway Key created every ~600 batches so the
metadata journal grows too. Sampled once a minute per node: Go runtime heap,
RSS, `applied_index`, and the sizes of the Ledger, `state.snap`, `archive.db`
and `history.db`. `halro-1` was restarted at 12 h. Started 2026-10-22 01:20:37
+08, stopped 2026-10-23 01:20:52 (Primary first), all three exiting 0.

Files: `soak-samples-10min.csv` (the per-minute samples thinned to one row per
node per ten minutes), `soak-events.log`, `soak-summary.txt` (final status,
byte comparison of the four logs, `doctor`, `ledger verify`, `audit verify`
per node).

### Heap is bounded

Median `heap_inuse` / RSS per two-hour window, MB:

| hour | n0 heap | n1 heap | n2 heap | n0 RSS | n1 RSS | n2 RSS | applied index |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 0 | 31.3 | 15.0 | 14.8 | 91.8 | 56.9 | 58.0 | 15,265 |
| 4 | 45.6 | 31.6 | 31.5 | 285.5 | 142.8 | 143.0 | 71,296 |
| 8 | 46.6 | 38.9 | 37.2 | 279.4 | 145.1 | 143.1 | 121,884 |
| 12 | 38.8 | 29.2 | 38.1 | 291.0 | 71.7 | 154.4 | 171,651 |
| 16 | 37.1 | 30.3 | 38.6 | 353.8 | 73.2 | 165.1 | 227,096 |
| 20 | 39.1 | 30.5 | 37.4 | 317.1 | 73.2 | 149.9 | 282,306 |
| 22 | 40.8 | 30.9 | 38.9 | 115.8 | 61.8 | 103.0 | 308,177 |

The Replica that ran the whole 24 hours (`halro-2`) climbs for the first ~5
hours and is flat from there while the Ledger keeps growing (321,055 frames,
27 sealed generations): the ratio of its last-6-hour median heap to its
2–8-hour median is 1.15, slope +0.4 MB/h after warm-up. What fills during the
climb is the Ledger's record-digest ring (65,536 entries, full around index
65k), which is fixed-size. A heap profile at 9 h showed the same top entry.
Single-sample peaks (154 MB on the Primary, 87 MB on the Replicas) are
transient and do not recur in the medians.

### Seal, compression, and the logs

- The Primary sealed 27 times and compressed 27 generations; `halro-2` applied
  all 27 seals and compressed 27, never sealing itself (design §4.2, §4.3).
  This covers the "Primary seals, Replica applies the seal and compresses" row
  the 2026-10-21 drill could not reach.
- At the end `halro-0` and `halro-2` hold byte-identical Ledger, Audit,
  Governance and metadata journals; `doctor`, `ledger verify` and
  `audit verify` pass on all three members.

### The restarted Replica stalled — the bug #448 fixed

`halro-1` reopened from its snapshot at 13:20:46 about 200 frames behind and
never caught up: the Primary logged `replication peer session queue is full`
3,318 times until the end, and `halro-1` stopped at index 158,068. This run
predates #448 (catch-up stuffed the whole gap into the 64-slot session queue
before the writer started, so the 65th frame always failed and every retry
restarted from the same index). The fix was verified with the real binary on a
stuck data directory and merged as `f19e4118`. Consequences here: `halro-1`'s
logs differ from the others ("2 distinct" in `soak-summary.txt`), its
second-half heap is that of an idle process, and the Primary's second-half
RSS includes rebuilding the gap on every retry. Boundedness is judged on
`halro-2`.

## 2. Price-pin cost on Linux

Phase 0a (#445) made `halro.db` a projection of a metadata journal, so every
metadata transaction, a price pin among them, adds an fsynced journal append.
On darwin the pin benchmark lost ~30–35% at 1 and 8 workers. This repeats it
on Linux.

Host: Linux 7.0.14 (OrbStack VM on an Apple M4 Mac), 6 vCPU, arm64. Temporary
bbolt/journal files on the container's own overlay filesystem, not a bind
mount, so fsync is the VM's (`env.txt`). Test binaries built with go1.26.10,
`CGO_ENABLED=0`, `GOARCH=arm64`. `-test.cpu 4`, six rounds, the two versions
interleaved within each round, temporary files removed between runs
(`run-price-pin.sh`, `run-group-commit.sh`). Benchmark:
`BenchmarkDeploymentPricePinCeiling` (`internal/store/bolt`).

Versions: base `51586ee7` (before the journal), main `871d5f3f` (after the
port), group commit = the `fix/metadata-group-commit` branch merged as
`a50c0e20` (#449).

| workers | base | main (`871d5f3f`) | group commit (#449) |
| ---: | ---: | ---: | ---: |
| 1 | 62.2 attempts/s | 46.3 (−25.6%, p=0.002) | 48.3 (−23.0% vs its base run, p=0.002) |
| 8 | 412.8 | 283.2 (−31.4%, p=0.002) | 299.1 (−28.1%, p=0.015) |
| 64 | 2,062 | 992 (−51.9%, p=0.002) | 2,047 (~ vs base, p=0.31; +106.5% vs main, p=0.002) |

Files: `base.txt` / `main.txt` and `benchstat.txt` (first pass);
`base-r2.txt` / `group-commit.txt` and `benchstat-group-commit.txt` (second
pass, base re-run alongside; written by the script as `r4-base.txt` /
`r4-fixed.txt`); `benchstat-main-vs-group-commit.txt` compares across the two
passes and is indicative only.

Reading it:

- At 1 and 8 workers the cost is the journal's fsync, as on darwin, and it is
  the price of the journal: ~25–30%.
- The 64-worker drop was not the fsync. The metadata batch layer took its place
  in the queue before taking the writer lock, so batches that queued behind a
  busy writer could only split, never merge (instrumented run: 7 operations per
  transaction, 76 ms waiting for the writer). #449 made it a group commit — take
  the commit turn, then drain the queue up to 64 per transaction — and the
  64-worker result returns to base.
- This is a VM's fsync on a laptop SSD, not production disk; the shape is the
  finding, not the absolute numbers.
