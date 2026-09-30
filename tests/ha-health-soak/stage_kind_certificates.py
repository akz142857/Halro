#!/usr/bin/env python3
"""Stage, but never install, replacement TLS certificates for the local kind HA soak."""

import argparse
import datetime as dt
import hashlib
import json
import os
import secrets
import subprocess
from pathlib import Path


DAYS = 14
ROOTS = {
    "cluster": "Halro kind HA health cluster CA",
    "query": "Halro kind HA health Prometheus query CA",
}
LEAVES = {
    "halro-0": ("cluster", "halro-0", ["DNS:halro-0.halro-members.halro.svc.cluster.local", "DNS:halro.halro.svc.cluster.local", "IP:127.0.0.1"], ["serverAuth", "clientAuth"]),
    "halro-1": ("cluster", "halro-1", ["DNS:halro-1.halro-members.halro.svc.cluster.local", "DNS:halro.halro.svc.cluster.local", "IP:127.0.0.1"], ["serverAuth", "clientAuth"]),
    "halro-2": ("cluster", "halro-2", ["DNS:halro-2.halro-members.halro.svc.cluster.local", "DNS:halro.halro.svc.cluster.local", "IP:127.0.0.1"], ["serverAuth", "clientAuth"]),
    "health-server": ("cluster", "halro-health-view-kind-local", ["DNS:ha-health.halro-monitoring.svc.cluster.local", "IP:127.0.0.1"], ["serverAuth", "clientAuth"]),
    "collector": ("cluster", "halro-collector-kind-local", ["DNS:ha-health.halro-monitoring.svc.cluster.local", "IP:127.0.0.1"], ["serverAuth", "clientAuth"]),
    "operator": ("cluster", "halro-operator-kind-local", ["DNS:ha-health.halro-monitoring.svc.cluster.local", "IP:127.0.0.1"], ["serverAuth", "clientAuth"]),
    "prometheus-scrape": ("cluster", "halro-prometheus-kind-local", ["DNS:ha-health.halro-monitoring.svc.cluster.local", "IP:127.0.0.1"], ["serverAuth", "clientAuth"]),
    "prometheus-query-server": ("query", "ha-prometheus.halro-monitoring.svc.cluster.local", ["DNS:ha-prometheus.halro-monitoring.svc.cluster.local"], ["serverAuth"]),
    "prometheus-query-client": ("query", "ha-health-prometheus-client-v2", ["DNS:ha-health-prometheus-client-v2"], ["clientAuth"]),
}
SECRET_FILES = {
    "halro/halro-ha-cluster-tls": {"ca.crt": "cluster/ca.crt", **{f"halro-{i}.{ext}": f"cluster/halro-{i}.{ext}" for i in range(3) for ext in ("crt", "key")}},
    "halro-monitoring/ha-health-tls": {"ca.crt": "cluster/ca.crt", "server.crt": "cluster/health-server.crt", "server.key": "cluster/health-server.key", "collector.crt": "cluster/collector.crt", "collector.key": "cluster/collector.key"},
    "halro-monitoring/ha-prometheus-tls": {"ca.crt": "cluster/ca.crt", "client.crt": "cluster/prometheus-scrape.crt", "client.key": "cluster/prometheus-scrape.key"},
    "halro-monitoring/ha-prometheus-web-tls": {"client-ca.crt": "query/ca.crt", "server.crt": "query/prometheus-query-server.crt", "server.key": "query/prometheus-query-server.key"},
    "halro-monitoring/ha-health-prometheus-client": {"ca.crt": "query/ca.crt", "client.crt": "query/prometheus-query-client.crt", "client.key": "query/prometheus-query-client.key"},
}


def run(*args, input_bytes=None):
    return subprocess.run(["openssl", *map(str, args)], input=input_bytes, check=True,
                          capture_output=True, timeout=30).stdout


def fingerprint(path):
    output = run("x509", "-in", path, "-noout", "-fingerprint", "-sha256").decode().strip()
    return output.partition("=")[2].replace(":", "").lower()


def public_key_from_certificate(path):
    pem = run("x509", "-in", path, "-pubkey", "-noout")
    return run("pkey", "-pubin", "-outform", "DER", input_bytes=pem)


def verify_leaf(root, cert, key, sans, purposes):
    for purpose in purposes:
        run("verify", "-CAfile", root, "-purpose", "sslserver" if purpose == "serverAuth" else "sslclient", cert)
    for san in sans:
        kind, value = san.split(":", 1)
        run("x509", "-in", cert, "-noout", "-checkhost" if kind == "DNS" else "-checkip", value)
    cert_public = public_key_from_certificate(cert)
    key_public = run("pkey", "-in", key, "-pubout", "-outform", "DER")
    if cert_public != key_public:
        raise ValueError(f"certificate and key differ: {cert}")
    run("x509", "-in", cert, "-checkend", str(74 * 3600), "-noout")


def stage(output):
    if not output.is_absolute() or output.exists() or not output.parent.is_dir():
        raise ValueError("--output must be a new absolute directory under an existing parent")
    previous_umask = os.umask(0o077)
    try:
        output.mkdir(mode=0o700)
        for root_name, common_name in ROOTS.items():
            directory = output / root_name
            directory.mkdir(mode=0o700)
            run("req", "-x509", "-newkey", "rsa:3072", "-nodes", "-sha256", "-days", str(DAYS),
                "-keyout", directory / "ca.key", "-out", directory / "ca.crt", "-subj", f"/CN={common_name}",
                "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign")
            run("x509", "-in", directory / "ca.crt", "-checkend", str(74 * 3600), "-noout")
        rows = []
        for name, (root_name, common_name, sans, purposes) in LEAVES.items():
            directory = output / root_name
            key, cert = directory / f"{name}.key", directory / f"{name}.crt"
            csr, extensions = directory / f"{name}.csr", directory / f"{name}.cnf"
            run("req", "-new", "-newkey", "rsa:3072", "-nodes", "-sha256", "-keyout", key,
                "-out", csr, "-subj", f"/CN={common_name}")
            extensions.write_text("[leaf]\nbasicConstraints=critical,CA:FALSE\n"
                                  "keyUsage=critical,digitalSignature,keyEncipherment\n"
                                  f"extendedKeyUsage={','.join(purposes)}\n"
                                  f"subjectAltName={','.join(sans)}\n", encoding="ascii")
            run("x509", "-req", "-in", csr, "-CA", directory / "ca.crt", "-CAkey", directory / "ca.key",
                "-set_serial", hex(secrets.randbits(127) | 1), "-out", cert, "-days", str(DAYS),
                "-sha256", "-extfile", extensions, "-extensions", "leaf")
            csr.unlink()
            extensions.unlink()
            verify_leaf(directory / "ca.crt", cert, key, sans, purposes)
            rows.append({"name": name, "root": root_name, "sans": sans, "purposes": purposes,
                         "sha256_fingerprint": fingerprint(cert),
                         "not_after": run("x509", "-in", cert, "-noout", "-enddate").decode().strip().partition("=")[2]})
        manifest = {"version": 1, "scope": "staged local kind certificates only; not installed or accepted",
                    "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(), "valid_days": DAYS,
                    "roots": {name: {"sha256_fingerprint": fingerprint(output / name / "ca.crt")}
                              for name in ROOTS}, "leaves": rows, "secret_files": SECRET_FILES,
                    "operator_files": {"ca.crt": "cluster/ca.crt", "operator.crt": "cluster/operator.crt",
                                       "operator.key": "cluster/operator.key"}}
        report = output / "stage-report.json"
        with report.open("x", encoding="utf-8") as file:
            json.dump(manifest, file, sort_keys=True, indent=2)
            file.write("\n")
            file.flush()
            os.fsync(file.fileno())
        directory_fd = os.open(output, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
        return hashlib.sha256(report.read_bytes()).hexdigest(), len(rows)
    finally:
        os.umask(previous_umask)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path, help="new private absolute staging directory")
    options = parser.parse_args()
    try:
        digest, count = stage(options.output)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        parser.error(str(error))
    print(json.dumps({"status": "staged_only", "leaf_count": count, "report_sha256": digest}, sort_keys=True))


if __name__ == "__main__":
    main()
