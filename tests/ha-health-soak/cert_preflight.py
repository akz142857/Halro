#!/usr/bin/env python3
"""Check the full declared Kubernetes certificate inventory for a soak window."""

import argparse
import base64
import datetime as dt
import hashlib
import json
import os
import re
import ssl
import subprocess
import time
from pathlib import Path


NAMESPACE = re.compile(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?\Z")
SECRET_NAME = re.compile(r"[a-z0-9]([-a-z0-9.]*[a-z0-9])?\Z")
CERT_KEY = re.compile(r"[A-Za-z0-9_.-]+\.crt\Z")
FILE_LABEL = re.compile(r"[A-Za-z0-9_-]+\Z")
PEM_CERT = re.compile(rb"-----BEGIN CERTIFICATE-----[A-Za-z0-9+/=\s]+-----END CERTIFICATE-----")
MAX_MANIFEST = 1 << 20


def load_manifest(path):
    raw = path.read_bytes()
    if len(raw) > MAX_MANIFEST:
        raise ValueError("certificate manifest is too large")
    manifest = json.loads(raw)
    if not isinstance(manifest, dict) or set(manifest) != {"version", "context", "secrets"} or manifest["version"] != 1:
        raise ValueError("certificate manifest must be version 1 with context and secrets")
    if not isinstance(manifest["context"], str) or not manifest["context"].strip():
        raise ValueError("certificate manifest requires an exact Kubernetes context")
    secrets = manifest["secrets"]
    if not isinstance(secrets, list) or not secrets:
        raise ValueError("certificate manifest requires nonempty secret inventory")
    seen = set()
    for entry in secrets:
        if not isinstance(entry, dict) or set(entry) != {"namespace", "name", "cert_keys"}:
            raise ValueError("certificate manifest has an invalid secret entry")
        namespace, name, keys = entry["namespace"], entry["name"], entry["cert_keys"]
        if not isinstance(namespace, str) or not NAMESPACE.fullmatch(namespace) or not isinstance(name, str) or not SECRET_NAME.fullmatch(name):
            raise ValueError("certificate manifest has an invalid namespace or secret name")
        if (namespace, name) in seen or not isinstance(keys, list) or not keys or any(
            not isinstance(key, str) or not CERT_KEY.fullmatch(key) for key in keys
        ) or len(set(keys)) != len(keys):
            raise ValueError("certificate manifest has duplicate or invalid certificate keys")
        seen.add((namespace, name))
    return manifest, hashlib.sha256(raw).hexdigest()


def fetch_secret(context, namespace, name):
    command = ["kubectl", "--context", context, "-n", namespace, "get", "secret", name, "-o"]
    # Kubectl evaluates the template itself. Only key names and public
    # certificate values reach this process; private key bytes do not.
    inventory = subprocess.run(command + ["go-template={{.metadata.namespace}}/{{.metadata.name}}|{{range $key, $value := .data}}{{$key}},{{end}}"],
                               check=True, capture_output=True, timeout=20).stdout.decode("ascii")
    identity, separator, names = inventory.partition("|")
    if separator != "|" or identity != namespace + "/" + name:
        raise ValueError("Kubernetes returned a different Secret identity")
    keys = [key for key in names.split(",") if key]
    if len(keys) != len(set(keys)) or any(not re.fullmatch(r"[A-Za-z0-9_.-]+", key) for key in keys):
        raise ValueError("Kubernetes returned duplicate or invalid Secret keys")
    certificates = {}
    for key in keys:
        if key.endswith(".crt"):
            template = 'go-template={{index .data "' + key + '"}}'
            certificates[key] = subprocess.run(command + [template], check=True, capture_output=True, timeout=20).stdout.decode("ascii").strip()
    return certificates


def inspect_certificate(pem):
    result = subprocess.run(
        ["openssl", "x509", "-noout", "-startdate", "-enddate", "-fingerprint", "-sha256"],
        input=pem, check=True, capture_output=True, timeout=10,
    )
    fields = dict(line.split("=", 1) for line in result.stdout.decode("ascii").splitlines())
    starts = ssl.cert_time_to_seconds(fields["notBefore"])
    expires = ssl.cert_time_to_seconds(fields["notAfter"])
    fingerprint = fields["sha256 Fingerprint"].replace(":", "").lower()
    if not re.fullmatch(r"[0-9a-f]{64}", fingerprint):
        raise ValueError("certificate fingerprint is invalid")
    return starts, expires, fingerprint


def split_certificate_chain(pem):
    matches = list(PEM_CERT.finditer(pem))
    if not matches:
        raise ValueError("certificate file has no PEM certificate")
    previous = 0
    for match in matches:
        if pem[previous:match.start()].strip():
            raise ValueError("certificate file contains non-certificate data")
        previous = match.end()
    if pem[previous:].strip():
        raise ValueError("certificate file contains trailing non-certificate data")
    return [match.group(0) for match in matches]


def parse_certificate_files(values):
    files = []
    seen = set()
    for value in values:
        label, separator, path = value.partition("=")
        if (separator != "=" or not FILE_LABEL.fullmatch(label) or label in seen or
                not os.path.isabs(path) or os.path.normpath(path) != path):
            raise ValueError("--certificate-file requires a unique label and clean absolute path")
        seen.add(label)
        files.append((label, path))
    return files


def read_certificate_file(path):
    return Path(path).read_bytes()


def check_window(manifest, manifest_sha256, valid_seconds, fetcher=fetch_secret, inspector=inspect_certificate,
                 now=None, files=(), reader=read_certificate_file):
    checked_at = time.time() if now is None else now
    deadline = checked_at + valid_seconds
    rows, failures = [], []
    for entry in manifest["secrets"]:
        namespace, name = entry["namespace"], entry["name"]
        identity = namespace + "/" + name
        try:
            data = fetcher(manifest["context"], namespace, name)
            if not isinstance(data, dict) or {key for key in data if key.endswith(".crt")} != set(entry["cert_keys"]):
                raise ValueError("certificate key inventory differs from manifest")
            for key in sorted(entry["cert_keys"]):
                encoded = data[key]
                if not isinstance(encoded, str):
                    raise ValueError("certificate value is invalid")
                for index, certificate in enumerate(split_certificate_chain(base64.b64decode(encoded, validate=True))):
                    starts, expires, fingerprint = inspector(certificate)
                    valid = starts <= checked_at and expires >= deadline
                    rows.append({"secret": identity, "key": key, "chain_index": index,
                                 "sha256_fingerprint": fingerprint,
                                 "not_before": dt.datetime.fromtimestamp(starts, dt.timezone.utc).isoformat().replace("+00:00", "Z"),
                                 "not_after": dt.datetime.fromtimestamp(expires, dt.timezone.utc).isoformat().replace("+00:00", "Z"),
                                 "covers_window": valid})
                    if not valid:
                        reason = "not_yet_valid" if starts > checked_at else "expires_before_required_window"
                        failures.append(identity + "/" + key + ":chain_" + str(index) + ":" + reason)
        except (ValueError, KeyError, UnicodeError, subprocess.SubprocessError, OSError) as error:
            failures.append(identity + ":" + type(error).__name__)
    for label, path in files:
        try:
            for index, certificate in enumerate(split_certificate_chain(reader(path))):
                starts, expires, fingerprint = inspector(certificate)
                valid = starts <= checked_at and expires >= deadline
                rows.append({"file_label": label, "file_path": path, "chain_index": index,
                             "sha256_fingerprint": fingerprint,
                             "not_before": dt.datetime.fromtimestamp(starts, dt.timezone.utc).isoformat().replace("+00:00", "Z"),
                             "not_after": dt.datetime.fromtimestamp(expires, dt.timezone.utc).isoformat().replace("+00:00", "Z"),
                             "covers_window": valid})
                if not valid:
                    reason = "not_yet_valid" if starts > checked_at else "expires_before_required_window"
                    failures.append("file:" + label + ":chain_" + str(index) + ":" + reason)
        except (ValueError, KeyError, UnicodeError, subprocess.SubprocessError, OSError) as error:
            failures.append("file:" + label + ":" + type(error).__name__)
    return {"version": 1, "status": "ready_for_window" if not failures else "certificate_window_blocked",
            "context": manifest["context"], "manifest_sha256": manifest_sha256,
            "checked_at": dt.datetime.fromtimestamp(checked_at, dt.timezone.utc).isoformat().replace("+00:00", "Z"),
            "required_valid_until": dt.datetime.fromtimestamp(deadline, dt.timezone.utc).isoformat().replace("+00:00", "Z"),
            "required_seconds": valid_seconds, "secret_count": len(manifest["secrets"]), "file_count": len(files),
            "certificates": rows, "failures": failures,
            "scope": "declared Secret and file certificate validity only; no trust, identity, rotation, or storage attestation"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", required=True, type=Path)
    parser.add_argument("--min-valid-seconds", required=True, type=int)
    parser.add_argument("--certificate-file", action="append", default=[], help="label=/clean/absolute/public-cert-path, repeat for files outside Secrets")
    parser.add_argument("--output", required=True, type=Path, help="new private evidence directory")
    options = parser.parse_args()
    if options.min_valid_seconds <= 0:
        parser.error("--min-valid-seconds must be positive")
    try:
        files = parse_certificate_files(options.certificate_file)
    except ValueError as error:
        parser.error(str(error))
    manifest, digest = load_manifest(options.manifest)
    report = check_window(manifest, digest, options.min_valid_seconds, files=files)
    previous_umask = os.umask(0o077)
    try:
        options.output.mkdir(mode=0o700)
        path = options.output / "certificate-window.json"
        with path.open("x", encoding="utf-8") as file:
            json.dump(report, file, sort_keys=True, indent=2)
            file.write("\n")
            file.flush()
            os.fsync(file.fileno())
        directory_fd = os.open(options.output, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        os.umask(previous_umask)
    print(json.dumps({"status": report["status"], "certificate_count": len(report["certificates"]),
                      "failure_count": len(report["failures"])}, sort_keys=True))
    return 0 if report["status"] == "ready_for_window" else 1


if __name__ == "__main__":
    raise SystemExit(main())
