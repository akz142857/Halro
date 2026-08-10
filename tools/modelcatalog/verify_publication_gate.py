#!/usr/bin/env python3
"""Fail-closed checks that bind a catalog PR to the protected publisher."""

from __future__ import annotations

import argparse
import re
from pathlib import Path


SHA_PATTERN = re.compile(r"[0-9a-f]{40}")
BRANCH_PATTERN = re.compile(r"catalog/publish-[0-9]+-[0-9]+")
PUBLICATION_PATH = "catalog/model-catalog-v1.json"


def validate_publication_gate(
    *,
    expected_publisher: str,
    actual_publisher: str,
    expected_repository: str,
    actual_head_repository: str,
    head_ref: str,
    pr_base_sha: str,
    current_main_sha: str,
    changed_files: list[str],
) -> None:
    if not expected_publisher:
        raise ValueError("CATALOG_PUBLISHER_LOGIN is not configured")
    if actual_publisher != expected_publisher:
        raise ValueError("publication PR was not authored by the protected publisher")
    if not expected_repository or actual_head_repository != expected_repository:
        raise ValueError("publication PR head must belong to this repository")
    if BRANCH_PATTERN.fullmatch(head_ref) is None:
        raise ValueError("publication PR branch does not match the protected workflow format")
    if SHA_PATTERN.fullmatch(pr_base_sha) is None or SHA_PATTERN.fullmatch(current_main_sha) is None:
        raise ValueError("publication PR base or current main SHA is invalid")
    if pr_base_sha != current_main_sha:
        raise ValueError("publication PR is not based on current main")
    if changed_files != [PUBLICATION_PATH]:
        raise ValueError("publication PR must change only the production catalog artifact")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--expected-publisher", required=True)
    parser.add_argument("--actual-publisher", required=True)
    parser.add_argument("--expected-repository", required=True)
    parser.add_argument("--actual-head-repository", required=True)
    parser.add_argument("--head-ref", required=True)
    parser.add_argument("--pr-base-sha", required=True)
    parser.add_argument("--current-main-sha", required=True)
    parser.add_argument("--changed-files", required=True, type=Path)
    args = parser.parse_args()
    changed_files = args.changed_files.read_text(encoding="utf-8").splitlines()
    try:
        validate_publication_gate(
            expected_publisher=args.expected_publisher,
            actual_publisher=args.actual_publisher,
            expected_repository=args.expected_repository,
            actual_head_repository=args.actual_head_repository,
            head_ref=args.head_ref,
            pr_base_sha=args.pr_base_sha,
            current_main_sha=args.current_main_sha,
            changed_files=changed_files,
        )
    except ValueError as error:
        parser.error(str(error))
    print("model catalog publication provenance verified")


if __name__ == "__main__":
    main()
