# HA health view: repository delivery and deployment boundary

`cmd/halro-ha-health` is an independent, read-only operator view. Run it in the
monitoring failure domain, never inside a Primary Pod. It reads the local
Prometheus HTTP API, evaluates a fixed set of HA signals, and serves a small
page with member status, authenticated-session topology, segmented index
history, current HA or member-scrape alerts, and a sampled firing-alert
timeline from the selected Prometheus range. Timeline boundaries are sample
times, not exact trigger/resolution timestamps or delivery receipts. The
request-impact panel shows server-observed write-route HTTP outcomes. The
optional client-final panel reads a separately accepted observer fleet; without
that integration, it stays unobserved. It has no
promotion, reseed, maintenance, or other mutation endpoint.

The history response also contains at most 200 sampled role, term,
incarnation, maintenance, authenticated peer-session, and replication-phase
changes. The page can show current or historical panels while always keeping
the current health cards visible. The current view does not poll the historical
range; while history is open, current cards refresh every 15 seconds and the
range query runs at most once a minute unless the operator changes the window
or refreshes manually. Current cards render without waiting for a long range
query. History and current alert requests fail independently so one failed
panel cannot erase valid cards or historical evidence. The index chart marks at most 80 recent
sampled change groups; a role change is an observed interval, not proof of
an authorized promotion. Each record gives the previous
sample and first sample with the new value when the gap is at most 45 seconds.
These are observation bounds; a short state between scrapes can be missed.
Conflicting series at one sample time, including duplicate term values and
contradictory 0/1 values for one role or phase, invalidate that field's sampled
change instead of letting query result order choose the apparent transition.
The authenticated `/ha/status` response also carries at most 128 role/term/
promise transitions from the current `StatePublisher` lifetime. Each event is
recorded only after `state.json` publication succeeds and has a local sequence
and publication time. The publisher start time and number dropped by the ring
are included. This memory ring resets on restart, so offline promotion and
transitions before the current process are explicitly outside its coverage.
The page keeps these true current-process transitions separate from sampled
Prometheus changes. Neither is a durable Audit history or promotion evidence.
On a Primary, `/ha/status` also returns the coordinator's process-local
`replicating`/`unavailable` boundary events, with a fixed reason enum,
coordinator start time, and a separate 128-event retention counter. The
current `replication_unavailable` value is captured from the same coordinator
snapshot. A Replica has no Primary coordinator event history; its exported
`replication_state` must not be read as Primary confirmation ability.
On a Replica, `/ha/status` separately returns process-local receive and apply
failure/recovery boundaries. The fixed reason enum contains no underlying
error text; target indexes are attempted work, not durable watermarks. The
128-event ring records state edges only, so an empty ring does not prove that
frames are arriving, applying, or caught up. Cancellation alone is not a
failure boundary. A receiver persistence failure poisons the receiver until
restart; an apply projection failure can transition from `blocked` to `ready`
after a successful retry.
With `-event-journal`, the independent service polls the fixed member status
inventory immediately and every 15 seconds, then atomically fsyncs a private
JSON evidence file. `GET /api/event-archive` and the evidence export expose up
to 1000 retained records across health-service restarts. The archive records
first observation, member collection failure, incarnation or source instance change, ring
overflow and sequence regression as explicit gaps. It deduplicates by member,
source, source start time and local event sequence. Polls cannot recover an
event lost between polls, during a member crash, or before the first poll;
retention eviction is counted. Every poll also reconciles the returned statuses
with the fixed member inventory: a missing, duplicate, or unexpected member
is reported as a partial collection rather than advancing the journal as a
complete observation. This is monitoring-domain evidence, not a
complete durable HA Audit chain. Journal failure is shown as
`journal_write_failed` and does not block member state publication. When
configured, a failed, stale, or partially collected event archive prevents
the overview from showing green; the Primary's HA role and safety decisions
remain independent of this diagnostic store.
`GET /api/latency` uses three fixed, cluster-scoped Prometheus expressions to
show per-member p95 for the Primary durable-to-confirm frame interval, Replica
sink Persist calls, and successful Replica apply batches over five minutes.
An absent quantile stays absent; the page displays “no sufficient samples”
instead of zero. The five-minute window can include data from a member that
subsequently went down, so this panel never overrides the member freshness
assessment. A failed latency query affects this panel without erasing the
core health observations.

`GET /api/impact` shows five-minute server-observed HTTP results for the fixed
HA client write route inventory. It requires all eight current outcome series
from **each** expected member using raw scrape times, then independently
requires all eight five-minute `increase` results from each member before
summing by outcome. Both queries use one millisecond-precision Prometheus
evaluation time; a server timestamp captured before an unpinned query must not
make otherwise complete results appear to come from the future. Missing,
duplicate, or stale series from one member make the panel “unobserved”; another
member's values cannot fill the gap. Partial rollouts show “unobserved” rather
than zero. Its Prometheus `increase` values are estimates,
and retries count separately. A 2xx stream can fail after headers, and this
server-side count does not prove the client received the full response.

### Client-final result boundary

The proposed external producer series, six outcomes, observer inventory,
freshness/coverage gate, and acceptance cases are specified in the
[client-final logical-operation contract](../contracts/ha-client-final-result.md).
This is an integration contract, not a claim that an observer is installed.

The read-only consumer is available behind `-client-final-manifest`. Do not
configure it until the external producer, its full traffic census, retry and
completion-receipt behavior, and target-environment failure tests have an
acceptance record. The manifest is a local operator assertion and is not itself
proof of those tests. It is read at process startup; a changed acceptance
record, class list or observer fleet requires a controlled service restart.
The file resolved by this path must be a nonempty regular file of at most
64 KiB, without group or world write permission. A projected symlink to a
regular ConfigMap file is accepted. The service checks the opened file's
identity and reads at most 64 KiB plus one byte, so a mistaken special or
oversized file cannot bypass the startup bound.
Example:

```json
{
  "version": 1,
  "acceptance_record": "ha-client-final-acceptance-2026-09-28",
  "observers": [
    {"region": "r1", "instance": "client-observer-a", "owner": "gateway-team", "scope": "public-write-routes"}
  ],
  "operation_classes": ["write", "stream"]
}
```

The configured instance/region and every class/outcome pair must be present.
`GET /api/client-final` queries raw `up`, observer-ready,
unreconciled-operation and counter samples over the whole five-minute window
at one fixed Prometheus evaluation time. Four raw queries run together; after
they pass coverage checks, the reset and increase queries run together. Each
series must begin before the
window, end within 30 seconds, have no gap above 30 seconds, and match the
exact manifest and fixed label schema. Every ready, unreconciled and outcome
counter sample must align within one second with that observer's corresponding
raw `up` scrape, including the first and last samples. It then requires
unique per-series `resets(...[5m]) == 0` and `increase(...[5m])` results.
An unknown observer, missing class/outcome, duplicate, extra label, reset,
missing target, failed query, empty window or incomplete coverage returns
`unobserved` with no counts. Without a
manifest the endpoint returns `not_configured`. The page only shows an
estimated outcome distribution for `observed`; it does not derive a
client-complete SLO from a deployment assertion alone. The fixed 30-second
gate assumes a scrape interval no longer than 15 seconds; verify the actual
scrape configuration and clock behavior before enabling the manifest.
The API response and fixed-range evidence export include the deployment's
acceptance-record ID as an operator cross-reference, not as independently
verified proof. The export also records the client-final status and counts, if
observed; raw Prometheus samples and caller receipts remain separate controlled
acceptance evidence.
Prometheus has two optional HA observer alerts: one for an existing target
that is down, and one for a scraped observer whose readiness or unreconciled
gauge does not prove coverage in the same `up` scrape. Neither rule invents
an expected observer when no target is configured. Reconcile the manifest
against the actual Prometheus target inventory and external traffic census
during acceptance; a missing
target configured nowhere remains a deployment gap, not a rule-derived green.

No member-side metric in this repository can prove that an SDK or caller
received and consumed a complete response. An instrumented client must emit
a separate logical operation result **after** its configured retries and, for
streams, after the terminal event/body is consumed. An ingress qualifies only
when it owns those retries and receives an explicit application-level client
completion receipt; a successful socket write is insufficient. Keep the
outcome vocabulary bounded:
`success`, `timeout`, `rejected`, `transport_failure`, `canceled`, and
`incomplete_stream`. A retry attempt is evidence about one attempt, not a
second logical operation. The monitoring series may use environment, region,
cluster, and a fixed operation class; it must not label request IDs, keys,
model names, payloads, addresses, or arbitrary errors. Preserve any sampled
request-level incident evidence behind the normal access controls instead of
placing it in Prometheus labels. The producer must remain observable when the
Primary is down.

This client-final source is a P1 integration and target-environment acceptance
requirement, not a claim made by the current HTTP response counter or required
confirmation barrier. The repository contains the consumer and its strict
coverage gate, but no production observer. Until the source is deployed and
accepted, leave `-client-final-manifest` unset.
An acceptance exercise must follow the real client retry and idempotency
contract, including streaming termination, timeout, and rejection cases;
provider-billable smoke requests require separate explicit authorization.

The page refreshes current health every 15 seconds and keeps its history
request separate from that refresh. A slow history query can still complete
after one or more current refreshes. When no successful current health result
has arrived for more than 30 seconds, all status cards turn unknown while the
last successful query time remains visible. Card expiry uses the browser's
monotonic elapsed time. The response
includes `observed_at` from the health service after it assembles the current
view. Confirmation evidence includes the oldest raw Ledger/metadata counter
sample time and the separate stable-epoch rule sample time. The service checks
member and both required-confirmation source times again
immediately before sending the response, so waiting on another read-only
source cannot leave a once-green result green after its samples expire.
Member age on the page is conservatively measured from that server time
plus monotonic elapsed time since the request began, so an operator
workstation's wall-clock offset cannot make a member appear fresh or stale.
A health response arriving more than 30 seconds after its request began is
discarded instead of reviving old green cards. A missing or invalid
`observed_at` fails the member freshness display closed. The displayed local update time is only a
presentation label, not the freshness source. Opening history or pressing Refresh
starts a new range query; while history stays open, the selected range is
polled at most once a minute using browser monotonic elapsed time, so a local
wall-clock adjustment neither skips the next scheduled query nor causes an
extra scheduled query. History query failure does not clear current
health, and current health failure does not erase previously fetched history.
The index chart omits samples outside JavaScript's exact integer range and
leaves a gap, so a large frame position is never shown as a rounded value.
An isolated valid sample is drawn as a point when adjacent samples are more
than 45 seconds apart; the chart never joins that gap. The horizontal axis
shows its first and last sample times, and narrow screens scroll the chart
instead of compressing its time scale.

`GET /api/evidence?minutes=15|60|360|1440` downloads a JSON snapshot of the
expected inventory, current HA Metrics samples, selected index/alert history,
and current alerts. It uses the same fixed cluster-scoped Prometheus queries as
the page. The file records source and capture time, but is not a signed
forensic record or a promotion recommendation. Treat it as operationally
sensitive and retain it under the incident's normal access controls.

The implementation plan and the distinction between observed health and
promotion eligibility are in [the HA health design](../todo/halro-ha-health-system.zh-CN.md).
The repository can prove its parser, evaluator, alert rules and TLS gate; a
target environment must separately prove routing, identity, member scrapes,
monitoring survival during Primary loss, and alert delivery. This artifact does
not change the HA production-entry gates.

## Inputs and labels

- Prometheus must scrape **every configured HA member**, including Replicas,
  using that member's versioned Metrics credential and mutually authenticated
  TLS. Provision one scrape config per member when credentials differ. Give
  each target an `instance` label equal to the configured `node_id`; set
  `environment`, `region`, `cluster`, and `expected_target="true"` at the
  scrape target. The `cluster` label must equal the member's configured
  `cluster_id`; the view checks both labels against
  `halro_cluster_member_info{cluster_id,node_id}` exported by the member.
  Missing or contradictory self-reported identity cannot yield green.
  A second Prometheus series for the same member and logical metric is also
  treated as a source conflict even when both values match; two scrape targets
  relabelled to one `instance` cannot jointly stand in for one member. For
  indexed, peer and incompatibility metrics, uniqueness is per fixed `kind`,
  `peer` or `reason`; role, incarnation, identity and scalar signals are each
  unique per member.
  A cluster-scoped series with no `instance` label is retained as an
  unidentified anomaly with its raw sample time, never silently discarded or
  counted as a verified extra member. It makes safety unknown until the target
  labels are corrected.
  The health parser also rejects nonbinary boolean gauges, frame positions
  outside the exact integer range, malformed samples, peers absent from the
  fixed inventory, and self-peer links. These observations cannot be hidden
  by another healthy-looking series in the same query.
  A green safety card additionally requires each fresh member to report its
  maintenance, startup-ready, and replication-unavailable gauges; all three
  fixed incompatibility reasons (`schema`, `key_challenge`, `spki`); and one
  peer-connection gauge for every other configured member. Missing series make
  safety unknown. The oldest and newest member-signal sample times must also
  align with that member's latest `up` sample within one second. This prevents
  a metric omitted by the most recent successful scrape from staying green
  merely because its prior sample is less than 30 seconds old. An observed maintenance, unready, unavailable, or incompatible
  member makes it degraded; known identity, term, or index conflicts retain
  critical precedence. `HalroMemberHASignalMissing` checks maintenance, term,
  promised term, each durable/confirmed/applied index, startup-ready,
  replication-unavailable, fixed incompatibility reasons, and peer gauges
  against that member's latest `up` raw sample timestamp; a
  Prometheus instant-vector lookback value cannot fill a missing current
  scrape. It alerts after one minute of incomplete same-scrape inventory and
  compares peer series count with configured target count, while the page
  also checks every peer name against `-members`.
  Relabel the scrape `job` to `halro` so the existing
  `HalroTargetDown` rule covers each **configured** member with `up=0`. A
  member omitted from Prometheus target configuration has no per-member `up`
  series and therefore needs inventory reconciliation; the rule's global
  `absent(up)` fallback has no member/cluster identity. The view selects member
  metrics and `up` only from `job="halro",expected_target="true"`; the HA
  alert and epoch rules apply the same source selector. An unrelated scrape
  with the same environment/cluster/instance cannot satisfy HA member
  coverage, change the historical member graph, suppress a missing-role alert,
  or establish a stable epoch.
- The `-members` argument is the independent expected-member inventory. A
  missing target is unknown, never implicitly removed from the denominator.
  Update the inventory only as part of an approved cluster change.
  Extra instance labels are retained with their sample time. A recent
  successful extra target or recent HA member metric is a current safety
  conflict; an old, failed, or malformed extra sample makes safety unknown.
  Both cases prevent green, and the page names the extra instance and its
  evidence quality.
- The view accepts a loopback Prometheus HTTP(S) origin without credentials,
  or a remote **HTTPS origin only** with `-prometheus-ca`,
  `-prometheus-client-cert` and `-prometheus-client-key`. It pins the remote
  CA, verifies the server name from the URL, presents its client certificate,
  rejects redirects and refuses a group/world-readable or symlinked client
  key; copy a projected Secret key into a private `0600` file first. The key
  and certificate are loaded at startup, so rotate them with a view rollout.
  The view issues fixed read queries; it never forwards the Prometheus API to
  browsers. Keep the remote Service behind mTLS and NetworkPolicy, with
  Prometheus admin, lifecycle and remote-write receiver APIs disabled. Native
  Prometheus mTLS protects the full internal read API; use a reviewed path
  proxy if the deployment needs stricter read-path isolation. The kind split
  example uses the [Prometheus 3.13 web TLS configuration](https://prometheus.io/docs/prometheus/3.13/configuration/https/).
- `-client-url` is optional and must point to the HTTPS **client Service root**,
  not a specific member. An HA member's root response exposes its `role` and
  `cluster_id`; the probe accepts a Primary only when the cluster ID matches
  `-cluster`. A different cluster is critical, and an older or malformed root
  response without the cluster ID cannot turn the card green. The probe retries
  a same-cluster Replica response within a bounded budget and never invokes a
  Provider. This root identity is public operational metadata, not an
  authentication credential. If omitted, the client-entry card stays unknown.
  For a separate, continuous alarm, the repository's independent
  `halro-deadman` supports an optional `halro` target with
  `mode: ha_client_root` and the same cluster-identity/Replica-retry semantics.
  Its persisted down/up event is separate from this page's single observation;
  neither route probe proves a client write or completed streamed response.

The event archive retains only transitions it actually collected. It cannot
close a crash gap between durable member-state publication and the current
process-local event ring. The proposed protocol and failure-injection gate for
that missing durable source are in
[HA durable transition evidence contract](../contracts/ha-transition-durability.md).
Version-3 members also expose `GET /ha/transitions` on the same machine-only
Metrics listener, with the same mTLS and separate HA bearer as `/ha/status`.
It returns at most 64 committed records per page and requires the returned
sequence, digest, and journal ID for continuation; private journal MACs are
not returned. Version-2 members return unavailable. With
`-durable-transition-journal`, the independent health service ingests and
persists authenticated pages separately from the poll-limited event archive.
The history view and `/api/durable-transitions` distinguish catch-up, stale,
unavailable and file failures per member. The private cursor manifest and
consecutively numbered closed segments retain all accepted pages for each
recorded member journal. A same-incarnation reseed changes the member journal
ID: the collector retains the old cursor and reports
`journal_changed_requires_reconciliation` after confirming the new authenticated
baseline. It does not accept a handoff automatically or claim the new chain is complete;
the [transition evidence contract](../contracts/ha-transition-durability.md#same-incarnation-member-reseed-is-a-separate-evidence-handoff)
defines the required offline recovery. After the retired member's final
authenticated head is fully imported, `-preflight-reseed-handoff` checks the
frozen old member, source Primary, approved seed and replacement baseline.
With the collector stopped, `-commit-reseed-handoff` atomically records the
old chain, the handoff and the new baseline in collector format 3. The
collector then resumes polling that new journal from sequence zero; absent an
approved record, it still refuses the switch. The collector rotates at a 3 MiB target, with a 4 MiB hard limit
per document; an oversized page or manifest still stops advancement. It does
not export to immutable storage yet. Member journals rotate
private 64 MiB segments and retain every old segment without archival
deletion. A traditional migration baseline cannot prove any earlier
transitions, and “caught up” refers only
to the last observed committed head, not the current instant. When this
optional source is configured, incomplete coverage prevents a green overall
health verdict; it does not turn a current member into a promotion candidate.
The API and history view show at most the latest 20 transition tuples per
member, without continuity digests; the private file keeps the verified chain.

For each member, add a scrape job with its own bearer-token and mTLS files to
the existing Prometheus configuration. For example, the first of three jobs:

```yaml
- job_name: halro-0-scrape
  metrics_path: /metrics
  scheme: https
  authorization:
    type: Bearer
    credentials_file: /run/secrets/halro-0-metrics-token
  tls_config:
    ca_file: /run/secrets/halro-metrics-ca.crt
    cert_file: /run/secrets/prometheus-client.crt
    key_file: /run/secrets/prometheus-client.key
    server_name: halro-0.metrics.internal
  static_configs:
    - targets: [halro-0.metrics.internal:9090]
      labels:
        instance: halro-0
        environment: production
        region: primary-region
        cluster: production-a
        expected_target: "true"
  relabel_configs:
    - target_label: job
      replacement: halro
```

Repeat with the other node IDs, addresses and credential files. Retain the
existing Prometheus and Alertmanager self-scrapes and rule files. The old
single-target `prometheus.yml` is a Standalone example, not an HA inventory.
Production tokens, certificates and target addresses belong in the target
Secret store; no value is shipped in this repository.

For the repository's Kubernetes HA example, use each Pod's stable headless
address as its own target, such as
`halro-0.halro-members.halro.svc.cluster.local:9090`, and set `tls_config.server_name`
to the same DNS name covered by that member's Metrics server certificate.
The example above uses a deployment-specific `metrics.internal` DNS instead;
do not mix its address with a certificate issued only for the Kubernetes DNS.
The headless Service includes NotReady addresses, so member loss remains
visible as an individual scrape failure rather than disappearing from the
target inventory. The optional
[`halro-ha-observability-ingress.example.yaml`](../../deploy/kubernetes/halro-ha-observability-ingress.example.yaml)
only permits matching monitoring Pod/namespace labels through NetworkPolicy;
it grants both monitoring workloads member 9090 access and only the health
service member Gateway 8080 access for the client Service root and optional
direct live/ready probes. Policies are additive, so other policies selecting
the members must also be audited. NetworkPolicy does not restrict Gateway
HTTP paths; do not give the health-service Pod write credentials. The target
deployment must bind those labels
to reviewed workloads, configure
monitoring egress, and prove both mTLS paths and the two independent bearer
credentials. Neither the Service nor the policy deploys Prometheus or the
independent health service.

## Start the independent view

Provision a server certificate, its private key, and a CA that signs the
client certificate used by the authenticated operator proxy. The proxy must
authenticate and audit each human user, authorize read-only access to `/`,
`/app.js`, and `/api/*`, and authenticate itself to this service with mTLS.
Alternatively an operator can access it directly with an individual client
certificate, subject to the same access audit requirement. Keep the listener
on loopback unless the target's network policy and identity boundary have been
reviewed.

```sh
make ha-health
./bin/halro-ha-health \
  -environment production -cluster production-a \
  -members halro-0,halro-1,halro-2 \
  -prometheus http://127.0.0.1:9091 \
  -listen 127.0.0.1:9105 \
  -tls-cert /run/secrets/ha-health-server.crt \
  -tls-key /run/secrets/ha-health-server.key \
  -client-ca /run/secrets/operator-proxy-ca.crt \
  -client-url https://halro-client.internal/ \
  -probe-ca /run/secrets/halro-gateway-ca.crt \
  -member-status-manifest /run/secrets/ha-member-status.json \
  -runbook-base-url https://docs.internal/ \
  -event-journal /var/lib/halro-ha-health/events.json
```

The versioned `halro-<os>-<arch>.tar.gz` release archive also carries
`halro-ha-health` with the same build identity as `halro` and
`halro-deadman`, plus a copy of this deployment guide. Install it on the
independent monitoring host and verify
`halro-ha-health -version` against the archive's release identity before
starting it. The source build above is useful for a local rehearsal; the
archive build is covered by the release workflow and CI build check.

For a controlled version-3 journal trial, add
`-durable-transition-journal /var/lib/halro-ha-health/durable-transitions.json`
and mount a private persistent directory without group/world write access.
The file and its lock have mode `0600`; a second service cannot own the same
file. Startup rejects a changed environment, cluster, member inventory,
missing closed segment or inconsistent stored chain. It verifies all closed
segments and the current manifest, then revalidates member pages before
reporting any member as caught up. Closed files use the suffix
`.segment.00000000000000000000`, incrementing with each rollover, and must be
backed up together with the manifest using a stopped service or an atomic
filesystem snapshot; a live file-by-file copy does not establish a consistent
generation. A prior unsegmented file is upgraded on
startup. Each document is limited to 4 MiB; all old segments are retained.
`/api/durable-transitions` and evidence export include local closed-segment
count, current manifest bytes and total retained file bytes; the history page
shows these as capacity planning evidence. They do not report volume free
space or prove an external archive. Monitor the persistent volume's free bytes
and inodes independently, and rehearse startup replay at the planned
retention size.
Do not enable this as a production retention guarantee until capacity,
immutable-sink and target-environment recovery gates in the durability
contract pass.

For an offline, read-only check of a stopped-service copy or atomic snapshot,
run `halro-ha-health -verify-durable-snapshot <absolute-manifest-path>
-environment <environment> -cluster <cluster> -members <complete-list>`.
It requires no serving TLS or Prometheus configuration. The result reports
`local_files_verified`, ordered file hashes and a deterministic inventory
root, plus each member's stored cursor and any missing baseline. Preserve the
report outside the collector's failure domain and compare it with a later
verification of the archived copy. The command refuses a held collector lock
but cannot make an ordinary live directory copy atomic; see the
[operations runbook](../runbooks/ha-operations.md#frozen-independent-collector-evidence-check).

The report also contains each chain's last observed committed head digest
and time. Add `-compare-member-snapshot-reports <private-manifest-path>` to
compare its current member heads and committed event-sequence hashes with the
separately authenticated member snapshot reports; this mode emits
`snapshot_heads_match` only when every expected member's stored cursor and
event sequence equal its frozen member report. The
comparison cannot authenticate report files on its own or replace immutable
archive receipts; the [member runbook](../runbooks/ha-operations.md#frozen-member-transition-evidence-check)
specifies the required archived-byte re-verification.

When matching configs and Master Keys are available on a secure verification
host, use `-verify-member-snapshot-manifest <private-manifest-path>` instead.
It verifies each member's frozen files and full MAC journal before comparing
the current incarnation's head and every retained collector event tuple to
the authenticated member projection. It emits
`member_authenticated_current_chain_match` with the verified member
inventories and event hashes. Earlier incarnations need their own frozen
member copies; this result does not establish a simultaneous cross-host
freeze or external immutable retention.

The same flag accepts a version-2 manifest with one entry per collected
`(node_id, incarnation)` to authenticate all retained collector chains. It
emits `member_authenticated_all_collected_chains_match` only when every
expected member and every collected incarnation is covered; the result still
starts at each member's authenticated baseline and needs external receipts.
When a reseed creates another journal in the same incarnation, use a version-3
manifest with one entry per `(node_id, incarnation, journal_id)`. It verifies
both generations and checks their frozen member inventories against the
recorded handoff. The local kind exercise covered all four retained chains;
it did not create an independent immutable archive receipt.

`-runbook-base-url` is optional. Set it to the HTTPS root of an operator-only
documentation origin that serves `/docs/runbooks/ha-operations.md` and
`/docs/observability/operations-runbook.md` under the same authentication and
audit boundary as the HA view. The origin must be a root URL without embedded
credentials, query, or fragment. With no configured origin, the page displays
the repository runbook path as text and does not create an external link.
Alert annotations become links only for the fixed operations runbook path and
an alphanumeric fragment; arbitrary annotation URLs remain text. Validate the
documentation route and access policy in the target environment before using
its links for incident response.

The service does not hold an Admin password, session Cookie, TOTP secret, or
member Metrics token. Prometheus owns scrape credentials and their rotation.
The optional machine status manifest names every expected member exactly once.
Each authenticated member response must name every other member from that
inventory exactly once in `peers[]`. Missing, duplicate, self, or unconfigured
peers yield `peer_inventory`; their connection values are not used to infer
remote replication progress.

Fresh Metrics and authenticated machine status for one member are compared for
incarnation, role, term, promised term, startup readiness, and replication
unavailability. A disagreement marks `metrics_disagreement` and prevents a
green safety or overall state. These are asynchronous observations, so a
switching interval is unknown pending investigation rather than proof of a
split brain.

Example manifest:

```json
{
  "members": [
    {"node_id":"halro-0","url":"https://halro-0.metrics.internal:9090/ha/status","live_url":"https://halro-0.metrics.internal:8443/health/live","ready_url":"https://halro-0.metrics.internal:8443/health/ready","token_file":"/run/secrets/halro-0-status-token","ca_file":"/run/secrets/halro-metrics-ca.crt","cert_file":"/run/secrets/health-collector.crt","key_file":"/run/secrets/health-collector.key","require_prefix_digest":true,"require_live_transitions":true,"require_availability_transitions":true,"require_replica_stage_transitions":true},
    {"node_id":"halro-1","url":"https://halro-1.metrics.internal:9090/ha/status","live_url":"https://halro-1.metrics.internal:8443/health/live","ready_url":"https://halro-1.metrics.internal:8443/health/ready","token_file":"/run/secrets/halro-1-status-token","ca_file":"/run/secrets/halro-metrics-ca.crt","cert_file":"/run/secrets/health-collector.crt","key_file":"/run/secrets/health-collector.key","require_prefix_digest":true,"require_live_transitions":true,"require_availability_transitions":true,"require_replica_stage_transitions":true},
    {"node_id":"halro-2","url":"https://halro-2.metrics.internal:9090/ha/status","live_url":"https://halro-2.metrics.internal:8443/health/live","ready_url":"https://halro-2.metrics.internal:8443/health/ready","token_file":"/run/secrets/halro-2-status-token","ca_file":"/run/secrets/halro-metrics-ca.crt","cert_file":"/run/secrets/health-collector.crt","key_file":"/run/secrets/health-collector.key","require_prefix_digest":true,"require_live_transitions":true,"require_availability_transitions":true,"require_replica_stage_transitions":true}
  ]
}
```

The `live_url` and `ready_url` entries are optional as a pair. They must use
the same member hostname as `/ha/status`, fixed HTTPS health paths, and a
certificate trusted by that entry's `ca_file`; a different port is allowed.
The collector sends its mTLS client certificate but never the HA status bearer
to these health paths, and never follows redirects. HTTP 200 and 503 become
true and false respectively; any other status or transport error is unknown
and, when the pair is configured, prevents a green overview. A Primary that
explicitly returns 503 from `/health/ready` degrades the overview. Replica
readiness is shown as a local fact because a lagging Replica may legitimately
be not ready while the Primary continues to serve. The URLs must reach each
member directly and must not point to a load-balanced client Service. Their
results remain visible even if `/ha/status` authentication fails, and they do
not replace the separate client-Service probe.
The collector owns `source`, `sampled_at`, `error`, `health_probe_error`,
`health_live`, and `health_ready`; values with those names in a member's JSON
response are discarded before the collector stamps its own observations.

`require_live_transitions` requires the current-process publisher ring on
every member. `require_availability_transitions` applies when that member
currently reports Primary; a Replica has no Primary coordinator ring. Missing
or malformed required evidence marks that member's machine status unknown.
`require_replica_stage_transitions` applies when that member reports Replica;
a Primary has no Replica receiver ring.
The event journal requires this exact member status manifest with both
`require_live_transitions`, `require_availability_transitions`, and
`require_replica_stage_transitions` enabled on
every entry. Put its file on
a persistent volume outside every HA member's failure domain. The parent
directory must not be writable by group or others; the file is mode `0600`.
The service holds an exclusive lock in the adjacent `events.json.lock` file;
a second service targeting the same journal refuses to start. Keep both files
on the same persistent volume and do not remove the lock file while running.
At startup, an existing file with a different environment, cluster, member
inventory, schema version, unsafe permissions, or malformed JSON is rejected
instead of silently reset. Include the volume in backup/retention policy and
exercise health-service restart, member restart, scrape outage, ring overflow,
disk write failure and retention eviction in the target environment. The
operator page shows archive freshness and gaps independently from current
member health. Disabling the flag disables background event collection.

On each HA member, enable a distinct machine status credential alongside
authenticated Metrics mTLS:

```yaml
metrics:
  enabled: true
  require_auth: true
  credential_file: /run/secrets/halro-0-metrics-credentials.json
  ha_status:
    enabled: true
    credential_file: /run/secrets/halro-0-status-credentials.json
    include_prefix_digest: true
  tls:
    enabled: true
    cert_file: /run/secrets/halro-0-metrics.crt
    key_file: /run/secrets/halro-0-metrics.key
    client_ca_file: /run/secrets/monitoring-clients-ca.crt
```

Set the paths for each node independently. The `ha_status` file must differ
from Metrics and audit-anchor credential files.
`include_prefix_digest` is off by default. Enable it only for the restricted
machine status scope, and set `require_prefix_digest` in the collector manifest
for every member that must supply that evidence. The endpoint hashes the
authenticated ordering head MAC; it never sends the MAC or cluster key.
The collector compares digests only at the same cluster, incarnation, and
durable index. A mismatch is critical and marks every member observed at that
position as conflicting, including a third member whose digest matches one
side of the disagreement. Matching digests alone do not authorize promotion.
Run `halro ha-status rotate --config <member-config>` against the writable
credential source; publish its credential file plus `.audit` and
`.revocations` sidecars as one versioned Secret set to the member. The member
credential source must survive restarts; copying only the one-time bearer
token would lose rotation and revocation state. Copy the one-time stdout token
into that member's collector Secret file with owner-only permissions. Use
`halro ha-status list`, `verify-audit`, and `revoke --version N` against the
writable source for the credential lifecycle. Roll out a revoke to the member
before treating it as effective, then verify the old token is denied. The collector rereads the
token and client certificate on each request; a CA change requires restarting
the collector because its trust pool is loaded at startup. Member status requests use a
fixed path, four-second timeout, TLS hostname verification, and no redirects.

On Kubernetes, a projected Secret with `defaultMode: 0440` does not satisfy the
collector's owner-only token-file check. Keep the projection out of the
collector container. A dedicated process running as the collector's non-root
UID can read that projection via its assigned volume group and atomically copy
each token to a private memory-backed `emptyDir` file with mode `0600`; mount
only that private directory into the collector. Synchronize projection changes
without logging token contents, remove a private copy if its source disappears,
and verify rotation and revocation from the collector API. Do not broaden the
token-file permission check to accept group-readable Secrets. The local kind
exercise uses this layout and has verified a member-side v2 rotation, collector
Secret propagation, permanent v1 revocation, and the resulting 401/200
application responses. This local result does not verify a production Secret
controller, long-lived certificate rotation, or another cluster's CNI.

Credential, transport, HTTP, and identity errors are shown as unknown for that
member; they do not silently become a zero index or a green overall view.
If status collection is omitted, the page explicitly says "not configured".
An inaccessible Prometheus API or failed scrape produces unknown cards. A
successful client root probe only proves Service routing to a Primary; it
does **not** prove a billable request or a newly confirmed write. The
confirmation card needs a recent successful required Ledger/metadata
confirmation barrier on the current Primary within a stable epoch; on an idle cluster
it remains unknown. `halro_replication_unavailable=0` never turns it green by
itself. A healthy confirmation conclusion first requires every configured
member's role to be known and exactly one observed Primary. If Replica
coverage is missing but a fresh, identity-consistent, unambiguous Primary
reports `replication_unavailable=1`, the card shows degraded for that known
block while safety and catchup remain unknown. A second observed Primary,
conflicted scrape, wrong identity, or missing role on an observed member keeps
the partial-coverage confirmation conclusion unknown. Both `ledger` and
`metadata` confirmed-outcome counters must have a
unique raw scrape sample no older than 30 seconds on the observed Primary.
The five-minute increase is ignored if either counter reset in that window,
or if the stable-epoch recording rule lacks one fresh raw sample for that
Primary. Thus an old process's success or a stopped rule evaluation cannot
satisfy a newly started Primary. `/api/health`
includes `confirmation_evidence` with the fixed query
source, candidate status, Primary instance, Prometheus five-minute estimated
increase, oldest current counter scrape time and separate query evaluation
time when one fresh candidate exists. Multiple candidate
series make the evidence ambiguous and cannot turn the card green. The card
links to this detail; it does not use an index curve as proof of a required
write confirmation. The overall card also reads this cluster's current Prometheus HA and
member-scrape alerts: a firing critical/warning alert prevents green, and a
failed alert query makes a would-be green overall state unknown. This is
independent of whether Alertmanager delivered a notification.

The safety card evaluates current contradictions only from members whose
oldest returned **raw scrape sample** is within the 30-second freshness window.
The service reads the last sample from a fixed 45-second Prometheus range
vector for each current HA metric and `up`; an instant query's evaluation
timestamp is not a scrape timestamp. The exported current evidence retains
these raw sample times. A series that has stopped updating cannot become fresh
merely because Prometheus still returns its last value during lookback. Missing
role observations yield unknown rather than a fabricated no-Primary finding.
The `HalroNoPrimary` rule likewise requires every configured Halro target to
have a successful `up` sample and exactly one role observation; target and role
gaps must be investigated through the independent member inventory,
`HalroTargetDown` for configured failed targets, and
`HalroMemberRoleMissing` for successfully scraped members, while duplicate
roles fire `HalroMemberRoleAmbiguous`. The role, identity and incarnation
alert rules compare each raw series sample time to its own target's successful
`up` sample time before counting it. Prometheus lookback can otherwise retain
an old Primary, role or incarnation after the next scrape has omitted or
changed it; those old samples are not current evidence. A second set of HA
safety and progress alerts applies the same gate to member
identity mismatch, index ordering, promised term, startup adjudication,
replication block, peer connectivity, confirmation/apply stalls and peer
incompatibility. The index-regression alert keeps its five-minute historical
comparison but requires the index, term and exactly one incarnation in the
current member scrape before interpreting that history. The no-connected-peer
alert also requires a complete current
peer gauge count; a missing gauge is handled as missing signal coverage.
A five-minute epoch record additionally requires term and incarnation raw
sample counts to equal the same member's `up` count over that window. The
record remains absent after a selective omission until that gap ages out;
the health service reads its raw record time and treats an absent or stale
record as unknown confirmation evidence. A Prometheus-wide scrape outage
can leave all three source counts equally sparse; the independent
monitoring-availability probe remains necessary.
A fresh index or identity contradiction is still surfaced even if another
member's observation is missing.
The catch-up card likewise checks progress, term, maintenance,
incompatibility and startup evidence for **every** observed Replica before
using one matching Replica to report observed catch-up. If another Replica's
required progress evidence is missing, the catch-up card remains unknown;
matching numeric indexes still do not authenticate a promotion prefix.

The current-alert detail reads the alert's evaluated vector value from
`/api/v1/alerts` and fetches the single matching rule in the loaded
`halro-alerts` group from `/api/v1/rules` on demand. It shows the current
expression, `for` duration, rule health, and last evaluation time when the
API provides them, alongside the latest member samples and their timestamps.
A missing, duplicate, invalid, or unavailable rule is explicitly marked as
such. These sources can have different collection times; the vector value is
not a raw metric sample, the currently loaded expression may differ from the
one active when the alert began, and the member samples do not reconstruct
the original trigger. Target acceptance must check the deployed rule group,
Prometheus API version, and actual alert-to-sample correlation.

## Acceptance and limits

Run the focused repository checks after rule or view changes:

```sh
GOCACHE=/tmp/halro-go-cache go test -count=1 ./internal/hahealth/ ./cmd/halro-ha-health/
GOCACHE=/tmp/halro-go-cache go test -count=1 ./internal/app/ -run 'TestGatewayRootIdentifiesHARoleForNonBillableServiceProbe|TestCapabilityMetrics'
./deploy/observability/validate.sh
```

Execute the [HA health target-environment matrix](../verification/ha-test-guide.zh-CN.md#4-ha-健康系统目标环境验收)
against the frozen candidate and inventory. It records the independent view
during Primary loss and manual promotion, member/source failures, identity and
Secret rotation, journal persistence, rule delivery, and restoration with
timestamped evidence and explicit pass/fail outcomes. A green repository test
or screenshot is not target acceptance.
The page does not consume cross-node authenticated ordering heads, so matching
index numbers are **not** a promotion decision. The browser is intentionally
shown a fixed read-only API, not a general PromQL console.
