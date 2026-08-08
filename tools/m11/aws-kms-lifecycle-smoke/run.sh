#!/bin/sh
set -eu

: "${HALRO_AWS_KMS_PRIMARY_KEY_ARN:?set an existing primary customer-managed KMS Key ARN}"
: "${HALRO_AWS_KMS_RECOVERY_KEY_ARN:?set an existing recovery customer-managed KMS Key ARN}"
: "${HALRO_AWS_KMS_REPLACEMENT_PRIMARY_KEY_ARN:?set an existing replacement primary customer-managed KMS Key ARN}"

HALRO_AWS_KMS_LIFECYCLE_REAL=1 \
  GOCACHE="${GOCACHE:-/tmp/halro-m11-kms-lifecycle-gocache}" \
  go test ./internal/app -run '^TestRealAWSKMSKeyLifecycle$' -count=1 -v
