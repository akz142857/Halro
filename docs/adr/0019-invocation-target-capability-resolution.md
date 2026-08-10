# ADR 0019: Invocation targets, capability claims, and deployment variants

- Status: **Implemented 2026-08-10.** The decision below is the 1.0.0 contract.
- Date: 2026-08-10
- Tracking: `docs/todo/provider-model-selection-and-capability-resolution.zh-CN.md`
- Related: `docs/contracts/provider-capabilities.md`

## Context

A Provider catalog can prove that an account can see a target, but it cannot by
itself prove which requests Halro may safely route to that target. The same
upstream identifier may also be reachable through several bindings with
different protocol and capability ceilings. Treating a `/models` item as a
ready Deployment therefore leaks capabilities across scopes and makes the UI
ask administrators to reconstruct internal protocol details.

## Decision

Halro separates three concepts:

1. An **Invocation Target** is the exact upstream identity that receives a
   request: model ID, Azure deployment, Bedrock foundation model/inference
   profile/provisioned throughput, or custom-endpoint model. Discovery proves
   availability only. Adapters report their target kinds and discovery features
   so the Admin UI has no Provider-type switch.
2. A **Capability Claim** is one assertion about one capability at the exact
   `(provider, target kind, target, binding, profile, location)` scope. Claims
   carry source, status, evidence, observation/expiry, and revision. Missing
   evidence stays unknown; contradictory evidence fails closed. Sources are
   `builtin_catalog`, `provider_metadata`, reserved `signed_catalog`,
   `verified_probe`, and `operator_declared`.
3. A **Deployment Variant** is one target resolved through exactly one enabled
   binding. Its capabilities are the dependency-closed intersection of active
   claims and the binding ceiling. Resolution emits zero to many variants, but
   one Deployment save selects exactly one variant and may only narrow it.

Provider metadata enters resolution only through an Adapter-owned allowlist.
Names, owners, model families, and unknown Provider fields never create claims.
Builtin claims do not expire; Provider metadata and probes are bounded evidence.
Expiry affects future resolution only and never rewrites an existing immutable
Deployment snapshot.

The Admin API is `GET /providers/{id}/invocation-targets` plus exact-target
`.../resolution`. The pre-1.0 `/providers/{id}/models` route is removed without
an alias. A save binds Provider, target, binding, profile, claim, and variant
revisions. If any reviewed input moved, the server returns
`409 resolution_changed`, bounded `mismatches[]`, and the latest resolution;
the UI requires confirmation again.

The normal UI is Provider → target → read-only capability summary. It does not
ask for a purpose or show the capability matrix. One variant is selected
automatically, several require choosing one, and zero links back to Provider
configuration. Unknown targets offer explicit verification or Advanced
onboarding. Advanced onboarding chooses a real binding and can only narrow its
ceiling.

## Security and release boundary

Catalog responses retain only allowlisted normalized metadata. Credentials,
headers, raw responses, Provider error bodies, and model output are not stored
as claims. Metrics use bounded labels and never target IDs.

The optional `signed_catalog` producer is implemented separately by ADR 0020.
It does not change the Claim scope, narrowing, conflict, or immutable snapshot
rules in this ADR.

## Invariants and review checklist

- Discovery never creates capabilities.
- Claims never escape their exact target/binding/profile/location scope.
- Missing and expired evidence never becomes unsupported and never widens.
- Conflicts and dependency failures emit no usable variant.
- A variant contains one binding and cannot exceed its ceiling.
- A save accepts one current variant revision and an optional retained subset.
- Existing snapshots do not change after catalog refresh or claim expiry.
- Unknown Provider fields and model names are ignored for capability inference.
- Ordinary UI has no Provider-name switch, purpose selector, or capability grid.
- Dynamic catalog transport is disabled by default and never enters the request
  path; enabling it does not change any saved snapshot automatically.

Changing any item above requires a versioned contract/ADR change. Adding an
Adapter target kind, a reviewed metadata mapping, or an exact builtin model
entry does not change the schema and may ship in a normal release.

## Consequences

New Provider models can appear immediately even when their capabilities remain
unknown. Reviewed structured metadata can resolve a new model without a Halro
release, while unsupported fields remain inert. A target may legitimately
produce zero or several variants, and administrators see that ambiguity rather
than a guessed binding. Stored Deployments remain deterministic because request
routing reads their immutable snapshot, not a mutable catalog.
