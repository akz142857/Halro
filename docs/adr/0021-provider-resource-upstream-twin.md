# ADR 0021: Whether a provider resource must have an upstream twin

- Status: Proposed — the Decision section is deliberately unfilled
- Date: 2026-08-13
- Supersedes: nothing
- Related: ADR 0009 (Phase 2 resource ownership), ADR 0006 (Anthropic Messages facade execution modes)

## Context

`domain.ProviderResource` represents a file, batch, or async invocation that a
Project owns. It carries an `UpstreamID`, and every lifecycle path treats that
identifier as a live handle on something the upstream also holds:

- `GetFile` calls `adapter.GetFile(ctx, requestID, resource.UpstreamID)`
  unconditionally (`internal/gateway/inference_resources_store.go:318`).
- `CleanupExpiredProviderResource` deletes the upstream object before it
  forgets the record (`:480`).
- `CreateBatch` begins by resolving `input_file_id` to a Halro file resource
  whose owner adapter implements `ResourceInferenceResourcesAdapter` (`:501`).

This assumption has never been written down because nothing has contradicted
it. Every resource Halro has ever created was created by uploading to, or
starting work on, an upstream that then held it too.

Anthropic's Message Batches API contradicts it. A batch is created with its
requests inline — `{"requests":[{"custom_id":…,"params":{…}}]}` — and refers to
no file at all. Halro's northbound surface is the OpenAI shape, where a batch is
created from an `input_file_id` pointing at a previously uploaded file.

Keeping that northbound shape for Anthropic therefore requires a file resource
that exists only in Halro: the caller uploads JSONL, Halro stores the bytes, and
at batch-creation time it reads them back and inlines them. Anthropic never sees
the file and has no identifier for it. All three paths above would then be
addressing an upstream object that does not exist.

Two further facts bound the choice.

The Anthropic Messages profile declares no files operation
(`internal/provider/profile.go:368`), so `CreateBatch`'s first step cannot
succeed for it today even before the resource question is reached.

Halro already keeps the uploaded bytes locally — `writeResourceObject` stores
them under the resource object directory and `DownloadFile` serves them from
there rather than from the upstream (`inference_resources_store.go:116-152`,
`:330-352`). The data needed to inline the requests is on hand. What is missing
is a way to say "this resource has no upstream twin" without every path that
assumes one breaking.

### What a review already established

A four-role review on 2026-08-13
([`docs/review/260813/batch-design-review.zh-CN.md`](../review/260813/batch-design-review.zh-CN.md))
rejected the three implementation decisions that had been proposed on top of the
unstated assumption that this question was already settled. Its findings
constrain any option chosen here:

- Batch results are outbound model output. Persisting them writes a response
  body outside its one-time response path, which this project's own invariants
  forbid; `internal/redaction` would have to run on the way to disk.
- `results_url` is a URL the upstream chooses. Dialling it would let the
  upstream decide which host Halro connects to, which is what
  `internal/safetransport` exists to prevent. Halro must build the URL from its
  configured endpoint instead.
- There is exactly one write ceiling today,
  `maxInferenceResourcesResponseBytes` (`internal/provider/openai/inference_resources.go:17`),
  and it is an adapter-private constant rather than a gateway policy. Removing
  it for one path removes the only bound; the read path has none at all, since
  `DownloadFile` reads whole objects into memory.
- `GET /v1/batches/{id}` runs under `route_total_timeout`, two minutes by
  default, and raising it forces `server.shutdown_timeout` up with it.

A defect the same review surfaced — batch file identifiers were never translated
— has since been fixed (`fix(gateway): give a batch's files identifiers the
caller can use`). That fix deliberately stayed inside the existing assumption:
the result files it registers do have upstream twins, so it needed no answer to
this question.

## Options

### A. Extend the resource model to admit local-only resources

Add an explicit notion of a resource with no upstream twin, and teach every
lifecycle path to branch on it: metadata answered from the record rather than
the upstream, expiry cleanup that deletes only the local object, backup that
still expects the object to exist (`internal/app/backup.go:688-691`).

Keeps one northbound shape across providers, which is the premise the gateway is
built on: an application should not have to know which upstream serves its
Project. Applications keep using the OpenAI SDK.

The cost is a permanent fork in a core domain concept. Every future path that
touches `ProviderResource` inherits the obligation to ask which kind it has, and
the answer is not obvious at the call site. The fork cannot be removed later
without breaking whatever came to depend on it.

### B. Expose Anthropic batches natively instead

Serve `POST /v1/messages/batches` as a native passthrough, the way
`nativeOperationPrimitive` already handles native operations
(`internal/provider/profile.go:119-121`). No local-only file resource is needed,
because callers submit inline requests exactly as Anthropic defines them.

Removes an entire translation layer: no JSONL-to-inline conversion, no results
materialised into a synthetic file to satisfy an `output_file_id`, no new
resource kind.

The cost lands on the caller. An application using the OpenAI SDK cannot reach
this endpoint, so a Project that moves between OpenAI and Anthropic batches has
to change code — which is the thing the portable surface exists to avoid. It
also adds a second northbound batch surface with different semantics, and the
compatibility manifest would have to say so plainly.

### C. Do not adapt Anthropic batches

Leave `/v1/batches` OpenAI-only, and record that batch processing is not
portable across providers today.

Costs nothing to build and forfeits the 50% batch discount on Anthropic. The
gap is already recorded in
[`docs/todo/provider-adaptation-gaps.zh-CN.md`](../todo/provider-adaptation-gaps.zh-CN.md);
choosing this makes the record permanent rather than pending.

## Decision

**Unfilled.** This ADR exists to put the choice in front of a human, not to
record one already made.

Whoever fills it should say which option, and — more usefully to a later reader
— why the others were refused. If A is chosen, the fork it introduces should be
named here, so the next person to add a resource kind knows the question exists.
If B is chosen, this is the place that answers "why can't I use the OpenAI SDK
for Anthropic batches".

## Consequences

To be written with the decision. Whichever option is taken, the constraints the
review established stand: outbound results pass through redaction before they
are stored or returned, Halro builds upstream URLs from its configured endpoint
rather than following ones the upstream supplies, and no path removes the only
write ceiling without putting a bounded one in its place.
