# Gateway idempotency contract

`Idempotency-Key` is an optional future-safe retry contract for chat completions
and embeddings. Clients that omit it retain existing behavior.

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
  may observe lifecycle state, but Heimdall does not promise replay of an SSE
  byte stream.

The initial durable store is a Standalone primitive. Runtime endpoint adoption
requires a separately reviewed API change so the current OpenAI compatibility
contract does not change accidentally before v1.
