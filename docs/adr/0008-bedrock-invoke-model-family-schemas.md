# ADR 0008: Bedrock InvokeModel uses versioned model-family schemas

- Status: Accepted
- Date: 2026-08-01
- Issue: [#40](https://github.com/akz142857/Heimdall/issues/40)

## Context

Amazon Bedrock `InvokeModel` carries model-specific JSON. A generic passthrough
would let Provider fields bypass Heimdall's semantic validation, capability
evidence, redaction and compatibility contract. Different model families also
have incompatible request, response, quota and accounting semantics.

The first requested family is Amazon Titan Text Embeddings V2. Its native API
accepts one non-empty text input, optional dimensions `256`, `512` or `1024`, a
normalization flag and an output-type list. The OpenAI embeddings endpoint also
allows arrays, token arrays, `base64`, `user` and arbitrary positive dimensions,
so it is not lossless for every valid OpenAI request.

## Decision

1. Register immutable Profile
   `bedrock.runtime.invoke.titan-embed-text-v2.v1` on the existing isolated
   `bedrock-runtime` Access Surface with explicit-session SigV4 credentials.
2. Bind only semantic `embed` to Provider Primitive
   `bedrock.invoke-model.titan-embed-text-v2`.
3. Pin this Profile to model ID `amazon.titan-embed-text-v2:0`. Other model IDs,
   inference-profile ARNs and future model revisions require their own reviewed
   Profile revision.
4. Accept exactly one UTF-8 string of at most 50,000 characters. Reject arrays,
   token arrays and empty input before Provider I/O. Multi-input fan-out is not
   silently synthesized because it needs explicit partial-failure, retry,
   ordering and accounting semantics.
5. Accept only float output and dimensions `256`, `512` or `1024`. Native calls
   force `normalize: true` and `embeddingTypes: ["float"]`.
6. Validate native output length, mirrored float vectors and
   `inputTextTokenCount`; map the latter to OpenAI prompt and total usage.
7. Never expose a general `InvokeModel` JSON endpoint. Each new family adds a
   versioned schema, capability matrix, raw-wire fixtures and explicit
   conversion-loss declaration.

## Consequences

- Existing `/v1/embeddings` clients work when they send the supported subset.
- Unsupported OpenAI fields fail before credentials, signing or network I/O.
- One Runtime credential can be selected by separate Provider instances, but
  Profiles retain independent capability evidence, limits, circuits and quota
  assumptions.
- Binary embeddings, batch fan-out, Rerank, Image and Async Invoke remain out of
  scope until separately designed.

## Verification

- raw HTTP path, SigV4 headers and exact native JSON;
- success mapping for 256-dimensional float vectors and usage;
- pre-I/O rejection for arrays, base64, invalid dimensions, `user` and wrong
  model family;
- malformed native vector and usage rejection;
- Profile/Primitive, compatibility manifest, Admin defaults and real-smoke
  coverage.
