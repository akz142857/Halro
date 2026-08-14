# ADR 0021: Whether a provider resource must have an upstream twin

- Status: Accepted
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
[`docs/prd/provider-adaptation-gaps.zh-CN.md`](../prd/provider-adaptation-gaps.zh-CN.md);
choosing this makes the record permanent rather than pending.

## Decision

**Option A.** A provider resource does not have to have an upstream twin.
`UpstreamID` may be empty, and that is an ordinary state rather than an
exception: it means Halro holds this object and the upstream has no need to know
it exists.

The reason is what a batch is, not what Anthropic is. Batching is a modality,
like embeddings or image generation. Which providers can serve a modality is a
property of the deployment an operator configured, and answering that question
is the gateway's job — not the caller's. An application addresses `/v1/batches`
and a public alias; if no deployment behind that alias can batch, it is told so
in those terms.

That is already how every other capability behaves here, and the routing code
already draws the distinction the caller needs (`internal/gateway/service.go:234-239`):
an alias that exists but cannot serve the operation answers
`400 unsupported_feature`, and an alias that does not exist answers
`404 model_not_found`. `/v1/embeddings` does not become an OpenAI-only endpoint
because DeepSeek has no embeddings; profiles that cannot serve it are simply not
in its list and routing passes them by. Batches have no reason to be different.

### Why the other options were refused

**B, a provider-shaped endpoint, was refused because it moves configuration into
the caller's code.** An application would have to know which upstream serves its
alias in order to choose an endpoint, and the alias exists precisely so that it
does not. Swapping providers — the most likely reason to want batching in the
first place, since the discount is the motive — would become a code change. The
count_tokens precedent does not carry: that is a query tool, not a workload, and
`/v1/messages` itself is a protocol shape that ten profiles serve, not a
provider's private door.

This does not close the door on an Anthropic-shaped batch surface later.
`/v1/messages/batches` would be the batch counterpart of `/v1/messages`: a
second *protocol* shape, chosen for native richness the portable shape cannot
carry, exactly as `Halro-Route-Mode: native` is chosen today (ADR 0006). That is
a different thing from a provider-named endpoint, and it is not a substitute for
this decision.

**Naming providers in paths** — `/v1/chat/anthropic/completions` and the like —
was considered and refused for the same reason, plus three of its own: it names
the provider when what actually varies is the wire protocol, so DeepSeek and
Kimi would get byte-identical duplicate endpoints; official SDKs address
`/v1/chat/completions`, so every provider would need its own base URL and the
application its own client per provider; and the alias already carries the
answer, making the path a second source of truth for it.

**C, leaving batches OpenAI-only, was refused** because the gap is not a
capability boundary of the product but an artefact of one implementation. It
remains the right answer if no workload needs Anthropic batching; that is a
scheduling question, not an architectural one.

### What the assumption actually was

Every resource having an upstream twin was never a rule. It was a property of
OpenAI being the only implementation. Halro already stores uploaded bytes itself
and serves them from its own object directory rather than from the upstream
(`internal/gateway/inference_resources_store.go:116-152`, `:330-352`); the
upstream copy is a detail of the OpenAI path. Admitting a resource without one
is not adding an exception to the model. It is removing a coincidence that had
been mistaken for a rule.

## Consequences

Three paths currently assume the twin and must ask instead of assuming: metadata
answered from the record rather than fetched (`:318`), expiry cleanup that
removes the local object without calling upstream (`:480`), and batch creation
that no longer requires the input file's owner to serve files (`:501`). Backup
is unaffected: a local-only resource always has its object, which is what backup
already requires (`internal/app/backup.go:688-691`).

The constraints the four-role review established are not relaxed by this
decision, and the implementation plan has to answer all of them:

- Batch results are outbound model output. They pass through redaction before
  they are stored or returned; `internal/redaction` has a streaming inspector
  for exactly this.
- Halro builds upstream URLs from its configured endpoint. `results_url` is a
  URL the upstream chose, and following it would let the upstream decide which
  host Halro dials.
- No path removes the only write ceiling without putting a bounded one in its
  place, on both the write and the read side.
- `completion_window` has no Anthropic counterpart. Per this project's rule that
  unsupported fields are rejected rather than dropped, only the value Halro can
  honour is accepted.

When results are fetched — lazily inside `GET /v1/batches/{id}`, or by a
background pass — is left to the implementation plan. The review split on it,
and the split turned on this decision, which is now made.
