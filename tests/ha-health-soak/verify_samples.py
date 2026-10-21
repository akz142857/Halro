#!/usr/bin/env python3
"""Read back a completed HA health sample directory and reconcile its evidence."""

import argparse
import collections
import datetime as dt
import hashlib
import json
import math
import os
import stat
from pathlib import Path

import collector


MAX_SUMMARY_BYTES = 32 * 1024
MAX_SAMPLES_BYTES = 128 * 1024 * 1024
MAX_LINE_BYTES = 16 * 1024


class EvidenceError(ValueError):
    pass


def no_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise EvidenceError("duplicate JSON key: " + key)
        result[key] = value
    return result


def reject_constant(value):
    raise EvidenceError("invalid JSON constant: " + value)


def decode_json(data, label):
    try:
        return json.loads(data, object_pairs_hook=no_duplicate_keys,
                          parse_constant=reject_constant)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise EvidenceError(label + " has invalid JSON") from error


def timestamp(value, label):
    if not isinstance(value, str):
        raise EvidenceError(label + " is not a timestamp")
    match = collector.RFC3339_NANO.fullmatch(value)
    if match is None:
        raise EvidenceError(label + " is not RFC3339")
    fraction = match.group("fraction")
    normalized = match.group("second")
    if fraction:
        normalized += "." + fraction[:6].ljust(6, "0")
    normalized += "+00:00" if match.group("zone") == "Z" else match.group("zone")
    try:
        return dt.datetime.fromisoformat(normalized).astimezone(dt.timezone.utc)
    except ValueError as error:
        raise EvidenceError(label + " has an invalid date") from error


def private_regular_file(path, maximum):
    try:
        info = path.lstat()
    except OSError as error:
        raise EvidenceError(str(path) + " cannot be read") from error
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
        raise EvidenceError(str(path) + " must be a private regular file")
    if info.st_size > maximum:
        raise EvidenceError(str(path) + " exceeds the size bound")


def verify_summary(summary):
    if not isinstance(summary, dict):
        raise EvidenceError("summary must be an object")
    if summary.get("kind") not in ("smoke_only", "candidate_observation_only"):
        raise EvidenceError("unknown collection kind")
    if summary.get("ha_acceptance") != "NOT_RUN":
        raise EvidenceError("sampler cannot sign HA acceptance")
    members = summary.get("members")
    if (not isinstance(members, list) or len(members) not in (2, 3) or
            any(not isinstance(member, str) or not member for member in members) or
            members != sorted(set(members))):
        raise EvidenceError("summary member inventory is invalid")
    if not isinstance(summary.get("environment"), str) or not summary["environment"]:
        raise EvidenceError("summary environment is missing")
    if not isinstance(summary.get("cluster"), str) or not summary["cluster"]:
        raise EvidenceError("summary cluster is missing")
    if summary["kind"] == "candidate_observation_only":
        candidate = summary.get("candidate_sha")
        if not isinstance(candidate, str) or not collector.COMMIT.fullmatch(candidate):
            raise EvidenceError("candidate SHA is invalid")
        digest = summary.get("image_digest")
        if (not isinstance(digest, str) or not digest.startswith("sha256:") or
                not collector.SHA256.fullmatch(digest[7:])):
            raise EvidenceError("candidate image digest is invalid")
        for key in ("config_sha256", "rules_sha256"):
            value = summary.get(key)
            if not isinstance(value, str) or not collector.SHA256.fullmatch(value):
                raise EvidenceError(key + " is invalid")
    for key in ("collection_complete", "sampling_continuous",
                "health_endpoint_coverage_complete"):
        if type(summary.get(key)) is not bool:
            raise EvidenceError(key + " must be Boolean")
    if summary["sampling_continuous"] and not summary["collection_complete"]:
        raise EvidenceError("continuous claim lacks completed collection")
    if summary["health_endpoint_coverage_complete"] and not summary["sampling_continuous"]:
        raise EvidenceError("coverage claim lacks continuous collection")
    for key in ("sample_count", "missed_slots"):
        if type(summary.get(key)) is not int or summary[key] < 0:
            raise EvidenceError(key + " must be a nonnegative integer")
    for key in ("elapsed_seconds", "max_monotonic_gap_seconds"):
        value = summary.get(key)
        if (type(value) not in (int, float) or value < 0 or value > 1e9 or
                not math.isfinite(value)):
            raise EvidenceError(key + " must be finite and nonnegative")
    if summary["sampling_continuous"] and (summary["missed_slots"] or
                                            summary["max_monotonic_gap_seconds"] > 30):
        raise EvidenceError("continuous claim conflicts with skipped slots or gap")
    if (summary["kind"] == "candidate_observation_only" and
            summary["collection_complete"] and
            summary["elapsed_seconds"] < collector.FORMAL_DURATION):
        raise EvidenceError("candidate collection ended before 72 hours")
    if (summary["kind"] == "candidate_observation_only" and
            summary["sampling_continuous"] and
            summary["sample_count"] < collector.FORMAL_DURATION // 15):
        raise EvidenceError("candidate has too few samples for a 15-second interval")
    start = timestamp(summary.get("started_at"), "started_at")
    finish = timestamp(summary.get("finished_at"), "finished_at")
    if finish < start:
        raise EvidenceError("finish precedes start")
    if summary["kind"] == "candidate_observation_only" and summary["collection_complete"]:
        wall_elapsed = (finish - start).total_seconds()
        if wall_elapsed < collector.FORMAL_DURATION - 60:
            raise EvidenceError("candidate wall-clock window is shorter than 72 hours")
        if abs(wall_elapsed - summary["elapsed_seconds"]) > 60:
            raise EvidenceError("candidate wall and monotonic durations disagree")
    return start, finish


def verify_observed(row, expected_members):
    cards = row.get("cards")
    if (not isinstance(cards, dict) or set(cards) != set(collector.CARDS) or
            any(not isinstance(value, str) or value not in collector.LEVELS
                for value in cards.values())):
        raise EvidenceError("observed row has invalid cards")
    members = row.get("members")
    if not isinstance(members, list) or len(members) != len(expected_members):
        raise EvidenceError("observed row has incomplete members")
    if [member.get("instance") for member in members if isinstance(member, dict)] != expected_members:
        raise EvidenceError("observed row member inventory differs from summary")
    for member in members:
        if member.get("up") is not None and type(member["up"]) is not bool:
            raise EvidenceError("observed row has invalid member up")
        if member.get("role") not in ("primary", "replica", "unknown"):
            raise EvidenceError("observed row has invalid member role")
        if member.get("sampled_at") is not None:
            timestamp(member["sampled_at"], "member sampled_at")
    if type(row.get("unexpected_sources")) is not int or row["unexpected_sources"] < 0:
        raise EvidenceError("observed row has invalid unexpected_sources")
    timestamp(row.get("server_observed_at"), "server_observed_at")


def verify(directory):
    directory = Path(directory)
    try:
        info = directory.lstat()
    except OSError as error:
        raise EvidenceError("sample directory cannot be read") from error
    if not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o077:
        raise EvidenceError("sample directory must be private and not a symlink")
    summary_path = directory / "summary.json"
    samples_path = directory / "samples.jsonl"
    private_regular_file(summary_path, MAX_SUMMARY_BYTES)
    private_regular_file(samples_path, MAX_SAMPLES_BYTES)
    summary_bytes = summary_path.read_bytes()
    summary = decode_json(summary_bytes, "summary.json")
    start, finish = verify_summary(summary)
    counts = collections.Counter()
    first = last = None
    sample_hash = hashlib.sha256()
    with samples_path.open("rb") as file:
        for number, line in enumerate(file, 1):
            if len(line) > MAX_LINE_BYTES or not line.endswith(b"\n"):
                raise EvidenceError(f"sample line {number} is too long or incomplete")
            sample_hash.update(line)
            row = decode_json(line, f"sample line {number}")
            if not isinstance(row, dict):
                raise EvidenceError(f"sample line {number} is not an object")
            sampled = timestamp(row.get("collected_at"), f"sample line {number} collected_at")
            if sampled < start or sampled > finish:
                raise EvidenceError(f"sample line {number} is outside the collection window")
            if last is not None:
                delta = (sampled - last).total_seconds()
                if delta < 0:
                    raise EvidenceError(f"sample line {number} moves backward in wall time")
                if summary["sampling_continuous"] and delta > 30:
                    raise EvidenceError(f"sample line {number} contradicts continuous coverage")
            if first is None:
                first = sampled
            last = sampled
            if row.get("result") == "observed":
                verify_observed(row, summary["members"])
                counts["observed"] += 1
                counts["overall_" + row["cards"]["overall"]] += 1
            elif row.get("result") == "unavailable":
                if not isinstance(row.get("failure"), str) or not row["failure"]:
                    raise EvidenceError(f"sample line {number} lacks a failure reason")
                if "cards" in row or "members" in row:
                    raise EvidenceError(f"sample line {number} reuses stale health data")
                counts["unavailable"] += 1
            else:
                raise EvidenceError(f"sample line {number} has an invalid result")
    if summary["sample_count"] != sum(counts[key] for key in ("observed", "unavailable")):
        raise EvidenceError("sample count differs from JSONL")
    declared = summary.get("counts")
    if not isinstance(declared, dict) or declared != dict(counts):
        raise EvidenceError("summary counts differ from JSONL")
    if summary["health_endpoint_coverage_complete"] and counts["unavailable"]:
        raise EvidenceError("complete coverage includes unavailable samples")
    if summary["sample_count"] and first is None:
        raise EvidenceError("summary claims samples but JSONL is empty")
    if summary["sampling_continuous"] and first is not None:
        if (first - start).total_seconds() > 30 or (finish - last).total_seconds() > 30:
            raise EvidenceError("continuous claim has an uncovered window edge")
    return {
        "local_structure": "consistent",
        "kind": summary["kind"],
        "candidate_sha": summary.get("candidate_sha", ""),
        "sample_count": summary["sample_count"],
        "counts": dict(counts),
        "first_sample_at": first.isoformat().replace("+00:00", "Z") if first else None,
        "last_sample_at": last.isoformat().replace("+00:00", "Z") if last else None,
        "collection_complete": summary["collection_complete"],
        "sampling_continuous": summary["sampling_continuous"],
        "health_endpoint_coverage_complete": summary["health_endpoint_coverage_complete"],
        "summary_sha256": hashlib.sha256(summary_bytes).hexdigest(),
        "samples_sha256": sample_hash.hexdigest(),
        "ha_acceptance": "NOT_RUN",
    }


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path, help="completed private collection directory")
    args = parser.parse_args(argv)
    try:
        result = verify(args.directory)
    except EvidenceError as error:
        parser.exit(1, "evidence invalid: " + str(error) + "\n")
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    main()
