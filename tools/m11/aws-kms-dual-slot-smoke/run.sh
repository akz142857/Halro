#!/bin/sh
set -eu

: "${HALRO_AWS_KMS_PRIMARY_KEY_ARN:?set an existing customer-managed Primary KMS Key ARN}"
: "${HALRO_AWS_KMS_RECOVERY_KEY_ARN:?set a different existing customer-managed Recovery KMS Key ARN}"

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SOURCE_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../../.." && pwd)
RESULT_PATH=${1:-"$SOURCE_ROOT/.tmp/m11-aws-kms-dual-slot-smoke.json"}
TEST_OUTPUT=$(mktemp "${TMPDIR:-/tmp}/halro-aws-kms-dual-test.XXXXXX")

cleanup() {
	rm -f "$TEST_OUTPUT"
}
trap cleanup EXIT INT TERM
chmod 600 "$TEST_OUTPUT"
mkdir -p "$(dirname -- "$RESULT_PATH")"

(
	cd "$SOURCE_ROOT"
	HALRO_AWS_KMS_DUAL_REAL=1 \
		go test ./internal/app -run '^TestRealAWSDualSlotInitializeAndRecovery$' -count=1 -v
) >"$TEST_OUTPUT"

SANITIZED=$(sed -n 's/^ *M11_AWS_KMS_DUAL_SLOT_EVIDENCE=//p' "$TEST_OUTPUT")
if [ -z "$SANITIZED" ]; then
	echo "real dual-Slot smoke did not emit sanitized evidence" >&2
	exit 1
fi
python3 -c 'import json,sys; print(json.dumps(json.loads(sys.argv[1]),indent=2,sort_keys=True))' "$SANITIZED" >"$RESULT_PATH"
chmod 600 "$RESULT_PATH"
cat "$RESULT_PATH"
