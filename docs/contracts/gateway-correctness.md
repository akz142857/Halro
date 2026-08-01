# Gateway correctness contract

This contract is normative for v1.

## Request and attempt

- A client request may produce multiple provider attempts.
- Client RPM and request concurrency count once per request.
- Provider concurrency, token usage, and cost count once per attempt.
- Every attempt receives an independent durable budget reservation.
- Retry/fallback stops when a new reservation cannot be obtained.
- A failure is ambiguous when the request may have reached the provider but no
  authoritative result was received (for example, a timeout or connection loss
  after dispatch). Ambiguous attempts are never retried or sent to a fallback,
  even if the transport error is otherwise transient. This prevents duplicate
  generation, side effects, and charges. The attempt is conservatively settled
  and still contributes to circuit-breaker availability state.
- Explicitly classified pre-execution failures and safe provider failures may
  retry or fall back within the configured attempt limits.

## Delivery boundary

The delivery boundary is the first response payload successfully written to the client.

- Retry/fallback is allowed before the boundary.
- Deployment switching is forbidden after the boundary.
- A streaming error after headers is an abnormal stream termination, not a new HTTP response.
- Normal SSE completion emits `[DONE]` exactly once; abnormal completion emits it zero times.

## Cancellation and accounting

Network work inherits request cancellation. Once an attempt may have reached a provider, ledger settlement uses a short cleanup context that is independent of client cancellation.

## Stream ownership

The handler is the sole stream owner. `Close` is idempotent. Frame, semantic event, redaction tail, tool arguments, and pending downstream write sizes are bounded. A downstream write failure cancels upstream work.

## Deadline hierarchy

- header/body read deadline;
- non-stream route total or stream-establishment deadline;
- per-attempt connect/TLS/response-header deadline;
- stream idle deadline;
- downstream per-write deadline;
- maximum stream duration.

Long SSE responses do not use one fixed `http.Server.WriteTimeout`.
