#!/usr/bin/env python3
"""Move the mechanical half of a release preparation.

The judgement half stays with a person: the changelog prose is written per pull
request under `## [Unreleased]`, and the assessment record's verdict is the
owner's. What this moves is the part that has actually been forgotten — v0.8.1
and v0.8.2 both shipped with the README advertising an image three releases old,
the web package version stale, and the dependency-license drift hashes pointing
at the previous release's files.

Run it on a clean tree at the commit the release will be prepared from:

    python3 tools/release/prepare_release.py v0.8.5

It refuses rather than guesses: an existing tag, an existing changelog section,
an empty Unreleased section, a version that does not look like a release, or a
drift-hash block it cannot find are all hard failures, because each of them means
the tree is not in the shape this script was written for.
"""

from __future__ import annotations

import argparse
import datetime as _datetime
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
CHANGELOG = ROOT / "CHANGELOG.md"
README = ROOT / "README.md"
WEB_PACKAGE = ROOT / "web/package.json"
WEB_LOCK = ROOT / "web/package-lock.json"
LICENSE_REVIEW = ROOT / "docs/verification/dependency-license-review.md"
DEFAULT_CONFIG = ROOT / "internal/config/default.yaml"
CONFIG_SNAPSHOTS = ROOT / "internal/config/testdata/releases"
ASSESSMENTS = ROOT / "docs/verification/assessments"

VERSION = re.compile(r"^v(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z][0-9A-Za-z.-]*)?$")
REPOSITORY = "akz142857/Halro"


class Refusal(Exception):
    """A state the caller has to fix; never worked around silently."""


def git(*arguments: str) -> str:
    return subprocess.run(
        ["git", "-C", str(ROOT), *arguments],
        check=True, capture_output=True, text=True,
    ).stdout.strip()


def git_optional(*arguments: str) -> str | None:
    """Ask git something the scaffold would like but can live without.

    The previous tag is not always resolvable — a shallow clone, or tags that
    were never fetched. That degrades the assessment scaffold; it must not stop
    the surfaces from moving.
    """
    result = subprocess.run(
        ["git", "-C", str(ROOT), *arguments], capture_output=True, text=True,
    )
    return result.stdout.strip() if result.returncode == 0 else None


def previous_release() -> str:
    """The most recent published version, read from the changelog's own history.

    Not from `git describe`: the tag for the version being prepared does not
    exist yet by design, and a tag from another branch would be the wrong
    answer.
    """
    for line in CHANGELOG.read_text(encoding="utf-8").splitlines():
        match = re.match(r"^## \[(\d+\.\d+\.\d+[^\]]*)\]", line)
        if match:
            return "v" + match.group(1)
    raise Refusal("CHANGELOG.md carries no released section to move forward from")


def unreleased_body(text: str) -> str:
    match = re.search(r"^## \[Unreleased\]\n(.*?)(?=^## \[)", text, re.S | re.M)
    if not match:
        raise Refusal("CHANGELOG.md has no '## [Unreleased]' section")
    return match.group(1).strip("\n")


def move_changelog(version: str, date: str, previous: str) -> str:
    text = CHANGELOG.read_text(encoding="utf-8")
    number = version[1:]
    if f"## [{number}]" in text:
        raise Refusal(f"CHANGELOG.md already has a section for {version}")
    body = unreleased_body(text)
    if not body.strip():
        raise Refusal(
            "## [Unreleased] is empty. A release section is the release's own "
            "description and is what the GitHub Release will say; write it in "
            "the pull requests, not here."
        )
    # Rewrite the span the parser found rather than reconstructing a string and
    # hoping it matches: the first draft of this rebuilt "## [Unreleased]\n" +
    # body, missed the blank line that actually sits between them, replaced
    # nothing, and reported success.
    match = re.search(r"^## \[Unreleased\]\n(.*?)(?=^## \[)", text, re.S | re.M)
    assert match is not None  # unreleased_body already refused otherwise
    text = (
        text[: match.start()]
        + f"## [Unreleased]\n\n## [{number}] - {date}\n\n{body}\n\n"
        + text[match.end():]
    )
    if f"## [{number}] - {date}" not in text:
        raise Refusal("changelog section move did not apply")
    link = f"[{number}]: https://github.com/{REPOSITORY}/compare/{previous}...{version}"
    previous_link = f"[{previous[1:]}]: https://github.com/{REPOSITORY}/compare/"
    if previous_link not in text:
        raise Refusal(f"CHANGELOG.md has no compare link for {previous} to insert above")
    index = text.index(previous_link)
    text = text[:index] + link + "\n" + text[index:]
    CHANGELOG.write_text(text, encoding="utf-8")
    return body


def move_readme(version: str, previous: str) -> int:
    text = README.read_text(encoding="utf-8")
    moved = text.count(previous)
    if moved == 0:
        raise Refusal(f"README.md names no {previous}; the version surfaces moved elsewhere")
    updated = text.replace(previous, version)
    if previous in updated:
        raise Refusal(f"README.md still names {previous} after the move")
    README.write_text(updated, encoding="utf-8")
    return moved


def move_web_version(version: str, previous: str) -> None:
    number, previous_number = version[1:], previous[1:]
    for path, expected in ((WEB_PACKAGE, 1), (WEB_LOCK, 2)):
        text = path.read_text(encoding="utf-8")
        needle = f'"version": "{previous_number}"'
        found = text.count(needle)
        if found != expected:
            raise Refusal(
                f"{path.relative_to(ROOT)} has {found} occurrences of "
                f"{needle}, expected {expected}"
            )
        path.write_text(text.replace(needle, f'"version": "{number}"'), encoding="utf-8")


def move_drift_hashes(version: str, previous: str) -> dict[str, str]:
    """Refresh the two hashes the version bump displaces, and only those.

    The Go and compatibility-suite hashes are deliberately left alone: if one of
    them moved, a dependency changed, and that is a review this script is not
    entitled to sign off.
    """
    text = LICENSE_REVIEW.read_text(encoding="utf-8")
    moved = {}
    for path in (WEB_PACKAGE, WEB_LOCK):
        name = str(path.relative_to(ROOT))
        digest = git("hash-object", name)
        pattern = re.compile(rf"^(- `{re.escape(name)}`: `)[0-9a-f]{{40}}(`)$", re.M)
        if not pattern.search(text):
            raise Refusal(f"dependency-license-review.md has no drift hash line for {name}")
        text = pattern.sub(rf"\g<1>{digest}\g<2>", text)
        moved[name] = digest
    # The sentence lists every release that moved these two hashes. Append to
    # it; do not replace its tail, which silently drops the release that was
    # newest until now.
    tail = re.compile(r"and now `(v[0-9][^`]*)`, each of which bumped the")
    found = tail.search(text)
    if not found:
        raise Refusal(
            "dependency-license-review.md does not carry the sentence listing "
            "which releases moved the web hashes; refresh it by hand"
        )
    if found.group(1) == version:
        raise Refusal(f"the drift-hash sentence already names {version}")
    text = tail.sub(f"`{found.group(1)}`, and now `{version}`, each of which bumped the", text, count=1)
    if f"and now `{version}`" not in text or f"`{found.group(1)}`, and now" not in text:
        raise Refusal("drift-hash sentence update did not apply")
    LICENSE_REVIEW.write_text(text, encoding="utf-8")
    return moved


def snapshot_default_config(version: str) -> Path:
    """Keep the configuration this release ships, so a later one can load it.

    The retirement table in internal/config is tested against these files rather
    than against hand-written ones: a fixture built from an author's idea of an
    old config tests the idea. Every published default.yaml was refused by the
    tree at the time this was added, and nothing noticed, because nothing had
    ever loaded a released config.
    """
    destination = CONFIG_SNAPSHOTS / f"{version}.yaml"
    CONFIG_SNAPSHOTS.mkdir(parents=True, exist_ok=True)
    destination.write_bytes(DEFAULT_CONFIG.read_bytes())
    return destination


def scaffold_assessment(version: str, date: str, previous: str) -> Path | None:
    path = ASSESSMENTS / f"{version}.md"
    if path.exists():
        return None
    unresolved = f"(range {previous}..HEAD could not be resolved; fetch tags and fill this in)"
    commits = git_optional("log", "--oneline", f"{previous}..HEAD")
    listing = git_optional("diff", "--name-only", f"{previous}..HEAD")
    files = listing.splitlines() if listing else []
    commits = commits or unresolved
    triggers = {
        "internal/store schemas, durable formats": any(f.startswith("internal/store/") or f.startswith("internal/ledger/") for f in files),
        "internal/provider/* wire behaviour, semantic mapping": any(f.startswith("internal/provider/") or f.startswith("internal/semantic/") for f in files),
        "auth / adminauth / redaction / contentscan / safetransport": any(
            f.startswith(("internal/auth/", "internal/adminauth/", "internal/redaction/", "internal/contentscan/", "internal/safetransport/")) for f in files
        ),
        "request hot path, budget / limiter / tokenguard": any(
            f.startswith(("internal/gatewayapi/", "internal/openaiapi/", "internal/anthropicapi/", "internal/budget/", "internal/limiter/", "internal/tokenguard/")) for f in files
        ),
        "web/": any(f.startswith("web/") for f in files),
    }
    rows = "\n".join(
        f"| {name} | {'**Touched** — say why the pass was or was not run' if hit else 'Untouched'} |"
        for name, hit in triggers.items()
    )
    path.write_text(
        f"""# {version} pre-release assessment

Date: {date}
Range reviewed: `{previous}` through the {version} release commit on `main`.
Assessor and owner: TODO

## Recommendation

TODO — GO or NO-GO, and for which channels.

## Scope and triggered passes

Range: {len(commits.splitlines()) if commits != unresolved else 0} commits, {len(files)} files.

```
{commits}
```

Trigger table from `docs/verification/release-assessment.md` §0, computed from the
range's paths. Each row still needs a sentence; a touched row that ran no pass has
to say why.

| Trigger row | Status |
| --- | --- |
{rows}

## Defect and review disposition

TODO

## Quality and recovery evidence

TODO — `make full-check` on Node 22 with nothing else competing; frontend test
count and bundle delta; real-binary smoke against a data directory written by the
previous released binary; the rollback direction.

## Invariants and upgrade impact

TODO — the nine invariants, each "untouched" or one line of justification.
Re-initialisation required: TODO. Rollback fence: TODO.

## Evidence boundaries

TODO — what was not run, and why that is a scoping decision rather than a waived
result.

## Publication verdict

TODO
""",
        encoding="utf-8",
    )
    return path


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("version", help="the version being prepared, e.g. v0.8.5")
    parser.add_argument("--date", default=None, help="release date, UTC, defaults to today")
    arguments = parser.parse_args()

    version = arguments.version
    if not VERSION.match(version):
        raise Refusal(f"version must look like v1.2.3 or v1.2.3-rc.1, got '{version}'")
    date = arguments.date or _datetime.datetime.now(_datetime.timezone.utc).strftime("%Y-%m-%d")
    if not re.match(r"^\d{4}-\d{2}-\d{2}$", date):
        raise Refusal(f"date must be YYYY-MM-DD, got '{date}'")

    tags = git("tag", "--list", version)
    if tags:
        raise Refusal(f"tag {version} already exists; a published version is never re-prepared")

    previous = previous_release()
    if previous == version:
        raise Refusal(f"{version} is already the newest changelog section")
    snapshot = CONFIG_SNAPSHOTS / f"{version}.yaml"
    if snapshot.exists():
        raise Refusal(f"{snapshot.relative_to(ROOT)} already exists; a published config is never re-snapshotted")

    move_changelog(version, date, previous)
    readme_moved = move_readme(version, previous)
    move_web_version(version, previous)
    hashes = move_drift_hashes(version, previous)
    config_snapshot = snapshot_default_config(version)
    assessment = scaffold_assessment(version, date, previous)

    print(f"prepared {version} (previous {previous}, date {date})")
    print(f"  CHANGELOG.md            section moved out of Unreleased, compare link added")
    print(f"  README.md               {readme_moved} version references")
    print(f"  web/package.json        version bumped")
    print(f"  web/package-lock.json   version bumped")
    for name, digest in hashes.items():
        print(f"  drift hash              {name} -> {digest}")
    print(f"  {config_snapshot.relative_to(ROOT)}   default.yaml snapshotted")
    if assessment:
        print(f"  {assessment.relative_to(ROOT)}   scaffolded, TODO sections to fill")
    else:
        print(f"  assessment record       already exists, left alone")
    print()
    print("Still yours, and not automatable:")
    print("  - read the scaffolded assessment's trigger table and answer every row")
    print("  - run `make full-check` on Node 22 with nothing else competing")
    print("  - smoke the release binary against a data directory written by the previous release")
    print("  - decide the verdict")
    print()
    print("Then: commit, open the pull request, merge, wait for that exact commit's")
    print("push CI, rehearse with dry_run=true, and dispatch with dry_run=false.")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Refusal as refusal:
        print(f"prepare_release: {refusal}", file=sys.stderr)
        sys.exit(1)
