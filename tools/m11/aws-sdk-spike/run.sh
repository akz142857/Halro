#!/bin/sh
set -eu

AWS_CONFIG_VERSION=v1.32.34
AWS_KMS_VERSION=v1.55.3
SYFT_IMAGE='anchore/syft@sha256:bd5357d2cd087f03af748dac24df48bfbc1723080d78f75f69aca1f2d429060e'

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SOURCE_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../../.." && pwd)
RESULT_ROOT=${1:-"$SOURCE_ROOT/.tmp/aws-sdk-spike"}
WORK_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/heimdall-aws-sdk-spike.XXXXXX")
CORE_ROOT="$WORK_ROOT/core"
AWS_ROOT="$WORK_ROOT/aws"
CORE_TAG=heimdall-m11-spike-core
AWS_TAG=heimdall-m11-spike-aws

cleanup() {
	docker image rm "$CORE_TAG" "$AWS_TAG" >/dev/null 2>&1 || true
	rm -rf "$WORK_ROOT"
}
trap cleanup EXIT INT TERM

mkdir -p "$RESULT_ROOT" "$CORE_ROOT" "$AWS_ROOT"
tar -C "$SOURCE_ROOT" \
	--exclude=.git --exclude=.tmp --exclude=bin --exclude=node_modules \
	-cf - . | tar -C "$CORE_ROOT" -xf -
tar -C "$CORE_ROOT" -cf - . | tar -C "$AWS_ROOT" -xf -
cp "$SCRIPT_DIR/aws_spike.go.txt" "$AWS_ROOT/cmd/heimdall/aws_spike.go"

(
	cd "$AWS_ROOT"
	go get "github.com/aws/aws-sdk-go-v2/config@$AWS_CONFIG_VERSION" \
		"github.com/aws/aws-sdk-go-v2/service/kms@$AWS_KMS_VERSION"
	go mod tidy
)

cp "$CORE_ROOT/go.mod" "$RESULT_ROOT/core.go.mod"
cp "$AWS_ROOT/go.mod" "$RESULT_ROOT/aws.go.mod"
cp "$CORE_ROOT/go.sum" "$RESULT_ROOT/core.go.sum"
cp "$AWS_ROOT/go.sum" "$RESULT_ROOT/aws.go.sum"
diff -u "$CORE_ROOT/go.mod" "$AWS_ROOT/go.mod" >"$RESULT_ROOT/go.mod.diff" || true
diff -u "$CORE_ROOT/go.sum" "$AWS_ROOT/go.sum" >"$RESULT_ROOT/go.sum.diff" || true

measure() {
	name=$1
	shift
	/usr/bin/time -p "$@" >"$RESULT_ROOT/$name.stdout" 2>"$RESULT_ROOT/$name.time"
}

mkdir -p "$WORK_ROOT/cache/core-build" "$WORK_ROOT/cache/aws-build"
measure core-clean-build env GOCACHE="$WORK_ROOT/cache/core-build" \
	go -C "$CORE_ROOT" build -trimpath -ldflags '-s -w' -o "$RESULT_ROOT/heimdall-core" ./cmd/heimdall
measure aws-clean-build env GOCACHE="$WORK_ROOT/cache/aws-build" \
	go -C "$AWS_ROOT" build -trimpath -ldflags '-s -w' -o "$RESULT_ROOT/heimdall-aws" ./cmd/heimdall

mkdir -p "$WORK_ROOT/cache/core-test" "$WORK_ROOT/cache/aws-test"
measure core-clean-test env GOCACHE="$WORK_ROOT/cache/core-test" go -C "$CORE_ROOT" test ./...
measure aws-clean-test env GOCACHE="$WORK_ROOT/cache/aws-test" go -C "$AWS_ROOT" test ./...

file_size() {
	if stat -f %z "$1" >/dev/null 2>&1; then
		stat -f %z "$1"
	else
		stat -c %s "$1"
	fi
}

module_count() {
	root=$1
	(cd "$root" && go list -m all | wc -l | tr -d ' ')
}

cold_start_total() {
	binary=$1
	output=$2
	: >"$output"
	index=0
	while [ "$index" -lt 25 ]; do
		/usr/bin/time -p "$binary" version >/dev/null 2>>"$output"
		index=$((index + 1))
	done
	awk '/^real / { total += $2; count++ } END { if (count == 0) print "0"; else printf "%.6f", total/count }' "$output"
}

CORE_COLD_START=$(cold_start_total "$RESULT_ROOT/heimdall-core" "$RESULT_ROOT/core-cold-start.time")
AWS_COLD_START=$(cold_start_total "$RESULT_ROOT/heimdall-aws" "$RESULT_ROOT/aws-cold-start.time")

# A live metadata endpoint proves that the linked AWS artifact does not touch
# workload identity or the network in File-mode static commands.
METADATA_LOG="$RESULT_ROOT/file-mode-metadata.log"
python3 -m http.server 18765 --bind 127.0.0.1 >"$METADATA_LOG" 2>&1 &
METADATA_PID=$!
sleep 1
AWS_EC2_METADATA_SERVICE_ENDPOINT=http://127.0.0.1:18765 \
AWS_EC2_METADATA_DISABLED=false \
AWS_CONFIG_FILE="$WORK_ROOT/nonexistent-config" \
AWS_SHARED_CREDENTIALS_FILE="$WORK_ROOT/nonexistent-credentials" \
	"$RESULT_ROOT/heimdall-aws" version >/dev/null
AWS_EC2_METADATA_SERVICE_ENDPOINT=http://127.0.0.1:18765 \
AWS_EC2_METADATA_DISABLED=false \
AWS_CONFIG_FILE="$WORK_ROOT/nonexistent-config" \
AWS_SHARED_CREDENTIALS_FILE="$WORK_ROOT/nonexistent-credentials" \
	"$RESULT_ROOT/heimdall-aws" config check --config "$AWS_ROOT/configs/config.example.yaml" >/dev/null
kill "$METADATA_PID"
wait "$METADATA_PID" 2>/dev/null || true
if [ -s "$METADATA_LOG" ]; then
	echo "File mode unexpectedly contacted the AWS metadata probe" >&2
	exit 1
fi

if (cd "$AWS_ROOT" && go list -deps ./internal/gateway | grep -E 'aws-sdk-go|internal/provider/aws|internal/kms/aws'); then
	echo "AWS SDK entered the Gateway request-path dependency graph" >&2
	exit 1
fi

docker build -q -t "$CORE_TAG" "$CORE_ROOT" >"$RESULT_ROOT/core-container.id"
docker build -q -t "$AWS_TAG" "$AWS_ROOT" >"$RESULT_ROOT/aws-container.id"
CORE_CONTAINER_SIZE=$(docker image inspect --format '{{.Size}}' "$CORE_TAG")
AWS_CONTAINER_SIZE=$(docker image inspect --format '{{.Size}}' "$AWS_TAG")

docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v "$RESULT_ROOT:/output" \
	"$SYFT_IMAGE" "docker:$CORE_TAG" -o spdx-json=/output/core.spdx.json >/dev/null
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v "$RESULT_ROOT:/output" \
	"$SYFT_IMAGE" "docker:$AWS_TAG" -o spdx-json=/output/aws.spdx.json >/dev/null

spdx_package_count() {
	python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["packages"]))' "$1"
}

set +e
(cd "$CORE_ROOT" && GOTOOLCHAIN=go1.26.5 go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...) >"$RESULT_ROOT/core-govulncheck.txt" 2>&1
CORE_VULN_STATUS=$?
(cd "$AWS_ROOT" && GOTOOLCHAIN=go1.26.5 go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...) >"$RESULT_ROOT/aws-govulncheck.txt" 2>&1
AWS_VULN_STATUS=$?
set -e

cat >"$RESULT_ROOT/summary.env" <<EOF
source_commit=$(git -C "$SOURCE_ROOT" rev-parse HEAD)
go_version=$(go version)
aws_config_version=$AWS_CONFIG_VERSION
aws_kms_version=$AWS_KMS_VERSION
core_module_count=$(module_count "$CORE_ROOT")
aws_module_count=$(module_count "$AWS_ROOT")
core_binary_bytes=$(file_size "$RESULT_ROOT/heimdall-core")
aws_binary_bytes=$(file_size "$RESULT_ROOT/heimdall-aws")
core_container_bytes=$CORE_CONTAINER_SIZE
aws_container_bytes=$AWS_CONTAINER_SIZE
core_spdx_packages=$(spdx_package_count "$RESULT_ROOT/core.spdx.json")
aws_spdx_packages=$(spdx_package_count "$RESULT_ROOT/aws.spdx.json")
core_clean_build_seconds=$(awk '/^real / {print $2}' "$RESULT_ROOT/core-clean-build.time")
aws_clean_build_seconds=$(awk '/^real / {print $2}' "$RESULT_ROOT/aws-clean-build.time")
core_clean_test_seconds=$(awk '/^real / {print $2}' "$RESULT_ROOT/core-clean-test.time")
aws_clean_test_seconds=$(awk '/^real / {print $2}' "$RESULT_ROOT/aws-clean-test.time")
core_cold_start_mean_seconds=$CORE_COLD_START
aws_cold_start_mean_seconds=$AWS_COLD_START
core_govulncheck_exit=$CORE_VULN_STATUS
aws_govulncheck_exit=$AWS_VULN_STATUS
file_mode_metadata_requests=0
gateway_aws_dependencies=0
EOF

cat "$RESULT_ROOT/summary.env"
