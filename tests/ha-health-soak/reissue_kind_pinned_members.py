#!/usr/bin/env python3
"""Reissue local kind member certificates under a staged CA without changing pinned SPKIs.

This is for routine certificate expiry in the isolated kind acceptance cluster.
It must not be used when a member private key may be compromised.
"""

import argparse
import base64
import datetime as dt
import hashlib
import json
import os
import re
import secrets
import subprocess
from pathlib import Path

import kind_tls_replace as rotation
import stage_kind_certificates as stage


MEMBERS = ("halro-0", "halro-1", "halro-2")
PIN = re.compile(r"sha256:[0-9a-f]{64}\Z")
MEMBER_SECRET = "halro/halro-ha-cluster-tls"


def spki(cert_bytes):
    public_pem = stage.run("x509", "-pubkey", "-noout", input_bytes=cert_bytes)
    public_der = stage.run("pkey", "-pubin", "-outform", "DER", input_bytes=public_pem)
    return "sha256:" + hashlib.sha256(public_der).hexdigest()


def parse_pins(values):
    pins = {}
    for value in values:
        name, separator, pin = value.partition("=")
        if not separator or name not in MEMBERS or name in pins or not PIN.fullmatch(pin):
            raise ValueError("--pin requires each unique member and its complete SPKI SHA-256")
        pins[name] = pin
    if set(pins) != set(MEMBERS) or len(set(pins.values())) != len(MEMBERS):
        raise ValueError("--pin must name three members with distinct SPKI hashes")
    return pins


def private_file(path):
    if path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077:
        raise ValueError("private source file is missing, linked or accessible to others")
    if path.stat().st_size > (1 << 20):
        raise ValueError("private source file is oversized")
    return path.read_bytes()


def write_private(path, content):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as file:
        file.write(content)
        file.flush()
        os.fsync(file.fileno())


def copy_stage(source, output, report):
    paths = {"cluster/ca.key", "query/ca.key", "cluster/ca.crt", "query/ca.crt"}
    for files in report["secret_files"].values():
        paths.update(files.values())
    paths.update(report["operator_files"].values())
    for name in ("cluster", "query"):
        (output / name).mkdir(mode=0o700)
    for relative in sorted(paths):
        if Path(relative).is_absolute() or ".." in Path(relative).parts:
            raise ValueError("stage material path escapes its private directory")
        write_private(output / relative, private_file(source / relative))


def reissue(output, source, source_report_digest, backup, backup_digest, pins):
    rotation.ensure_private_directory(source)
    rotation.ensure_private_directory(backup.parent)
    if not output.is_absolute() or output.exists():
        raise ValueError("--output must name a new absolute directory")
    rotation.ensure_private_directory(output.parent)
    source_report = rotation.load_pinned_report(
        source / "stage-report.json", source_report_digest,
        "staged local kind certificates only; not installed or accepted",
    )
    backup_raw = private_file(backup)
    if hashlib.sha256(backup_raw).hexdigest() != backup_digest:
        raise ValueError("member backup SHA-256 differs")
    frozen = json.loads(backup_raw)
    old_data = rotation.validate_secret(
        frozen, "halro", "halro-ha-cluster-tls", source_report["secret_files"][MEMBER_SECRET],
    )
    if source_report["secret_files"] != stage.SECRET_FILES:
        raise ValueError("source stage Secret inventory differs from local kind")
    if source_report["operator_files"] != {
        "ca.crt": "cluster/ca.crt", "operator.crt": "cluster/operator.crt",
        "operator.key": "cluster/operator.key",
    }:
        raise ValueError("source stage operator inventory differs")
    for member in MEMBERS:
        cert = old_data[member + ".crt"]
        key = old_data[member + ".key"]
        if spki(cert) != pins[member]:
            raise ValueError("frozen member certificate differs from configured SPKI pin: " + member)
        key_public = stage.run("pkey", "-pubout", "-outform", "DER", input_bytes=key)
        if "sha256:" + hashlib.sha256(key_public).hexdigest() != pins[member]:
            raise ValueError("frozen member key differs from configured SPKI pin: " + member)
    # The pinned backup contains both CAs at this stage. Certificate/key
    # equality and explicit live configuration pins are the identity gate.
    previous_umask = os.umask(0o077)
    try:
        output.mkdir(mode=0o700)
        copy_stage(source, output, source_report)
        rows = {row["name"]: dict(row) for row in source_report["leaves"]}
        for member in MEMBERS:
            name, sans, purposes = stage.LEAVES[member][1:]
            directory = output / "cluster"
            key = directory / (member + ".key")
            cert = directory / (member + ".crt")
            key.unlink()
            cert.unlink()
            write_private(key, old_data[member + ".key"])
            csr = directory / (member + ".csr")
            extensions = directory / (member + ".cnf")
            stage.run("req", "-new", "-key", key, "-out", csr, "-subj", "/CN=" + name)
            write_private(
                extensions,
                ("[leaf]\nbasicConstraints=critical,CA:FALSE\n"
                 "keyUsage=critical,digitalSignature,keyEncipherment\n"
                 "extendedKeyUsage=" + ",".join(purposes) + "\n"
                 "subjectAltName=" + ",".join(sans) + "\n").encode("ascii"),
            )
            stage.run("x509", "-req", "-in", csr, "-CA", directory / "ca.crt",
                      "-CAkey", directory / "ca.key", "-set_serial",
                      hex(secrets.randbits(127) | 1), "-out", cert, "-days",
                      str(stage.DAYS), "-sha256", "-extfile", extensions,
                      "-extensions", "leaf")
            csr.unlink()
            extensions.unlink()
            stage.verify_leaf(directory / "ca.crt", cert, key, sans, purposes)
            if spki(cert.read_bytes()) != pins[member]:
                raise ValueError("reissued member SPKI differs: " + member)
            if stage.fingerprint(cert) == stage.fingerprint(source / "cluster" / (member + ".crt")):
                raise ValueError("reissued member certificate was not changed")
            rows[member]["sha256_fingerprint"] = stage.fingerprint(cert)
            rows[member]["not_after"] = stage.run(
                "x509", "-in", cert, "-noout", "-enddate",
            ).decode().strip().partition("=")[2]
        report = dict(source_report)
        report["generated_at"] = dt.datetime.now(dt.timezone.utc).isoformat()
        report["leaves"] = [rows[row["name"]] for row in source_report["leaves"]]
        report["member_key_mode"] = "preserved_configured_spki_for_routine_renewal"
        report["member_spki_pins"] = pins
        report["source_stage_report_sha256"] = source_report_digest
        report["member_backup_sha256"] = backup_digest
        report_path = output / "stage-report.json"
        write_private(report_path, (json.dumps(report, sort_keys=True, indent=2) + "\n").encode())
        directory_fd = os.open(output, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
        return hashlib.sha256(report_path.read_bytes()).hexdigest()
    finally:
        os.umask(previous_umask)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-stage", type=Path, required=True)
    parser.add_argument("--source-report-sha256", required=True)
    parser.add_argument("--member-backup", type=Path, required=True)
    parser.add_argument("--member-backup-sha256", required=True)
    parser.add_argument("--pin", action="append", default=[])
    parser.add_argument("--output", type=Path, required=True)
    options = parser.parse_args()
    try:
        digest = reissue(options.output, options.source_stage, options.source_report_sha256,
                         options.member_backup, options.member_backup_sha256,
                         parse_pins(options.pin))
    except (ValueError, KeyError, OSError, json.JSONDecodeError, subprocess.SubprocessError) as error:
        parser.error(type(error).__name__ + ": " + str(error))
    print(json.dumps({"status": "staged_pinned_member_renewal_only",
                      "report_sha256": digest, "member_count": len(MEMBERS)}, sort_keys=True))


if __name__ == "__main__":
    main()
