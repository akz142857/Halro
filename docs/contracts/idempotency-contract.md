# Idempotency contract

## Data-plane requests

`Idempotency-Key` is an optional future-safe retry contract for chat completions
and embeddings. Data-plane clients that omit it retain existing behavior.

- Keys are scoped to the authenticated Project, not globally.
- A key is 1 to 128 visible ASCII characters and must not contain whitespace.
- Identity stores a SHA-256 fingerprint of the canonical operation and request;
  request bodies and Provider credentials are never stored as identity.
- Reusing a key with a different fingerprint is a deterministic conflict.
- States are `reserved`, `in_progress`, `completed`, and `unknown`.
- Terminal records are bounded by an explicit expiry. Expiry permits a later new
  execution and therefore must be longer than the advertised retry window.
- `unknown` means the Provider may have executed but no final result is known.
  It is never silently converted to a retryable or refunded result.
- Streaming response bodies are not retained by the Phase 0 primitive. A retry
  may observe lifecycle state, but Halro does not promise replay of an SSE
  byte stream.

The initial durable store is a Standalone primitive. Runtime endpoint adoption
requires a separately reviewed API change so the current OpenAI compatibility
contract does not change accidentally before v1.

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
