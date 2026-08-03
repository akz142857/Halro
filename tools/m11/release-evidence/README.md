# M11 final release-evidence gate

`verify.py` is the fail-closed final gate for declaring AWS KMS mode
production-ready. It validates one sanitized JSON bundle containing evidence
for the exact release commit. It does not run AWS operations and does not make
an incomplete environment look complete.

The bundle must prove:

- all 14 real-AWS scenarios from the M11 milestone;
- engineering/security review of the frozen KMSWrapper, error taxonomy and
  Accepted ADR 0010 decision;
- an Adapter-approved Workload Identity source and three distinct hashed KMS
  Key identities with a reviewed failure-domain record;
- CloudTrail correlation wherever a KMS API request is expected;
- the complete logs/errors/Audit/Metrics/bbolt/backup/heap Secret Canary;
- EKS and VM/systemd deployment, including the three EKS CrashLoop cases;
- Primary and Recovery restore by an operator who is not an implementation
  author, followed by explicit Recovery permission revocation;
- all release artifacts, SHA-256 digests and verified Sigstore bundles;
- Security, Backend, SRE and Release approval by four distinct reviewers of the
  same full commit SHA.

Raw AWS ARNs, access-key IDs and private keys are rejected. Retained evidence
must use hashes or immutable references to access-controlled records. Tokens,
ciphertext, plaintext keys and credentials must never enter the bundle.

Run the unit tests:

```sh
python3 -B -m unittest tools/m11/release-evidence/test_verify.py
```

Validate the final sanitized bundle:

```sh
python3 tools/m11/release-evidence/verify.py \
  /secure/evidence/m11-release-evidence.json
```

Only `M11_RELEASE_EVIDENCE=PASS` permits G4–G16 and the milestone to be marked
Complete. Store the actual bundle in the restricted release evidence system,
not in the public repository.
