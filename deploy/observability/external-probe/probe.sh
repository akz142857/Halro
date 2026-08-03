#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: probe.sh <heimdall-health-url> <prometheus-ready-url> <interval-seconds>" >&2
  exit 2
fi

heimdall_url=$1
prometheus_url=$2
interval=$3
webhook_config=/run/secrets/webhook.curl

case "$heimdall_url" in https://*) ;; *) echo "Heimdall probe URL must use HTTPS" >&2; exit 2 ;; esac
case "$prometheus_url" in https://*) ;; *) echo "Prometheus probe URL must use HTTPS" >&2; exit 2 ;; esac

case "$interval" in
  ''|*[!0-9]*) echo "interval must be an integer" >&2; exit 2 ;;
esac
if [ "$interval" -lt 10 ]; then
  echo "interval must be at least 10 seconds" >&2
  exit 2
fi
if [ ! -r "$webhook_config" ]; then
  echo "independent webhook curl config is not readable" >&2
  exit 1
fi

notify() {
  service=$1
  state=$2
  curl --fail --silent --show-error --max-time 10 \
    --config "$webhook_config" \
    --header 'Content-Type: application/json' \
    --data "{\"source\":\"heimdall-independent-probe\",\"service\":\"$service\",\"state\":\"$state\"}"
}

check() {
  service=$1
  url=$2
  state_file="/tmp/$service.state"
  state=down
  if curl --fail --silent --show-error --max-time 5 --output /dev/null "$url"; then
    state=up
  fi
  previous=unknown
  if [ -r "$state_file" ]; then
    previous=$(sed -n '1p' "$state_file")
  fi
  if [ "$state" != "$previous" ]; then
    notify "$service" "$state"
  fi
  printf '%s\n' "$state" >"$state_file"
}

while true; do
  notify probe heartbeat
  check heimdall "$heimdall_url"
  check prometheus "$prometheus_url"
  sleep "$interval"
done
