# Observability operations runbook

Status: Phase A1 baseline
Owner: SRE

## First response

1. Confirm the expected target exists and inspect Prometheus `up` and scrape
   error without printing authorization headers.
2. Check Halro `/health/live` and `/health/ready` through their approved
   paths.
3. Check WAL errors and Ledger queue before treating Usage values as
   authoritative.
4. Compare request, attempt, fallback, deployment health, and capacity pressure
   to distinguish upstream degradation from gateway saturation.
5. Check Prometheus rule evaluation, Alertmanager notification, TSDB disk, and
   the independent dead-man monitor.
6. For AWS KMS/Key Slot alerts, follow the
   [M11 production operations runbook](../runbooks/m11-production-operations.md)
   and correlate the bounded Halro Audit `correlation_id` with CloudTrail.

Use the authenticated, audited Prometheus query path to inspect `up`, the
`halro:*` recording rules and the source `halro_*` series. Do not enable
a public Prometheus UI or unrestricted API. Useful checks include:

- target health: `up{job="halro"}`;
- request/error traffic: `halro:requests:rate5m` and
  `halro:requests_errors:ratio5m`;
- tail latency: `histogram_quantile(0.95, sum by (le, environment, region,
  cluster) (rate(halro_request_latency_seconds_bucket[5m])))`;
- accounting queues: `halro:ledger_queue:ratio` and
  `halro_usage_analytics_lagging`;
- provider pressure: `halro:deployments_unhealthy:count` and
  `halro:deployment_capacity:ratio`.

## HA client-final panel

The independent HA page keeps Service reachability, the Primary's internal
confirmation barrier, server HTTP outcomes, and client-final logical
operations separate. `GET /api/client-final` returns `not_configured` until an
external caller/ingress producer has passed the
[client-final acceptance contract](../contracts/ha-client-final-result.md) and
its manifest is enabled. A server 2xx or confirmed replication index cannot
fill this gap.

- For `query_failed`, inspect the approved Prometheus query path and target
  error; preserve the query time and raw response before retrying.
- For `coverage_incomplete`, compare every manifest observer, region and
  operation class with the `halro-client-observer` target inventory. Check raw
  `up`, ready, unreconciled-operation and all six result counters at the same
  scrape times across the full five-minute window. Inspect reset and duplicate
  series evidence. Do not remove an unavailable observer or outcome from the
  manifest to make the panel appear healthy.
- For `empty_window`, record that no terminal client operation was observed in
  the window. Do not interpret this as a 100% success interval; compare the
  separate ingress traffic census and caller receipts.
- For `observed`, preserve the deployment acceptance-record ID, exported
  estimated counts, raw Prometheus samples and private caller receipts under
  the incident's evidence controls. Confirm that the approved route/fleet
  mapping still covers current traffic before using the result operationally.

The manifest is loaded at service startup. Change it only after the observer
inventory and acceptance evidence have been reviewed, then restart the
independent view under the normal deployment procedure.

## Alert procedures

### HalroNoPrimary

- Trigger: all configured `up{job="halro",expected_target="true"}` members in one environment, region, and cluster scrape successfully, each has exactly one role observation **from that same scrape**, and none reports Primary for two minutes. If a target or current-scrape role is missing, treat Primary status as unknown and investigate target/identity collection alerts first.
- Immediate: stop client writes at the load balancer, compare each member's authenticated `cluster status`, term, promised term, durable/applied index, and replication journal head. Do not start two candidates or edit `cluster/state.json`.
- Recover: choose the most up-to-date stopped Replica, fence the old Primary outside Halro, and run the documented `halro cluster promote` ceremony with peer promises. Restore client routing only after exactly one Primary reaches ready and a Replica confirms its leadership frame.
- Escalate: if no candidate has the confirmed prefix or fencing cannot be proven, keep the cluster unavailable and involve the incident commander and data owner.

The optional independent `halro-deadman` `ha_client_root` target reports
client Service routing separately from `HalroNoPrimary`. A single Replica
route is normal; repeated checks that never reach a same-cluster Primary,
an unidentified route, transport failure or a wrong cluster enter its
persisted down/up alarm path. Check the target ID and reason code against the
client Service endpoints and the health page before inferring member loss.
This root probe does not perform a write or establish client-final success.

### HalroMultiplePrimaries

- Trigger: more than one member in the same environment, region, and cluster reports Primary in its current successful scrape; this fires immediately.
- Immediate: remove all client traffic, isolate replication and client networks, preserve all data directories and process logs, and identify the highest term. Do not let either side accept another write and do not choose by wall-clock time.
- Recover: treat this as a safety incident. Fence every lower-term or unproven member externally, compare authenticated state and journals, then retain only the member whose term and confirmed prefix are justified by durable promises. Manual reconciliation is required before restarting replicas.
- Escalate: page SRE and Security immediately; archive the conflicting state, promise records, audit evidence, and fencing proof.

### HalroMemberIncarnationConflict

- Trigger: members under one environment, region, and cluster report more than one incarnation in their current successful scrapes; this fires immediately.
- Immediate: stop routing writes, verify each target's `instance` and scrape configuration, then compare the authenticated cluster status and state files. A stale or duplicated scrape target must be ruled out before interpreting the conflict as a runtime split.
- Recover: follow the cluster's approved reconfiguration or recovery procedure. Never merge data directories or select an incarnation from the chart alone.

### HalroMemberIdentityMissing

- Trigger: a configured HA target is scraped successfully but does not emit `halro_cluster_member_info` in that scrape for one minute. A prior sample still visible through Prometheus lookback does not count as current identity evidence.
- Immediate: check the member version, HA mode, metric exposition, and target labels. Keep the health conclusion unknown until the application itself reports its identity.
- Recover: repair the scrape or upgrade the member; do not fill the absent identity from the Prometheus target label.

### HalroMemberRoleMissing

- Trigger: a configured HA target is scraped successfully but emits no `halro_cluster_role` observation in that scrape for one minute. A prior role sample still visible through Prometheus lookback does not count as current role evidence.
- Immediate: check scrape freshness, member version, HA mode, and Metrics exposition. Treat the member's role as unknown even if another member appears to be Primary; preserve the raw samples and compare authenticated member status.
- Recover: repair the exporter or target inventory. Do not infer a Replica role from the absence of a Primary role metric.

### HalroMemberRoleAmbiguous

- Trigger: one configured member exposes more than one HA role series in the same successful scrape; this fires immediately. Historical role series from earlier scrapes do not establish a conflict.
- Immediate: stop using that member's role in safety or promotion decisions. Preserve the raw series with labels and sample times, then compare authenticated member status and scrape relabeling; a duplicate target or exporter bug can fabricate a role conflict.
- Recover: correct the member or scrape configuration and verify exactly one role series remains. Do not select one role from the chart by hand.

### HalroMemberHASignalMissing

- Trigger: a configured member is scraped successfully for one minute but its maintenance, term, promised term, durable/confirmed/applied index, startup-ready, replication-unavailable, three fixed incompatibility-reason series, or expected peer-connection series are absent from the **same raw scrape as `up`**. Each index kind must appear exactly once; the peer count is compared with the configured target count minus one. An instant-vector lookup can show an older value during Prometheus's lookback period; compare `timestamp(metric)` with `timestamp(up)` or inspect raw range samples before declaring the current scrape complete.
- Immediate: open that node's HA health detail to identify the missing signal, then compare the raw `/metrics` response with Prometheus target and metric relabeling. Keep the safety conclusion unknown unless another fresh observation already proves a stronger condition. A zero value from another member cannot replace a missing series.
- Recover: repair the exporter, rollout version, target inventory, or relabeling, and verify that every configured member again emits the full signal set. A matching peer count alone does not prove the peer labels are correct; reconcile their names with the deployment inventory.

### HalroMemberDuplicateScrapeSource

- Trigger: more than one `up{job="halro",expected_target="true"}` series claims the same environment, region, cluster, and `instance`. This rule has no HA vector joins and fires immediately. HA rules normalize the same-member `up` sample timestamp before matching raw signals; the shared stable-epoch record requires one logical term, incarnation, and `up` source over its five-minute window.
- Immediate: treat this member's role, index, and identity observations as ambiguous. Preserve both complete target label sets and raw sample times; compare the Prometheus target inventory, service discovery, and relabeling with the fixed member list. The health page must mark the member conflicted and keep safety and overall status non-green. Do not infer two running members or a split-brain from duplicate scrape series alone.
- Recover: remove the extra target or label source, then verify the health page's 45-second raw window again contains exactly one source per logical member. The duplicate alert can remain active until the old `up` series leaves Prometheus's instant-vector lookback; its resolution and the stable-epoch record may take longer than the page recovery. Check that every HA alert and recording rule remains `ok` throughout. Any rule error is an observability failure, not a healthy cluster observation.

### HalroTransitionJournalCapacityUnreadable

- Trigger: a member freshly reports `halro_replication_member_state_version=3` while its same-scrape capacity-readable sample is zero or missing for one minute. V2 members omit capacity series and do not trigger this rule. A failed scrape uses the target-down procedure.
- Immediate: preserve the member data directory and check its active `transitions.journal` segment and local storage errors. The bytes and segment gauges are withheld while accounting is inconsistent; do not replace them with zero or delete old segments to silence the alert.
- Recover: authenticate a frozen member snapshot using the [HA operations procedure](../runbooks/ha-operations.md#frozen-member-transition-evidence-check), repair only through an approved offline recovery path, then verify that the capacity gauge returns to 1 and the retained file inventory matches. Check volume free bytes and inodes separately.

### HalroMemberIdentityMismatch

- Trigger: a member's `node_id` or `cluster_id` from its **current successful scrape** differs from the target's `instance` or `cluster` label; this fires immediately. An earlier wrong identity retained by Prometheus lookback is not a current mismatch.
- Immediate: stop using this target in HA decisions, verify the actual member configuration and scrape inventory, and preserve the raw identity sample. A swapped or mislabeled target can make other observations appear to belong to the wrong node.
- Recover: correct the target inventory or the member configuration through the approved HA procedure; never relabel a conflicting member merely to make the alert green.

### HalroReplicationIndexOrderingInvalid

- Trigger: a member reports `durable < confirmed` or `confirmed < applied` with both operands from the same successful scrape; this fires immediately. If an operand is missing, inspect HA signal coverage instead of comparing it with a retained old sample.
- Immediate: preserve the raw scrape, authenticated status, state file, and journal evidence. Remove that member from operational decisions until the discrepancy is explained.
- Recover: use the HA recovery runbook; do not edit a watermark or advance a local state file by hand.

### HalroPromisedTermInvalid

- Trigger: one member reports `promised_term < term` in the same successful scrape; this fires immediately because its durable promise cannot be below the term it says it occupies.
- Immediate: preserve the raw Metrics scrape and authenticated member status, then compare the persisted state and journal under the approved incident procedure. Exclude this member from candidate decisions while the contradiction is unresolved.
- Recover: investigate stale or mislabeled scrape data first, then storage or state publication failure. Do not edit the promise or term by hand to silence the alert.

### HalroReplicationIndexRegressed

- Trigger: one local index moves backward inside a five-minute stable term/incarnation window, and the index, term and single incarnation are still present in the member's current successful scrape; the shared epoch rule excludes expected term/incarnation transitions. If a current signal is missing, investigate coverage before asserting an ongoing regression.
- Immediate: preserve the member's raw samples, restart history, state file, and journal. Compare authenticated state before any promotion or reseed decision.
- Recover: investigate storage rollback or incorrect state restoration; do not treat the lower index as a clean new baseline without an approved new incarnation.

### HalroAwaitingOperator

- Trigger: a member reports Primary role and incomplete startup adjudication in the same successful scrape for two minutes.
- Immediate: inspect peer connectivity, certificate identity, cluster/incarnation equality, term, promised term, and the `leadership_established` confirmation index. A 503 from readiness is expected while the gate is closed.
- Recover: restore the peer path or carry out the documented fencing and promotion ceremony. Never bypass the startup gate by changing the state file.
- Escalate: keep client traffic disabled if the peer presents a conflicting term, role, incarnation, or node identity.

### HalroReplicationUnavailable

- Trigger: the member's current successful scrape reports both Primary role and an internal replication block for one minute. This signal is not a positive confirmation test: zero means no known internal block, not that a new frame would receive an ACK.
- Immediate: check the authenticated peer connection, Replica apply status, disk sync errors, and confirmation/apply lag; do not retry an ambiguous client operation without its idempotency key.
- Recover: restore the existing Replica and let it catch up from the Primary journal. If it cannot be recovered, use a separately approved reseed procedure; do not promote an out-of-date Replica merely to restore availability.
- Escalate: if the Primary is also unhealthy, freeze client writes and begin the manual promotion runbook only after external fencing.

### HalroReplicationNoConnectedPeer

- Trigger: the current Primary scrape reports every configured peer connection gauge, all zero, for one minute. A missing peer gauge makes this particular conclusion unknown and triggers `HalroMemberHASignalMissing` instead. Zero current connections are strong negative evidence for new replicated confirmations, but a connected session does not prove ACK or apply progress.
- Immediate: check each configured peer's process, replication transport, certificate, and Metrics scrape freshness. In a two-member cluster, required writes cannot regain availability until the only peer returns.
- Recover: restore a valid authenticated peer and verify an actual confirmation before declaring the cluster fully healthy.

### HalroReplicationConfirmationStalled

- Trigger: the Primary has a **current-scrape** durable-to-confirmed backlog and confirmed index, and that index has not advanced in five minutes in one observed term/incarnation, sustained for one further minute. The shared `halro:ha_epoch_stable:bool` rule suppresses cross-epoch comparisons.
- Immediate: inspect the peer ACK path and each member's authenticated index. A small, persistent backlog still needs investigation; index count is not a time or byte lag measure.
- Recover: restore the confirmation path and observe the confirmed index advance. Do not infer success merely from `halro_replication_unavailable == 0`.

### HalroReplicaApplyStalled

- Trigger: the current Replica scrape reports an unapplied backlog and applied index, and that index has not advanced for five minutes in one observed term/incarnation, sustained for one more minute. A stable backlog with continuing apply progress does not fire this rule. The shared `halro:ha_epoch_stable:bool` rule suppresses cross-epoch comparisons.
- Immediate: inspect its apply error, disk capacity/I/O, state/applied watermark, and per-store cursor. This is a stall signal, not a complete promotion-eligibility decision; inspect input/apply rates and the exact authenticated prefix before an operation.
- Recover: repair the Replica and let the ordered applier catch up. If recovery requires replacement, take the member through maintenance and an approved seed rather than copying live files.
- Escalate: when this is the only Replica, treat loss of promotion capacity as a degraded-HA incident even if Primary traffic is healthy.

### HalroMemberIncompatible

- Trigger: a member's current successful scrape reports peer rejection for schema/protocol compatibility, Master Key challenge, or SPKI pinning for two minutes. An old rejection retained by Prometheus lookback is not a current fault.
- Immediate: read the bounded `reason` label, compare authenticated `cluster status` on both members, and verify the configured certificate pin and Secret generation.
- Recover: complete the ordered rolling-upgrade sequence for a schema mismatch, or repair certificate/Master Key distribution and restart the affected member. Never relax mTLS or edit `state.json`.
- Escalate: keep the peer out of promotion consideration until a fresh authenticated session succeeds and the alert clears after restart.

### HalroTargetDown

- Trigger: a statically configured Halro scrape target reports `up=0` for two minutes. The rule also has a global `absent(up{job="halro",expected_target="true"})` fallback when the entire selected scrape set disappears; that fallback has no member or cluster labels. A member never entered in Prometheus target configuration cannot be detected by this rule: reconcile the health page's independent `-members` inventory with Prometheus targets.
- Immediate: check process/listener health, Metrics authentication and TLS, then the Prometheus target error.
- Escalate: page the service owner if readiness cannot be restored without restart or rollback.

### HalroClientObserverDown

- Trigger: an already configured `halro-client-observer` target with `expected_client_observer="true"` reports `up=0` for one minute. This rule stays silent when no external observer has been deployed; it cannot detect an observer omitted entirely from Prometheus configuration.
- Immediate: compare the accepted `-client-final-manifest` inventory with Prometheus targets, inspect the target scrape error and the independent observer's failure domain, then preserve the last raw result samples. The client-final panel must be `unobserved` while the target is unavailable.
- Recover: restore the approved target and require a full fresh five-minute coverage window before treating its result distribution as observed. Do not remove the target from the manifest to silence the alert.

### HalroClientObserverCoverageIncomplete

- Trigger: a successfully scraped accepted observer has no same-scrape ready gauge equal to 1, or no same-scrape unreconciled-operation gauge equal to 0, for one minute. This includes a missing or stale gauge, an unready producer, and at least one unresolved logical operation.
- Immediate: inspect the producer's durable start/terminal records and recovery state, then compare raw gauge timestamps with `up`. Preserve private operation IDs only in controlled evidence; never add them to metric labels.
- Recover: reconcile indeterminate operations and repair the producer/exporter. After the gauges recover, require the entire displayed five-minute window to pass the page's coverage gate; a cleared alert alone does not validate client outcomes.

### HalroWALAppendErrors

- Trigger: a Ledger WAL write or sync error occurred in the last five minutes.
- Immediate: stop new Provider traffic, inspect filesystem capacity/permissions/I/O, and preserve the data directory.
- Escalate: treat committed-data corruption or repeated fsync failure as an accounting incident; do not edit the WAL.

### HalroShutdownTruncatedAttempts

- Trigger: `halro_shutdown_truncated_attempts_total` increased in the last ten minutes; the process exhausted its graceful-shutdown budget and forcibly closed one or more active Provider attempts.
- Immediate: preserve the previous process logs and data directory, run `halro doctor`, and identify the restart window and affected in-flight request/attempt IDs from the Ledger and structured attempt records. Do not retry ambiguous calls automatically.
- Recover: confirm startup recovery settled every `AttemptStarted` lease conservatively, `halro_accounting_pending_leases` returned to zero, readiness is healthy, and Project balances include the recovered attempts before restoring ordinary traffic.
- Escalate: page the accounting owner if recovery fails, a pending lease remains, or the configured shutdown timeout is lower than the longest permitted request/stream duration. Increase a timeout only after proving the host can drain within it.

### HalroAccountingLeaseStale

- Trigger: a pending Accounting Lease remains older than `max(gateway.route_total_timeout, gateway.stream_max_duration)` for two minutes. The exported `halro_accounting_pending_lease_normal_max_age_seconds` is that configured ceiling; the alert's `for` duration is scrape/jitter grace, not permission for a longer request.
- Immediate: correlate the oldest lease with active Provider attempts and request deadlines. Check for a stuck adapter, blocked network read, stalled Ledger writer, or an incomplete startup recovery; do not delete or edit the lease.
- Recover: cancel or drain through the normal request lifecycle, or restart only through the documented graceful-shutdown path. Require a terminal Attempt settlement, a non-success Request finalization where appropriate, zero stale leases, and a healthy `halro doctor` result.
- Escalate: page the accounting and Provider owners when the lease cannot be closed within one additional route timeout, or immediately if WAL/readiness is degraded. Treat the Provider result as ambiguous unless the durable attempt record proves it was never sent.

### HalroUsageAnalyticsLagging

- Trigger: derivative Usage aggregation has remained behind the Ledger for ten minutes.
- Immediate: inspect queue depth, checkpoint progress, host I/O and the authoritative Ledger watermark.
- Escalate: if the lag keeps growing, protect Ledger durability first and rebuild the derivative only from verified records.

### HalroLedgerQueueHigh

- Trigger: the durable append queue stays above 75 percent for five minutes.
- Immediate: compare request rate, WAL sync latency, batch size and project-lock wait.
- Escalate: shed traffic or increase approved capacity before the queue fills and accounting becomes unavailable.

### HalroAlertDeliveryFailing

- Trigger: application alert delivery fails or drops for ten minutes.
- Immediate: inspect the receiver endpoint, credential, SafeTransport rejection class and delivery queue.
- Escalate: use the independent dead-man path and Security/SRE channels if operational alerts cannot be delivered.

### HalroDeploymentUnhealthy

- Trigger: one probed Deployment remains down for five minutes.
- Immediate: inspect its Provider credential, endpoint, model/region, last test and upstream status.
- Escalate: disable or replace the Deployment if healthy fallbacks are insufficient or the provider is persistently unavailable.

### HalroMultipleDeploymentsUnhealthy

- Trigger: at least two Deployments in one cluster are unhealthy for three minutes.
- Immediate: look for a shared Provider, credential, network, DNS or regional dependency before changing routes.
- Escalate: invoke the provider-outage plan and preserve budget/accounting controls while shifting approved traffic.

### HalroCredentialUnusable

- Trigger: an upstream refused a credential with 401 or a lapsed subscription, and the gate has taken it out of service.
- Why it is critical rather than a warning: this suspension has no window. A dead key does not heal, so nothing in Halro will end it — it clears only when the credential's revision advances, which means somebody replacing the secret. Until then every route on that credential is refused, and the only signal is this alert.
- Immediate: `GET /admin/api/v1/route-suspensions` names the scope; the metric deliberately carries no credential id. Confirm upstream-side: expired key, revoked key, cancelled subscription, wrong account.
- Act: replace the credential. Saving it advances its revision, and the topology activation that a credential mutation already runs drops the suspension then and there — no second step, no waiting for traffic. The alert clears on the next scrape.
- If the refusal was resolved upstream without touching the stored secret, re-saving the credential unchanged still advances its revision and remains the first-choice way to clear it: it is the action that also fixes what the upstream was refusing.
- A direct clear exists for the case where the secret is right and the gate is holding a refusal already dealt with upstream: `DELETE /admin/api/v1/route-suspensions/{scope_id}`, where `scope_id` comes from the listing, or `halro route clear-suspension --scope-id <id>` while the instance is stopped. It is an administrative action and writes Audit (`route_suspension.clear`) in the same transaction that removes the stored row. The gate re-suspends on the next refusal, so clearing a credential that is still dead costs one request and tells you so.
- This suspension survives a restart. It is one of the two kinds Halro stores — the other is quota — precisely so that a restart does not hand a dead key a request from every caller again.
- Escalate: if the credential is shared by several Deployments, all of them are down together — check whether an approved fallback on a different credential exists before repointing traffic.

### HalroProviderQuotaExhausted

- Trigger: an upstream answered that its quota is spent, for five minutes. Halro reads that from the code in the refusal body rather than from the status: six of the nine upstreams put an exhausted allowance and an ordinary rate limit on the same 429, and the code is the only thing that separates them (`internal/gateway/refusal_codes.go`).
- Immediate: `GET /admin/api/v1/route-suspensions` names the scope; confirm the billing state upstream. Halro cannot tell a monthly allowance from a prepaid balance.
- Note: the suspension ends on its own window and is retried with a real request, because no model-list probe consumes quota and therefore none can prove it came back. Expect recovery to lag a top-up by up to the current window, which doubles to six hours if the upstream keeps refusing.
- After a top-up, the window is the only thing still holding the route out. `DELETE /admin/api/v1/route-suspensions/{scope_id}` ends it immediately — the same audited clear as for a credential — and the gate re-suspends if the upstream is still refusing.
- This suspension survives a restart, so restarting Halro is not a way to shorten the window. That is deliberate: re-admitting every exhausted account on restart is the tax the admission gate exists to remove.
- Escalate: if this alias has no unaffected fallback, the caller-facing answer is a 503 `all_candidates_suspended` naming quota — route traffic elsewhere or raise the account limit.
- One case is deliberately not detected: Anthropic's monthly spend cap answers with the same status and the same `rate_limit_error` type as an ordinary rate limit, and is separable only by the absence of a `retry-after` header. Halro reads it as a rate limit, so an Anthropic account that has hit its spend cap never reaches this alert: it shows as `halro_route_suspended{reason="rate_limited"}` on the Anthropic credential, retried on a one-second-to-one-minute window that cannot succeed until the cap is raised. Inferring an indefinite suspension from a missing header would be the expensive way to be wrong, so the signal to watch for is a rate-limit suspension that never clears. The same is true of a Gemini daily quota: its refusal reaches Halro as the RPC name `RESOURCE_EXHAUSTED`, which is what a per-minute limit also answers with, so it too shows as a rate-limit suspension that does not clear.

### HalroFallbackSaturation

- Trigger: fallback exceeds 25 percent with at least 20 recent requests for five minutes.
- Immediate: identify the primary Deployment failure class and compare fallback capacity, latency and cost.
- Escalate: stop the degraded primary or cap traffic if fallback spend/capacity threatens the project boundary.

### HalroProviderCapacityPressure

- Trigger: Provider active requests stay above 85 percent of its configured concurrency for five minutes.
- Immediate: inspect per-Deployment pressure, queueing and upstream quota; confirm the configured ceiling is intentional.
- Escalate: scale or request quota only with evidence; otherwise shed load before all routes on the Provider stall.

### HalroHighErrorRate

- Trigger: request errors exceed five percent with at least 20 recent requests for ten minutes.
- Immediate: split errors by policy, Provider, Deployment and protocol; correlate latency and fallback signals.
- Escalate: rollback the responsible configuration or isolate the upstream when the rate breaches the service objective.

### Watchdog

- Trigger: this alert continuously fires; missing notifications are the failure signal.
- Immediate: the independent receiver checks its last firing timestamp and direct Prometheus/Alertmanager probes.
- Escalate: missing Watchdog beyond the approved grace window is a monitoring outage even if Core cannot page itself.

### HalroWALSyncSlow

- Trigger: mean Ledger durability barrier time exceeds the host baseline for ten minutes.
- Immediate: inspect storage latency, saturation, WAL batch size and noisy-neighbor activity.
- Escalate: move to approved storage or reduce traffic if fsync latency threatens queue and request objectives.

### HalroAccountingProjectLockSaturated

- Trigger: mean wait on one project's accounting lock exceeds 250 ms for fifteen minutes.
- Immediate: identify the hot project and compare its concurrency/request pattern with WAL sync latency.
- Escalate: enforce project capacity limits or isolate workload; do not weaken per-project accounting serialization.

### AlertmanagerNotificationFailing

- Trigger: Alertmanager reports notification failures for two minutes.
- Immediate: inspect receiver reachability, secret file, TLS and Alertmanager logs through the authenticated path.
- Escalate: notify through the independent dead-man channel and treat simultaneous Core failure as loss of paging.

### PrometheusRuleEvaluationFailing

- Trigger: Prometheus rule evaluations fail for two minutes.
- Immediate: inspect the failing rule/group, source-series cardinality and query/resource errors.
- Escalate: rollback the rule change if evaluation cannot be restored promptly; a loaded file is not a working rule.

### PrometheusConfigReloadFailing

- Trigger: the last Prometheus configuration reload failed for two minutes.
- Immediate: run the pinned config/rule validators, inspect reload logs and retain the last known-good configuration.
- Escalate: rollback rather than restart repeatedly if validation and the live error disagree.

### PrometheusTSDBDiskHigh

- Trigger: Prometheus TSDB plus WAL exceeds the local 3.5 GiB warning budget for ten minutes.
- Immediate: inspect retention, ingestion growth, compaction and filesystem free space without deleting live blocks.
- Escalate: expand approved storage or reduce retention before the 4.25 GiB critical threshold.

### PrometheusTSDBDiskCritical

- Trigger: Prometheus TSDB plus WAL exceeds 4.25 GiB for five minutes.
- Immediate: protect the host from disk exhaustion, preserve recent monitoring evidence and stop nonessential ingestion.
- Escalate: page SRE and execute the approved TSDB recovery/capacity plan; do not hand-delete WAL or block files.

### HalroAuditAnchorStale

1. Confirm anchoring is enabled and compare the last-emission timestamp with
   the exported configured interval and process start time.
2. Check the independent sink credential, reachability and audit-anchor
   authentication failures without disabling signature or chain verification.
3. Escalate to Security immediately; restore emission and independently verify
   a new chain head before treating the external witness as current.

### HalroProjectDailyBudgetRefusingTraffic

A Project reached `daily_budget_micros_usd` — the cap its operator set in the
console — and the Gateway is refusing its requests with 403. The budget is
accounted against a reservation *before* any upstream is called, so nothing was
spent past it; what is happening is that a caller is being turned away.

1. Find the Project from the Usage breakdown. The alert carries no Project
   label on purpose: a per-Project series grows with the customer list, and the
   console already answers "which one" without that cost.
2. Decide which this is. A Project that reached its cap on the last day of a
   billing period is working as configured and needs nothing. A Project that
   reached it at 09:00 is either under-budgeted or doing something it did not
   do yesterday.
3. If it is the second, check the Usage breakdown for a change in rate before
   raising the budget — a leaked Gateway Key looks exactly like a popular
   application until you compare it with last week.
4. Raising the budget is a console change on the Project and takes effect
   immediately. There is no instance-wide cap to consult: the Project budget is
   the cap, deliberately, so there is one number per tenant and one place it
   lives.

### HalroRunBudgetRefusingTraffic

The same, for a Run's own budget under Run Governance. The Run is bounded
separately from the Project that owns it, so this fires while the Project still
has room.

1. Identify the Run and its Work Unit from Run Governance.
2. A Run that exhausted its budget has either been mis-sized or is looping.
   Check its attempt count against what the Work Unit expected before extending
   it.

### HalroProviderSpendUnreported

`halro_cost_usd_total` is absent from the scrape entirely.

1. This is not "spend stopped". The counter is restored from the usage
   checkpoint and rebuilt from the Ledger, so its absence means the metrics
   path or the accounting aggregate is broken — and every cost signal is silent
   while it is.
2. Check the metrics endpoint's reachability and credential, then
   `halro doctor` for the accounting and Parquet checks.
3. Budget *enforcement* is unaffected: it runs in the request path against the
   Ledger, not against this metric. What is lost is the ability to see it.

### HalroAdminAuditBacklogStuck

An administrative mutation and its audit intent commit in one transaction; the
append to the audit log happens afterwards. A backlog that drains is the
ordinary recovery from a crash between the two. A backlog that does not drain
means mutations are still being accepted while the records describing them are
not landing — the audit trail going quiet without anything failing loudly.
`HalroAuditAnchorStale` cannot see this: an anchor witnesses the chain that
exists, not the records missing from it.

1. Read `halro_admin_audit_intents_pending` and compare it with
   `halro_admin_audit_delivery_failures_total`. A rising failure count names a
   delivery that is being attempted and refused; a flat one with a stuck gauge
   means nothing is retrying.
2. Run `halro doctor` offline. Its `admin_audit_backlog` check reports the same
   backlog from the store, which distinguishes a metrics-path problem from a
   real one.
3. Check the audit log's filesystem for space and permissions, then
   `halro audit verify` on a stopped instance to confirm the chain itself is
   intact before anything is appended to it.
4. Escalate to Security. Until the backlog drains, treat every administrative
   change in the window as unrecorded: the mutations took effect and the trail
   does not describe them.

### HalroAdminAuditBacklogUnreadable

`halro_admin_audit_intents_pending` reports `-1` when the backlog could not be
read. Zero is the healthy answer, so an unreadable store must not be able to
report it — an integrity signal that goes silent when its own source breaks is
worse than no signal.

1. Check metadata store health and the data directory's permissions and space.
2. Run `halro doctor`; a failing `metadata` or `admin_audit_backlog` check
   names the same fault from outside the serving process.
3. Escalate to Security. The trail's state is unknown, which is not the same as
   healthy, and no administrative change should be assumed recorded until the
   gauge reads zero or higher.

### HalroAdminAuditDeliveryFailing

At least one append of a durable mutation's audit record failed in the window.
The mutation is already in effect; only its record is missing.

1. Identify the failure from the instance's logs and whether the backlog gauge
   is also non-zero. A failure that clears with the backlog is a transient the
   retry absorbed.
2. If the backlog is not draining, follow **HalroAdminAuditBacklogStuck**.
3. Record the window in the incident log either way: a retried delivery still
   means the trail was, for a time, behind the state it describes.

### HalroDeploymentCapabilityEvidenceDegraded

1. Determine whether the family is absent (store read unknown) or the
   `conflicting` state is non-zero (trusted evidence disagrees).
2. Keep affected deployments withheld, inspect store health, then compare their
   immutable snapshots with current provider-profile and catalog evidence.
3. Escalate if the series stays absent or conflict cannot be reconciled; close
   only after all four bounded states are visible and `conflicting` is zero.

### HalroSignedModelCatalogDegraded

1. Inspect `halro_signed_model_catalog_refresh_total` by `status` and
   `error_class`; check the catalog URL, signature, pinned public key, TLS and
   clock without weakening signature verification.
2. Confirm Halro is using its last-known-good or bundled catalog, and inspect
   `halro_signed_model_catalog_degraded_since_seconds` to bound the exposure.
3. Restore a valid signed catalog or intentionally disable the dynamic source;
   require the degraded gauge to return to zero before closing the incident.

### HalroCapabilityDriftDetected

1. Split the alert by `reason`: `catalog` means catalog evidence narrowed;
   `profile` means the provider profile no longer supports the active snapshot.
2. Identify deployments withheld from routing and compare their immutable
   capability snapshots with the current signed catalog and provider profile.
3. Do not override the hold. Re-detect and explicitly activate a reviewed
   deployment revision, or restore the trusted evidence, then verify routing.

### HalroCapabilityDetectionFailureRateHigh

1. Break down `halro_model_capability_detection_total` by `provider_type`,
   `status`, and `source`; correlate failures with probe totals, duration and
   provider-call counts.
2. Check provider credentials, endpoint reachability, rate limits and the
   bounded detection-call budget. Detection calls are control-plane traffic
   and are not project usage-accounting evidence.
3. Fix the provider or configuration and rerun detection. Require at least five
   recent terminal detections with a failure ratio at or below 50 percent
   before resolving the incident.

### HalroTLSCertificateExpiringSoon

1. Read `halro_tls_certificate_expiry_seconds` by `scope` and `name` to identify
   which certificate is short. `scope="serving"` is the Gateway and Admin
   listeners; `scope="metrics"` is the mutually authenticated scrape endpoint.
2. Obtain the replacement and write it over the `cert_file` and `key_file` of
   the matching `tls.certificates` entry. Write both before signalling: the
   pair is read together, and a reload that finds only one of them replaced
   keeps the whole previous set.
3. Send `SIGHUP` (`systemctl reload halro`, or `kill -HUP <pid>`). Confirm
   `halro_reload_total{item="tls",status="success"}` increased and that the
   expiry gauge moved. The `TLS certificate loaded` record carries the new
   fingerprint, which is what `openssl s_client | openssl x509 -fingerprint`
   reads back from the outside. Existing connections are not interrupted.
4. If the reload reports an error, the previous certificate is still being
   served — the instance is not down. Fix the files and signal again.

### HalroTLSCertificateExpired

1. Treat as an outage: clients are refusing the handshake, not the server.
2. Follow HalroTLSCertificateExpiringSoon from step 2. No restart is required,
   and restarting will not help if the files on disk are still the expired ones.
3. If no replacement is available yet, the instance cannot serve TLS. Do not
   fall back to plaintext on a routable address; the configuration refuses it.

### HalroReloadFailing

1. Break down `halro_reload_total{status="error"}` by `item`. The previous value
   for that item is still in force, so this is drift rather than an outage.
2. Read the process log for `reload item failed`; it names the item and the
   underlying error without quoting file contents.
3. `item="tls"` or `item="metrics_tls"`: the keypair or client CA on disk does
   not load. `halro doctor --config <path>` reports the same check offline and
   can be run while the instance serves.
4. `item="log_level"`: the configuration file no longer validates, so the level
   was not taken from it. The whole file is checked before one value is used —
   fix the unrelated error the log names.
5. `item="log_file"`: the log path could not be reopened. Check the directory's
   ownership and permissions (`0700` directory, `0600` file).
6. After fixing, signal again and require one `status="success"` increase for
   the affected item, plus a `halro_reload_last_success_timestamp_seconds`
   that moves.

## Credential rotation

1. Generate a new active Metrics credential with a bounded overlap.
2. Atomically replace Prometheus's `0400/0440` credential file and reload.
3. Require `up == 1` for two scrape intervals.
4. Revoke the retiring credential.
5. Run `halro metrics verify-audit --config <path>` and send its returned
   sequence/hash chain head to the independent immutable audit platform.
6. Prove the old token receives 401 and record the secret-free audit evidence
   and independently anchored chain hash.

Never put the token in a command argument, environment variable, ticket,
dashboard, screenshot, or shell history.

## Metrics certificate rotation

1. Issue the replacement server certificate and Prometheus client certificate
   from the approved CA, retaining the old trust path for the overlap window.
2. Atomically replace the mounted files with mode `0400/0440`.
3. Perform a controlled Halro restart; Metrics TLS material is intentionally
   loaded at listener startup, so replacing files alone is not a reload.
4. Require two successful mTLS scrapes, then remove the old client identity and
   trust anchor and restart again if the CA bundle changed.
5. Prove missing, expired, and wrong-CA clients fail and archive the chain head,
   restart, scrape, and rollback evidence.

## Recovery

Restore Prometheus, Alertmanager configuration/state and Metrics identity state
only from encrypted, authorized backups. Restore credential lifecycle state
from a snapshot at or after the latest revocation watermark; otherwise keep
credentials revoked and rotate anew. If TSDB is formally classified as a
disposable cache, rebuild it according to the approved RPO instead of silently
claiming historical recovery.

## Independent dead-man drill

1. Confirm the external receiver is receiving `Watchdog` and the synthetic
   probe heartbeat. Record their last-seen timestamps without exposing webhook
   credentials.
2. Stop Prometheus. Require both missing `Watchdog` and the direct Prometheus
   probe to alarm within the approved detection objective; restore Prometheus
   and require both signals to recover.
3. Stop Alertmanager while Prometheus remains healthy. Require missing
   `Watchdog` and the direct Alertmanager probe to alarm; restore Alertmanager
   and require recovery.
4. Stop the external probe. Require the receiver's probe-heartbeat alarm, then
   restore the probe and require recovery.
5. Demonstrate that Core host/storage loss and Core notification credentials
   cannot suppress or authorize the independent path. Archive timestamps,
   payload hashes, delivery receipts and audit IDs in immutable evidence.

Never deploy the external probe beside Core as production evidence. It must use
authenticated HTTPS checks and a dedicated receiver credential; it must not
reuse Alertmanager's operational or Watchdog webhook identity.

## Local Core runtime smoke

Run `./deploy/observability/smoke.sh` on a development host where loopback ports
9090, 9091, 9093 and 19094 are unused. The script uses a unique Compose project,
temporary local-only credentials, an authenticated mock Metrics target and a
mock webhook. It verifies Core target health, loaded rules, the continuous
`Watchdog` and an Alertmanager API firing/resolved lifecycle. Cleanup removes only the
unique smoke project and temporary files.

If a development service already owns one of those ports, do not stop it just
for this check. Set `HALRO_SMOKE_METRICS_PORT`,
`HALRO_SMOKE_PROMETHEUS_PORT`, `HALRO_SMOKE_ALERTMANAGER_PORT`, and
`HALRO_SMOKE_WEBHOOK_PORT` to four distinct unused ports in `1024..65535`.
The smoke creates runtime-only config copies with those addresses; shipped Core
configuration and the existing service are not modified.

This is repository/runtime regression evidence only. It does not exercise
target PKI, the real Contact Point, an independent dead-man receiver, immutable
audit, backup/restore, production capacity or four-party sign-off, and therefore
cannot change any Phase D checklist row from `BLOCKED`.

## Required drills

- Halro and expected-target loss;
- WAL append error and analytics lag;
- multiple deployment failure and fallback saturation;
- Prometheus/Alertmanager stop and TSDB full/read-only;
- external probe stop and Watchdog/probe-heartbeat loss;
- credential and certificate rotation/revocation;
- unauthorized local process, SSRF, and audit tampering.

Record target-environment outcomes in `admission-checklist.md`. A repository
test result is supporting evidence only and cannot close a production gate.
