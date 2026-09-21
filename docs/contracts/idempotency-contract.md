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
- States are the resource plane's: `reserved`, `in_flight`, `unknown` and
  `completed`. The key moves to `in_flight` **before** anything is dispatched,
  so an interruption from that point on is remembered as possibly served.
- A repeat is answered, never awaited — Halro does not hold a socket open
  against another request's outcome:

  | The key has | Answer |
  |---|---|
  | a different request fingerprint | `409 idempotency_conflict` |
  | a request still running, or one whose outcome is unknown | `409 idempotency_in_progress` |
  | a request that completed | `409 idempotency_completed` |
  | a reservation from a process that is gone | admitted; it never reached the upstream |

- A failed request still spends its key. The upstream was reached, so a retry
  is the second call the key was sent to prevent; it settles as `unknown`,
  which is the existing vocabulary for an outcome nobody can determine and is
  never silently converted into a retryable or refunded result.
- A streaming repeat is refused **before the stream opens**, as an ordinary HTTP
  error rather than an event inside a stream the caller has been told is
  starting. Halro does not retain SSE bodies and promises no replay of one.
- A record expires 24 hours after it is written. Expiry permits a later new
  execution and is longer than any client retry window.
- An instance with no resource store refuses the header with `503
  idempotency_unavailable` rather than accepting a durable promise it cannot
  keep.

The mechanism is the resource plane's, under a `ProviderResource` kind of its
own (`inference_call`) and the same revision checks every other resource gets.
It is deliberately not a second implementation: an earlier standalone lifecycle
store was written, never reached, and removed for exactly that reason.

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
