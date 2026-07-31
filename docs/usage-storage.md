# Usage storage and retention

Heimdall treats the Ledger WAL as the accounting authority. The live aggregate,
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

## Operations

Run these while the server is stopped. The commands acquire the same exclusive
data-directory lock as the server.

```text
heimdall usage compact --config ./config.yaml
heimdall usage verify --config ./config.yaml
heimdall usage prune --config ./config.yaml
heimdall usage prune --config ./config.yaml --before 2026-05-01
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

DuckDB is optional and is not linked into the Heimdall binary.

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
