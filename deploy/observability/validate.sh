#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
observability="$root/deploy/observability"
sh -n "$observability/external-probe/probe.sh"
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
printf 'validation-only-not-a-production-secret\n' >"$temporary/metrics-token"
chmod 0400 "$temporary/metrics-token"

for dashboard in "$observability"/grafana/dashboards/*.json; do
  jq -e . "$dashboard" >/dev/null
done

# The single-host example must not publish management ports or advertise a
# non-loopback listener. Cross-namespace deployments use the mTLS listener.
if rg -n '(^|[[:space:]])ports:|0\.0\.0\.0|host\.docker\.internal|169\.254\.169\.254' \
  "$observability/compose.example.yaml" "$observability/grafana/provisioning"; then
  echo "unsafe observability network target or published port" >&2
  exit 1
fi

if rg -n -i '(bearer[[:space:]]+[A-Za-z0-9._~-]{12,}|secureJsonData|authorization:[[:space:]]*Bearer|password:[[:space:]]*[^$<{])' \
  "$observability" --glob '!validate.sh'; then
  echo "possible secret material in observability provisioning" >&2
  exit 1
fi

docker run --rm \
  -v "$observability/prometheus:/etc/prometheus:ro" \
  -v "$temporary:/run/secrets:ro" \
  --entrypoint /bin/promtool \
  prom/prometheus@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996 \
  check config /etc/prometheus/prometheus.yml

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
