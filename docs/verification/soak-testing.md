# 24-hour soak gate

The release soak must run for at least 24 hours against the exact RC commit. A
shorter run is useful only to validate the harness and is permanently labelled
`smoke_only`; it cannot satisfy the release gate.

Build the production binary, start it with an isolated copy of production-like
configuration and a bounded-cost Route, then run:

```bash
export HEIMDALL_GATEWAY_KEY='gw_...'
export HEIMDALL_METRICS_TOKEN='...'
go run ./tests/soak \
  -pid "$(pgrep -n heimdall)" \
  -commit '<exact-40-character-RC-commit>' \
  -model chat \
  -duration 24h \
  -sample-interval 1m \
  -request-interval 10s \
  -output "soak-artifacts-$(date -u +%Y%m%dT%H%M%SZ)"
```

The Gateway and Metrics URLs default to their loopback listeners and can be
overridden with `-gateway-url` and `-metrics-url`. Secrets are accepted only
from environment variables, kept in memory, never passed as process arguments,
and never written to artifacts. Use a dedicated Project with an explicit daily
budget; at the default interval the run attempts at most 8,640 small requests.

The fresh output directory contains:

- `samples.jsonl`: timestamped current RSS, goroutines, open FDs, durable WAL
  queue/capacity/errors, derivative queue/lag, and request counters;
- `requests.jsonl`: timestamps and aggregate success/failure counters without
  response bodies, prompts, URLs, or credentials;
- `summary.json`: exact commit, start/end/max samples, explicit limits, failures,
  and `release_24h` or `smoke_only` status.

The 24-hour gate fails when final RSS grows by more than max(64 MiB, 25% of its
start), goroutines or FDs grow by more than 20, the final WAL queue is nonzero,
the peak WAL queue exceeds 75% capacity, WAL append errors increase, analytics
remain lagging, no request completes, or the request failure rate exceeds 1%.
Archive the whole directory with the RC release evidence. Inspect time-series
shape as well as the automated result: a sawtooth GC pattern is expected, but
monotonic unbounded growth is a release blocker even if the endpoint happens to
fall inside its allowance.

For a harness smoke test, use a new output directory and a short duration such
as `-duration 2m -sample-interval 10s -request-interval 2s`. A smoke result is
never promoted or renamed to `release_24h`.
