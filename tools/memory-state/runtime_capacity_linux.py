#!/usr/bin/env python3
"""Run the opt-in real Runtime fixture in isolated, cached-image Linux containers.

Cross-compile internal/app's tests first. Each sample uses a new volume and two
distinct containers (seed, restart); only this invocation's resources are removed.
The volume cache is not evicted: fresh process/cgroup is not cold filesystem cache.
"""

import argparse
import hashlib
import json
import pathlib
import subprocess
import time
import uuid

from source_fingerprints import fingerprints


IMAGE = "sha256:7e3898f7b011a107d0ef7393d5f604a6e0c0ff05ac4f2476630a8af21059ec9b"
ROOT = pathlib.Path(__file__).resolve().parents[2]


def validate_report(report, count, phase, memory_bytes, peak_ceiling_bytes):
    if (report.get("phase") != phase or report.get("requests") != count
            or report.get("events") != count * 5 or "after_close" not in report):
        raise ValueError("capacity report does not describe the complete selected fixture")
    for label in ("before", "after_gc", "after_close"):
        point = report[label]
        if point.get("cgroup_max") != str(memory_bytes):
            raise ValueError("actual cgroup limit differs from selected limit")
        for field in ("rss_bytes", "cgroup_current_bytes", "cgroup_peak_bytes"):
            value = point.get(field)
            if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
                raise ValueError("missing physical capacity measurement")
        if (point["cgroup_current_bytes"] > point["cgroup_peak_bytes"]
                or point["cgroup_peak_bytes"] > peak_ceiling_bytes):
            raise ValueError("actual physical peak exceeds the selected acceptance ceiling")
        # Each phase starts in a new cgroup. Even initialization before the
        # first Go measurement must not have hit the hard memory limit.
        counters = point.get("cgroup_events", {})
        for field in ("oom", "oom_kill", "max"):
            value = counters.get(field)
            if not isinstance(value, int) or isinstance(value, bool) or value < 0:
                raise ValueError("missing actual cgroup pressure counters")
            if value != 0:
                raise ValueError("capacity fixture hit cgroup pressure/OOM limits")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--requests", nargs="+", type=int, default=[100, 1000, 10000])
    parser.add_argument("--repeat", type=int, default=1)
    parser.add_argument("--mode", choices=("fresh", "selected"), default="fresh",
                        help="selected migrates the actual seed and reopens its seven authenticated roots")
    parser.add_argument("--storage", choices=("volume", "tmpfs"), default="volume",
                        help="tmpfs supplies finite inode evidence on hosts whose volume filesystem cannot report it; its pages count against cgroup memory")
    parser.add_argument("--memory-mib", type=int, default=1024)
    parser.add_argument("--gomemlimit-mib", type=int, default=512)
    parser.add_argument("--peak-ceiling-mib", type=int,
                        help="Maximum actual cgroup peak; defaults to the hard limit")
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    if args.repeat < 1 or any(n < 1 or n > 1_000_000 for n in args.requests):
        parser.error("positive repeat and 1..1000000 requests required")
    if args.peak_ceiling_mib is None:
        args.peak_ceiling_mib = args.memory_mib
    if not (128 <= args.memory_mib <= 65536
            and 32 <= args.gomemlimit_mib < args.memory_mib
            and 32 <= args.peak_ceiling_mib <= args.memory_mib):
        parser.error("finite memory, Go memory and physical peak limits required")
    memory_bytes = args.memory_mib << 20
    peak_ceiling_bytes = args.peak_ceiling_mib << 20
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    source_before = fingerprints(ROOT)
    scope = f"{args.mode} Standalone synthetic lifecycle, fresh processes, warm filesystem cache; not full capacity or production acceptance"
    (output / "source-sha256.json").write_text(json.dumps(source_before, indent=2) + "\n")
    (output / "verification.json").write_text(json.dumps({
        "result": "incomplete", "sample_count": 0,
        "scope": scope,
    }, indent=2) + "\n")
    command_log = []

    def docker(*arguments, check=True, timeout=30):
        started = time.monotonic()
        result = subprocess.run(["docker", *map(str, arguments)], capture_output=True,
                                text=True, timeout=timeout)
        command_log.append({"argv": ["docker", *map(str, arguments)],
                            "exit_code": result.returncode,
                            "elapsed_seconds": time.monotonic() - started,
                            "stdout": result.stdout, "stderr": result.stderr})
        (output / "commands.json").write_text(json.dumps(command_log, indent=2) + "\n")
        if check and result.returncode:
            raise RuntimeError(f"docker {arguments[0]} failed: {result.stderr}")
        return result

    image = json.loads(docker("image", "inspect", IMAGE).stdout)[0]
    info = json.loads(docker("info", "--format", '{"KernelVersion":{{json .KernelVersion}},"OSType":{{json .OSType}}}').stdout)
    digest = hashlib.sha256()
    with binary.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    environment = {"image_id": image["Id"], "image_architecture": image["Architecture"],
                   "kernel": info["KernelVersion"], "os": info["OSType"],
                   "binary_sha256": digest.hexdigest(),
                   "cpus": 2, "memory_bytes": memory_bytes, "swap_total_bytes": memory_bytes,
                   "gomaxprocs": 2, "gomemlimit": f"{args.gomemlimit_mib}MiB", "network": "none",
                   "peak_ceiling_bytes": peak_ceiling_bytes,
                   "rootfs": "readonly", "source_mount": "binary only, readonly",
                   "samples": args.requests, "repeat": args.repeat,
                   "runtime_mode": args.mode,
                   "storage": args.storage,
                   "cache_state": "fresh process and cgroup, filesystem cache not evicted"}
    (output / "environment.json").write_text(json.dumps(environment, indent=2) + "\n")
    summary = []
    for count in args.requests:
        for repetition in range(1, args.repeat + 1):
            sample = output / f"requests-{count}-run-{repetition}"
            sample.mkdir()
            volume = "halro-runtime-capacity-" + uuid.uuid4().hex
            created = False
            completed = False
            containers = []
            try:
                volume_options = []
                if args.storage == "tmpfs":
                    volume_options = ["--driver=local", "--opt=type=tmpfs", "--opt=device=tmpfs",
                                      "--opt=o=size=6g,nr_inodes=200000"]
                docker("volume", "create", "--label", "halro.experiment=runtime-capacity", *volume_options, volume)
                created = True
                if args.storage == "tmpfs":
                    # A tmpfs volume loses its bytes when the last mount exits.
                    # Keep one isolated mount alive across the two processes;
                    # it does not read the fixture or produce any test evidence.
                    keeper = volume + "-mount"
                    containers.append(keeper)
                    docker("run", "--detach", "--name", keeper, "--pull=never",
                           "--network=none", "--read-only", "--cap-drop=ALL",
                           "--security-opt=no-new-privileges", "--pids-limit=16",
                           "--memory=32m", "--memory-swap=32m", "--mount",
                           f"type=volume,src={volume},dst=/scratch", IMAGE,
                           "/bin/sh", "-c", "sleep 3600")
                for phase in ("seed", "restart"):
                    name = volume + "-" + phase
                    containers.append(name)
                    started = time.monotonic()
                    docker("run", "--detach", "--name", name, "--pull=never",
                           "--network=none", "--read-only", "--cap-drop=ALL",
                           "--security-opt=no-new-privileges", "--pids-limit=128",
                           "--cpus=2", f"--memory={memory_bytes}", f"--memory-swap={memory_bytes}",
                           "--mount", f"type=bind,src={binary},dst=/app.test,readonly",
                           "--mount", f"type=volume,src={volume},dst=/scratch",
                           "--workdir=/scratch", "--env=TMPDIR=/scratch",
                           "--env=GOMAXPROCS=2", f"--env=GOMEMLIMIT={args.gomemlimit_mib}MiB",
                           f"--env=HALRO_RUNTIME_CAPACITY_PHASE={phase}",
                           "--env=HALRO_RUNTIME_CAPACITY_ROOT=/scratch/fixture",
                           f"--env=HALRO_RUNTIME_CAPACITY_REQUESTS={count}",
                           f"--env=HALRO_RUNTIME_CAPACITY_MODE={args.mode}", IMAGE,
                           "/app.test", "-test.run=^TestRuntimeCapacityLinux$",
                           "-test.count=1", "-test.v", "-test.timeout=8m")
                    # Poll this specific live container; never restart on a timeout.
                    while True:
                        state = json.loads(docker("inspect", "--format", "{{json .State}}", name).stdout)
                        if not state["Running"]:
                            break
                        if time.monotonic() - started > 510:
                            docker("kill", name, check=False)
                            state = json.loads(docker("inspect", "--format", "{{json .State}}", name).stdout)
                            break
                        time.sleep(2)
                    (sample / f"{phase}-state.json").write_text(json.dumps(state, indent=2) + "\n")
                    logs = docker("logs", name, check=False)
                    (sample / f"{phase}.log").write_text(logs.stdout + logs.stderr)
                    if state["ExitCode"] != 0 or state["OOMKilled"]:
                        raise RuntimeError(f"{count} {phase} failed: {state}")
                    docker("cp", f"{name}:/scratch/{phase}.json", sample / f"{phase}.json")
                    report = json.loads((sample / f"{phase}.json").read_text())
                    validate_report(report, count, phase, memory_bytes, peak_ceiling_bytes)
                    if report.get("runtime_mode") != args.mode:
                        raise RuntimeError("reported Runtime mode differs from requested mode")
                    if args.mode == "selected" and phase == "restart" and not report.get("authenticated_selected"):
                        raise RuntimeError("restart did not use authenticated selected roots")
                    if args.mode == "selected" and phase == "seed":
                        if report.get("migration_real_pressure") is not True:
                            raise RuntimeError("migration did not use its actual OS pressure monitor")
                        budget = report.get("migration_resource_budget", {})
                        if budget.get("TotalRAMBytes") != memory_bytes:
                            raise RuntimeError("logical migration RAM and actual cgroup limit differ")
                    if phase == "restart":
                        summary.append(report)
                        (output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
                        print(json.dumps({"requests": count, "run": repetition,
                                          "open_seconds": report["open_ns"] / 1e9,
                                          "heap_bytes": report["after_gc"]["heap_alloc_bytes"],
                                          "rss_bytes": report["after_gc"]["rss_bytes"],
                                          "cgroup_peak_bytes": report["after_gc"]["cgroup_peak_bytes"]}), flush=True)
                        completed = True
            finally:
                if not completed:
                    (output / "verification.json").write_text(json.dumps({
                        "result": "failed", "sample_count": len(summary),
                        "failed_requests": count, "failed_repetition": repetition,
                        "source_unchanged": source_before == fingerprints(ROOT),
                        "scope": scope,
                    }, indent=2) + "\n")
                # Explicit identities ensure no unrelated containers/volumes are touched.
                for name in containers:
                    docker("rm", "--force", name, check=False)
                if created:
                    docker("volume", "rm", volume, check=False)
    unchanged = source_before == fingerprints(ROOT)
    (output / "verification.json").write_text(json.dumps({
        "result": "verified" if unchanged else "failed", "source_unchanged": unchanged,
        "sample_count": len(summary), "peak_ceiling_bytes": peak_ceiling_bytes,
        "scope": scope,
    }, indent=2) + "\n")
    if not unchanged:
        raise RuntimeError("capacity experiment source changed while samples ran")


if __name__ == "__main__":
    main()
