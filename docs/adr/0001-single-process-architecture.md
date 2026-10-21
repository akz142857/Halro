# ADR 0001: Single-process, single-binary architecture

- Status: Accepted
- Date: 2026-07-31

## Context

Halro is intended to be the Redis-like LLM gateway: small operational surface, predictable performance, and no required external database, cache, queue, or frontend runtime.

## Decision

- The v1 consistency boundary is one Go process.
- Runtime state is stored in one data directory protected by an exclusive OS lock.
- bbolt is a projection of the metadata journal, plus node-local derived keys.
  It was "bbolt stores transactional metadata" until HA Phase 0a
  ([#315](https://github.com/akz142857/Halro/issues/315)) made every
  authoritative write pass through an append-only, MAC'd write-ahead log first;
  bbolt has no log of its own and cannot be replicated, so the authority moved
  to the file that can. The per-key split between what is journalled and what
  each node derives for itself is §5.2 of
  [`docs/todo/halro-ha-architecture.zh-CN.md`](../todo/halro-ha-architecture.zh-CN.md),
  landed as a static table in `internal/store/bolt/journal_class.go`. Nothing
  about the consistency boundary changes: one process, one data directory, one
  exclusive lock.
- One append-only Ledger WAL stores budget and provider-attempt usage facts.
- Checkpoints, live aggregates, and Parquet are rebuildable derivatives.
- React is a build-time dependency only; static assets are embedded with `go:embed`.

## Consequences

- Two active processes may not share a data directory.
- Horizontal scaling is outside v1.
- A standby may use a separately restored snapshot, but only one writer may be active.
- Correct crash recovery and backup watermarks are release-blocking behavior.
