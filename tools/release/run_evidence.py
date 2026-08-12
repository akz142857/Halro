#!/usr/bin/env python3
"""Create and verify a release-run evidence manifest."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path
from typing import Any


COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
POSITIVE_INTEGER_RE = re.compile(r"^[1-9][0-9]*$")


class RunEvidenceError(ValueError):
    pass


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RunEvidenceError(message)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def artifact_inventory(release_dir: Path) -> list[dict[str, Any]]:
    require(release_dir.is_dir(), "release directory does not exist")
    inventory: list[dict[str, Any]] = []
    for path in sorted(release_dir.rglob("*")):
        require(not path.is_symlink(), f"release artifact must not be a symlink: {path}")
        if not path.is_file():
            continue
        inventory.append(
            {
                "name": path.relative_to(release_dir).as_posix(),
                "sha256": sha256_file(path),
                "size_bytes": path.stat().st_size,
            }
        )
    require(len(inventory) > 0, "release directory contains no artifacts")
    return inventory


def create_manifest(
    release_dir: Path,
    *,
    run_id: str,
    run_attempt: str,
    commit: str,
    ref: str,
    workflow_ref: str,
) -> dict[str, Any]:
    require(POSITIVE_INTEGER_RE.fullmatch(run_id) is not None, "run_id must be a positive integer")
    require(POSITIVE_INTEGER_RE.fullmatch(run_attempt) is not None, "run_attempt must be a positive integer")
    require(COMMIT_RE.fullmatch(commit) is not None, "commit must be a full lowercase Git SHA")
    require(ref.startswith("refs/tags/v"), "ref must be a version tag ref")
    require("/.github/workflows/release.yml@" in workflow_ref, "workflow_ref must identify release.yml")
    return {
        "schema_version": 1,
        "run": {
            "id": int(run_id),
            "attempt": int(run_attempt),
            "commit": commit,
            "ref": ref,
            "workflow_ref": workflow_ref,
        },
        "artifacts": artifact_inventory(release_dir),
    }


def verify_manifest(manifest: Any, release_dir: Path) -> None:
    require(isinstance(manifest, dict), "manifest must be an object")
    require(manifest.get("schema_version") == 1, "unsupported manifest schema_version")
    run = manifest.get("run")
    require(isinstance(run, dict), "manifest run must be an object")
    create_manifest(
        release_dir,
        run_id=str(run.get("id", "")),
        run_attempt=str(run.get("attempt", "")),
        commit=run.get("commit", ""),
        ref=run.get("ref", ""),
        workflow_ref=run.get("workflow_ref", ""),
    )
    recorded = manifest.get("artifacts")
    require(isinstance(recorded, list), "manifest artifacts must be an array")
    actual = artifact_inventory(release_dir)
    require(recorded == actual, "release artifacts do not match the recorded names, sizes, and SHA-256 digests")


def write_manifest(args: argparse.Namespace) -> None:
    manifest = create_manifest(
        args.release_dir,
        run_id=args.run_id,
        run_attempt=args.run_attempt,
        commit=args.commit,
        ref=args.ref,
        workflow_ref=args.workflow_ref,
    )
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def verify_file(args: argparse.Namespace) -> None:
    manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
    verify_manifest(manifest, args.release_dir)


def main() -> int:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)
    create = subparsers.add_parser("create")
    create.add_argument("--release-dir", type=Path, required=True)
    create.add_argument("--output", type=Path, required=True)
    create.add_argument("--run-id", required=True)
    create.add_argument("--run-attempt", required=True)
    create.add_argument("--commit", required=True)
    create.add_argument("--ref", required=True)
    create.add_argument("--workflow-ref", required=True)
    create.set_defaults(handler=write_manifest)

    verify = subparsers.add_parser("verify")
    verify.add_argument("--release-dir", type=Path, required=True)
    verify.add_argument("--manifest", type=Path, required=True)
    verify.set_defaults(handler=verify_file)
    args = parser.parse_args()
    try:
        args.handler(args)
    except (OSError, json.JSONDecodeError, RunEvidenceError) as error:
        print(f"RELEASE_RUN_EVIDENCE=FAIL: {error}")
        return 1
    print(f"RELEASE_RUN_EVIDENCE=PASS command={args.command}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
