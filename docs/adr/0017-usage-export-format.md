# ADR 0017: NDJSON as an alternative Usage export format

- Status: Accepted
- Date: 2026-08-06
- Source: `docs/review/260805.md` §八「【建议】Parquet 导出为"可丢弃派生态"引入 6 个间接
  依赖，包括地理几何库 `twpayne/go-geom`… `internal/usage/parquet.go`（986
  行）。全项目投入产出比最差的一处依赖。建议降级为可选导出或改为无依赖的
  NDJSON。」; tracked as the Parquet half of P2-23 in `docs/review/progress.md`.
- Builds on: [ADR 0014](0014-ledger-wal-backup-compatibility.md) (the backup
  manifest fields this decision must not disturb).

## Context

Usage is a derived, rebuildable projection of the Ledger — CLAUDE.md is
explicit that it "must never be treated as a source of truth for balances."
`internal/usage/parquet.go` (986 lines) is the only place in the module tree
that pulls in `github.com/parquet-go/parquet-go`, which drags six indirect
dependencies with it, including `twpayne/go-geom` — a geometry library with no
relationship to anything Heimdall does. For a derived, discardable artifact,
that is the worst dependency-weight-to-value ratio anywhere in the project.

The finding is about ROI, not correctness: nothing about the Parquet export is
wrong. The two datasets (Settlement/attempt rows, cost-adjustment rows) each
have a JSON manifest (`Manifest`/`AdjustmentManifest`) recording per-partition
SHA-256, sequence range, record count and cost totals; `Verify`/`Reconcile`
already treat the physical file as one input among several checked facts, not
as the source of truth itself — the manifest and the Ledger replay are. That
existing shape is what makes swapping the physical format tractable at all:
almost everything in `parquet.go` (partitioning by date, manifest commit,
checksum/count/total verification, reconciliation against the Ledger,
retention pruning) has nothing to do with Parquet specifically.

## Decision

### NDJSON becomes a second, format, not a replacement

`usage.export_format` (config, default `parquet`) selects what *new*
partitions are written as. Existing Parquet partitions are never rewritten —
the same "existing bytes are never rewritten" discipline the Ledger and Audit
already follow. A deployment that sets `ndjson` keeps every historical
`.parquet` file exactly as it is; only partitions published after the change
land as `.ndjson`.

### Format is recorded per file, not once per manifest

`ManifestFile`/`AdjustmentManifestFile` gain a `Format` field
(`"parquet"`/`"ndjson"`, empty decodes as `"parquet"` for manifests written
before this ADR — the same backward-reading convention `SchemaVersion` already
uses). An operator who flips the config mid-life gets a manifest whose older
entries say `parquet` and newer ones say `ndjson`, and every read path
respects each file's own recorded format rather than a single global
assumption. This is the same reasoning ADR 0014 applied to the Ledger's
per-frame epoch: a mixed history is the normal case, not a special one.

### The row shape does not change, only its container

`parquetAttempt`, `parquetAttemptV2` and `parquetAdjustment` already are the
canonical in-memory representation `Verify` compares against a value rebuilt
from the Ledger (`row != toParquetAttempt(expected)`, a direct struct
comparison). Adding `json:"..."` tags alongside the existing `parquet:"..."`
tags on these same structs and writing them one-JSON-object-per-line
(`.ndjson`) reuses that comparison unchanged — an NDJSON partition and a
Parquet partition of the same data compare bit-for-bit equal once decoded.
Nothing in `Export`, `Verify`, `Reconcile`, or `PruneBefore`'s orchestration
needs to know which container it is looking at; only the leaf read/write
functions (`writeParquetAtomic`/`parquet.ReadFile[T]` vs new
`writeNDJSONAtomic`/`readNDJSONFile[T]`) branch on `Format`.

### The backup/restore contract does not change

`internal/backup/archive.go`'s `Manifest.UsageManifestVersion` /
`AdjustmentManifestVersion` already only carry the `SchemaVersion` ints through
as opaque compatibility markers — ADR 0014 never gave `internal/app/backup.go`
any Parquet-specific knowledge; it calls `exporter.LoadManifest()` and
`exporter.Verify()` and trusts what comes back. Because format is now internal
to the Usage manifest each of those calls already reads, restore validates a
mixed-format usage directory with no new code path: `Verify` already
dispatches per file.

### `writeNDJSONAtomic` mirrors `writeParquetAtomic` exactly

Same temp-file-in-target-directory, `fsync`, atomic rename, directory-`fsync`
sequence `writeParquetAtomic`/`commitManifest` already use — the durability
story for a partition file does not depend on what is inside it.

## Rejected alternatives

### Replace Parquet outright

Rejected. `docs/review/progress.md` already recorded why a flag-day rewrite is
the wrong shape for this: "它不只是删依赖：ADR 0014 的备份 manifest 固定了两个
Parquet manifest 版本与水位，restore 会校验这两个数据集" — a deployment that
has already published Parquet partitions must keep being able to read and
restore them. A second format that new writes can opt into, without
disturbing history, is the only version of "downgrade" that does not also
change ADR 0014's compatibility guarantees.

### A brand-new manifest schema for NDJSON

Rejected. `ManifestFile`/`AdjustmentManifestFile` already carry everything a
reader needs (checksum, sequence range, record count, cost totals) regardless
of container; inventing a parallel schema would duplicate that bookkeeping for
no benefit, and would be exactly the kind of "two answers to one question in
one data directory" ADR 0016 already argued against for a different pair of
formats.

### Auto-detect format from file extension instead of a manifest field

Rejected. The manifest is already the single source of truth for what a
partition file is supposed to contain (path, checksum, sequence range) —
teaching the reader to infer meaning from a filename suffix instead of trusting
the recorded field reintroduces exactly the kind of implicit-format inference
the checksum/manifest design exists to avoid.

## Consequences

- `go.mod` keeps the `parquet-go` dependency — this ADR does not remove it,
  since existing installations may hold Parquet partitions indefinitely.
  Nothing here reduces the dependency footprint of a Heimdall binary; it gives
  new deployments (or ones willing to let history age out under retention) a
  path to stop adding to it.
- `Manifest`/`AdjustmentManifest` readers written against pre-ADR-0017 code
  will decode NDJSON-formatted manifests fine (the JSON envelope is unchanged)
  but will fail to open `.ndjson` partition files if pointed at Parquet-only
  logic — this is exactly the "old binary rejects what it cannot read" shape
  every other format evolution in this codebase already follows, not a new
  risk.
- Verification and reconciliation reports become meaningful per mixed-format
  manifest without an operator needing to know or care which partitions are
  which container.

## Required verification

- byte-for-byte existing Parquet fixture preservation, and a manifest with one
  Parquet and one NDJSON partition both verifying and reconciling correctly
  against the same Ledger replay;
- `writeNDJSONAtomic` crash-injection at the same points `writeParquetAtomic`
  is already tested at (partial write, missing fsync, interrupted rename);
- `Verify`/`Reconcile`/`PruneBefore` exercised against an NDJSON-only manifest,
  a Parquet-only manifest, and a mixed one;
- a restore drill where the archived usage directory is mixed-format, proving
  `internal/app/backup.go` needs no changes to validate it;
- an old manifest with no `Format` field on any entry (pre-ADR-0017) still
  decoding every entry as Parquet.
