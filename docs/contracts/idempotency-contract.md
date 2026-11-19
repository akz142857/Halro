# Idempotency contract

## Data-plane requests

`Idempotency-Key` is optional on `POST /v1/chat/completions` and
`POST /v1/embeddings`. A client that omits it keeps exactly the behaviour it had
and Halro writes nothing durable for the request.

**What the key buys is at-most-once execution upstream. It does not buy response
replay, and it never will by accident.** Replaying a completed answer would mean
storing it, and this gateway keeps caller content in exactly two places, both
because the caller asked it to (ADR 0021, ADR 0024). A generation nobody asked
to be stored does not become the third. A caller who needs an answer they can
come back for has `background: true`, which stores the answer because that is
what it was asked to do.

- Keys are scoped to the authenticated Project, not globally, so two Projects
  may use the same external key.
- A key is 1 to 128 visible ASCII characters and must not contain whitespace.
  A malformed key is `400 invalid_idempotency_key`, refused before anything is
  written and before any upstream call.
- Identity is a SHA-256 of the key and a SHA-256 fingerprint of the public model
  and the request. Neither the key, the request, nor the answer is stored.
- The record carries the lifecycle and the route it was first sent to, and never
  an object. That is enforced by validation rather than by convention.
- States are `reserved`, `in_flight`, `unknown`, `completed` and, for unary
  generation, `retryable_refused`. The key moves to `in_flight` immediately before the
  Provider call and nowhere earlier, so an interruption from that point on is
  remembered as possibly served — and everything that refuses a request without
  the upstream hearing of it stays on the `reserved` side of that line.
- A repeat is answered, never awaited — Halro does not hold a socket open
  against another request's outcome:

  | The key has | Answer |
  |---|---|
  | a different request fingerprint | `409 idempotency_conflict` |
  | a request still running, or one whose outcome is unknown | `409 idempotency_in_progress` |
  | a request that completed | `409 idempotency_completed` |
  | unary generation refused by explicit upstream rate limits | the same fingerprint can claim the key by revision after the route is eligible |
  | a reservation from a process that is gone | admitted; it never reached the upstream |
  | a record past its 24 hours | admitted, whatever the first request's outcome was, and whatever the new request is |

- A unary generation request that ends in `429 provider_rate_limit` is marked
  `retryable_refused` when every dispatched attempt was explicitly and
  unambiguously rate-limited and accounting finalized. A retry keeps the same
  fingerprint and record, and its 24 hours restart from the retry, so the key
  cannot expire under the call it is protecting. A failed attempt to record the refusal returns
  `503 resource_store_unavailable` and leaves the key protected.
- A failure after dispatch with an uncertain outcome spends its key. Timeouts,
  ambiguous upstream failures, and interruptions before the outcome is durably
  recorded remain protected as in-flight or unknown.
- A request Halro refuses **on its own** — a capability filter, redaction, a
  token limit, a Project budget, a per-attempt reservation, or the failure to
  record the dispatch itself — gets its key back. Nothing was billed and nothing
  is ambiguous, so the reservation is released and the caller's own retry is
  answered by the limit it broke rather than by a conflict about a call that was
  never made.
- If the reservation cannot be written, the answer says which of the two things
  happened: `409 idempotency_in_progress` when the key is genuinely held by
  another request, and `503 resource_store_unavailable` when the store could not
  record it. A store that cannot answer whether a key has been used is
  `503`, never read as a free key.
- A streaming repeat is refused **before the stream opens**, as an ordinary HTTP
  error rather than an event inside a stream the caller has been told is
  starting. Halro does not retain SSE bodies and promises no replay of one.
- A record expires 24 hours after it is written, in **every** state it can be
  in. Expiry permits a later new execution — including one carrying a different
  request, because a key nobody remembers issuing is free for whatever it is
  next used for — and is longer than any client retry window. This is the one
  resource kind whose expired record is reaped whatever its creation status:
  every other kind withholds an ambiguous record because something may still
  exist upstream, and this one owns nothing at all.
- An instance with no resource store refuses the header with `503
  idempotency_unavailable` rather than accepting a durable promise it cannot
  keep.

The mechanism is the resource plane's, under a `ProviderResource` kind of its
own (`inference_call`) and the same revision checks every other resource gets.
It is deliberately not a second implementation: an earlier standalone lifecycle
store was written, never reached, and removed for exactly that reason. Lookup is
by index — `(project, kind, key hash)` in its own bucket — because this is the
first kind whose population scales with inference traffic rather than with how
many files and batches a Project keeps.

## Admin create requests

`Idempotency-Key` is required on the Admin create endpoints for Providers,
Deployments, Routes, Projects, and Gateway Keys. A key is scoped to the
authenticated administrator and resource kind, may contain up to 256 bytes,
and is used to derive the server-side record ID. This is intentionally a
different contract from the data-plane 1–128 visible-ASCII key above.

- A missing, empty, or over-256-byte key returns `400
  idempotency_key_required` before mutation.
- Retrying a request whose first attempt committed returns `409
  <resource>_idempotency_replay` with the existing `id`. The server does not
  claim the current request created or replay the stored representation; the
  original body may differ under a reused key.
- A different key represents a deliberate second create and derives a different
  record ID.
- Gateway Key plaintext is shown only in the first successful response. A 409
  replay can identify the existing key but cannot recover its plaintext; revoke
  and issue a new key if the first response was lost.
