#!/usr/bin/env python3
"""Read-only, bounded HA health sampler; it does not create load or declare HA PASS."""

import argparse
import collections
import datetime as dt
import http.client
import json
import math
import os
import re
import signal
import ssl
import time
from pathlib import Path
from urllib.parse import urlsplit


MAX_RESPONSE = 256 * 1024
FORMAL_DURATION = 72 * 60 * 60
LEVELS = {"healthy", "degraded", "critical", "unknown"}
CARDS = ("overall", "client", "confirmation", "safety", "catchup")
SHA256 = re.compile(r"[0-9a-f]{64}\Z")
COMMIT = re.compile(r"[0-9a-f]{40}\Z")
RFC3339_NANO = re.compile(
    r"(?P<second>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})"
    r"(?:\.(?P<fraction>\d{1,9}))?"
    r"(?P<zone>Z|[+-]\d{2}:\d{2})\Z"
)


def utc_now():
    return dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")


def parse_options(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True, help="HTTPS HA health root URL")
    parser.add_argument("--ca", required=True, type=Path)
    parser.add_argument("--cert", required=True, type=Path)
    parser.add_argument("--key", required=True, type=Path)
    parser.add_argument("--environment", required=True)
    parser.add_argument("--cluster", required=True)
    parser.add_argument("--members", required=True, help="exact comma-separated node IDs")
    parser.add_argument("--duration-seconds", type=float, default=120)
    parser.add_argument("--interval-seconds", type=float, default=15)
    parser.add_argument("--output", required=True, type=Path, help="new private directory")
    parser.add_argument("--candidate-sha", default="")
    parser.add_argument("--image-digest", default="")
    parser.add_argument("--config-sha256", default="")
    parser.add_argument("--rules-sha256", default="")
    options = parser.parse_args(argv)
    try:
        target = urlsplit(options.url)
        target_port = target.port
    except ValueError:
        parser.error("--url has an invalid port or host")
    if (target.scheme != "https" or not target.hostname or target.username or
            target.password or target.path not in ("", "/") or target.query or
            target.fragment or target_port == 0):
        parser.error("--url must be an HTTPS root without credentials or query")
    members = options.members.split(",")
    if len(members) not in (2, 3) or any(not member or member.strip() != member for member in members) or len(set(members)) != len(members):
        parser.error("--members must contain exactly two or three distinct node IDs")
    if options.duration_seconds <= 0 or options.interval_seconds <= 0 or options.interval_seconds > 30:
        parser.error("duration and interval must be positive; interval must be at most 30 seconds")
    if not options.environment.strip() or not options.cluster.strip():
        parser.error("--environment and --cluster must be nonempty")
    if options.duration_seconds >= FORMAL_DURATION:
        if not 5 <= options.interval_seconds <= 15:
            parser.error("72-hour collection requires a 5-15 second interval")
        if not COMMIT.fullmatch(options.candidate_sha):
            parser.error("72-hour collection requires an exact 40-character candidate SHA")
        if not options.image_digest.startswith("sha256:") or not SHA256.fullmatch(options.image_digest[7:]):
            parser.error("72-hour collection requires an exact image digest")
        if not SHA256.fullmatch(options.config_sha256) or not SHA256.fullmatch(options.rules_sha256):
            parser.error("72-hour collection requires configuration and rule SHA-256 values")
    options.target = target
    options.member_ids = sorted(members)
    return options


def fetch_health(options, context):
    connection = http.client.HTTPSConnection(options.target.hostname, options.target.port, context=context, timeout=10)
    try:
        connection.request("GET", "/api/health", headers={"Accept": "application/json", "Cache-Control": "no-store"})
        response = connection.getresponse()
        body = response.read(MAX_RESPONSE + 1)
        if len(body) > MAX_RESPONSE:
            raise ValueError("response_too_large")
        if response.status != 200:
            raise ValueError("http_%d" % response.status)
        return json.loads(body)
    finally:
        connection.close()


def summarize_health(payload, options):
    if not isinstance(payload, dict) or payload.get("environment") != options.environment or payload.get("cluster") != options.cluster:
        raise ValueError("cluster_identity_mismatch")
    members = payload.get("members")
    if not isinstance(members, list) or payload.get("expected_members") != len(options.member_ids):
        raise ValueError("inventory_missing")
    ids = [member.get("instance") for member in members if isinstance(member, dict)]
    if any(not isinstance(instance, str) for instance in ids):
        raise ValueError("inventory_mismatch")
    if sorted(ids) != options.member_ids:
        raise ValueError("inventory_mismatch")
    observed_at = payload.get("observed_at")
    if not isinstance(observed_at, str):
        raise ValueError("observation_time_missing")
    timestamp = RFC3339_NANO.fullmatch(observed_at)
    if timestamp is None:
        raise ValueError("observation_time_invalid")
    fraction = timestamp.group("fraction")
    zone = timestamp.group("zone")
    normalized = timestamp.group("second")
    if fraction:
        normalized += "." + fraction[:6]
    normalized += "+00:00" if zone == "Z" else zone
    try:
        parsed = dt.datetime.fromisoformat(normalized)
    except ValueError as error:
        raise ValueError("observation_time_invalid") from error
    if parsed.tzinfo is None:
        raise ValueError("observation_time_invalid")
    if abs((dt.datetime.now(dt.timezone.utc) - parsed).total_seconds()) > 60:
        raise ValueError("observation_time_skew")
    cards = {}
    for name in CARDS:
        card = payload.get(name)
        level = card.get("level") if isinstance(card, dict) else None
        if level not in LEVELS:
            raise ValueError("card_schema_invalid")
        cards[name] = level
    unexpected = payload.get("unexpected_members", [])
    if not isinstance(unexpected, list):
        raise ValueError("inventory_invalid")
    return {
        "server_observed_at": observed_at,
        "cards": cards,
        "members": [{"instance": member["instance"], "up": member.get("up") if isinstance(member.get("up"), bool) else None,
                     "role": member.get("role") if member.get("role") in ("primary", "replica") else "unknown",
                     "sampled_at": member.get("sampled_at") if isinstance(member.get("sampled_at"), str) else None}
                    for member in sorted(members, key=lambda member: member["instance"])],
        "unexpected_sources": len(unexpected),
    }


def write_json_line(file, item):
    file.write(json.dumps(item, sort_keys=True, separators=(",", ":")) + "\n")
    file.flush()
    os.fsync(file.fileno())


def collect(options, fetcher=fetch_health, clock=time.monotonic, sleeper=time.sleep):
    previous_umask = os.umask(0o077)
    try:
        options.output.mkdir(mode=0o700)
        samples_path = options.output / "samples.jsonl"
        summary_path = options.output / "summary.json"
        context = ssl.create_default_context(cafile=str(options.ca))
        context.load_cert_chain(str(options.cert), str(options.key))
        start = clock()
        started_at = utc_now()
        deadline = start + options.duration_seconds
        next_poll = start
        counts = collections.Counter()
        missed_slots = 0
        max_gap = 0.0
        last_poll = None
        interrupted = False
        stop_requested = False

        def stop(_signum, _frame):
            nonlocal stop_requested
            stop_requested = True

        old_handler = signal.signal(signal.SIGTERM, stop)
        try:
            with samples_path.open("x", encoding="utf-8") as samples:
                while not stop_requested and clock() < deadline:
                    remaining = next_poll - clock()
                    if remaining > 0:
                        sleeper(min(remaining, 1))
                        continue
                    poll_started = clock()
                    if last_poll is not None:
                        max_gap = max(max_gap, poll_started - last_poll)
                    last_poll = poll_started
                    item = {"collected_at": utc_now()}
                    try:
                        item.update(summarize_health(fetcher(options, context), options))
                        item["result"] = "observed"
                        counts["observed"] += 1
                        counts["overall_" + item["cards"]["overall"]] += 1
                    except (OSError, ssl.SSLError, http.client.HTTPException, ValueError, TypeError) as error:
                        item["result"] = "unavailable"
                        item["failure"] = str(error) if isinstance(error, ValueError) and str(error).startswith(("http_", "response_", "cluster_", "inventory_", "observation_", "card_")) else type(error).__name__
                        counts["unavailable"] += 1
                    write_json_line(samples, item)
                    next_poll += options.interval_seconds
                    if next_poll <= clock():
                        skipped = math.floor((clock() - next_poll) / options.interval_seconds) + 1
                        missed_slots += skipped
                        next_poll += skipped * options.interval_seconds
        except KeyboardInterrupt:
            interrupted = True
        finally:
            signal.signal(signal.SIGTERM, old_handler)
        elapsed = clock() - start
        sample_count = counts["observed"] + counts["unavailable"]
        collection_complete = not interrupted and not stop_requested and elapsed >= options.duration_seconds
        sampling_continuous = collection_complete and missed_slots == 0 and max_gap <= 30 and sample_count >= math.ceil(options.duration_seconds / options.interval_seconds)
        summary = {
            "kind": "candidate_observation_only" if options.duration_seconds >= FORMAL_DURATION else "smoke_only",
            "collection_complete": collection_complete,
            "sampling_continuous": sampling_continuous,
            "health_endpoint_coverage_complete": sampling_continuous and counts["unavailable"] == 0,
            "sample_count": sample_count,
            "missed_slots": missed_slots,
            "max_monotonic_gap_seconds": round(max_gap, 3),
            "started_at": started_at,
            "finished_at": utc_now(),
            "elapsed_seconds": round(elapsed, 3),
            "environment": options.environment,
            "cluster": options.cluster,
            "members": options.member_ids,
            "candidate_sha": options.candidate_sha,
            "image_digest": options.image_digest,
            "config_sha256": options.config_sha256,
            "rules_sha256": options.rules_sha256,
            "counts": dict(counts),
            "ha_acceptance": "NOT_RUN",
            "scope": "read-only observations; no workload, failure injection, RTO/RPO, or archive proof",
        }
        temporary = options.output / ".summary.tmp"
        with temporary.open("x", encoding="utf-8") as file:
            file.write(json.dumps(summary, sort_keys=True, indent=2) + "\n")
            file.flush()
            os.fsync(file.fileno())
        temporary.replace(summary_path)
        directory_fd = os.open(options.output, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
        return summary
    finally:
        os.umask(previous_umask)


if __name__ == "__main__":
    result = collect(parse_options())
    print(json.dumps({"collection_complete": result["collection_complete"], "kind": result["kind"], "counts": result["counts"]}, sort_keys=True))
    raise SystemExit(0 if result["collection_complete"] else 1)
