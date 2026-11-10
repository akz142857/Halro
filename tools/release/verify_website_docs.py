#!/usr/bin/env python3
"""Bind a prepared website snapshot and its public docs to one Halro release."""
from __future__ import annotations

import argparse
import hashlib
from html.parser import HTMLParser
import json
from pathlib import Path
import re
import subprocess
import time
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[2]
ENTRY_POINTS = tuple(
    f"src/content/docs/{locale}/{page}"
    for locale in ("docs", "en/docs")
    for page in ("index.mdx", "guides/quickstart-install.md")
)


def identity(version: str, commit: str) -> None:
    if not re.fullmatch(r"v\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?", version):
        raise ValueError("version must be an explicit release version")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("commit must be a full lowercase SHA")


def field(source: str, key: str) -> str | None:
    match = re.search(rf"^\s*{re.escape(key)}: ['\"]?([^'\"\s,]+)", source, re.M)
    return match[1] if match else None


def validate_candidate(files: dict[str, bytes], manifest: bytes, version: str, commit: str) -> dict:
    identity(version, commit)
    distribution = files["src/config/distribution.ts"].decode()
    if field(distribution, "version") != version or field(distribution, "releaseCommit") != commit:
        raise ValueError("website distribution does not target the exact release")
    metadata = json.loads(files["src/data/contracts.meta.json"])
    if (metadata.get("source_ref"), metadata.get("source_commit")) != (version, commit):
        raise ValueError("website API contracts target another release")
    digest = hashlib.sha256(manifest).hexdigest()
    if files["src/data/endpoint-manifests.json"] != manifest:
        raise ValueError("website API manifest differs from the release source")
    if metadata.get("sha256") != digest or metadata.get("source_sha256") != digest:
        raise ValueError("website API manifest digest is stale")
    if json.loads(files["src/data/served-profile-state.json"]).get("source_commit") != commit:
        raise ValueError("served-profile state targets another commit")
    for path in ENTRY_POINTS:
        source = files[path].decode()
        sections = source.split("\n---\n", 1)
        if len(sections) != 2:
            raise ValueError(f"{path}: missing frontmatter")
        front, body = sections
        if field(front, "sourceRevision") != commit or not re.search(
            rf"verifiedAgainst:\s*\{{\s*release:\s*{re.escape(version)},\s*commit:\s*{commit}\s*\}}", front
        ):
            raise ValueError(f"{path}: docs review is not pinned to the release")
        if "/en/" in path and field(front, "translatedFromRevision") != commit:
            raise ValueError(f"{path}: translation review is stale")
        if version not in body:
            raise ValueError(f"{path}: current release is not visible")
        pins = re.findall(r"(?:VERSION=|ghcr\.io/akz142857/halro:)(v\d+\.\d+\.\d+(?:-[\w.-]+)?)", body)
        if any(pin != version for pin in pins):
            raise ValueError(f"{path}: stale installation version")
    return {"version": version, "halro_commit": commit, "manifest_sha256": digest,
            "entry_points": list(ENTRY_POINTS), "result": "verified"}


def git(repo: Path, *args: str) -> bytes:
    return subprocess.check_output(["git", "-C", str(repo), *args])


def candidate(args) -> dict:
    identity(args.version, args.commit)
    identity(args.version, args.website_commit)
    repo = Path(args.website_dir).resolve()
    if git(repo, "rev-parse", "HEAD").decode().strip() != args.website_commit:
        raise ValueError("website checkout is not the requested immutable commit")
    if git(repo, "status", "--porcelain", "--untracked-files=no").strip():
        raise ValueError("website checkout contains uncommitted changes")
    paths = ("src/config/distribution.ts", "src/data/contracts.meta.json", "src/data/endpoint-manifests.json",
             "src/data/served-profile-state.json", *ENTRY_POINTS)
    files = {path: git(repo, "show", f"{args.website_commit}:{path}") for path in paths}
    manifest = git(ROOT, "show", f"{args.commit}:docs/compatibility/endpoint-manifests.json")
    result = validate_candidate(files, manifest, args.version, args.commit)
    result["website_commit"] = args.website_commit
    return result


class VisibleText(HTMLParser):
    def __init__(self):
        super().__init__()
        self.parts = []
        self.hidden = 0

    def handle_starttag(self, tag, attrs):
        if tag in ("script", "style"):
            self.hidden += 1

    def handle_endtag(self, tag):
        if tag in ("script", "style"):
            self.hidden = max(0, self.hidden - 1)

    def handle_data(self, value):
        if not self.hidden:
            self.parts.append(value)


def public_expectations(version: str, commit: str) -> dict[str, list[str]]:
    identity(version, commit)
    return {
        "/docs/": [f"{version} 正式文档"],
        "/en/docs/": [f"{version} documentation"],
        "/docs/guides/quickstart-install/": [f"VERSION={version}"],
        "/en/docs/guides/quickstart-install/": [f"VERSION={version}"],
        "/docs/api/chat-completions/": [commit[:12]],
        "/en/docs/api/chat-completions/": [commit[:12]],
    }


def fetch(path: str) -> str:
    request = Request("https://halro.ai" + path, headers={"User-Agent": "Halro-release-docs-check", "Cache-Control": "no-cache"})
    with urlopen(request, timeout=15) as response:
        if response.status != 200 or "text/html" not in response.headers.get("Content-Type", ""):
            raise ValueError(f"{path}: expected HTML with status 200")
        return response.read().decode("utf-8")


def check_public(version: str, commit: str, fetch_page=fetch) -> list[str]:
    problems = []
    for path, expected in public_expectations(version, commit).items():
        try:
            parser = VisibleText()
            parser.feed(fetch_page(path))
            text = "".join(parser.parts)
            if any(term not in text for term in expected):
                problems.append(f"{path}: release content has not arrived")
        except Exception as error:
            problems.append(f"{path}: {type(error).__name__}")
    return problems


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="mode", required=True)
    for mode in ("candidate", "public"):
        command = sub.add_parser(mode)
        command.add_argument("--version", required=True)
        command.add_argument("--commit", required=True)
        command.add_argument("--output", type=Path)
        if mode == "candidate":
            command.add_argument("--website-dir", required=True)
            command.add_argument("--website-commit", required=True)
        else:
            command.add_argument("--wait-seconds", type=int, default=0)
    args = parser.parse_args()
    identity(args.version, args.commit)
    if args.mode == "candidate":
        result = candidate(args)
    else:
        if not 0 <= args.wait_seconds <= 1200:
            raise ValueError("wait-seconds must be between 0 and 1200")
        deadline = time.monotonic() + args.wait_seconds
        while True:
            problems = check_public(args.version, args.commit)
            if not problems:
                break
            print("Website docs pending: " + "; ".join(problems), flush=True)
            if time.monotonic() >= deadline:
                raise ValueError("website docs acceptance failed; repair the website and rerun this job, never replace release assets")
            time.sleep(min(20, max(0, deadline - time.monotonic())))
        result = {"version": args.version, "halro_commit": args.commit, "result": "verified",
                  "public_paths": list(public_expectations(args.version, args.commit))}
    rendered = json.dumps(result, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(rendered)
    print(rendered, end="")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ValueError, KeyError, subprocess.CalledProcessError) as error:
        raise SystemExit(f"website docs: {error}")
