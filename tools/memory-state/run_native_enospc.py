#!/usr/bin/env python3
"""Run actual ENOSPC fixtures only inside dedicated small, isolated Linux tmpfs.

Requires a locally cached image with the requested architecture. No pulls, network,
host disk filling, provider traffic or production acceptance are performed.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

from source_fingerprints import fingerprints


CASES = (
    ("audit", "TestAuditActualLinuxENOSPCPreservesPrefixAndReopensPartialTail", "AUDIT"),
    ("governance", "TestGovernanceActualLinuxENOSPCPreservesPrefixAndReopensPartialTail", "GOVERNANCE"),
    ("metadatajournal", "TestMetadataActualLinuxENOSPCPreservesPrefixAndReopensPartialTail", "METADATA"),
    ("ledger", "TestLedgerActualLinuxENOSPCPreservesPrefixAndReopensPartialTail", "LEDGER"),
    ("replication", "TestOrderingActualLinuxENOSPCPreservesHeadAndReopens", "ORDERING"),
    ("ledger", "TestLedgerProjectionActualLinuxENOSPCDiscardsAndRebuilds", "LEDGER"),
    ("store/bolt", "TestMetadataProjectionActualLinuxENOSPCRequiresReopen", "METADATA_STORE"),
    ("app", "TestRuntimeMetadataActualLinuxENOSPCRetiresTrafficAndRecovers", "RUNTIME"),
    ("app", "TestBudgetedBackupActualLinuxENOSPCRetiresOnlyOwnCiphertext", "BACKUP"),
)


def accepted(name, state, log):
    return (state.get("ExitCode") == 0 and not state.get("OOMKilled")
            and f"=== RUN   {name}\n" in log
            and f"--- PASS: {name} (" in log
            and f"--- SKIP: {name} (" not in log
            and "actual tmpfs ENOSPC:" in log
            and "free_blocks_at_failure=0" in log
            and "no space left on device" in log)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="Exact locally cached image ID (sha256:...)")
    parser.add_argument("--output", required=True, type=Path, help="New evidence directory")
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--case", action="append", choices=[case[1] for case in CASES],
                        help="Run only these named cases; default is the complete inventory")
    args = parser.parse_args()
    cases = [case for case in CASES if not args.case or case[1] in args.case]
    if not args.image.startswith("sha256:") or len(args.image) != 71:
        parser.error("use the full immutable sha256 image ID")
    if not 1 <= args.timeout <= 900:
        parser.error("timeout must be between 1 and 900 seconds")
    root = Path(__file__).resolve().parents[2]
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    commands = []
    before = fingerprints(root)
    (out / "source-sha256.json").write_text(json.dumps(before, indent=2) + "\n")

    def run(argv, *, check=True, timeout=30, env=None):
        result = subprocess.run(argv, cwd=root, capture_output=True, text=True,
                                timeout=timeout, env=env)
        commands.append({"argv": argv, "exit_code": result.returncode,
                         "stdout": result.stdout, "stderr": result.stderr})
        (out / "commands.json").write_text(json.dumps(commands, indent=2) + "\n")
        if check and result.returncode:
            raise RuntimeError(f"command failed: {argv[0]}: {result.stderr}")
        return result

    image = json.loads(run(["docker", "image", "inspect", args.image]).stdout)[0]
    if image["Id"] != args.image or image.get("Os") != "linux" or image.get("Architecture") not in ("arm64", "amd64"):
        raise RuntimeError("image identity, Linux platform or architecture not verified")
    (out / "image.json").write_text(json.dumps(image, indent=2) + "\n")
    (out / "host.json").write_text(run(["docker", "info", "--format", "{{json .}}"]).stdout)
    results = []
    failure = None
    try:
        with tempfile.TemporaryDirectory(prefix="halro-enospc-binaries-") as temporary:
            env = dict(os.environ, GOOS="linux", GOARCH=image["Architecture"], CGO_ENABLED="0", GOPROXY="off")
            binaries = {}
            for package in dict.fromkeys(case[0] for case in cases):
                binary = Path(temporary) / (package.replace("/", "-") + ".test")
                run(["go", "test", "-c", "-o", str(binary), "./internal/" + package], env=env, timeout=300)
                binaries[package] = binary
            for package, name, env_store in cases:
                sample = out / name
                sample.mkdir()
                container = "halro-enospc-" + uuid.uuid4().hex
                binary = binaries[package]
                started = time.monotonic()
                try:
                    run(["docker", "run", "--detach", "--name", container, "--pull=never",
                         "--network=none", "--read-only", "--cap-drop=ALL",
                         "--security-opt=no-new-privileges", "--pids-limit=128", "--cpus=2",
                         "--memory=256m", "--memory-swap=256m",
                         "--mount", f"type=bind,src={binary},dst=/app.test,readonly",
                         "--mount", f"type=bind,src={root},dst=/workspace,readonly",
                         "--workdir=/workspace/internal/" + package,
                         "--tmpfs", "/scratch:rw,size=16m,nr_inodes=4096",
                         "--env=TMPDIR=/scratch", "--env=GOMAXPROCS=2", "--env=GOMEMLIMIT=128MiB",
                         "--env=HALRO_" + env_store + "_ENOSPC_ROOT=/scratch", args.image,
                         "/app.test", "-test.run=^" + name + "$", "-test.count=1", "-test.v",
                         "-test.timeout=" + str(args.timeout) + "s"])
                    while True:
                        state = json.loads(run(["docker", "inspect", "--format", "{{json .State}}", container]).stdout)
                        if not state["Running"]:
                            break
                        if time.monotonic() - started > args.timeout + 30:
                            raise RuntimeError("container test deadline exceeded")
                        time.sleep(1)
                    captured = run(["docker", "logs", container], check=False)
                    log = captured.stdout + captured.stderr
                    (sample / "tests.log").write_text(log)
                    (sample / "state.json").write_text(json.dumps(state, indent=2) + "\n")
                    result = {"package": package, "name": name, "state": state,
                              "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                              "elapsed_seconds": time.monotonic() - started,
                              "accepted": accepted(name, state, log)}
                    results.append(result)
                    print(name, "PASS" if result["accepted"] else "FAIL", flush=True)
                    if not result["accepted"]:
                        raise RuntimeError("actual named ENOSPC fixture not accepted: " + name)
                finally:
                    run(["docker", "rm", "--force", container], check=False)
    except Exception as error:
        failure = str(error)
    unchanged = before == fingerprints(root)
    summary = {"scope": "local actual native/projection ENOSPC; not complete Runtime/HA/capacity acceptance",
               "selected_cases": [case[1] for case in cases],
               "cases": results, "source_unchanged": unchanged, "failure": failure,
               "accepted": failure is None and unchanged and len(results) == len(cases)
                           and all(result["accepted"] for result in results)}
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    return 0 if summary["accepted"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
