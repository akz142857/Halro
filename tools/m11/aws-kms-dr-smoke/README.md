# M11 AWS KMS disaster-recovery smoke

This opt-in smoke uses two existing, independent customer-managed symmetric KMS
Keys and the active Workload Identity. It creates no AWS resources and changes
no Key, Grant, or Policy state.

```bash
export HALRO_AWS_KMS_PRIMARY_KEY_ARN='arn:aws:kms:...:key/...'
export HALRO_AWS_KMS_RECOVERY_KEY_ARN='arn:aws:kms:...:key/...'
./tools/m11/aws-kms-dr-smoke/run.sh
```

The test creates an encrypted backup of an ephemeral local instance, verifies
the outer archive, restores once through Primary, then restores again through
an explicitly confirmed Recovery Slot. Output contains only hashes and
booleans. Archive matching CloudTrail metadata separately and revoke temporary
Recovery authorization immediately after the exercise.

The automated smoke is not the independent-operator sign-off. A second operator
must still execute `docs/runbooks/m11-kms-disaster-recovery.md` using only the
published Runbook and archive the resulting evidence.
