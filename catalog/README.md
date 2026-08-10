# Signed model catalog publication area

`model-catalog-v1.json` is the production artifact read by Halro from the
compile-time endpoint. It must be created only by the protected
`model-catalog-publish.yml` workflow from an externally signed candidate.

Candidate branches may place signed envelopes under `catalog/candidates/`.
They are inputs to the protected workflow, not production artifacts. Do not
commit private keys, KMS credentials, raw Provider responses, tenant data, or
credentials anywhere in this tree.

The schema, signing bytes, approval process, rollback rules, and recovery steps
are defined in `docs/runbooks/model-catalog-publishing.md` and ADR 0020.
`unsigned-snapshot-v1.example.json` is a non-production authoring example; it
contains no signature and cannot be published or consumed by Halro.
