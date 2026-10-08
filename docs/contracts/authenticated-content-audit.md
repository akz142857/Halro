# Authenticated content root audit

`authenticatedstate.Store.Audit` explicitly pins one exact root and authenticates every reachable node and value. `Snapshot.Audit` provides the same traversal for an existing immutable pin. A successful receipt records the fixed root identity plus actual verified row, node, value-byte and read-byte counts. It does not cover unreachable/orphan pages, other roots or native log history and grants no startup, role or business readiness authority.

The DFS retains a height/fanout-bounded stack and one value, reusing the 32 MiB logical IO workspace. A Store audit occupies one of the existing four pin slots and its 4 KiB logical pin allowance. There is one active Store audit, with no waiting queue; unrelated COW updates can publish newer roots while it runs. These logical allowances are not Linux RSS/cgroup calibration evidence.

The caller must supply positive maximum duration and maximum no-progress duration; the latter cannot exceed the former. There are no production defaults. The Snapshot audit clock starts after exclusive reader-lock admission, before header checks and workspace acquisition. Store pin selection precedes this clock. Progress is a successful authenticated node or value read, never a caller heartbeat. `Progress` is a synchronous observer of fixed-size counters and must not reenter the same Snapshot.

A watchdog permanently faults every Store pin on deadline or no-progress failure, even while the actual audit is blocked in IO or its observer. Synchronous boundary and return checks prevent scheduler delay from producing an overdue successful receipt. Every failed audit returns zero progress/receipt. Integrity corruption also faults every Store pin; ordinary caller cancellation and observer errors do not grant integrity-failure evidence. A standalone Snapshot retains audit deadline failures in its own reader state.

Store Close cancels its audit and joins actual Snapshot work before retiring its FD and workspace. A blocking observer must actually return; cancellation does not refund live work or claim that an operating-system IO has ended. Persistent disk ownership remains until the original creator performs qualified Discard.

This is an explicit component operation. Autonomous scheduling, independently accepted production deadlines, stale-audit readiness/promotion gates, old native-source auditing and default trusted startup remain unimplemented. M3 remains disabled. Evidence: [local regression report](../verification/performance/2026-10-04-content-root-audit/README.md).
