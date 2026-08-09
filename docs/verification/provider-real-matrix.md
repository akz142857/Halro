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
