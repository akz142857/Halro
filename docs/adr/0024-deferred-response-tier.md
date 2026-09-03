# ADR 0024: A deferred tier for Responses, and what it may store

- Status: Accepted
- Date: 2026-09-03
- Amends: ADR 0005 (Stateless OpenAI Responses facade), resource-ownership clause
- Related: ADR 0011 (accounting lease crash recovery), ADR 0018 (project admission
  and the accounting write path), ADR 0021 (provider resource upstream twin),
  ADR 0001 (single-process architecture)

## Context

ADR 0005 refused `background` along with the rest of the Responses stateful
surface, and closed with a condition rather than a prohibition:

> A future stored tier requires a new ADR defining provider/deployment/profile/
> region binding, lifecycle, encryption, deletion, and failover behavior.

This is that ADR. The requirement that reopened the question is narrow: a caller
wants to submit a generation, drop the connection, and collect the answer later
by id. It is not a request for conversation state, for stored prompts, or for
provider-owned Response objects.

Halro already has one shape for "submit now, collect later". `POST /v1/batches`,
`POST /v1/async/invocations` and `POST /v1/files` mint a `domain.ProviderResource`
owned by a Project, carrying the route it was created under, an idempotency
hash, a creation status, and a TTL that an hourly reaper enforces
(`internal/app/provider_resources.go`). ADR 0021 already settled that such a
resource need not have an upstream twin. Nothing in the ownership model has to
change to hold one more kind.

What does have to change is the answer to a different question, and it is the
reason this needs an ADR rather than a feature branch.

### The data-classification question

`internal/vault/vault.go:99-106` states the current position exactly:

> This is the only material Halro stores that a caller wrote — a prompt, tool
> arguments, an upstream error body

Failure capture is the single place a caller's own bytes reach disk today. It is
off by default (`internal/config/config.go:460-475`), bounded by size, by daily
count, and by a 24-hour retention; it is sealed with a scoped AEAD bound to the
request and the project; it is readable only through one audited admin action;
and it captures failures alone — "A successful call is never stored, which is
what keeps this a small tail of traffic rather than a copy of it."

A deferred tier puts the **output of successful requests** on disk, as the normal
case rather than the tail. Whatever else it is, it is a change to what an
operator's data directory contains. That is the decision here; the endpoint
shape is the easy part.

### Two things already wrong that this tier would otherwise inherit

Neither is caused by deferred retrieval; both are load-bearing for it.

`writeResourceObject` (`internal/gateway/inference_resources_store.go:118`) writes
provider objects in the clear — temp file, `Chmod(0600)`, `Sync`, `Rename` — into
a 0700 directory. Batch inputs and batch results, which are caller-written
prompts and model output respectively, land there as plaintext. That is a lower
standard than `EncryptFailurePayload` applies to the same class of material.

`GetBatch` (`:1012`) runs every poll through the full accounting envelope:
`beginRequestRun` + `startAttempt` + `finish` + `finalize`, a complete ADR 0018
request lifecycle per call, with the fixed request cost explicitly zeroed. For a
batch that is correct — only the upstream knows the status, so a poll *is* a
request. For deferred retrieval it is not: the answer is already on local disk. A
caller polling every two seconds would write ~1800 groups of WAL frames an hour
and consume 1800 RPM slots, all of it noise.

## Decision

Halro serves a **deferred tier** on the Responses facade: `background: true` on
`POST /v1/responses`, with `GET /v1/responses/{id}`,
`POST /v1/responses/{id}/cancel`, and `DELETE /v1/responses/{id}`.

The rest of ADR 0005's resource-ownership clause stands unchanged. `store: true`,
`previous_response_id`, `conversation`, prompt resources, metadata persistence,
input-item listing and compaction remain unavailable. `background` is not an
opening of the stateful tier; it is one stateless generation whose answer is
collected on a second connection.

Three boundaries are constitutive, not tunable.

**Polling is authoritative.** Any later notification mechanism saves a poll; it
never becomes the only delivery path for a result. A design where a failed push
loses the answer is fail-open, and this project does not build those.

**The deferred tier is not session state.** The record holds one request and one
answer. It does not accumulate, it cannot be referenced by a later request, and
it expires.

**Storing a successful answer is a classification change.** The storage
discipline is therefore inherited from failure capture rather than from the
existing plaintext object directory: sealed, scoped, size-bounded, short-lived,
and off unless a Project turns it on.

### The six answers ADR 0005 required

**provider / deployment / profile / region binding.** Resolved once at submission
and pinned on the record — the same six fields `ProviderResource` already carries.
The worker does not re-resolve at dequeue. Between submission and execution a
deployment may be edited, disabled or deleted; re-resolving would serve the caller
an answer from a route that did not exist when they asked, while pinning fails the
request with an error that says the selected deployment is gone. The second is
explainable to the caller and the first is not.

**Lifecycle.** `queued` → `in_progress` → `completed` | `failed` | `cancelled`. A
terminal record enters a cool-off window on first successful retrieval and is
reaped after it. `queued` may be cancelled deterministically; `in_progress` is
cancelled best-effort, settled conservatively under ADR 0011, and reported as
possibly having cost money upstream.

**Encryption.** A new vault scope, shaped exactly like `EncryptFailurePayload`:
scoped AEAD keyed by `(resource id, project id)`, so an object renamed onto a
different record or lifted into another install fails to open rather than opening
as somebody else's traffic. The fix is applied to `writeResourceObject` as a
whole rather than as a second, encrypted path beside the plaintext one — the
plaintext write is wrong for batch objects too, and pre-1.0 this project fixes a
wrong construct in place instead of letting it survive beside its replacement.
The caller's input is kept only while it is still needed to make the upstream
call, and is erased once the upstream has answered.

**Deletion.** Explicit `DELETE`; cool-off reclamation after first retrieval; TTL
expiry through the existing hourly reaper. Default TTL 24 hours, matching
`DefaultFailureCaptureRetain` — it is the same class of material, so it does not
get a longer life than failure capture has. No new cleanup loop.

**Failover.** There is none, by ADR 0001: one process owns one data directory. On
restart, a `queued` record whose `ReservedBy` names a departed instance is
re-enqueued, because nothing was sent upstream and re-sending is safe. A record
that reached `in_progress` is **failed unconditionally** and settled
conservatively.

**Relationship to provider state.** None. Under ADR 0021 the record has no
upstream twin: the upstream sees one ordinary synchronous generation and never
learns that Halro answered it later.

### Why `in_progress` cannot survive a restart

This is the substantive difference between the deferred tier and every other
resource kind Halro holds, and it is a contract, not an implementation detail.

A batch has an upstream handle. After a restart Halro can ask the upstream what
happened, and the answer is authoritative. A deferred response has no handle: it
was a plain synchronous HTTP call, and when the process died the socket died with
it. Halro cannot determine whether the upstream completed the work, whether it
billed for it, or what it said. Under ADR 0011 an outcome nobody can determine is
settled conservatively and never silently refunded, and the only honest status to
show the caller is `failed` with an error that says the call may have been billed
upstream.

Callers must be told this in the user-facing documentation, because it changes how
they write their retry logic: a `background` submission does not survive a Halro
restart.

### Accounting

Submission performs admission only — authentication, source policy, Token Guard,
route resolubility, queue depth, and one RPM slot — and **writes no ledger
events**. Dequeue writes the ADR 0018 sequence verbatim as the synchronous path
does: `ReservationCreated`, fsync, `AttemptStarted`, fsync, provider I/O,
`AttemptSettled`, `RequestFinalized`. Retrieval writes nothing at all.

Reserving budget at submission was refused. ADR 0011 requires the reservation to
be durable before provider I/O, not before queueing; reserving early would let a
request that waits ten minutes hold ten minutes of a Project's daily budget, and
would force crash recovery to distinguish an unsettled lease that was queued from
one that was sent — a state that exists for no other reason. Reserving at dequeue
leaves the recovery state machine untouched. The price is that an over-budget
submission is accepted and later reports `failed`; a cheap non-authoritative
balance check at submission catches the obvious cases without writing to the
ledger.

Retrieval deliberately does **not** copy `GetBatch`'s envelope. `GetBatch` is
correct to run under it because it makes an upstream call; retrieval makes none,
and putting a poll loop through the accounting write path would inflate the WAL
and the usage partitions with events that describe no work.

### Admission is split, and only there

`limiter.Manager.Acquire` decides RPM, TPM and concurrency in one locked
admission and returns a lease. The deferred tier needs those to happen at
different moments: an RPM slot at submission, TPM and a concurrency slot at
dequeue.

RPM is charged at submission because RPM means requests per minute, and a
`background` submission is a request; leaving it uncharged would make the
deferred tier a way around a Project's RPM ceiling. Concurrency is *not* charged
at submission because it means upstream calls in flight, and a queued request is
not in flight; charging it would cap the queue at `MaxConcurrency` — a Project
allowing five concurrent calls could not queue a sixth — which defeats queueing.

The synchronous path keeps a single atomic admission. The split is an additional
pair of entry points into the same locked state, not two sequential `Acquire`
calls, which would double-charge RPM.

### Bounded queue

Queue depth is bounded per Project and a full queue answers `429` with
`Retry-After`. An unbounded queue is fail-open under exactly the condition that
matters: when the upstream slows down, it grows silently, consuming memory and
disk while having promised an answer for every entry. A bounded queue refuses
under pressure, in the caller's face, at the moment the pressure exists.

### Not in this decision

Result push is not part of it. If polling later proves burdensome, a thin
notification carrying `{id, status, project_id, timestamp}` — a signal, never a
result — can be evaluated on top of a working polling tier, and delivering result
bodies to caller-supplied URLs requires its own ADR: it re-runs redaction on a
second path, it makes delivery failure a data-loss event, and a caller-chosen
callback URL is the SSRF amplifier `internal/safetransport` exists to prevent.

Long-polling is not part of it: it holds a connection and a goroutine per waiting
caller and would need its own ceiling aligned with `MaxConcurrency`, or it becomes
a second way around the concurrency limit.

Streaming retrieval is not part of it. Replaying an SSE event sequence requires
persisting the event stream rather than the final answer, which is a materially
larger storage commitment. `background: true` and `stream: true` are mutually
exclusive.

`POST /v1/chat/completions` is unchanged. OpenAI defines no background semantics
there, and inventing them would produce a dialect endpoint no SDK addresses.

## Options considered

**A new `POST /v1/deferred` endpoint.** Simpler to specify, and it would avoid
amending ADR 0005. Refused: `docs/contracts/adding-a-northbound-endpoint.md`
counts pinned-SDK black-box behaviour as a class of compatibility evidence, and an
invented endpoint can never produce it. The official SDKs already have this call
shape.

**Accepting `store: true` alongside `background: true`.** Refused. In OpenAI's
model `store` means the provider retains the Response. Halro's upstream retains
nothing — the record is Halro's own object under ADR 0021 — so accepting `store`
would assert something untrue to the caller. It stays rejected, and the deviation
is recorded in the manifest.

**Leaving the object directory in plaintext and sealing only deferred objects.**
Refused on this project's pre-1.0 rule: a wrong construct does not get to survive
beside its replacement. It also fails on its own terms, since batch inputs are
caller-written prompts and batch results are model output — the same material,
under the same argument.

## Consequences

The northbound profile `openai.responses.stateless.v1` is no longer accurately
named and is renamed rather than duplicated, with its revision raised and the
state semantics carrying the difference. Three routes are added to the manifest
and one entry is renamed.

`domain.ProviderResourceKind` gains a kind, `ProviderResource` gains the fields a
state machine and a cool-off window need, and the metadata schema takes one
migration.

Provider objects change format on disk. An object whose sealed-envelope magic is
absent is reclaimed with its record at startup, and the count is logged once.
`data/provider-objects/` is empty in practice today, so the migration cost is
currently zero and rises with real batch usage — which is an argument for doing
it now rather than a reason it is free forever.

Operators get a feature that is off by default and enabled per Project, because
turning it on changes what their data directory holds.

Callers get an answer they can collect later, and one behaviour they must code
against: a request in flight when Halro restarts comes back `failed`.
