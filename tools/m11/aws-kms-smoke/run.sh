#!/bin/sh
set -eu

: "${HEIMDALL_AWS_KMS_KEY_ARN:?set a customer-managed KMS Key ARN}"

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SOURCE_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../../.." && pwd)
RESULT_PATH=${1:-"$SOURCE_ROOT/.tmp/m11-aws-kms-smoke.json"}
PRIVATE_FILE=$(mktemp "${TMPDIR:-/tmp}/heimdall-aws-kms-private.XXXXXX")
ENCRYPT_EVENTS=$(mktemp "${TMPDIR:-/tmp}/heimdall-aws-kms-encrypt.XXXXXX")
DECRYPT_EVENTS=$(mktemp "${TMPDIR:-/tmp}/heimdall-aws-kms-decrypt.XXXXXX")
TEST_OUTPUT=$(mktemp "${TMPDIR:-/tmp}/heimdall-aws-kms-test.XXXXXX")

cleanup() {
	rm -f "$PRIVATE_FILE" "$ENCRYPT_EVENTS" "$DECRYPT_EVENTS" "$TEST_OUTPUT"
}
trap cleanup EXIT INT TERM
chmod 600 "$PRIVATE_FILE" "$ENCRYPT_EVENTS" "$DECRYPT_EVENTS" "$TEST_OUTPUT"
mkdir -p "$(dirname -- "$RESULT_PATH")"

START_TIME=$(python3 -c 'import datetime; print((datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(minutes=5)).isoformat())')
AWS_REGION=$(python3 -c 'import sys; parts=sys.argv[1].split(":",5); print(parts[3] if len(parts)==6 else "")' "$HEIMDALL_AWS_KMS_KEY_ARN")
if [ -z "$AWS_REGION" ]; then
	echo "HEIMDALL_AWS_KMS_KEY_ARN is not a full ARN" >&2
	exit 1
fi
(
	cd "$SOURCE_ROOT"
	HEIMDALL_AWS_KMS_REAL=1 \
	HEIMDALL_AWS_KMS_PRIVATE_EVIDENCE_FILE="$PRIVATE_FILE" \
		go test ./internal/kms/awskms -run '^TestRealAWSKMSWorkloadIdentityEncryptDecrypt$' -count=1 -v
) >"$TEST_OUTPUT"

SANITIZED=$(sed -n 's/^ *M11_AWS_KMS_EVIDENCE=//p' "$TEST_OUTPUT")
if [ -z "$SANITIZED" ]; then
	echo "real smoke did not emit sanitized evidence" >&2
	exit 1
fi

attempt=1
while [ "$attempt" -le 12 ]; do
	aws cloudtrail lookup-events --lookup-attributes AttributeKey=EventName,AttributeValue=Encrypt \
		--region "$AWS_REGION" --start-time "$START_TIME" --output json >"$ENCRYPT_EVENTS"
	aws cloudtrail lookup-events --lookup-attributes AttributeKey=EventName,AttributeValue=Decrypt \
		--region "$AWS_REGION" --start-time "$START_TIME" --output json >"$DECRYPT_EVENTS"
	if CORRELATION=$(python3 "$SCRIPT_DIR/verify_cloudtrail.py" "$PRIVATE_FILE" "$ENCRYPT_EVENTS" "$DECRYPT_EVENTS"); then
		python3 -c 'import json,sys; evidence=json.loads(sys.argv[1]); evidence.update(json.loads(sys.argv[2])); print(json.dumps(evidence,indent=2,sort_keys=True))' \
			"$SANITIZED" "$CORRELATION" >"$RESULT_PATH"
		chmod 600 "$RESULT_PATH"
		cat "$RESULT_PATH"
		exit 0
	fi
	attempt=$((attempt + 1))
	sleep 10
done

echo "CloudTrail did not expose both correlated KMS events within 120 seconds" >&2
exit 1
