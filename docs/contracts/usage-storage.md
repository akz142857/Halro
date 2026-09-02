# Usage storage and retention

Halro treats the Ledger WAL as the accounting authority. The live aggregate,
bbolt checkpoint, and Parquet files are rebuildable derivatives. Budget
admission and recovery never depend on Parquet.

Live aggregation uses a bounded, non-blocking derivative queue. Saturation
never delays a committed Ledger append: it raises a lag marker and drops the
notification, then replays from the aggregate watermark before checkpoint or
Parquet publication. Queue depth, dropped notifications, and lag state are
exported as metrics.

## Files

```text
data/
├── ledger/ledger.wal
├── usage/manifest.json
└── usage/date=YYYY-MM-DD/usage-<min-sequence>-<max-sequence>.parquet
```

Each Parquet row represents one settled Provider attempt. It contains stable
request/attempt/event IDs, Project/Key/Route/Provider/model IDs, token and
micro-USD totals, UTC timestamps, outcome/error class, latency, retry count,
and fallback count. Prompt, response, credentials, authorization headers, and
provider keys are not part of the schema.

Publishing uses temporary file creation, file `fsync`, atomic rename,
directory `fsync`, and then an atomic manifest commit. The manifest records
the schema version, exported Ledger sequence, per-file SHA-256, row count,
token totals, and cost totals. A crash before manifest commit can leave an
orphan file; the next compaction validates and adopts an identical orphan.

## The console checkpoint

The aggregate the console reads is checkpointed into `metadata.db` so a restart
replays only the tail of the WAL rather than all of it. It is stored as a head
plus a series of immutable record segments:

- the **head** (`meta/usage_checkpoint`) carries the watermark, the window's
  floor, the totals and histograms, the requests still in flight, the dedup
  window, and an index of the segments — nothing in it grows with the number of
  records held;
- each **segment** (`usage_checkpoint_segments`, keyed by a big-endian id) holds
  a contiguous run of attempts and request summaries in ledger order, and is
  never rewritten once sealed.

A checkpoint round writes the head, the segments it adds or replaces, the
segments the window has trimmed past, and the daily-rollup increment **in one
bbolt transaction**. A head that committed without its segments would name
records nobody can read, and a checkpoint that advanced without its increment
would leave the rollup describing a prefix of the WAL nobody can name.

A round rewrites the head and at most one open segment, so its cost follows what
arrived since the previous round rather than what the window holds. Restore
reads segments one at a time, refuses any checkpoint it cannot fully read — a
missing segment, a segment that disagrees with the head, records out of ledger
order — and drops records below the stored floor, which is what lets a partially
trimmed segment stay untouched until it can be deleted whole.

Both derivatives are cleared together whenever either is unusable; the Ledger
rebuilds them.

## Operations

Run these while the server is stopped. The commands acquire the same exclusive
data-directory lock as the server.

```text
halro usage compact --config ./config.yaml
halro usage verify --config ./config.yaml
halro usage prune --config ./config.yaml
halro usage prune --config ./config.yaml --before 2026-05-01
```

`compact` replays the Ledger, writes only attempts newer than the manifest
watermark, and immediately reconciles Event IDs against the Ledger view.
`verify` checks safe manifest paths, SHA-256, schema version, duplicate Event
IDs, row counts, sequence bounds, cost, and tokens. It prints a JSON
Ledger-to-Parquet reconciliation report; successful output has `missing`,
`duplicates`, and `extra` equal to zero. Any mismatch fails the command.

`prune` is deliberately explicit. Without `--before`, it uses
`usage.retention_days` (default 90). It commits a manifest excluding expired
partitions before unlinking those files. It does not delete the Ledger WAL;
WAL segment deletion remains disabled until segment rotation, backup pins, and
full cross-layer retention proofs are implemented.

## DuckDB examples

DuckDB is optional and is not linked into the Halro binary.

```sql
SELECT provider_id, provider_model,
       sum(cost_micros_usd) / 1000000.0 AS cost_usd
FROM read_parquet('data/usage/date=*/usage-*.parquet', hive_partitioning=true)
GROUP BY 1, 2
ORDER BY cost_usd DESC
LIMIT 20;
```

```sql
SELECT project_id, key_id,
       sum(provider_input_tokens + provider_output_tokens) AS tokens
FROM read_parquet('data/usage/date=*/usage-*.parquet', hive_partitioning=true)
WHERE completed_at_utc >= now() - INTERVAL 7 DAY
GROUP BY 1, 2
ORDER BY tokens DESC;
```

```sql
SELECT date_trunc('hour', completed_at_utc) AS hour,
       count(*) AS attempts,
       count(*) FILTER (WHERE status <> 'success')::DOUBLE / count(*) AS error_rate,
       avg(latency_millis) AS average_latency_ms
FROM read_parquet('data/usage/date=*/usage-*.parquet', hive_partitioning=true)
GROUP BY 1
ORDER BY 1;
```
