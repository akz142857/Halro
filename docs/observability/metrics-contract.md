# Metrics contract

Status: Accepted for Standalone Phase A0
Owner: Application Architecture
Reviewers: SRE, Security

This document is the compatibility contract for Heimdall's Prometheus
exposition. `docs/contracts/metrics-reference.md` remains the operator-facing inventory.

## Invariants

- Application metrics use the `heimdall_` prefix. Standard Go/process metrics
  retain their ecosystem names.
- Durations are seconds; byte quantities end in `_bytes`; counters end in
  `_total`.
- `status`, `direction`, `reason`, `provider_type`, `operation`, `error_class`,
  `purpose`, and Key Slot `state` are finite enums.
- `provider_id` and `deployment_id` are managed opaque identifiers. They must
  not contain a hostname, customer name, credential fragment, URL, model name,
  request ID, key ID, project ID, or source address.
- Target labels such as `environment`, `region`, `cluster`, and `instance` are
  added by Prometheus, not by Heimdall.
- No current metric has `shard`, `role`, or `authority` semantics. Adding one
  requires the Cluster ownership ADR to define which replicas emit it and how
  it aggregates.
- Metrics are derivative observations. Ledger remains authoritative for usage
  and accounting.
- KMS metrics never label a Key ARN, account, Slot ID, request ID, ciphertext,
  identity, error text, or Vault fingerprint. Recovery is explicit; therefore
  `heimdall_kms_automatic_fallback_total` is an invariant-zero tripwire rather
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
