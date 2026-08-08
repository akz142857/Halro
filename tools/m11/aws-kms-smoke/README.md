# Real AWS KMS smoke evidence

This opt-in harness uses the official SDK default credential chain and an
existing customer-managed symmetric KMS Key. It performs one Encrypt and one
Decrypt with the versioned Halro Encryption Context, verifies the fixed
protected payload, then correlates both AWS request IDs with CloudTrail.

It never creates, enables, disables, schedules deletion of, or changes policy
on an AWS resource.

```sh
export HALRO_AWS_KMS_KEY_ARN='arn:aws:kms:REGION:ACCOUNT:key/KEY-ID'
./tools/m11/aws-kms-smoke/run.sh /absolute/sanitized-evidence.json
```

The caller needs `kms:Encrypt`, `kms:Decrypt` and
`cloudtrail:LookupEvents`. Use IRSA, ECS task role, EC2 instance role or another
temporary Workload Identity supported by the official default chain. Do not set
long-lived access-key fields in Halro configuration.

Raw Key ARN, account and request IDs exist only in `0600` temporary files and
are deleted on exit. The retained JSON contains SHA-256 digests, region, sizes,
context version, result and CloudTrail correlation booleans; it contains no
Master Key, protected payload, ciphertext, token or credential.
