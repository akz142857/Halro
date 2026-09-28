#!/usr/bin/env python3
"""Render an isolated, read-only Kubernetes HA health sampling Job."""

import argparse
import hashlib
import json
import os
import re
import subprocess
from pathlib import Path
from urllib.parse import urlsplit

import collector


DNS_LABEL = re.compile(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?\Z")
IMAGE = re.compile(r"[^\s@]+@sha256:[0-9a-f]{64}\Z")
SCRIPT = Path(__file__).with_name("collector.py")
ROOT = SCRIPT.parents[2]
SCRIPT_IN_GIT = "tests/ha-health-soak/collector.py"


def dns_label(value, option):
    if len(value) > 63 or not DNS_LABEL.fullmatch(value):
        raise ValueError(f"{option} must be a Kubernetes DNS label")
    return value


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("run-name", "namespace", "url", "secret-name", "environment",
                 "cluster", "members", "candidate-sha", "image-digest",
                 "config-sha256", "rules-sha256", "python-image", "manifest"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--node", help="dedicated monitoring node hostname")
    parser.add_argument("--storage-class", default="standard")
    parser.add_argument("--pvc-size", default="1Gi")
    parser.add_argument("--duration-seconds", type=int, default=collector.FORMAL_DURATION)
    parser.add_argument("--interval-seconds", type=int, default=15)
    args = parser.parse_args(argv)
    try:
        dns_label(args.run_name, "--run-name")
        dns_label(args.run_name + "-script", "--run-name")
        dns_label(args.namespace, "--namespace")
        dns_label(args.secret_name, "--secret-name")
        dns_label(args.storage_class, "--storage-class")
        if args.node:
            if len(args.node) > 253 or any(not DNS_LABEL.fullmatch(part) for part in args.node.split(".")):
                raise ValueError("--node must be a DNS hostname")
        if not IMAGE.fullmatch(args.python_image):
            raise ValueError("--python-image must be pinned by sha256 digest")
        if not re.fullmatch(r"[1-9][0-9]*(Mi|Gi)", args.pvc_size):
            raise ValueError("--pvc-size must be a positive Mi or Gi quantity")
        target = urlsplit(args.url)
        if not target.hostname or not target.hostname.endswith(".svc.cluster.local"):
            raise ValueError("--url must address a cluster-internal Service DNS name")
        collector.parse_options([
            "--url", args.url, "--ca", "/work/tls/ca.crt",
            "--cert", "/work/tls/collector.crt", "--key", "/work/tls/collector.key",
            "--environment", args.environment, "--cluster", args.cluster,
            "--members", args.members, "--duration-seconds", str(args.duration_seconds),
            "--interval-seconds", str(args.interval_seconds),
            "--output", "/evidence/" + args.run_name,
            "--candidate-sha", args.candidate_sha, "--image-digest", args.image_digest,
            "--config-sha256", args.config_sha256, "--rules-sha256", args.rules_sha256,
        ])
        if args.duration_seconds < collector.FORMAL_DURATION:
            raise ValueError("this renderer requires a full 72-hour candidate window")
        if not Path(args.manifest).is_absolute():
            raise ValueError("--manifest must be an absolute path")
    except ValueError as error:
        parser.error(str(error))
    return args


def verify_source_binding(candidate_sha, script):
    try:
        head = subprocess.run(
            ["git", "-C", str(ROOT), "rev-parse", "HEAD"],
            check=True, capture_output=True, timeout=10,
        ).stdout.decode("ascii").strip()
        committed_script = subprocess.run(
            ["git", "-C", str(ROOT), "show", "HEAD:" + SCRIPT_IN_GIT],
            check=True, capture_output=True, timeout=10,
        ).stdout
    except (OSError, UnicodeError, subprocess.CalledProcessError,
            subprocess.TimeoutExpired) as error:
        raise ValueError("cannot read the frozen Git candidate and collector") from error
    if head != candidate_sha:
        raise ValueError("candidate SHA differs from the checked-out Git HEAD")
    if committed_script != script:
        raise ValueError("collector.py differs from the frozen candidate bytes")


def render(args, script):
    script_sha = hashlib.sha256(script.encode("utf-8")).hexdigest()
    name = args.run_name
    namespace = args.namespace
    image = args.python_image
    args_list = [
        "--url", args.url,
        "--ca", "/work/tls/ca.crt",
        "--cert", "/work/tls/collector.crt",
        "--key", "/work/tls/collector.key",
        "--environment", args.environment,
        "--cluster", args.cluster,
        "--members", args.members,
        "--duration-seconds", str(args.duration_seconds),
        "--interval-seconds", str(args.interval_seconds),
        "--output", "/evidence/" + name,
        "--candidate-sha", args.candidate_sha,
        "--image-digest", args.image_digest,
        "--config-sha256", args.config_sha256,
        "--rules-sha256", args.rules_sha256,
    ]
    metadata = lambda resource_name: {"name": resource_name, "namespace": namespace}
    mounts = [
        {"name": "script", "mountPath": "/app", "readOnly": True},
        {"name": "work", "mountPath": "/work"},
        {"name": "evidence", "mountPath": "/evidence"},
    ]
    pod = {
        "metadata": {"labels": {"app": "ha-health-soak"}},
        "spec": {
            "automountServiceAccountToken": False,
            "restartPolicy": "Never",
            "terminationGracePeriodSeconds": 60,
            "securityContext": {"seccompProfile": {"type": "RuntimeDefault"}},
            "initContainers": [
                {
                    "name": "prepare", "image": image,
                    "command": ["sh", "-ec", "mkdir -p /work/tls\n"
                                "cp /source/ca.crt /source/collector.crt /source/collector.key /work/tls/\n"
                                "chown -R 65532:65532 /work /evidence\n"
                                "chmod 700 /work/tls /evidence\nchmod 600 /work/tls/*\n"],
                    "volumeMounts": [
                        {"name": "tls-source", "mountPath": "/source", "readOnly": True},
                        {"name": "work", "mountPath": "/work"},
                        {"name": "evidence", "mountPath": "/evidence"},
                    ],
                },
                {
                    "name": "verify-script", "image": image,
                    "command": ["python3", "-c",
                                "import hashlib, pathlib\n"
                                "actual = hashlib.sha256(pathlib.Path('/app/collector.py').read_bytes()).hexdigest()\n"
                                f"assert actual == '{script_sha}'\n"],
                    "volumeMounts": [mounts[0]],
                },
            ],
            "containers": [
                {
                    "name": "sampler", "image": image,
                    "command": ["python3", "/app/collector.py"], "args": args_list,
                    "env": [{"name": "PYTHONDONTWRITEBYTECODE", "value": "1"}],
                    "securityContext": {
                        "runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                        "allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                        "capabilities": {"drop": ["ALL"]},
                    },
                    "resources": {
                        "requests": {"cpu": "10m", "memory": "32Mi"},
                        "limits": {"memory": "128Mi"},
                    },
                    "volumeMounts": mounts,
                },
            ],
            "volumes": [
                {"name": "script", "configMap": {"name": name + "-script", "defaultMode": 0o444}},
                {"name": "tls-source", "secret": {"secretName": args.secret_name,
                                                  "defaultMode": 0o400,
                                                  "items": [{"key": key, "path": key} for key in
                                                            ("ca.crt", "collector.crt", "collector.key")]}},
                {"name": "work", "emptyDir": {"medium": "Memory"}},
                {"name": "evidence", "persistentVolumeClaim": {"claimName": name}},
            ],
        },
    }
    if args.node:
        pod["spec"]["nodeSelector"] = {"kubernetes.io/hostname": args.node}
    return {
        "apiVersion": "v1", "kind": "List", "items": [
            {"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata(name + "-script"),
             "immutable": True, "data": {"collector.py": script}},
            {"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": metadata(name),
             "spec": {"accessModes": ["ReadWriteOnce"],
                      "storageClassName": args.storage_class,
                      "resources": {"requests": {"storage": args.pvc_size}}}},
            {"apiVersion": "batch/v1", "kind": "Job", "metadata": metadata(name),
             "spec": {"backoffLimit": 0,
                      "activeDeadlineSeconds": args.duration_seconds + 3600,
                      "template": pod}},
        ],
    }


def main(argv=None):
    args = parse_args(argv)
    script_bytes = SCRIPT.read_bytes()
    try:
        verify_source_binding(args.candidate_sha, script_bytes)
    except ValueError as error:
        raise SystemExit("candidate source binding failed: " + str(error)) from error
    script = script_bytes.decode("utf-8")
    manifest = render(args, script)
    destination = Path(args.manifest)
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    previous_umask = os.umask(0o077)
    try:
        descriptor = os.open(destination, flags, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as file:
            json.dump(manifest, file, indent=2)
            file.write("\n")
            file.flush()
            os.fsync(file.fileno())
    finally:
        os.umask(previous_umask)
    print(json.dumps({"manifest": str(destination), "script_sha256":
                      hashlib.sha256(script.encode("utf-8")).hexdigest(),
                      "run_name": args.run_name}, sort_keys=True))


if __name__ == "__main__":
    main()
