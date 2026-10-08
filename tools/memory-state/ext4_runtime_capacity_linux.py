#!/usr/bin/env python3
"""Actual isolated ext4, fresh seed/restart processes sharing one finite cgroup.

By default the disposable container owns the backing file. The explicit host
option creates one new exclusive preallocated /tmp file; existing host files
and devices are never adopted. Local synthetic experiment, not production.
"""
import argparse
import hashlib
import json
import pathlib
import subprocess
import time
import uuid

from source_fingerprints import fingerprints
from runtime_capacity_linux import validate_report
from host_backing import HostBacking, HostBackingFailure
from host_loop_retirement import finish_host_join

ROOT = pathlib.Path(__file__).resolve().parents[2]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--requests", type=int, required=True)
    parser.add_argument("--direct-io", action="store_true",
                        help="require DIO=1 on the private loop")
    parser.add_argument("--host-backing-directory", type=pathlib.Path,
                        help="create a NEW exclusive physically preallocated file under this /tmp directory; always require DIO=1")
    parser.add_argument("--image-mib", type=int, default=32768,
                        help="128..32768 MiB private ext4 image (default: 32768)")
    args = parser.parse_args()
    if not 1 <= args.requests <= 1_000_000:
        parser.error("1..1000000 requests required")
    if not 128 <= args.image_mib <= 32768:
        parser.error("128..32768 image MiB required")
    binary = args.binary.resolve(strict=True)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    before = fingerprints(ROOT)
    (output / "source-before.json").write_text(json.dumps(before, indent=2) + "\n")
    records = []

    def docker(*args, check=True):
        r = subprocess.run(["docker", *map(str, args)], capture_output=True, text=True, timeout=30)
        records.append({"argv": ["docker", *map(str, args)], "code": r.returncode,
                        "stdout": r.stdout, "stderr": r.stderr})
        (output / "commands.json").write_text(json.dumps(records, indent=2) + "\n")
        if check and r.returncode:
            raise RuntimeError(r.stderr)
        return r

    host = None
    name = "halro-ext4-capacity-" + uuid.uuid4().hex
    result = {"result": "incomplete"}
    started = time.monotonic()
    state = None
    runtime_log = ""
    launched = False
    runner = None
    runner_before = None
    try:
        # Host space qualification precedes Docker, creation or any suite IO.
        if args.host_backing_directory:
            host = HostBacking(args.host_backing_directory, args.image_mib, output / "host-backing.json")
        image = json.loads(docker("image", "inspect", "ubuntu:24.04").stdout)[0]
        direct = bool(args.direct_io or host)
        runner = ROOT / ("tools/memory-state/ext4_host_fixture_linux.sh" if host else
                         "tools/memory-state/ext4_direct_fixture_linux.sh" if direct else
                         "tools/memory-state/ext4_fixture_linux.sh")
        runner_before = hashlib.sha256(runner.read_bytes()).hexdigest()
        digest = hashlib.sha256()
        with binary.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1 << 20), b""):
                digest.update(chunk)
        environment = {"image_id": image["Id"], "binary_sha256": digest.hexdigest(),
            "runner_sha256": runner_before, "requests": args.requests,
            "memory_bytes": 2 << 30, "peak_ceiling_bytes": 1536 << 20, "swap_total_bytes": 2 << 30,
            "gomemlimit": "768MiB", "cpus": 2, "ext4_image_mib": args.image_mib,
            "loop_direct_io_required": direct, "host_backing": host.record if host else None,
            "cache": "shared captured ext4/cache charges, fresh seed and restart processes, same cgroup",
            "scope": "actual cold migration and selected Standalone restart; synthetic lifecycle; not whole V02 acceptance"}
        (output / "environment.json").write_text(json.dumps(environment, indent=2) + "\n")
        result["environment"] = environment
        # Fixed limits and original seed/restart processes remain unchanged.
        script = f'''export HALRO_RUNTIME_CAPACITY_ROOT="$HALRO_EXT4_ROOT/fixture"
export HALRO_RUNTIME_CAPACITY_MODE=selected HALRO_RUNTIME_CAPACITY_REQUESTS={args.requests}
export HALRO_RUNTIME_CAPACITY_PHASE=seed
/suite.test -test.run '^TestRuntimeCapacityLinux$' -test.v -test.timeout=4h || exit "$?"
export HALRO_RUNTIME_CAPACITY_PHASE=restart
/suite.test -test.run '^TestRuntimeCapacityLinux$' -test.v -test.timeout=4h
'''
        mounts = ["--mount", f"type=bind,src={binary},dst=/suite.test,readonly",
                  "--mount", f"type=bind,src={runner},dst=/runner.sh,readonly"]
        if host:
            host.verify()
            # Never mount the host parent or an existing host device.
            mounts += ["--mount", f"type=bind,src={host.path},dst=/halro-host-owned/state.ext4.img"]
        docker("run", "--detach", "--name", name, "--pull=never", "--privileged", "--network=none",
            "--memory=2g", "--memory-swap=2g", "--cpus=2", "--pids-limit=256",
            "-e", "GOMAXPROCS=2", "-e", "GOMEMLIMIT=768MiB", "-e", f"HALRO_EXT4_IMAGE_MIB={args.image_mib}",
            *mounts, image["Id"], "/bin/sh", "/runner.sh", "/bin/sh", "-c", script)
        launched = True
        while True:
            state = json.loads(docker("inspect", name).stdout)[0]["State"]
            if not state["Running"]:
                break
            time.sleep(10)
        log = docker("logs", name)
        runtime_log = log.stdout + log.stderr
        (output / "runtime.log").write_text(runtime_log)
        (output / "container-state.json").write_text(json.dumps(state, indent=2) + "\n")
        if state["ExitCode"] != 0 or state["OOMKilled"]:
            raise RuntimeError("actual ext4 experiment failed; original runtime/state logs retained")
        backing = "/halro-host-owned/state.ext4.img" if host else "/halro-fixture/state.ext4.img"
        if direct and f"HALRO_EXT4_LOOP direct_io=1 owned_backing={backing}" not in runtime_log:
            raise RuntimeError("missing actual owned-loop DIO=1 qualification")
        reports = {}
        for line in runtime_log.splitlines():
            if "RUNTIME_CAPACITY " in line:
                report = json.loads(line.split("RUNTIME_CAPACITY ", 1)[1])
                validate_report(report, args.requests, report["phase"], 2 << 30, 1536 << 20)
                if report["phase"] in reports:
                    raise RuntimeError("duplicate capacity phase")
                reports[report["phase"]] = report
        if set(reports) != {"seed", "restart"}:
            raise RuntimeError("missing completed native/migration/restart evidence")
        result.update(result="passed", reports=reports)
    except BaseException as error:
        if isinstance(error, HostBackingFailure):
            host = error.owner
        result.update(result="failed", error=str(error))
        if launched:
            log = docker("logs", name, check=False)
            runtime_log = log.stdout + log.stderr
            (output / "runtime.log").write_text(runtime_log)
        raise
    finally:
        cleanup_error = None
        if host:
            try:
                finish_host_join(host, state, runtime_log, docker, image["Id"], name, ROOT)
            except BaseException as error:
                cleanup_error = str(error)
                host.record['state'] = 'cleanup_unconfirmed_retained'
                host._save()
                result.update(result="failed", cleanup_error=cleanup_error)
        after = fingerprints(ROOT)
        (output / "source-after.json").write_text(json.dumps(after, indent=2) + "\n")
        runner_after = hashlib.sha256(runner.read_bytes()).hexdigest() if runner else None
        result.update(elapsed_seconds=time.monotonic() - started,
                      source_unchanged=before == after and runner_before == runner_after,
                      runner_after_sha256=runner_after)
        (output / "verification.json").write_text(json.dumps(result, indent=2) + "\n")
        if launched:
            docker("rm", "-f", name, check=False)
        if cleanup_error and result.get('error') is None:
            raise RuntimeError(cleanup_error)


if __name__ == "__main__":
    main()
