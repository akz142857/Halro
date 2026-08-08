# Endpoint compatibility manifests

[`endpoint-manifests.json`](endpoint-manifests.json) is the reviewed,
machine-readable compatibility contract for Halro's currently exposed LLM
endpoints. It records the northbound profile and revision, method/path, accepted
and emitted field sets, stream events, state semantics, SDK test matrix,
documented deviations, and the built-in provider profiles eligible for routing.
Each profile also has an explicit coverage record naming its own maturity
status, northbound fields that cannot be represented, and any declared semantic
transform. Endpoint maturity never promotes an individual provider profile:
for example, `/v1/embeddings` remains compatible while the Phase 2 Titan Text
Embeddings V2 coverage remains experimental.

The JSON is generated conceptually from `compatibility.BuiltinEndpointManifests`
and enforced as a golden snapshot by the Go test suite. Any change requires a
deliberate compatibility review. A provider profile listed in the manifest is
filtered by request-derived capabilities, field-level coverage, and deployment
evidence. Unsupported fields are rejected before provider I/O instead of being
silently dropped.

Compatibility tests are layered: registry tests verify immutable
profile-to-primitive bindings and canonical bridge invariants, while each real
Provider adapter owns transport fixtures for its request, response, and stream
wire format. The bridge fixture deliberately does not claim six-provider wire
compatibility.

`POST /v1/chat/completions`, `POST /v1/embeddings`, and the explicitly
stateless `POST /v1/responses` tier are published as compatible. The Responses tier treats an
omitted `store` as false and rejects all state/resource fields before Provider
I/O; see [ADR 0005](../adr/0005-stateless-responses-facade.md). Future API
families and any stored Responses tier must add separate, versioned manifests
before they can be described as compatible.

The Phase 2 media and resource endpoints are published as `experimental`.
Gateway contract tests and provider transport fixtures do not substitute for an
official SDK black-box matrix or real-provider release gates. In particular,
`/v1/rerank` and `/v1/async/invocations` are Halro extensions, not OpenAI
surfaces, and therefore never claim OpenAI SDK coverage. Their SDK matrices stay
empty until a truthful client compatibility suite exists.
