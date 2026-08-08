# Real AWS dual-Slot smoke

This opt-in gate uses two existing customer-managed symmetric AWS KMS Keys. It
never creates, changes, disables, schedules deletion for, or modifies policy on
an AWS resource. The active AWS SDK credential source must be one of the
Workload Identity/IAM Role sources enforced by the Adapter and must temporarily
authorize both Keys during initialization.

```sh
HALRO_AWS_KMS_PRIMARY_KEY_ARN='arn:aws:kms:REGION:ACCOUNT:key/UUID' \
HALRO_AWS_KMS_RECOVERY_KEY_ARN='arn:aws:kms:REGION:ACCOUNT:key/UUID' \
  ./tools/m11/aws-kms-dual-slot-smoke/run.sh
```

The test creates only a temporary local Halro data directory, initializes
both Slots, independently unlocks the same Vault through both Keys, executes
the explicit audited Recovery path, and emits only hashed identifiers and
boolean failure-domain evidence. The Recovery authorization must be revoked by
the operator immediately after the command finishes.
