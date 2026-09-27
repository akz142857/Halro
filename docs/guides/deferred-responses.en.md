# Deferred Responses (`background: true`)

Applies to `POST /v1/responses`. See [ADR 0024](../adr/0024-deferred-response-tier.md) for the contract and [`endpoint-manifests.json`](../compatibility/endpoint-manifests.json) for the machine-readable `openai.responses.create.v1`, `openai.responses.get.v1`, `openai.responses.cancel.v1`, and `openai.responses.delete.v1` entries. [简体中文版](deferred-responses.zh-CN.md).

## In one sentence

Submission immediately returns a `queued` Response object, so the connection can close. Retrieve the result later using the same ID. The upstream provider still sees an ordinary synchronous generation request; it does not know that Halro delivers the answer later.

## Prerequisite

**Off by default; enable it per Project.** In the Admin Console, open **Projects → Deferred responses** and enable background submission for the Project.

This opt-in changes what the instance keeps in its data directory. Before it is enabled, caller-written content is persisted only by the separately opt-in failure capture feature. With deferred responses enabled, the **complete answers to successful requests** are routinely persisted as well. Each answer is sealed with the Master Key and bound to its record ID and Project ID. It is retained for no more than 24 hours.

## Submit

```http
POST /v1/responses
Authorization: Bearer gw_...
Idempotency-Key: <optional; strongly recommended>

{"model": "my-alias", "input": "...", "background": true}
```

Immediate response:

```json
{"id": "resp_...", "object": "response", "status": "queued", "background": true,
 "model": "my-alias", "output": [], "usage": null}
```

- `background: true` cannot be combined with `stream: true`: deferred retrieval promises one final answer, not a replayable event stream.
- `store: true` is still rejected, including when sent with `background`. In the OpenAI API, `store` asks the provider to retain the response; Halro does not ask the upstream to do that. Accepting it would make a false claim to the caller.
- Submission consumes one RPM slot because it is a request, but it does **not** consume a concurrency slot while it waits in the queue.
- The queue is bounded (default: 100). A full queue returns `429` with `Retry-After`.

## Retrieve

```http
GET /v1/responses/resp_...
```

| `status` | Meaning | Response header |
| --- | --- | --- |
| `queued` | Accepted and waiting to run | `Retry-After` |
| `in_progress` | Calling the upstream provider | `Retry-After` |
| `completed` | Succeeded; `output` and `usage` are complete | — |
| `failed` | Failed; `error` explains why | — |
| `cancelled` | Cancelled | — |

Follow the server's `Retry-After` header when polling. It increases with time already waited, from 1 to 10 seconds. The interval is a header rather than a field on the Response object because nonstandard object fields can break some SDK parsers.

**Polling creates no accounting event and makes no upstream call.** The result is already on local disk. A Batch `GET /v1/batches/{id}` is different: it asks the upstream for status on every poll, so each poll is a real, accounted request. The distinction is whether an upstream handle exists, not the endpoint's style.

A missing record, a record belonging to another Project, or a record past its retrieval grace period returns `404`.

## Cancel and delete

```http
POST   /v1/responses/{id}/cancel
DELETE /v1/responses/{id}
```

- Cancelling a `queued` request is definitive: no upstream call has been sent.
- Cancelling an `in_progress` request is best effort: the upstream call may already have happened. ADR 0011 requires conservative accounting, and the terminal record explicitly says it may have incurred an upstream charge.
- Cancelling a terminal request returns `409`.
- `DELETE` removes the record and its two sealed objects. It does not reverse accounting that already occurred. Deleting a request that still owes an answer returns `409`; cancel it first.

## When the Project reaches its TPM or concurrency limit

The request remains `queued` rather than failing; that is why the queue exists. Waiting is bounded by queue depth and the 24-hour TTL. A request that never gets a turn before expiry becomes `failed` with `deferred_response_expired`.

## Execution time limit

Each deferred execution uses the same `route_total_timeout` limit as a synchronous request (default: two minutes). A timeout produces `failed` with `deferred_response_timeout` and **may still have incurred a charge**. Without this overall bound, a body returned one byte at a time could hold one of the limited workers indefinitely: SafeTransport's connection and response-header timeouts alone do not bound the whole body.

## Answer size limit

A deferred answer is limited to 1 MiB. If it exceeds that limit, the request becomes `failed` with `deferred_response_too_large`, **even though the upstream call happened and was charged**. The limit governs what Halro can retain; Halro cannot know the final answer size before the upstream produces it. Use a synchronous request when you need a longer answer.

## Redaction happens at generation, not retrieval

Outbound redaction runs before the answer is written to disk, using the redaction policy in effect **at generation time**. Later changes to the Project's policy do not rewrite stored answers. Synchronous answers are also redacted when generated, but a deferred answer can remain on disk for up to 24 hours. After tightening a policy, explicitly `DELETE` any stored answers that must be removed immediately.

## Retention

- TTL: 24 hours, matching failure capture because both retain similar material.
- After the first successful retrieval, a 15-minute grace period allows repeated retrieval. An HTTP 200 leaving Halro does **not** prove the caller received it, so retrieval does not immediately delete the result.
- The record and its objects are removed when the grace period ends, the TTL expires, or the caller explicitly uses `DELETE`.

## A required rule for your retry logic

**An `in_progress` request does not survive a Halro restart.** After restart, every deferred request that was executing becomes `failed`. Its error explains that an upstream charge may have occurred and that the answer cannot be retrieved.

A Batch has an upstream handle that can be queried after restart. A deferred generation is merely a synchronous HTTP call to the provider: when Halro's process dies, its socket dies too. Halro cannot determine whether the call finished, was charged, or produced an answer. ADR 0011 treats that ambiguity conservatively; `failed` is the only honest status.

A `queued` request is different: it was never sent upstream and is queued again after restart.

## Accounting

Once dequeued, a background request follows the **same Ledger path** as the corresponding synchronous request: the same reservation, attempt sequence, and settlement order. It executes through the same code path, not a parallel implementation.

Reservation happens when the request leaves the queue, before contacting the upstream, rather than at submission. A request that waits ten minutes does not reserve ten minutes of daily budget. The tradeoff is that an over-budget submission can be accepted and later report `failed`.
