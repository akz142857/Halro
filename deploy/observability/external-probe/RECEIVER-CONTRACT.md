# Independent receiver contract (v1)

The receiver is outside the Heimdall, Prometheus, Alertmanager, and probe
failure domains. A successful HTTP response means the event is durably stored,
not merely accepted into process memory.

Events are keyed by `(probe_id, environment, region, cluster)`. `event_id` is
the idempotency key and `sequence` is monotonically increasing for one
`probe_id`. The receiver must retain the greatest sequence and must not let a
late event extend a heartbeat deadline.

The probe delivers its durable outbox in strict FIFO order. It never attempts
a later queued event after an earlier event fails. A successful delivery whose
local delivery-audit record cannot be synced remains queued and may be sent
again with the same `event_id`; the receiver must acknowledge that duplicate
idempotently. Sequence gaps are valid because an unsent heartbeat may be
superseded by a newer heartbeat before delivery.

For a `kind=heartbeat` event, the deadline is:

```
observed_at + heartbeat_ttl
```

If that deadline passes without a newer heartbeat, the receiver sends a
`firing` notification through a contact point and credential not used by
Alertmanager. The first newer heartbeat sends exactly one `resolved`
notification. Receiver time may reject events whose `observed_at` is
unreasonably far in the future; arrival time must not turn a replayed heartbeat
into proof of current health.

For `kind=state_transition`, `state=down` sends `firing` and `state=up` sends
`resolved`, idempotently by event ID. `pending_*` states are never sent. The
receiver records receipt, duplicate/replay decisions, deadline transitions,
delivery attempts, and delivery results in its independent immutable audit
store. Payloads and audit records never contain probe, target, or notification
credentials.

Production admission must demonstrate the receiver's durable acknowledgement,
TTL alarm, recovery, replay rejection, separate credential/contact point, and
independent failure domain. This repository contract is not that evidence.
