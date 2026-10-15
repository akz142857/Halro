# ADR 0004: Distributed evolution through project ownership

- Status: Accepted for Phase 0 foundations
- Date: 2026-08-01

## Context

ADR 0001 deliberately makes one process and one exclusively locked data
directory the v1 consistency boundary. Running independent processes behind a
load balancer would multiply limits and create competing accounting authorities.
Connection affinity cannot repair that correctness failure.

## Decision

Halro keeps three explicit operating modes:

- **Standalone** is the v1 default and has one local owner.
- **HA** will be one replicated leader/follower group. It improves availability,
  but does not increase the write capacity of one Project.
- **Cluster** will contain multiple HA shards. A versioned Project Directory
  maps each Project to exactly one owning shard.

Project is the primary consistency and sharding boundary. Gateway keys,
authorization, budget reservations, rate limits, Token Guard enforcement,
request attempts, and settlement for a Project must have one logical writer.

An accepted HTTP or SSE request stays with its execution owner until completion.
An active Provider stream is never migrated. After owner failure a client may
retry under the idempotency contract, but Halro does not claim exactly-once
Provider execution. A call that might have reached a Provider without a durable
settlement remains an auditable, conservatively accounted unknown outcome.

Future HA groups require quorum election and fencing. Election alone is not a
safety boundary: an old leader must be unable to commit mutations or start new
Provider calls after a higher ownership epoch exists.

## Phase 0 constraints

Phase 0 adds versioned mutation, ownership, idempotency, replay, and measurement
contracts without adding consensus, cluster transport, shared storage, or a
second writer. Standalone uses epoch 1.

## Consequences

- State is classified as authoritative, rebuildable, or node-local.
- Authoritative mutations need deterministic canonical representations.
- Wall-clock values, random identifiers, and external results are facts supplied
  to a mutation, not values generated while replaying it.
- Mixed-version nodes must reject unsupported mutation schemas and epochs.
- Project migration will require freeze, drain, copy, epoch cutover, and fencing.
