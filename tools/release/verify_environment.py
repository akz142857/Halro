#!/usr/bin/env python3
"""Verify the repository's release Environment protection response."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


class EnvironmentVerificationError(ValueError):
    pass


def require(condition: bool, message: str) -> None:
    if not condition:
        raise EnvironmentVerificationError(message)


def verify_environment(payload: Any, expected_name: str) -> int:
    require(isinstance(payload, dict), "environment response must be an object")
    require(payload.get("name") == expected_name, f"environment name must be {expected_name!r}")
    rules = payload.get("protection_rules")
    require(isinstance(rules, list), "environment protection_rules must be an array")
    reviewer_rules = [rule for rule in rules if isinstance(rule, dict) and rule.get("type") == "required_reviewers"]
    require(len(reviewer_rules) == 1, "environment must have exactly one required_reviewers protection rule")
    reviewer_rule = reviewer_rules[0]
    reviewers = reviewer_rule.get("reviewers")
    require(isinstance(reviewers, list) and len(reviewers) > 0, "environment must require at least one reviewer")
    require(reviewer_rule.get("prevent_self_review") is True, "environment must prevent self-review")
    for index, entry in enumerate(reviewers):
        require(isinstance(entry, dict), f"required reviewer {index} must be an object")
        require(entry.get("type") in {"User", "Team"}, f"required reviewer {index} has unsupported type")
        require(isinstance(entry.get("reviewer"), dict), f"required reviewer {index} is missing reviewer metadata")
    return len(reviewers)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("response", type=Path, help="JSON returned by GitHub's environment endpoint")
    parser.add_argument("--name", default="v1-release")
    args = parser.parse_args()
    try:
        payload = json.loads(args.response.read_text(encoding="utf-8"))
        reviewer_count = verify_environment(payload, args.name)
    except (OSError, json.JSONDecodeError, EnvironmentVerificationError) as error:
        print(f"RELEASE_ENVIRONMENT=FAIL: {error}")
        return 1
    print(
        f"RELEASE_ENVIRONMENT=PASS name={args.name} "
        f"required_reviewers={reviewer_count} prevent_self_review=true"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
