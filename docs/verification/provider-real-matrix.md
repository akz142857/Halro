# Real-account Provider matrix

The final release requires a passing real-account smoke for every GA Provider
profile on the exact RC commit. Unit, golden, fake-server, SDK compatibility,
and capability-contract tests remain mandatory but do not replace this gate.

The matrix runner covers OpenAI, Azure OpenAI, DeepSeek, and one explicitly
reviewed OpenAI-compatible endpoint. It verifies non-stream chat, semantic SSE,
embeddings wherever the profile declares embeddings, and the same bounded
fixed-protocol capability-detection plan used by the Admin control plane.
Capability detection is enabled only in this opt-in child process, may incur
additional charges, and must verify `chat` for the configured model without
exceeding eight calls. Configure dedicated,
budget-limited credentials through environment variables using these prefixes:

- `HALRO_MATRIX_OPENAI_...`
- `HALRO_MATRIX_AZURE_OPENAI_...`
- `HALRO_MATRIX_DEEPSEEK_...`
- `HALRO_MATRIX_OPENAI_COMPATIBLE_...`

Each prefix requires `BASE_URL`, `API_KEY`, and `MODEL`. OpenAI, Azure, and the
reviewed compatible endpoint also require `EMBEDDING_MODEL`; Azure additionally
requires `API_VERSION`. Then run:

```bash
go run ./tests/provider-matrix \
  -commit '<exact-lowercase-40-character-RC-commit>' \
  -output provider-matrix.json
```

Credentials are transferred to only the selected child test through its
environment, removed from captured output, and never included in the evidence
file. The runner fails closed for missing profiles, failed requests, or per-
profile timeout. Archive the `0600` JSON evidence with RC artifacts. Review it
before sharing because third-party toolchains can change their diagnostics.

The safe evidence contains bounded call/supported counts and
`stable_negative=not_configured` when no reviewed, stable unsupported model is
available. It never contains the model ID, fixed probe input, generated output,
Provider error body, request ID, endpoint, or credential. A transient error is
never accepted as `unsupported`; the fake-server contract tests cover those
negative classifications even when a real account has no stable negative
model.

Gemini and Bedrock are Beta and therefore do not satisfy or block the GA matrix,
but each has a separate opt-in real smoke test under its adapter package. Run
those when the corresponding Beta is included in an RC deployment.

## Invocation-target resolution RC checks

For an RC that includes the cross-provider selection flow, archive a second
Admin control-plane checklist alongside the matrix evidence:

- OpenAI and DeepSeek list account-visible targets by name; a newly visible,
  uncatalogued ID remains `unknown` and never inherits capabilities by name or
  owner.
- Anthropic and Gemini retain only the reviewed structured metadata fields.
  Unknown fields are inert; Gemini generation methods establish operations only.
- Bedrock continues to filter inactive and non-invocable summaries, retains
  input/output modalities, and keeps region/ARN semantics exact.
- Azure works with only its data-plane credential through manual Deployment
  name plus explicit canonical-model mapping; absence of ARM identity is not a
  failure.
- The Admin selector renders one visible name column, keyboard selection, a
  read-only summary for one variant, an explicit radio choice for many, and a
  Provider-settings exit for zero variants.
- Saving a stale variant returns `resolution_changed`, loads the latest
  resolution, and keeps Save disabled until the operator confirms again.

These checks require real Provider accounts on the exact RC commit. Their
absence remains an external release gate, not a reason to substitute fixture
evidence or mark the account behavior as verified.

### Local fixture browser RC evidence (2026-08-10)

The repository-local browser pass used the production Admin bundle with fixture
Provider responses and verified:

- an unknown target exposes exactly the explicit verification and Advanced
  onboarding exits before any detection request is sent;
- the selector and result regions expose their combobox/listbox, option, status,
  fieldset/legend, and button/link semantics to the accessibility tree;
- the selector remains keyboard operable; and
- at a 390 × 844 viewport the deployment modal and selector produce no
  horizontal page overflow.

This is local fixture evidence for the Phase 3 Admin UX gate only. It does not
establish account-visible catalogs, permissions, model availability, billing,
or invocation behavior for any real Provider. The real-account checks above
remain pending on the exact RC commit and continue to block release where the
corresponding profile is in scope.

## Dynamic signed catalog local security evidence (2026-08-10)

The Phase 4 repository-local gate passed with:

```bash
go test ./...
go vet ./...
go test -race ./internal/modelcatalog ./internal/app \
  -run 'Test(Manager|SignedSnapshot|BundledSnapshot|ParseTrustRoots|ProductionTrustRoots|SignedCatalog|Invocation|CapabilitySnapshot|ResolutionMetric|RuntimeWrites)'
npm test --prefix web
npm run build --prefix web
```

The focused suite covers disabled-mode zero network, SafeTransport private/DNS
rejection, environment-proxy bypass, redirect refusal, compressed and decoded
limits, compression ratio, strict fields, tampered signatures, schema and
capability-dictionary bounds, expiry, revision pinning, rollback and sequence
reuse, exact revocation, old/new key overlap, mode-0600 atomic last-known-good,
degradation/recovery, and continued use of existing immutable Deployment
snapshots when the signed catalog is unavailable. The production Admin bundle
also passed its artifact secret scan.

This evidence does not claim that GitHub environment protection, two distinct
reviewers, a production KMS/HSM key, repository public-root variable, or a live
published catalog has been configured. Those are explicit production
activation gates in `docs/runbooks/model-catalog-publishing.md`; dynamic updates
must remain disabled until they are evidenced for the target repository and
release.

## AWS Bedrock Mantle: no real-account evidence (2026-08-12)

The three Bedrock Mantle profiles — `bedrock.mantle.openai.chat.v1`,
`bedrock.mantle.openai.responses.v1`, `bedrock.mantle.anthropic.messages.v1` —
have **no real-account evidence at any commit**. The matrix runner does not
cover them, no `HALRO_MATRIX_BEDROCK_MANTLE_...` prefix exists, and no request
from this build has reached a real Bedrock Mantle endpoint.

What is pinned instead, entirely against fake servers:

- request paths `/v1/chat/completions`, `/v1/responses`, `/anthropic/v1/messages`;
- credential rendering: bearer `Authorization` on the OpenAI-shaped profiles,
  `x-api-key` on the Anthropic Messages profile, with the other credential
  headers explicitly cleared;
- `anthropic-version` pinned on the Messages profile, `store:false` on Responses;
- no project or workspace header on any profile, so requests are associated with
  the account's default Bedrock project;
- 401 and 403 classified as authentication failures that are neither retried nor
  failed over to another deployment;
- probe cost shape: the OpenAI-shaped profiles read one model's metadata, while
  the Messages profile issues a real one-token inference call.

These come from AWS documentation and this repository's code. They are contract
tests, not evidence: they prove Halro sends what the documentation describes,
not that the service accepts it.

### Running the Mantle smoke when it is authorised

The harness exists; no run has happened. It is opt-in twice over: the matrix
runner ignores Beta profiles unless asked, and the smoke itself skips unless
every variable is set.

```bash
export HALRO_MATRIX_BEDROCK_MANTLE_BASE_URL="https://bedrock-mantle.<region>.api.aws"
export HALRO_MATRIX_BEDROCK_MANTLE_API_KEY="<dedicated, budget-limited Bedrock API key>"
export HALRO_MATRIX_BEDROCK_MANTLE_MODEL="<exact upstream model id>"
export HALRO_MATRIX_BEDROCK_MANTLE_MANTLE_PROFILE="chat"   # or responses, messages
# optional; omit to exercise the account default project
export HALRO_MATRIX_BEDROCK_MANTLE_BEDROCK_PROJECT_ID="proj_..."

go run ./tests/provider-matrix \
  -commit '<exact-lowercase-40-character-commit>' \
  -include-beta \
  -output provider-matrix.json
```

Each run exercises one wire profile with a non-stream and a streaming call, and
requires both to report usage — a run Halro could not account for is not
evidence that Halro can serve the profile. Three wire profiles is three runs.

`-include-beta` never moves the GA release gate: Beta results carry
`tier: "beta"` and `passed` counts GA results only. Without the flag the Beta
rows are still emitted with `status: "not_run"`, so silence cannot be read as
coverage.

### What a Mantle evidence row may contain

Each row records the cell it covers — `region`, `wire_profile`,
`authentication`, `project_mode` — plus a `target_digest`. The digest is
`sha256(region, wire profile, authentication, project mode, exact model)`: it
lets a later reader confirm two runs used the same target, or match a claimed
target against a custody record they hold locally, without this shared file
naming an account's model entitlements. The exact model, the key, the account
ID, the project ID, request and response bodies, and provider request IDs never
enter it.

A run proves one cell. It says nothing about the other two wire profiles,
another model, another region, or the other project mode, and nothing about
capabilities the run did not exercise. A transient failure, a rate limit or a
5xx must never be recorded as `unsupported`. Until at least one row exists, the
release notes carry Mantle as a known limitation and any statement that Mantle
"works" is a statement about fixtures.
