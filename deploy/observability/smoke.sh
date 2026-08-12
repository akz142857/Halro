#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
usage: deploy/observability/smoke.sh

Runs an isolated local Prometheus/Alertmanager Core runtime smoke. It requires
docker compose, curl, jq, grep, and python3. The default
loopback ports are 9090, 9091, 9093, and 19094; override them with
HALRO_SMOKE_METRICS_PORT, HALRO_SMOKE_PROMETHEUS_PORT,
HALRO_SMOKE_ALERTMANAGER_PORT, and HALRO_SMOKE_WEBHOOK_PORT.
EOF
}

if [ "${1:-}" = "--help" ] || [ "${1:-}" = "-h" ]; then
  usage
  exit 0
fi
if [ "$#" -ne 0 ]; then
  usage >&2
  exit 2
fi

for command in docker curl jq grep python3; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "required command not found: $command" >&2
    exit 1
  fi
done
docker compose version >/dev/null

root=$(unset CDPATH; cd -- "$(dirname -- "$0")/../.." && pwd)
observability="$root/deploy/observability"
watchdog_delivery_budget_seconds=150
watchdog_group_interval=$(awk '
  /receiver: independent-deadman-webhook/ { watchdog = 1 }
  watchdog && /group_interval:/ { print $2; exit }
' "$observability/alertmanager/alertmanager.yml")
watchdog_group_interval_seconds=$(python3 - "$watchdog_group_interval" <<'PY'
import re
import sys

match = re.fullmatch(r"([0-9]+)([smh])", sys.argv[1])
if not match:
    raise SystemExit("Watchdog group_interval must use an integer s, m, or h duration")
value = int(match.group(1))
print(value * {"s": 1, "m": 60, "h": 3600}[match.group(2)])
PY
)
minimum_watchdog_budget=$((2 * watchdog_group_interval_seconds + 30))
if [ "$watchdog_delivery_budget_seconds" -lt "$minimum_watchdog_budget" ]; then
  echo "Watchdog smoke budget ${watchdog_delivery_budget_seconds}s is below 2 x group_interval + 30s (${minimum_watchdog_budget}s)" >&2
  exit 1
fi
temporary=$(mktemp -d)
project="halro-observability-smoke-$$"
override="$temporary/compose.override.yaml"
events="$temporary/webhook-events.jsonl"
mock_log="$temporary/mock.log"
secrets="$temporary/secrets"
runtime_prometheus="$temporary/prometheus"
runtime_alertmanager="$temporary/alertmanager"
smoke_token=halro-observability-smoke-token
authorization_scheme=Bearer
metrics_port=${HALRO_SMOKE_METRICS_PORT:-9090}
prometheus_port=${HALRO_SMOKE_PROMETHEUS_PORT:-9091}
alertmanager_port=${HALRO_SMOKE_ALERTMANAGER_PORT:-9093}
webhook_port=${HALRO_SMOKE_WEBHOOK_PORT:-19094}
mock_pid=
compose_started=false

for port in "$metrics_port" "$prometheus_port" "$alertmanager_port" "$webhook_port"; do
  case "$port" in
    ''|*[!0-9]*) echo "invalid smoke port: $port" >&2; exit 2 ;;
  esac
  if [ "$port" -lt 1024 ] || [ "$port" -gt 65535 ]; then
    echo "smoke port outside 1024..65535: $port" >&2
    exit 2
  fi
done
if [ "$(printf '%s\n' "$metrics_port" "$prometheus_port" "$alertmanager_port" "$webhook_port" | sort -u | wc -l | tr -d ' ')" -ne 4 ]; then
  echo "smoke ports must be distinct" >&2
  exit 2
fi

compose() {
  docker compose -p "$project" \
    -f "$observability/compose.example.yaml" \
    -f "$override" "$@"
}

cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$compose_started" = true ]; then
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  if [ -n "$mock_pid" ]; then
    kill "$mock_pid" >/dev/null 2>&1 || true
    wait "$mock_pid" >/dev/null 2>&1 || true
  fi
  rm -rf "$temporary"
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

# The shipped Core uses host networking and fixed loopback listeners. Refuse
# to disturb an existing Halro or monitoring stack.
python3 - "$metrics_port" "$prometheus_port" "$alertmanager_port" "$webhook_port" <<'PY'
import socket
import sys

for raw_port in sys.argv[1:]:
    port = int(raw_port)
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        sock.bind(("127.0.0.1", port))
    except OSError as error:
        raise SystemExit(f"loopback port {port} is unavailable: {error}")
    finally:
        sock.close()
PY

mkdir -p "$secrets" "$runtime_prometheus" "$runtime_alertmanager"
chmod 0755 "$secrets"
# Compose may canonicalize /var to /private/var on macOS. Use the physical path
# in both the override and the merge assertion.
secrets=$(unset CDPATH; cd -- "$secrets" && pwd -P)
printf '%s\n' "$smoke_token" >"$secrets/metrics-token"
printf 'http://127.0.0.1:%s/operations\n' "$webhook_port" >"$secrets/alertmanager-webhook-url"
printf 'http://127.0.0.1:%s/watchdog\n' "$webhook_port" >"$secrets/alertmanager-watchdog-url"
chmod 0444 "$secrets"/*

cp "$observability"/prometheus/*.yml "$runtime_prometheus/"
cp "$observability/alertmanager/alertmanager.yml" "$runtime_alertmanager/"
python3 - "$runtime_prometheus/prometheus.yml" "$metrics_port" "$prometheus_port" "$alertmanager_port" <<'PY'
from pathlib import Path
import re
import sys

path = Path(sys.argv[1])
text = path.read_text(encoding="utf-8")
mapping = dict(zip(("9090", "9091", "9093"), sys.argv[2:]))
text = re.sub(
    r"127\.0\.0\.1:(9090|9091|9093)",
    lambda match: "127.0.0.1:" + mapping[match.group(1)],
    text,
)
path.write_text(text, encoding="utf-8")
PY
runtime_prometheus=$(unset CDPATH; cd -- "$runtime_prometheus" && pwd -P)
runtime_alertmanager=$(unset CDPATH; cd -- "$runtime_alertmanager" && pwd -P)
grep -Eq "targets: \[127\.0\.0\.1:$metrics_port\]" "$runtime_prometheus/prometheus.yml"
grep -Eq "targets: \[127\.0\.0\.1:$prometheus_port\]" "$runtime_prometheus/prometheus.yml"
test "$(grep -Ec "127\.0\.0\.1:$alertmanager_port" "$runtime_prometheus/prometheus.yml")" -eq 2

cat >"$override" <<EOF
services:
  prometheus:
    command:
      - --config.file=/etc/prometheus/prometheus.yml
      - --storage.tsdb.path=/prometheus
      - --storage.tsdb.retention.time=7d
      - --storage.tsdb.retention.size=5368709120B
      - --web.listen-address=127.0.0.1:$prometheus_port
    volumes:
      - type: bind
        source: $runtime_prometheus
        target: /etc/prometheus
        read_only: true
      - type: bind
        source: $secrets
        target: /run/secrets
        read_only: true
  alertmanager:
    command:
      - --config.file=/etc/alertmanager/alertmanager.yml
      - --storage.path=/alertmanager
      - --web.listen-address=127.0.0.1:$alertmanager_port
      - --cluster.listen-address=
    volumes:
      - type: bind
        source: $runtime_alertmanager
        target: /etc/alertmanager
        read_only: true
      - type: bind
        source: $secrets
        target: /run/secrets
        read_only: true
EOF

# Fail if Compose merging did not replace the fixed production Secret source,
# or if the service inventory unexpectedly changes.
test "$(compose config --services | sort | tr '\n' ' ')" = "alertmanager prometheus "
compose config --format json | jq -e --arg source "$secrets" '
  [.services.prometheus, .services.alertmanager] | all(
    .volumes | any(.target == "/run/secrets" and .source == $source and .read_only == true)
  )' >/dev/null

# One process provides an authenticated Halro Metrics endpoint and records
# real Alertmanager webhook payloads. It binds loopback only.
python3 - "$events" "$smoke_token" "$metrics_port" "$webhook_port" <<'PY' >"$mock_log" 2>&1 &
import http.server
import json
import sys
import threading

event_path = sys.argv[1]
token = "Bearer " + sys.argv[2]
metrics_port = int(sys.argv[3])
webhook_port = int(sys.argv[4])
metrics = b"""# HELP halro_requests_total Requests handled by the smoke target.\n# TYPE halro_requests_total counter\nhalro_requests_total{status=\"success\"} 1\n"""

class MetricsHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/metrics":
            self.send_error(404)
            return
        if self.headers.get("Authorization") != token:
            self.send_error(401)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.send_header("Content-Length", str(len(metrics)))
        self.end_headers()
        self.wfile.write(metrics)

    def log_message(self, *_):
        pass

class WebhookHandler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        payload = self.rfile.read(length)
        try:
            decoded = json.loads(payload)
        except Exception:
            self.send_error(400)
            return
        with open(event_path, "a", encoding="utf-8") as output:
            output.write(json.dumps({"path": self.path, "payload": decoded}, separators=(",", ":")) + "\n")
            output.flush()
        self.send_response(204)
        self.end_headers()

    def log_message(self, *_):
        pass

servers = [
    http.server.ThreadingHTTPServer(("127.0.0.1", metrics_port), MetricsHandler),
    http.server.ThreadingHTTPServer(("127.0.0.1", webhook_port), WebhookHandler),
]
for server in servers:
    threading.Thread(target=server.serve_forever, daemon=True).start()
threading.Event().wait()
PY
mock_pid=$!

wait_http() {
  url=$1
  attempts=${2:-60}
  while [ "$attempts" -gt 0 ]; do
    if curl --fail --silent --show-error --max-time 2 "$url" >/dev/null 2>&1; then
      return 0
    fi
    attempts=$((attempts - 1))
    sleep 1
  done
  echo "timed out waiting for $url" >&2
  return 1
}

wait_event() {
  wanted_status=$1
  wanted_environment=$2
  attempts=${3:-90}
  while [ "$attempts" -gt 0 ]; do
    if python3 - "$events" "$wanted_status" "$wanted_environment" <<'PY'
import json
import os
import sys

path, wanted_status, wanted_environment = sys.argv[1:]
if not os.path.exists(path):
    raise SystemExit(1)
for line in open(path, encoding="utf-8"):
    record = json.loads(line)
    payload = record.get("payload", {})
    if payload.get("status") != wanted_status:
        continue
    for alert in payload.get("alerts", []):
        labels = alert.get("labels", {})
        if labels.get("alertname") != "Watchdog":
            continue
        if wanted_environment == "*" or labels.get("environment") == wanted_environment:
            raise SystemExit(0)
raise SystemExit(1)
PY
    then
      return 0
    fi
    attempts=$((attempts - 1))
    sleep 1
  done
  echo "timed out waiting for $wanted_status Watchdog event ($wanted_environment)" >&2
  # Past the retry budget the timeout means delivery is broken, not merely late,
  # so print what the two ends of the path actually hold. Without this the
  # failure is indistinguishable from the retry case it is meant to outlive.
  echo "--- webhook events received ---" >&2
  if [ -s "$events" ]; then
    tail -n 20 "$events" >&2
  else
    echo "(none)" >&2
  fi
  echo "--- Alertmanager alerts ---" >&2
  curl --fail --silent --show-error --max-time 3 \
    "http://127.0.0.1:$alertmanager_port/api/v2/alerts" >&2 || echo "(query failed)" >&2
  return 1
}

wait_alert_state() {
  wanted_presence=$1
  wanted_environment=$2
  wanted_alertname=$3
  attempts=${4:-60}
  while [ "$attempts" -gt 0 ]; do
    alerts=$(curl --fail --silent --show-error --max-time 3 \
      "http://127.0.0.1:$alertmanager_port/api/v2/alerts")
    present=$(printf '%s' "$alerts" | jq -r \
      --arg environment "$wanted_environment" --arg alertname "$wanted_alertname" \
      'any(.[]; .labels.environment == $environment and .labels.alertname == $alertname)')
    if [ "$present" = "$wanted_presence" ]; then
      return 0
    fi
    attempts=$((attempts - 1))
    sleep 1
  done
  echo "timed out waiting for Alertmanager state $wanted_presence: $wanted_alertname ($wanted_environment)" >&2
  return 1
}

attempts=20
while [ "$attempts" -gt 0 ]; do
  if curl --fail --silent --show-error --max-time 2 \
    -H "Authorization: ${authorization_scheme} $smoke_token" \
    "http://127.0.0.1:$metrics_port/metrics" >/dev/null 2>&1; then
    break
  fi
  attempts=$((attempts - 1))
  sleep 1
done
if [ "$attempts" -eq 0 ]; then
  echo "mock Metrics endpoint did not start" >&2
  sed -n '1,80p' "$mock_log" >&2
  exit 1
fi

compose_started=true
compose up -d prometheus alertmanager
wait_http "http://127.0.0.1:$prometheus_port/-/ready" 60
wait_http "http://127.0.0.1:$alertmanager_port/-/ready" 60

# Wait up to four scrape intervals and require every Core target to be healthy.
attempts=60
while [ "$attempts" -gt 0 ]; do
  targets=$(curl --fail --silent --show-error --max-time 3 "http://127.0.0.1:$prometheus_port/api/v1/targets")
  if printf '%s' "$targets" | jq -e '
    [.data.activeTargets[] | select(.labels.job == "halro" or .labels.job == "prometheus" or .labels.job == "alertmanager")] as $core |
    ($core | length) == 3 and ($core | all(.health == "up"))' >/dev/null; then
    break
  fi
  attempts=$((attempts - 1))
  sleep 1
done
if [ "$attempts" -eq 0 ]; then
  echo "Core targets did not all become healthy" >&2
  compose ps >&2
  exit 1
fi
rules=$(curl --fail --silent --show-error --max-time 3 "http://127.0.0.1:$prometheus_port/api/v1/rules")
printf '%s' "$rules" | jq -e '[.. | objects | .name? // empty] | index("Watchdog") != null' >/dev/null

# The repository Watchdog must traverse Prometheus and the real Alertmanager
# routing configuration to the mock receiver.
#
# The budget has to clear one delivery retry rather than only the nominal path.
# The configured budget is guarded above against the Watchdog route's actual
# group_interval, so a routing change cannot silently reintroduce the flaky
# nominal-path-only wait this smoke is meant to prevent.
wait_event firing '*' "$watchdog_delivery_budget_seconds"

# Exercise an identifiable firing/resolved state lifecycle through the real
# Alertmanager API. Watchdog intentionally has send_resolved=false because its
# silence is the failure signal; resolved delivery belongs to the ordinary
# operations route and remains a Phase D Contact Point test.
lifecycle_environment="smoke-api-$$"
python3 - "$lifecycle_environment" >"$temporary/alert.json" <<'PY'
import datetime
import json
import sys

environment = sys.argv[1]
now = datetime.datetime.now(datetime.timezone.utc)
ends = now + datetime.timedelta(seconds=20)
print(json.dumps([{
    "labels": {
        "alertname": "HalroSmokeLifecycle",
        "environment": environment,
        "cluster": "smoke",
        "source": "halro-core-runtime-smoke",
    },
    "annotations": {"summary": "Core runtime smoke lifecycle"},
    "startsAt": (now - datetime.timedelta(seconds=1)).isoformat().replace("+00:00", "Z"),
    "endsAt": ends.isoformat().replace("+00:00", "Z"),
}]))
PY
curl --fail --silent --show-error --max-time 5 \
  -H 'Content-Type: application/json' --data-binary @"$temporary/alert.json" \
  "http://127.0.0.1:$alertmanager_port/api/v2/alerts" >/dev/null
wait_alert_state true "$lifecycle_environment" HalroSmokeLifecycle 20
# The bounded endsAt drives resolution without mutating the repository rule
# set or relying on an Alertmanager deletion API.
wait_alert_state false "$lifecycle_environment" HalroSmokeLifecycle 60

echo "Core runtime smoke passed: targets, rules, Watchdog delivery, and Alertmanager lifecycle verified"
