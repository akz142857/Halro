#!/usr/bin/env python3
"""Guard one local kind TLS Secret replacement; dry-run unless --apply is given."""

import argparse
import base64
import hashlib
import json
import os
import re
import subprocess
from pathlib import Path


CONTEXT = "kind-halro-ha-health-local"
SHA256 = re.compile(r"[0-9a-f]{64}\Z")


def digest(data):
    return hashlib.sha256(data).hexdigest()


def spki_sha256(material, private=False):
    if private:
        public_der = subprocess.run(
            ["openssl", "pkey", "-pubout", "-outform", "DER"], input=material,
            check=True, capture_output=True, timeout=20,
        ).stdout
    else:
        public_pem = subprocess.run(
            ["openssl", "x509", "-pubkey", "-noout"], input=material,
            check=True, capture_output=True, timeout=20,
        ).stdout
        public_der = subprocess.run(
            ["openssl", "pkey", "-pubin", "-outform", "DER"], input=public_pem,
            check=True, capture_output=True, timeout=20,
        ).stdout
    return "sha256:" + digest(public_der)


def load_pinned_report(path, expected_digest, status):
    if not SHA256.fullmatch(expected_digest):
        raise ValueError("report SHA-256 must be 64 lowercase hex characters")
    if path.is_symlink() or not path.is_file():
        raise ValueError("pinned report is missing or linked")
    raw = path.read_bytes()
    if digest(raw) != expected_digest:
        raise ValueError("pinned report SHA-256 differs")
    report = json.loads(raw)
    if report.get("version") != 1 or report.get("status", report.get("scope")) != status:
        raise ValueError("pinned report version or status differs")
    return report


def read_material(root, relative):
    path = Path(relative)
    if path.is_absolute() or ".." in path.parts or not path.parts:
        raise ValueError("material path escapes its private directory")
    result = root / path
    if (result.is_symlink() or not result.is_file() or not result.resolve().is_relative_to(root.resolve())
            or result.stat().st_size > (1 << 20)):
        raise ValueError("material file is missing, linked, or oversized")
    return result.read_bytes()


def ensure_private_directory(path):
    if (not path.is_absolute() or path.is_symlink() or not path.is_dir()
            or path.stat().st_mode & 0o077):
        raise ValueError("material directory must exist with private permissions")


def validate_roots(stage_dir, overlap_dir, overlap_report):
    for root in ("cluster", "query"):
        expected = overlap_report["roots"][root]
        for directory, relative, key in ((overlap_dir, root + "-old-ca.crt", "old_ca_pem_sha256"),
                                         (stage_dir, root + "/ca.crt", "new_ca_pem_sha256"),
                                         (overlap_dir, root + "-overlap-ca.crt", "overlap_bundle_sha256")):
            if digest(read_material(directory, relative)) != expected[key]:
                raise ValueError("CA material differs from pinned overlap report")


def fetch_secret(namespace, name):
    result = subprocess.run(["kubectl", "--context", CONTEXT, "-n", namespace,
                             "get", "secret", name, "-o", "json"],
                            check=True, capture_output=True, timeout=20)
    return json.loads(result.stdout)


def validate_secret(secret, namespace, name, keys):
    metadata = secret.get("metadata", {})
    if metadata.get("namespace") != namespace or metadata.get("name") != name:
        raise ValueError("Kubernetes returned a different Secret identity")
    if not metadata.get("resourceVersion") or secret.get("type") != "Opaque" or secret.get("immutable"):
        raise ValueError("Secret resource version, type, or mutability differs")
    if metadata.get("ownerReferences") or metadata.get("finalizers"):
        raise ValueError("Secret has unsupported owners or finalizers")
    if "kubectl.kubernetes.io/last-applied-configuration" in metadata.get("annotations", {}):
        raise ValueError("Secret has a last-applied annotation that may duplicate private keys")
    data = secret.get("data", {})
    if set(data) != set(keys):
        raise ValueError("Secret key inventory differs from staged manifest")
    decoded = {}
    for key, value in data.items():
        if not isinstance(value, str):
            raise ValueError("Secret data is invalid")
        decoded[key] = base64.b64decode(value, validate=True)
    return decoded


def plan_secret(secret, identity, phase, stage_dir, overlap_dir, stage_report):
    if identity not in stage_report["secret_files"]:
        raise ValueError("Secret is outside the staged kind inventory")
    namespace, name = identity.split("/", 1)
    files = stage_report["secret_files"][identity]
    live = validate_secret(secret, namespace, name, files)
    ca_keys = [key for key, path in files.items() if path.endswith("/ca.crt")]
    if len(ca_keys) != 1:
        raise ValueError("Secret must have exactly one mapped CA field")
    ca_key = ca_keys[0]
    root = files[ca_key].split("/", 1)[0]
    old_ca = read_material(overlap_dir, root + "-old-ca.crt")
    bundle = read_material(overlap_dir, root + "-overlap-ca.crt")
    new_ca = read_material(stage_dir, files[ca_key])
    expected = dict(live)
    if phase == "trust":
        if live[ca_key] not in (old_ca, bundle):
            raise ValueError("trust phase requires the original or overlap CA")
        for key in files:
            if key != ca_key and key.endswith(".crt"):
                old_cert = read_material(overlap_dir, namespace + "-" + name + "-" + key)
                if live[key] != old_cert:
                    raise ValueError("trust phase requires the frozen old leaf certificates")
        expected[ca_key] = bundle
    elif phase == "leaves":
        if live[ca_key] != bundle:
            raise ValueError("leaf phase requires the overlap CA bundle")
        if (identity == "halro/halro-ha-cluster-tls" and
                stage_report.get("member_key_mode") != "preserved_configured_spki_for_routine_renewal"):
            raise ValueError("member leaves require a pinned SPKI renewal stage")
        old_certs = {}
        for key, relative in files.items():
            if key == ca_key:
                continue
            new_bytes = read_material(stage_dir, relative)
            if key.endswith(".crt"):
                old_bytes = read_material(overlap_dir, namespace + "-" + name + "-" + key)
                if live[key] not in (old_bytes, new_bytes):
                    raise ValueError("leaf phase found an unknown certificate")
                old_certs[key] = old_bytes
            expected[key] = new_bytes
        if (identity == "halro/halro-ha-cluster-tls" and
                stage_report.get("member_key_mode") == "preserved_configured_spki_for_routine_renewal"):
            pins = stage_report.get("member_spki_pins", {})
            if set(pins) != {"halro-0", "halro-1", "halro-2"}:
                raise ValueError("member renewal stage lacks the complete pinned SPKI inventory")
            if any(live[key] != expected[key] for key in files if key.endswith(".key")):
                raise ValueError("member renewal private key differs from the pinned stage")
            for member, pin in pins.items():
                if not isinstance(pin, str) or not pin.startswith("sha256:") or not SHA256.fullmatch(pin[7:]):
                    raise ValueError("member renewal SPKI pin is invalid")
                if (spki_sha256(old_certs[member + ".crt"]) != pin or
                        spki_sha256(expected[member + ".crt"]) != pin or
                        spki_sha256(live[member + ".key"], private=True) != pin):
                    raise ValueError("member renewal certificate or key differs from configured SPKI pin: " + member)
            cert_keys = sorted(old_certs)
            if any(live[key] == old_certs[key] for key in cert_keys) and any(
                    live[key] != old_certs[key] for key in cert_keys):
                raise ValueError("leaf phase found a partially changed Secret")
        elif any(live[key] == expected[key] for key in files if key != ca_key) and any(
                live[key] != expected[key] for key in files if key != ca_key):
            raise ValueError("leaf phase found a partially changed Secret")
    elif phase == "final":
        if live[ca_key] not in (bundle, new_ca):
            raise ValueError("final phase requires the overlap or new CA")
        for key, relative in files.items():
            if key != ca_key and live[key] != read_material(stage_dir, relative):
                raise ValueError("final phase requires every new leaf and key")
        expected[ca_key] = new_ca
    else:
        raise ValueError("unknown rotation phase")
    replacement = {"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
                   "metadata": {"namespace": namespace, "name": name,
                                "resourceVersion": secret["metadata"]["resourceVersion"]},
                   "data": {key: base64.b64encode(value).decode("ascii") for key, value in expected.items()}}
    for field in ("labels", "annotations"):
        if secret["metadata"].get(field):
            replacement["metadata"][field] = secret["metadata"][field]
    return replacement, expected, sorted(key for key in files if expected[key] != live[key])


def replace_secret(namespace, replacement, dry_run):
    command = ["kubectl", "--context", CONTEXT, "-n", namespace, "replace"]
    if dry_run:
        command.append("--dry-run=server")
    command.extend(["-f", "-", "-o", "name"])
    result = subprocess.run(command, input=json.dumps(replacement).encode(),
                            check=True, capture_output=True, timeout=20)
    if result.stdout.decode().strip() != "secret/" + replacement["metadata"]["name"]:
        raise ValueError("Kubernetes returned an unexpected replacement identity")


def write_backup(directory, identity, phase, secret):
    ensure_private_directory(directory)
    namespace, name = identity.split("/", 1)
    path = directory / (phase + "-" + namespace + "-" + name + "-rv" +
                        secret["metadata"]["resourceVersion"] + ".json")
    raw = json.dumps(secret, sort_keys=True, separators=(",", ":")).encode() + b"\n"
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as file:
        file.write(raw)
        file.flush()
        os.fsync(file.fileno())
    directory_fd = os.open(directory, os.O_RDONLY)
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)
    return digest(raw)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--secret", required=True, help="namespace/name from the staged manifest")
    parser.add_argument("--phase", required=True, choices=("trust", "leaves", "final"))
    parser.add_argument("--stage-dir", required=True, type=Path)
    parser.add_argument("--stage-report-sha256", required=True)
    parser.add_argument("--overlap-dir", required=True, type=Path)
    parser.add_argument("--overlap-report-sha256", required=True)
    parser.add_argument("--expect-resource-version")
    parser.add_argument("--apply", action="store_true", help="replace one Secret after a server dry-run")
    parser.add_argument("--backup-dir", type=Path, help="required private directory when applying")
    options = parser.parse_args()
    if options.apply and (not options.expect_resource_version or not options.backup_dir):
        parser.error("--apply requires --expect-resource-version and --backup-dir")
    try:
        ensure_private_directory(options.stage_dir)
        ensure_private_directory(options.overlap_dir)
        stage_report = load_pinned_report(options.stage_dir / "stage-report.json",
                                          options.stage_report_sha256,
                                          "staged local kind certificates only; not installed or accepted")
        overlap_report = load_pinned_report(options.overlap_dir / "overlap-report.json",
                                            options.overlap_report_sha256,
                                            "public_chain_overlap_prepared_not_applied")
        if overlap_report.get("context") != CONTEXT:
            raise ValueError("overlap report names a different Kubernetes context")
        validate_roots(options.stage_dir, options.overlap_dir, overlap_report)
        if options.secret not in stage_report["secret_files"]:
            raise ValueError("Secret is outside the staged kind inventory")
        namespace, name = options.secret.split("/", 1)
        live = fetch_secret(namespace, name)
        if options.expect_resource_version and live["metadata"]["resourceVersion"] != options.expect_resource_version:
            raise ValueError("Secret resource version differs from operator expectation")
        replacement, expected, changed = plan_secret(live, options.secret, options.phase,
                                                      options.stage_dir, options.overlap_dir, stage_report)
        if changed:
            replace_secret(namespace, replacement, dry_run=True)
            if options.apply:
                backup_digest = write_backup(options.backup_dir, options.secret, options.phase, live)
                replace_secret(namespace, replacement, dry_run=False)
                after = fetch_secret(namespace, name)
                if validate_secret(after, namespace, name, expected) != expected:
                    raise ValueError("post-replacement Secret data differs")
                status = "applied_and_read_back"
                after_version = after["metadata"]["resourceVersion"]
            else:
                backup_digest = None
                status = "server_dry_run_only"
                after_version = None
        else:
            backup_digest = None
            status = "already_at_phase_target"
            after_version = live["metadata"]["resourceVersion"]
        print(json.dumps({"status": status, "secret": options.secret, "phase": options.phase,
                          "resource_version_before": live["metadata"]["resourceVersion"],
                          "resource_version_after": after_version,
                          "changed_keys": changed, "backup_sha256": backup_digest}, sort_keys=True))
    except (ValueError, KeyError, OSError, json.JSONDecodeError, subprocess.SubprocessError) as error:
        parser.error(type(error).__name__ + ": " + str(error) if isinstance(error, ValueError) else type(error).__name__)


if __name__ == "__main__":
    main()
