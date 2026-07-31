# ADR 0001: Single-process, single-binary architecture

- Status: Accepted
- Date: 2026-07-31

## Context

Heimdall is intended to be the Redis-like LLM gateway: small operational surface, predictable performance, and no required external database, cache, queue, or frontend runtime.

## Decision

- The v1 consistency boundary is one Go process.
- Runtime state is stored in one data directory protected by an exclusive OS lock.
- bbolt stores transactional metadata.
- One append-only Ledger WAL stores budget and provider-attempt usage facts.
- Checkpoints, live aggregates, and Parquet are rebuildable derivatives.
- React is a build-time dependency only; static assets are embedded with `go:embed`.

## Consequences

- Two active processes may not share a data directory.
- Horizontal scaling is outside v1.
- A standby may use a separately restored snapshot, but only one writer may be active.
- Correct crash recovery and backup watermarks are release-blocking behavior.
