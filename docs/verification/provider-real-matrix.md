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
