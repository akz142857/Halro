# HA client-final logical-operation result contract (integration boundary)

Status: proposed P1 producer contract. This repository contains an optional
manifest-gated consumer, but no production client/ingress producer. The
series must not be counted as delivered HA telemetry until an independently
deployed source and the acceptance cases below pass.

## Ownership and unit of observation

The producer is an instrumented caller that owns all retries and consumes the
final response, or an ingress that owns the documented retry contract **and
receives an explicit application-level completion receipt from that caller**.
Finishing a socket write, sending the final stream frame, or closing a
connection does not prove that the caller consumed it. A Halro member, a
server HTTP handler, a reverse proxy without that receipt, and an SDK test
with retries disabled cannot produce this fact. The producer must remain
observable while the Primary and client Service are down.

One observation is **one logical client operation**, from its first attempt
through all configured retries and complete response consumption. Attempts do
not each increment the counter. For a stream, receiving HTTP 2xx or the first
chunk is not completion: the protocol's terminal success event and response
end must both be consumed. A duplicate logical-operation event must be
deduplicated before metric export. The private deduplication key must never be
a Prometheus label or exposed in the HA health page.

## Proposed Prometheus series

| Series | Type and HELP | Labels | Producer requirement |
| --- | --- | --- | --- |
| `halro_ha_client_logical_operation_results_total` | Counter: "Final client-observed outcome of one HA logical operation after configured retries and complete response consumption." | `operation_class`, `outcome` | Initialize every approved class/outcome pair to zero on startup; increment exactly one pair once per completed logical operation. |
| `halro_ha_client_observer_ready` | Gauge: "Whether this client-final observer is currently able to record and export logical-operation results." | none | Emit 1 only while the observer's result pipeline and exporter are available; emit 0 on a known recording fault. A missing gauge is unknown, never ready. |
| `halro_ha_client_observer_unreconciled_operations` | Gauge: "Started client logical operations whose terminal client result cannot be reconciled after their deadline or observer recovery." | none | Report from durable records, including after restart; ordinary in-flight operations are excluded. Zero is required before a client-final rate can be shown. Missing or unreadable state is unknown, not zero. |

The scrape target supplies `environment`, `region`, `cluster`, `instance`, a
dedicated `job="halro-client-observer"`, and `expected_client_observer="true"`.
These labels identify the approved observer inventory; they are not copied
from client requests. `operation_class` is a finite, reviewed deployment
manifest of public operation families, not a URL, model, provider, Project, or
user-provided value. The producer publishes its exact enum and the mapping of
routes/SDK calls to it before the first rollout; a new class requires a
contract revision and coverage test. Export these series without
producer-supplied sample timestamps so Prometheus gives `up` and all observer
series one scrape timestamp. `outcome` is exactly one of:

| Outcome | Terminal condition |
| --- | --- |
| `success` | The final successful response was fully consumed and validated; a stream also delivered its protocol terminal success event and ended cleanly. |
| `rejected` | A complete final response rejecting the operation was received after retries. A 409 from an idempotency record is a rejection, not proof that the earlier attempt's response reached this caller. |
| `timeout` | The logical operation deadline expired, including during retries or a stream. |
| `transport_failure` | No complete final response arrived because of a connection, TLS, or transport failure before a stream began. |
| `canceled` | The caller explicitly canceled the logical operation before a deadline or terminal result. |
| `incomplete_stream` | A stream began but ended without a valid terminal success event and clean response end, unless explicit cancellation or the logical deadline already determined `canceled` or `timeout`. |

Apply the precedence `canceled` (explicit caller action), `timeout` (deadline),
`incomplete_stream`, `transport_failure`, `rejected`, then `success` to make
overlapping observations deterministic. An operation whose outcome is
indeterminate must never be guessed as success; record a coverage fault and
keep its result out of success-rate calculations until reconciled.

The producer must durably record each logical-operation start and its single
terminal classification, then derive cumulative counters from terminal
records without double counting on replay. On restart, unmatched starts must
be reconciled against the logical deadline and any durable terminal receipt;
those still indeterminate after recovery enter `unreconciled_operations`.
The observer must keep `observer_ready=0` while its lifecycle store is
unreadable or recovery has not finished. A process-local counter alone cannot
prove coverage across a crash after response consumption. The approved observer
inventory must also map all intended ingress routes or instrumented client
fleets to an owner; counting only a convenient subset is not a fleet-wide
success rate.

No series may contain request or idempotency IDs, keys, model names, addresses,
payloads, arbitrary error strings, or user identifiers. Private incident
samples follow the normal access and retention controls. The monitoring
service may query this source only after its owner, complete observer inventory,
scrape credentials, class manifest, and failure-domain placement are fixed.

## Coverage and display gate

The independent HA view may show a client-final outcome distribution or rate
only when **every approved observer** is present, `up=1`,
`halro_ha_client_observer_ready=1`, its durable
`halro_ha_client_observer_unreconciled_operations=0`, and every approved class/outcome current
counter has a unique raw scrape no older than the configured freshness bound.
Every observer must have remained ready, with no unreconciled operation and
without a scrape gap, throughout the **entire** displayed window. The same
observer/class/outcome set must have computable window increments,
with reset and duplicate-series behavior explicitly tested. An absent counter
is not zero; a partially upgraded observer or an empty window is unobserved.
Counter increases count logical operations, not retries. Prometheus `increase`
is an estimate and must be labelled as such. A complete set of zero-valued
counters proves only that no final outcomes were recorded in the window; it
does not prove that attempted operations succeeded or that observer coverage
was complete before the window began.

The P0 Service root probe, internal confirmation-wait counter, server HTTP
result distribution, and client-final distribution remain separate panels and
separate evidence sources. Neither server 2xx nor an internal ACK can fill a
missing client-final result. Until the producer and all coverage gates pass,
the HA view must explicitly say that client-final results are unobserved (in
the page's language) and must not publish a client-complete success rate or
SLO from these narrower sources.

## Acceptance before enabling the panel

Use the real supported SDK/ingress retry and completion-receipt configuration
and synthetic, nonbillable operations wherever possible. Preserve observer
logs and private operation IDs only in controlled evidence, then reconcile
their one-to-one terminal records with counter deltas. Compare a separate
ingress traffic census or client-fleet call inventory against durable start
records so omitted callers cannot be hidden by a complete-looking result
series. Exercise at least: success; final 4xx and 503; an initial
`not_primary` followed by success; deadline before and
during a stream; caller cancellation; connection reset before response; 2xx
headers followed by truncated stream; complete streamed success; idempotency
409 after an ambiguous prior attempt; observer restart and counter reset; one
observer disappearing while another stays healthy; and Primary loss. Prove
the producer remains observable in the last two cases. Do not run billable
real-provider smoke tests without explicit authorization.

Record the exact producer version, manifest, target inventory, metric HELP and
labels, scrape times, raw counter samples, and final client receipts. Only
then add the emitted series to the exported metrics reference and configure
the health-page manifest. Target-environment acceptance and RTO/SLO evidence remain
separate from this repository contract.
