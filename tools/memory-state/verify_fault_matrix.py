#!/usr/bin/env python3
"""Run the locally implemented §9.1 checks without treating gaps as acceptance.

Only explicitly named local fixture tests run. Missing tests, skipped selected
tests, package failures and process timeouts are failures. An incomplete matrix
always remains partial, even when every implemented check passes.
"""

import argparse
import json
import pathlib
import re
import subprocess
import sys
import time

from source_fingerprints import fingerprints


ROOT = pathlib.Path(__file__).resolve().parents[2]
MATRIX = ROOT / "docs/contracts/memory-bounded-fault-matrix.json"


def source_fingerprints():
    return fingerprints(ROOT)


def evaluate_tests(events, package, selected, returncode):
    outcomes = {}
    for event in events:
        if event.get("Package") == package and event.get("Test") in selected:
            if event.get("Action") in {"pass", "fail", "skip"}:
                outcomes[event["Test"]] = event["Action"]
    return {name: ("package_failed" if returncode != 0 and outcomes.get(name) == "pass"
                   else outcomes.get(name, "not_executed")) for name in selected}


def read_events(path):
    with path.open() as stream:
        for line in stream:
            try:
                yield json.loads(line)
            except json.JSONDecodeError:
                continue


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--timeout", type=int, default=600)
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--require-complete", action="store_true")
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("positive timeout required")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    matrix = json.loads(MATRIX.read_text())
    grouped = {}
    for scenario in matrix["scenarios"]:
        for check in scenario["checks"]:
            grouped.setdefault(check["package"], set()).add(check["test"])
    source_hashes = source_fingerprints()
    (output / "source-sha256.json").write_text(json.dumps(source_hashes, indent=2) + "\n")
    environment = {
        "go_version": subprocess.run(["go", "version"], cwd=ROOT, check=True,
                                     capture_output=True, text=True).stdout.strip(),
        "head": subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT, check=True,
                               capture_output=True, text=True).stdout.strip(),
        "race": args.race, "timeout_seconds_per_package": args.timeout,
        "matrix_version": matrix["version"],
    }
    (output / "environment.json").write_text(json.dumps(environment, indent=2) + "\n")
    results, commands = {}, []
    # Sequential packages keep this failure matrix's disk and RAM load explicit.
    # go test's JSON stream goes to disk, not to a retained output string.
    for package, selected in grouped.items():
        names = sorted(selected)
        expression = "^(" + "|".join(re.escape(name) for name in names) + ")$"
        command = ["go", "test", "-count=1", "-json", f"-timeout={args.timeout}s"]
        if args.race:
            command.append("-race")
        command.extend(["./" + package, "-run", expression])
        log = output / (package.replace("/", "-") + ".jsonl")
        started = time.monotonic()
        timed_out = False
        with log.open("w") as stream:
            try:
                process = subprocess.run(command, cwd=ROOT, stdout=stream,
                                         stderr=subprocess.STDOUT, timeout=args.timeout + 60)
                code = process.returncode
            except subprocess.TimeoutExpired:
                code, timed_out = -1, True
        # Parse only status events; never retain test output/history in memory.
        outcomes = evaluate_tests(read_events(log), "github.com/akz142857/Halro/" + package, selected, code)
        for name, status in outcomes.items():
            results[package + "/" + name] = status
        commands.append({"argv": command, "exit_code": code, "timed_out": timed_out,
                         "elapsed_seconds": time.monotonic() - started, "log": log.name})
        (output / "commands.json").write_text(json.dumps(commands, indent=2) + "\n")
        print(f"{package}: exit={code}, {len(names)} required tests", flush=True)
    scenarios = []
    for scenario in matrix["scenarios"]:
        checks = [{**check, "result": results[check["package"] + "/" + check["test"]]}
                  for check in scenario["checks"]]
        failed = any(check["result"] != "pass" for check in checks)
        status = "failed" if failed else "partial" if scenario["remaining"] else "verified"
        scenarios.append({**scenario, "checks": checks, "result": status})
    source_unchanged = source_hashes == source_fingerprints()
    failed = not source_unchanged or any(scenario["result"] == "failed" for scenario in scenarios)
    complete = all(scenario["result"] == "verified" for scenario in scenarios)
    summary = {"result": "failed" if failed else "verified" if complete else "partial",
               "source_unchanged": source_unchanged,
               "scope": "local §9.1 fixture evidence; not Linux capacity or production acceptance",
               "scenarios": scenarios}
    (output / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n")
    return 1 if failed else 2 if args.require_complete and not complete else 0


if __name__ == "__main__":
    sys.exit(main())
