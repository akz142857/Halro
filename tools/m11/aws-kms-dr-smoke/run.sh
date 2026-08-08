#!/bin/sh
set -eu

: "${HALRO_AWS_KMS_PRIMARY_KEY_ARN:?set an existing primary customer-managed KMS Key ARN}"
: "${HALRO_AWS_KMS_RECOVERY_KEY_ARN:?set an existing recovery customer-managed KMS Key ARN}"

HALRO_AWS_KMS_DR_REAL=1 \
  GOCACHE="${GOCACHE:-/tmp/halro-m11-kms-dr-gocache}" \
  go test ./internal/app -run '^TestRealAWSKMSDisasterRecovery$' -count=1 -v
