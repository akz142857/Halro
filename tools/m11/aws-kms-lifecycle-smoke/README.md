# M11 AWS KMS lifecycle smoke

This opt-in smoke uses three existing customer-managed symmetric KMS Keys. It
does not create, modify, disable, or schedule deletion of AWS resources. The
active Workload Identity must be allowlisted for the required `Encrypt` and
`Decrypt` calls; static/shared-file credentials are rejected by the adapter.

```bash
export HEIMDALL_AWS_KMS_PRIMARY_KEY_ARN='arn:aws:kms:...:key/...'
export HEIMDALL_AWS_KMS_RECOVERY_KEY_ARN='arn:aws:kms:...:key/...'
export HEIMDALL_AWS_KMS_REPLACEMENT_PRIMARY_KEY_ARN='arn:aws:kms:...:key/...'
./tools/m11/aws-kms-lifecycle-smoke/run.sh
```

The test initializes an ephemeral local instance, rewraps Primary to the third
Key, rotates the Master Key under the replacement Primary and Recovery Keys,
and verifies the persisted Vault generation. Output contains hashes and
booleans only; it never prints ARNs, ciphertext, tokens, or plaintext keys.

After the run, archive matching CloudTrail event metadata using the existing
M11 verifier, revoke any temporary Recovery permission, and record backup
disposition/restore-drill evidence in
`docs/milestones/evidence/m11-05-key-lifecycle-2026-08-03.md`.
