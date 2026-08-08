#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
observability="$root/deploy/observability"
core_compose="$observability/compose.example.yaml"
macos_compose="$observability/compose.macos.example.yaml"
jq -e . "$observability/external-probe/config.schema.json" >/dev/null
jq -e . "$observability/external-probe/event.schema.json" >/dev/null
grep -Eq 'scalar%28max%28timestamp%28up' \
  "$observability/external-probe/config.example.yaml"
(
  cd "$root"
  go test ./deploy/observability
)
(
  cd "$root"
  go run ./cmd/halro-deadman \
    -config deploy/observability/external-probe/config.example.yaml \
    -check-config
)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
# The pinned containers run as 65534:65534, so this validation-only Secret
# must be traversable and readable by that account. The fixed value below is
# not a production credential and the directory is removed on exit.
chmod 0755 "$temporary"
printf 'validation-only-not-a-production-secret\n' >"$temporary/metrics-token"
chmod 0444 "$temporary/metrics-token"

# The single-host example must not publish management ports or advertise a
# non-loopback listener. Cross-namespace deployments use the mTLS listener.
if grep -En '(^|[[:space:]])ports:|0\.0\.0\.0|host\.docker\.internal|169\.254\.169\.254' \
  "$core_compose"; then
  echo "unsafe observability network target or published port" >&2
  exit 1
fi

grep -Eq 'repeat_interval: 1m' "$observability/alertmanager/alertmanager.yml"
for label in environment region cluster; do
  grep -Eq "^[[:space:]]+$label:" "$observability/prometheus/prometheus.yml"
done
grep -Fq -- '--storage.tsdb.retention.size=5368709120B' "$core_compose"
grep -Fq -- '--cluster.listen-address=' "$core_compose"

core_services=$(docker compose -f "$core_compose" config --services | sort)
expected_core_services=$(printf '%s\n' alertmanager prometheus)
if [ "$core_services" != "$expected_core_services" ]; then
  echo "default observability profile must contain only Prometheus and Alertmanager" >&2
  printf 'actual services:\n%s\n' "$core_services" >&2
  exit 1
fi

macos_services=$(docker compose -f "$core_compose" -f "$macos_compose" config --services | sort)
if [ "$macos_services" != "$expected_core_services" ]; then
  echo "macOS override must preserve the Core service set" >&2
  exit 1
fi
docker compose -f "$core_compose" -f "$macos_compose" config --format json |
  jq -e '[.services.prometheus, .services.alertmanager] | all(
    .volumes | any(.source == "/private/tmp/halro-observability" and
      .target == "/run/secrets" and .read_only == true)
  )' >/dev/null

docker compose -f "$observability/external-probe.example.yaml" config --quiet

docker compose -f "$core_compose" config --format json |
  jq -e '.services.alertmanager.volumes |
    any(.target == "/run/secrets" and .read_only == true)' >/dev/null

if grep -REn -i --exclude=validate.sh \
  '(bearer[[:space:]]+[A-Za-z0-9._~-]{12,}|secureJsonData|authorization:[[:space:]]*Bearer|password:[[:space:]]*[^$<{])' \
  "$observability"; then
  echo "possible secret material in observability provisioning" >&2
  exit 1
fi

docker run --rm \
  -v "$observability/prometheus:/etc/prometheus:ro" \
  -v "$temporary:/run/secrets:ro" \
  --entrypoint /bin/promtool \
  prom/prometheus@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996 \
  check config /etc/prometheus/prometheus.yml

echo "Prometheus/Alertmanager Core configuration validated"

docker run --rm \
  -v "$observability/prometheus:/etc/prometheus:ro" \
  --entrypoint /bin/promtool \
  prom/prometheus@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996 \
  test rules /etc/prometheus/rule-tests.yml

docker run --rm \
  -v "$observability/alertmanager:/etc/alertmanager:ro" \
  --entrypoint /bin/amtool \
  prom/alertmanager@sha256:27c475db5fb156cab31d5c18a4251ac7ed567746a2483ff264516437a39b15ba \
  check-config /etc/alertmanager/alertmanager.yml

echo "observability provisioning validated"
