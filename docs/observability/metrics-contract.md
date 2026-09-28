# Metrics contract

Status: Accepted for Standalone Phase A0
Owner: Application Architecture
Reviewers: SRE, Security

This document is the compatibility contract for Halro's Prometheus
exposition. `docs/contracts/metrics-reference.md` remains the operator-facing inventory.

## Invariants

- Application metrics use the `halro_` prefix. Standard Go/process metrics
  retain their ecosystem names.
- Durations are seconds; byte quantities end in `_bytes`; counters end in
  `_total`.
- `status`, `direction`, `reason`, `provider_type`, `operation`, `error_class`,
  `purpose`, and Key Slot `state` are finite enums.
- `provider_id` and `deployment_id` are managed opaque identifiers. They must
  not contain a hostname, customer name, credential fragment, URL, model name,
  request ID, key ID, project ID, or source address.
- Target labels such as `environment`, `region`, `cluster`, and `instance` are
  added by Prometheus, not by Halro.
- HA members export their own `halro_cluster_member_info{cluster_id,node_id}`,
  `halro_cluster_role{role}`, and local replication indexes. These HA identity
  labels come from fixed member configuration, not request data. A health view
  must compare them with the target's `cluster` and `instance` labels. The
  independent view, HA alert rules, and HA epoch recording rule select raw
  member metrics from `job="halro",expected_target="true"`; another job's
  same-named series cannot supply a member, clear a missing-role alert, or
  provide a stable epoch. Rules that aggregate HA state must scrape every
  configured member, select the current `environment` and `cluster`, and keep
  member-local metrics distinct by `instance`. The shared stable-epoch record
  compares term and incarnation raw sample counts with the same member's
  five-minute `up` count, requires one source for each logical term,
  incarnation, and `up` series, and checks the current scrape; a selective
  missing or duplicated sample cannot be hidden by Prometheus lookback.
  Replica metrics do not assert
  Primary write availability. There is no `shard` or generic `authority` label;
  introducing either requires an
  ownership contract.
- `halro_ha_status_auth_failures_total` counts rejected machine HA status reads
  locally. It has no token, certificate, client, or error-reason labels; the
  bounded access log carries the diagnostic outcome without expanding metric
  cardinality.
- All HA members expose `halro_replication_member_state_version` as the
  authenticated local state format version. Version-3 members additionally
  expose `halro_replication_transition_journal_segments`
  and `halro_replication_transition_journal_bytes` without extra labels. These
  count retained member journal files and their known bytes, including the
  active segment, but exclude `state.json`, free disk space and any external
  archive. `halro_replication_transition_journal_capacity_readable` is 1 when
  the member can account for those files and 0 when the active file changed or
  cannot be read; byte and segment samples are omitted in that case. A
  successfully scraped version-3 member must have exactly one current
  readable, byte and segment sample, with readable equal to 1. The capacity
  alert treats missing or duplicated same-scrape gauges as incomplete
  accounting, including while Prometheus lookback exposes an older value. A
  version-2 member exposes none of these series, rather than a misleading
  zero-byte complete history. Monitor the persistent volume's free bytes and
  inodes separately, and do not treat these gauges as chain-authentication or
  archive receipts.
- The Primary's `halro_replication_durable_to_confirm_seconds` measures a
  local-durable frame through durable quorum confirmation, not an HTTP request
  outcome. Recovered frames lack a process-local start and are omitted.
  `halro_replication_last_confirmed_timestamp_seconds` reports the last new
  confirmation observed by this process, with zero meaning none yet.
- `halro_replication_required_confirmation_wait_total{store,outcome}` counts
  terminal required Ledger or metadata confirmation barrier outcomes on the
  Primary. Both labels have fixed enums (`ledger|metadata` and
  `confirmed|deadline|unavailable|canceled|error`). It excludes writes that
  fail before a wait and internal provider-object/startup/shutdown waits; it
  does not represent final client HTTP responses or a request success rate.
- `halro_ha_client_write_http_responses_total{outcome}` counts terminal server
  observations for the fixed HA Gateway client write route inventory. The
  bounded outcomes distinguish 2xx, 4xx, Replica `not_primary`, Primary 503,
  timeout, other 5xx, canceled, and write error. A Primary 503 alone does not
  identify replication as its cause. It includes
  pre-Ledger refusals and counts retries separately. A committed 2xx stream
  can later fail, so this is not a client-received success count.
- The proposed `halro_ha_client_logical_operation_results_total`,
  `halro_ha_client_observer_ready`, and
  `halro_ha_client_observer_unreconciled_operations` belong to an independently deployed
  client/ingress producer, not this application exposition. Their proposed
  labels and semantics are in
  [the client-final integration contract](../contracts/ha-client-final-result.md).
  Do not add them to the exported metrics reference or use them for an HA
  success rate until the producer and coverage acceptance are verified.
  The optional `HalroClientObserverDown` and
  `HalroClientObserverCoverageIncomplete` rules are dormant without an
  `expected_client_observer="true"` target. Once configured, they report
  target loss, missing readiness/coverage gauges, and unreconciled operations;
  the coverage rule compares both gauge sample times with the same target's
  `up` scrape so Prometheus lookback cannot reuse an older healthy value.
  They cannot detect a target omitted entirely from Prometheus inventory.
- Replica `halro_replication_sink_persist_seconds` measures native sink calls
  for new frames, including failures; `halro_replication_apply_batch_seconds`
  measures successful confirmed-prefix projection batches. Neither uses a
  peer, request, or frame label.
- Metrics are derivative observations. Ledger remains authoritative for usage
  and accounting.
- KMS metrics never label a Key ARN, account, Slot ID, request ID, ciphertext,
  identity, error text, or Vault fingerprint. Recovery is explicit; therefore
  `halro_kms_automatic_fallback_total` is an invariant-zero tripwire rather
  than evidence that automatic failover exists.

## Compatibility

Metric name, type, HELP, and label names are a versioned public operations
contract. Additive series are compatible. Renaming, deleting, changing a type,
or changing a label set requires release notes and a rule migration,
and one release of overlap where technically possible.

CI parses representative exposition and rejects unknown labels and sensitive
canaries. Alert queries must select `environment` and `cluster`.

## Histogram decision

Classic histograms are the Phase C compatibility baseline. Native histograms
remain deferred until the supported Prometheus, remote-write, and
long-term storage matrix is proven. Request and attempt histograms are derived
from Ledger events in the Usage aggregate, use the aggregate watermark for
exactly-once replay, and persist bucket counts in the Usage checkpoint.

## Completion

- Contract tests parse TYPE, HELP, names, and labels.
- Replay/checkpoint tests prove histogram equality.
- `docs/contracts/metrics-reference.md` exactly lists exported application metrics.
