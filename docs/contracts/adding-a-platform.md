# Adding a provider platform

Normative checklist for adding a Provider Profile. Every step names the file it
belongs in and the test that fails if it is skipped, so this document is a map
rather than a memory aid — nothing here has to be remembered, only found.

`TestTheChecklistNamesGuardsThatExist` holds the test names below to guards that
are actually in the tree, so a renamed or deleted guard cannot leave a step here
citing something that no longer runs.

## Read this first: the guard is the step

Adding MiniMax in August 2026 produced two defects that reached an operator's
screen. Both were registration steps that had been missed. The useful thing is
which ones:

- A hand-written provider-type list in the Admin write path, which was not in
  this document at all and had no guard. The console offered a type its own
  server refused.
- A flag answering two questions at once, so turning off target enumeration also
  turned off the credential-only connection test.

Every step that *was* in this document, with a named guard, was done correctly —
because skipping one turns a test red with the step's name in the message. The
correlation is not "many steps, therefore mistakes". It is **unguarded steps,
therefore mistakes**.

So the shape of this document changed: each step now states its guard, and a
step with no mechanical guard says so in those words. Do not add a step here
without deciding what fails when it is skipped, and do not describe a step as
covered without having removed the registration and watched the test fail.

## Why it is several places and not one

A single description per platform is not reachable in this codebase, and the
reason is the dependency direction rather than an oversight:

```
domain         no internal dependencies (leaf)
semantic       no internal dependencies (leaf)
compatibility  → domain, semantic
provider       → compatibility, domain, semantic
```

The lowest package that could hold both the primitive bindings and the field
rules is `provider`. But the profile table has to stay in `domain`: it carries
the capability ceiling, and `ProviderProfileBinding.Validate` needs that ceiling
to check a stored record before anything else runs. Move the table up and domain
loses the ceiling; leave the ceiling behind and it is duplicated. Both are worse
than several registrations, so the layering stands and the registrations are made
loud instead of few.

This is the argument against collapsing the declarations into one place. It is
not an argument against collapsing *some* of them, and there is a live proposal
to: `docs/prd/platform-registration-consolidation.zh-CN.md`.

Almost every step below fails a named test when skipped, and the two that do not
are listed under "Steps with no mechanical guard". None of them fails the
compiler — `go build` passes with a profile registered in the table alone — so
the test suite is the thing that tells you what is missing.

## The steps

### 1. The profile identifier

`internal/domain/provider_profile.go` — a `ProviderProfileID` constant. It has to
be a Go constant because every other step refers to it by name at compile time.

Identifiers are never reused, even for a profile that was removed: a reused
identifier silently adopts whatever the old one was registered for.

### 2. The table row

`internal/domain/provider_table.go` — access surface, credential scheme, base URL
template, and the capability `Defaults` and `Ceiling`.

Defaults is what a new connection claims; Ceiling is the most any deployment on
it may declare. Keep them equal unless the difference is a deliberate opt-in an
operator reaches for — `TestOnlyNamedProfilesHaveAWiderCeiling` holds the list of
profiles allowed to differ and what each gap is allowed to contain.

### 3. Operation → primitive bindings

`internal/provider/profile_bindings.go` — which operations the profile serves
and the southbound primitive each one maps to.

Guarded by `TestCeilingWithinProfileManifestOperations`, which walks the domain
table and refuses a profile whose ceiling claims an operation no primitive is
bound to.

### 4. Field-level declarations

`internal/compatibility/provider_fields.go` — what a request may carry that this
profile cannot represent.

Guarded by `TestEveryProfileRegistersItsOwnFieldRules`. Skipping it is the step
that used to be silent: an unregistered profile falls back to the legacy rules,
which are fail-closed and therefore look like success — the platform runs, serves
plain text, and refuses tools, images and structured output with nothing saying
why.

Declare a fact here only if it is a property of a request member. A property of
the target is a capability instead, in step 2: "cannot fetch an image" was a
field declaration written once per endpoint before it became `fetched_image`, and
three spellings of one fact is how you know it was in the wrong layer.

### 5. Endpoint coverage

`internal/compatibility/manifest.go` — one `ProfileCoverage` row per endpoint the
profile serves.

Two guards, in opposite directions:

- `TestEveryChatProfileAppearsInAnEndpointManifest` — a profile reachable through
  no endpoint manifest at all.
- `TestTheManifestDeclaresEverythingTheRulesRefuse` — a member step 4 refuses that
  this row does not declare. The reverse is allowed: a manifest may declare more
  than the rules can derive, because some endpoint members never reach the
  semantic model to be refused by a rule.

`EndpointCompatibilityManifest.Validate` additionally refuses a coverage entry
naming a field the endpoint does not model, and a manifest that omits any profile.

### 6. Adapter construction

`internal/app/provider_adapters.go` — building the adapter, its authorizer and
its endpoint from a stored connection.

Guarded by `TestEveryReachableProfileBuildsAnAdapter`, which walks every
non-withheld profile in the domain table and builds one against a fake secret
per credential scheme, and by
`TestEveryReachableProfileReachesTheNetworkWhenCalled`, which then drives one
generation through it and requires the call to reach the transport.

This step used to be described here as the one no test covers, on the grounds
that it needs a credential and a client. It needs a *plausible* one of each,
which is a fixture. Adding a credential scheme without a fixture fails the first
guard by name rather than going unchecked.

### 7. Provider-type admission — nothing to do, and why

There is no seventh step, and its absence is the thing to understand.

`internal/app/admin_providers.go` gated both credential and connection saves on
`implementedProviderType`, a hand-written switch over provider types. It was the
third copy of that list — after the profile table and `ProviderInstance.Validate`
— and it fell behind exactly the way `provider_table.go`'s own header warns a
private list will: MiniMax was registered in all six steps above, offered by the
console's provider-profiles endpoint, and refused on save with `provider type is
not implemented`. No test saw it, because the new platform's coverage entered
below this gate rather than through it.

It now reads `domain.IsRegisteredProviderType`, which looks the type up in the
same table the console enumerates.
`TestEveryOfferedProviderTypeIsAcceptedOnSave` holds the two together, and
`TestEveryRegisteredProviderTypePassesInstanceValidation` holds the table to the
switch in `ProviderInstance.Validate` that is still a switch.

Nothing to do for a new platform. **Do not reintroduce a list here.**

### 8. Anthropic-wire platforms only

A platform speaking Anthropic Messages touches three more registrations, each
holding a different half of native mode:

- `internal/compatibility/anthropic/native.go` — the schema that validates and
  forwards the caller's bytes.
- `internal/gateway/service.go`, `isNativeAnthropicProfile` — whether native is
  offered for this (profile, surface) pair at all.
- `internal/domain/provider_profile.go`, `ProfileSendsAnthropicBetas` — whether
  an `anthropic-beta` header may be forwarded. Deliberately a subset: a platform
  can serve native and still not accept beta headers.

Guarded by `TestNativeAnthropicListsAgree`, which requires the first two to
match exactly and the third to be a subset, and by `TestNoNativeProfileIsWithheld`.
The dangerous direction is a profile offered as native with no schema
registered — that is a byte-for-byte forward with nothing inspecting it.

`internal/provider/capability_detection.go`, `reasoningProbeEffort`, also has to
exclude the profile: the probe reads its answer through the portable Chat
mapping, which refuses the signed thinking blocks an Anthropic-wire upstream
returns, so asking pays for an answer that cannot be read. Guarded by
`TestEveryAnthropicWireProfileIsExcludedFromTheReasoningProbe`.

### Steps with no mechanical guard

Three, named here so they are not mistaken for covered.

**A generation primitive left out of `semanticGenerationPrimitives`**
(`internal/provider/primitive.go`). Nothing fails. `Resolve` falls back to the
legacy Chat path, and an adapter whose Responses branch translates will address
the same endpoint anyway. The cost is a lossier translation — the semantic
request passes through the OpenAI Chat intermediate representation and takes its
losses, which the profile's field rules do not declare because on the semantic
path they do not happen. This was checked by removing the entry and watching
nothing break; the opposite mistake, declaring a primitive semantic whose
adapter is not a `SemanticGenerator`, does fail by name.

**The model catalogue** (`internal/modelcatalog/builtin.go`). Skipping it is
legal, as below. Nothing enforces that a platform's models are seeded.

**A wrong primitive in `profileOperationTable`** that still leaves every
primitive constant bound — swapping two profiles' primitives, say.
`ProfileManifest.Validate` checks a builtin manifest against the table it was
built from, which is a tautology; it stays meaningful only for a manifest a
caller supplies. This is not a protection the merge removed: the two tables it
replaced could only detect each other *disagreeing*, and a binding written
wrongly in both passed then too.

**Nothing catches it.** This entry used to name two guards, and the v0.5.0 review
measured both by making the mistake on purpose and running the tree:

- Swapping the chat and stream primitives of `ProfileOpenAIChatEmbeddings` and
  `ProfileDeepSeekChat` — every constant still bound — leaves the whole of
  `internal/` green.
- `TestEveryPrimitiveConstantIsBoundBySomeProfile` only notices a mistake that
  *orphans* a constant, which a swap does not.
- A platform's own wiring test does not read this table at all.
  `TestMiniMaxWiringAddressesOneRoutePerProfile` builds the adapter through
  `adapterBuilders` and asserts the route it addresses; swapping the MiniMax Chat
  and Responses primitives leaves it passing. The route is a property of the
  adapter builder, not of this table, so no route assertion can validate a
  binding.

Write the wiring test anyway — it is what catches step 6 — but do not count it
here. What a guard would have to assert is the link nothing currently expresses:
that the primitive named for `(profile, operation)` is the one the adapter for
that profile actually executes for that operation. Until something asserts it, a
binding written wrongly is caught by review or not at all, and the cost is a
wrong primitive on the attempt records and in `filterPrimitiveTargets` — an
error that never turns a test red.

## After the steps

`TestProviderProfilesGoldenMatchesConsoleFixture` regenerates the console's view
of the profile matrix. Run it with `HALRO_UPDATE_GOLDEN=1` and read the diff: it
is what every connection form will offer, and the diff is the review.

If the platform's models should not have to be declared by hand, add them to
`internal/modelcatalog/builtin.go` under the seeding policy stated at the top of
that file — exact identifiers, from documentation, with the sources and the date.
Skipping this is legal and costs the operator a manual capability declaration,
which is a widening, which costs a revalidation and a route taken out of service.
