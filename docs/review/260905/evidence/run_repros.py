#!/usr/bin/env python3
"""Run selected synthetic review fixtures without adding tests to the checkout."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    action = parser.add_mutually_exclusive_group(required=True)
    action.add_argument("--list", action="store_true")
    action.add_argument("--check", action="store_true")
    action.add_argument("--dry-run", metavar="CASE")
    action.add_argument("--run", metavar="CASE", help="case ID or all; sequential")
    parser.add_argument("--allow-different-sha", action="store_true",
                        help="explicitly permit a different checkout for fix verification")
    args = parser.parse_args()
    evidence = Path(__file__).resolve().parent
    manifest = json.loads((evidence / "cases.json").read_text())
    cases = manifest["cases"]
    if args.list:
        for name, case in cases.items():
            print(name, case["package"], case["selector"])
        return 0
    root = Path.cwd().resolve()
    if not (root / "go.mod").is_file():
        parser.error("Run from the Halro repository root containing go.mod.")
    selection = args.run or args.dry_run or "all"
    if selection != "all" and selection not in cases:
        parser.error("Unknown case: " + selection)
    selected = cases if selection == "all" else {selection: cases[selection]}
    for name, case in selected.items():
        source = evidence / case["source"]
        if not source.is_file():
            parser.error(f"{name}: missing archived fixture {source}")
        if hashlib.sha256(source.read_bytes()).hexdigest() != case["archived_sha256"]:
            parser.error(f"{name}: archived fixture hash mismatch")
        for rel in case["required_files"]:
            if not (root / rel).is_file():
                parser.error(f"{name}: missing baseline fixture/source {rel}; full checkout required")
        if (root / case["virtual"]).exists():
            parser.error(f"Refusing to overlay an existing file: {case['virtual']}")
    if not shutil.which("go") or not shutil.which("git"):
        parser.error("Go and Git must be installed; no tool installation is performed.")
    revision = subprocess.run(["git", "rev-parse", "HEAD"], cwd=root,
                              text=True, capture_output=True, check=True).stdout.strip()
    if revision != manifest["baseline"] and not args.allow_different_sha:
        parser.error(f"HEAD {revision} differs from baseline; use --allow-different-sha intentionally")
    print(f"HEAD={revision}; baseline={manifest['baseline']}", flush=True)
    if args.check:
        print(f"Checked {len(selected)} fixture hashes, dependencies and unused virtual paths.")
        print("This check does not compile tests or establish a clean production tree.")
        return 0
    # Prevent inherited Go flags selecting an unrelated overlay or updating modules.
    env = os.environ.copy()
    env["GOFLAGS"] = ""
    env["GOWORK"] = "off"
    env["GOPROXY"] = "off"
    env["GOSUMDB"] = "sum.golang.org"  # preserve toolchain checksum verification
    env["GOTOOLCHAIN"] = "auto"  # cached toolchain allowed; GOPROXY=off prevents downloads
    failed = False
    for name, case in selected.items():
        # All test data belongs to repository t.TempDir helpers. No user config is loaded.
        # The only overlay replaces an absent virtual test path, never production source.
        with tempfile.TemporaryDirectory(prefix="halro-evidence-overlay-") as tmp:
            overlay = Path(tmp) / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {
                str(root / case["virtual"]): str(evidence / case["source"])
            }}))
            command = ["go", "test", "-mod=readonly", "-count=1", "-timeout=90s"]
            if case["race"]:
                command.append("-race")
            command += ["-overlay", str(overlay), "./" + case["package"],
                        "-run", case["selector"], "-v"]
            print(f"\n[{name}] {shlex.join(command)}", flush=True)
            if args.dry_run:
                continue
            started = time.monotonic()
            try:
                result = subprocess.run(command, cwd=root, env=env, timeout=180)
                code = result.returncode
            except subprocess.TimeoutExpired:
                code = 124
            print(f"RESULT {name} exit={code} seconds={time.monotonic()-started:.3f}", flush=True)
            failed = failed or code != 0
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
