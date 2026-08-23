# Adding a provider platform

Normative checklist for adding a Provider Profile. Every step names the file it
belongs in and the test that fails if it is skipped, so this document is a map
rather than a memory aid — nothing here has to be remembered, only found.

`TestTheChecklistNamesGuardsThatExist` holds the test names below to guards that
are actually in the tree, so a renamed or deleted guard cannot leave a step here
citing something that no longer runs.

## Why it is six places and not one

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
than six registrations, so the layering stands and the registrations are made
loud instead of few.

Every step below fails a named test when skipped. None of them fails the
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

`internal/provider/profile.go` — which operations the profile serves and the
southbound primitive each one maps to.

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

`internal/app/providers.go` — building the adapter, its authorizer and its
endpoint from a stored connection.

This is the one step no test covers, because it needs credentials and a client.
It fails at runtime instead, with `provider profile is not implemented`, on the
first attempt to build a connection.

## After the steps

`TestProviderProfilesGoldenMatchesConsoleFixture` regenerates the console's view
of the profile matrix. Run it with `HALRO_UPDATE_GOLDEN=1` and read the diff: it
is what every connection form will offer, and the diff is the review.

If the platform's models should not have to be declared by hand, add them to
`internal/modelcatalog/builtin.go` under the seeding policy stated at the top of
that file — exact identifiers, from documentation, with the sources and the date.
Skipping this is legal and costs the operator a manual capability declaration,
which is a widening, which costs a revalidation and a route taken out of service.
