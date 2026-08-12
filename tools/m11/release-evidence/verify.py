#!/usr/bin/env python3
"""Fail-closed verifier for the final M11 production release evidence bundle."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from datetime import datetime
from pathlib import Path
from typing import Any


SCENARIOS = {
    "primary_unwrap",
    "recovery_unwrap",
    "identity_not_ready",
    "permission_denied",
    "throttling",
    "primary_disabled",
    "primary_pending_deletion",
    "context_mismatch",
    "ciphertext_or_payload_tampered",
    "wrong_vault_valid_key",
    "running_after_kms_unavailable",
    "cold_restart",
    "rotate_kill_points",
    "primary_and_recovery_restore",
}

CLOUDTRAIL_NOT_APPLICABLE = {
    "identity_not_ready",
    "running_after_kms_unavailable",
}

SECRET_CANARY_DOMAINS = {
    "logs",
    "errors",
    "audit",
    "metrics",
    "bbolt",
    "backup",
    "heap",
}

SIGNOFF_ROLES = {"Security", "Backend", "SRE", "Release"}

WORKLOAD_IDENTITY_SOURCES = {
    "WebIdentityCredentials",
    "CredentialsEndpointProvider",
    "EC2RoleProvider",
    "AssumeRoleProvider",
}

RELEASE_ARTIFACTS = {
    "halro-linux-amd64.tar.gz",
    "halro-linux-arm64.tar.gz",
    "halro-darwin-amd64.tar.gz",
    "halro-darwin-arm64.tar.gz",
    "halro-container.tar.gz",
    "halro-deadman-container.tar.gz",
    "halro.spdx.json",
    "halro-binaries.spdx.json",
    "checksums.txt",
}

SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
RAW_SECRET_PATTERNS = (
    re.compile(r"arn:aws:", re.IGNORECASE),
    re.compile(r"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b"),
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
)

SENSITIVE_FIELD_RE = re.compile(
    r"(?:^|_)(?:access_key|credential|ciphertext|master_key|plaintext|private_key|secret|session_token|token)(?:$|_)",
    re.IGNORECASE,
)

TOP_LEVEL_FIELDS = {
    "schema_version",
    "release",
    "design_reviews",
    "aws_environment",
    "aws_scenarios",
    "secret_canary",
    "deployments",
    "recovery_drill",
    "supply_chain",
    "signoffs",
}


class EvidenceError(ValueError):
    pass


def require(condition: bool, message: str) -> None:
    if not condition:
        raise EvidenceError(message)


def require_object(value: Any, path: str) -> dict[str, Any]:
    require(isinstance(value, dict), f"{path} must be an object")
    return value


def require_list(value: Any, path: str) -> list[Any]:
    require(isinstance(value, list), f"{path} must be an array")
    return value


def require_nonempty_string(value: Any, path: str) -> str:
    require(isinstance(value, str) and value.strip() != "", f"{path} must be a non-empty string")
    require(value.strip() not in {"—", "-", "pending", "todo", "tbd"}, f"{path} is a placeholder")
    return value


def require_timestamp(value: Any, path: str) -> str:
    text = require_nonempty_string(value, path)
    try:
        parsed = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError as exc:
        raise EvidenceError(f"{path} must be an RFC3339 timestamp") from exc
    require(parsed.tzinfo is not None, f"{path} must include a timezone")
    return text


def require_pass(value: Any, path: str) -> None:
    require(value == "pass", f"{path} must be 'pass'")


def require_evidence_refs(value: Any, path: str) -> None:
    refs = require_list(value, path)
    require(len(refs) > 0, f"{path} must contain at least one immutable evidence reference")
    for index, ref in enumerate(refs):
        require_nonempty_string(ref, f"{path}[{index}]")


def walk_strings(value: Any) -> list[str]:
    if isinstance(value, str):
        return [value]
    if isinstance(value, list):
        return [text for item in value for text in walk_strings(item)]
    if isinstance(value, dict):
        return [text for item in value.values() for text in walk_strings(item)]
    return []


def reject_raw_secrets(bundle: dict[str, Any]) -> None:
    def check_keys(value: Any) -> None:
        if isinstance(value, dict):
            for key, item in value.items():
                if key != "secret_canary":
                    require(SENSITIVE_FIELD_RE.search(key) is None, f"bundle contains forbidden sensitive field: {key}")
                check_keys(item)
        elif isinstance(value, list):
            for item in value:
                check_keys(item)

    check_keys(bundle)
    for text in walk_strings(bundle):
        for pattern in RAW_SECRET_PATTERNS:
            require(pattern.search(text) is None, "bundle contains a raw AWS ARN, credential, or private key")


def verify_release(bundle: dict[str, Any]) -> tuple[str, list[str]]:
    release = require_object(bundle.get("release"), "release")
    commit = require_nonempty_string(release.get("commit"), "release.commit")
    require(COMMIT_RE.fullmatch(commit) is not None, "release.commit must be a full lowercase Git commit SHA")
    require_nonempty_string(release.get("tag"), "release.tag")
    require_nonempty_string(release.get("ci_run"), "release.ci_run")
    for gate in ("test", "race", "vet", "vulnerability_scan", "kms_boundary", "kill_point_matrix"):
        require_pass(release.get(gate), f"release.{gate}")
    require(release.get("reachable_vulnerabilities") == 0, "release.reachable_vulnerabilities must be 0")
    authors = require_list(release.get("implementation_authors"), "release.implementation_authors")
    normalized_authors = [require_nonempty_string(author, f"release.implementation_authors[{index}]") for index, author in enumerate(authors)]
    require(len(normalized_authors) > 0, "release.implementation_authors must not be empty")
    return commit, normalized_authors


def verify_design_reviews(bundle: dict[str, Any]) -> None:
    reviews = require_object(bundle.get("design_reviews"), "design_reviews")
    expected = {"kms_wrapper_engineering", "error_taxonomy_security", "adr_0010_accepted"}
    require(set(reviews) == expected, f"design_reviews must contain exactly: {', '.join(sorted(expected))}")
    reviewers = []
    for review_name, raw in reviews.items():
        review = require_object(raw, f"design_reviews.{review_name}")
        require_pass(review.get("status"), f"design_reviews.{review_name}.status")
        reviewers.append(require_nonempty_string(review.get("reviewer"), f"design_reviews.{review_name}.reviewer"))
        require_timestamp(review.get("approved_at"), f"design_reviews.{review_name}.approved_at")
        require_evidence_refs(review.get("evidence"), f"design_reviews.{review_name}.evidence")
    require(len(set(reviewers)) >= 2, "design reviews must include at least two distinct reviewers")


def verify_scenarios(bundle: dict[str, Any]) -> None:
    scenarios = require_list(bundle.get("aws_scenarios"), "aws_scenarios")
    by_id: dict[str, dict[str, Any]] = {}
    for index, raw in enumerate(scenarios):
        item = require_object(raw, f"aws_scenarios[{index}]")
        scenario_id = require_nonempty_string(item.get("id"), f"aws_scenarios[{index}].id")
        require(scenario_id not in by_id, f"duplicate AWS scenario: {scenario_id}")
        by_id[scenario_id] = item
    require(set(by_id) == SCENARIOS, f"aws_scenarios must contain exactly: {', '.join(sorted(SCENARIOS))}")

    for scenario_id, item in by_id.items():
        path = f"aws_scenarios[{scenario_id}]"
        require_pass(item.get("status"), f"{path}.status")
        require_timestamp(item.get("performed_at"), f"{path}.performed_at")
        require_nonempty_string(item.get("operator"), f"{path}.operator")
        require_evidence_refs(item.get("evidence"), f"{path}.evidence")
        if scenario_id in CLOUDTRAIL_NOT_APPLICABLE:
            require(item.get("cloudtrail_correlated") is False, f"{path}.cloudtrail_correlated must be false")
            require_nonempty_string(item.get("cloudtrail_not_applicable_reason"), f"{path}.cloudtrail_not_applicable_reason")
        else:
            require(item.get("cloudtrail_correlated") is True, f"{path}.cloudtrail_correlated must be true")


def verify_aws_environment(bundle: dict[str, Any]) -> None:
    environment = require_object(bundle.get("aws_environment"), "aws_environment")
    require(
        environment.get("identity_source") in WORKLOAD_IDENTITY_SOURCES,
        "aws_environment.identity_source must be an approved Workload Identity source",
    )
    require_nonempty_string(environment.get("region"), "aws_environment.region")
    account_hash = require_nonempty_string(environment.get("account_sha256"), "aws_environment.account_sha256")
    require(SHA256_RE.fullmatch(account_hash) is not None, "aws_environment.account_sha256 must be lowercase SHA-256")
    key_hashes = require_object(environment.get("key_sha256"), "aws_environment.key_sha256")
    require(set(key_hashes) == {"primary", "recovery", "replacement_primary"}, "aws_environment.key_sha256 must contain Primary, Recovery and Replacement Primary")
    normalized_hashes = []
    for purpose, digest in key_hashes.items():
        text = require_nonempty_string(digest, f"aws_environment.key_sha256.{purpose}")
        require(SHA256_RE.fullmatch(text) is not None, f"aws_environment.key_sha256.{purpose} must be lowercase SHA-256")
        normalized_hashes.append(text)
    require(len(set(normalized_hashes)) == 3, "Primary, Recovery and Replacement Primary must be different KMS Keys")
    require_pass(environment.get("failure_domain_review"), "aws_environment.failure_domain_review")
    require_evidence_refs(environment.get("evidence"), "aws_environment.evidence")


def verify_secret_canary(bundle: dict[str, Any]) -> None:
    canary = require_object(bundle.get("secret_canary"), "secret_canary")
    require(set(canary) == SECRET_CANARY_DOMAINS, f"secret_canary must contain exactly: {', '.join(sorted(SECRET_CANARY_DOMAINS))}")
    for domain in SECRET_CANARY_DOMAINS:
        require_pass(canary.get(domain), f"secret_canary.{domain}")


def verify_deployments(bundle: dict[str, Any]) -> None:
    deployments = require_object(bundle.get("deployments"), "deployments")
    for target in ("eks", "vm_systemd"):
        item = require_object(deployments.get(target), f"deployments.{target}")
        require_pass(item.get("status"), f"deployments.{target}.status")
        require_timestamp(item.get("performed_at"), f"deployments.{target}.performed_at")
        require_evidence_refs(item.get("evidence"), f"deployments.{target}.evidence")
    eks = require_object(deployments["eks"], "deployments.eks")
    for scenario in ("identity_not_ready", "permission_denied", "primary_disabled"):
        require_pass(require_object(eks.get("crashloop"), "deployments.eks.crashloop").get(scenario), f"deployments.eks.crashloop.{scenario}")


def verify_recovery(bundle: dict[str, Any], implementation_authors: list[str]) -> None:
    drill = require_object(bundle.get("recovery_drill"), "recovery_drill")
    for gate in ("primary_restore", "recovery_restore", "disabled_or_deleted_primary", "temporary_permission_revoked"):
        require_pass(drill.get(gate), f"recovery_drill.{gate}")
    operator = require_nonempty_string(drill.get("independent_operator"), "recovery_drill.independent_operator")
    require(operator not in implementation_authors, "recovery_drill.independent_operator must not be an implementation author")
    require_timestamp(drill.get("performed_at"), "recovery_drill.performed_at")
    require_timestamp(drill.get("permission_revoked_at"), "recovery_drill.permission_revoked_at")
    require_evidence_refs(drill.get("evidence"), "recovery_drill.evidence")


def verify_supply_chain(bundle: dict[str, Any]) -> None:
    supply_chain = require_object(bundle.get("supply_chain"), "supply_chain")
    require_pass(supply_chain.get("sbom_review"), "supply_chain.sbom_review")
    require_pass(supply_chain.get("checksums_verified"), "supply_chain.checksums_verified")
    artifacts = require_list(supply_chain.get("artifacts"), "supply_chain.artifacts")
    by_name: dict[str, dict[str, Any]] = {}
    for index, raw in enumerate(artifacts):
        artifact = require_object(raw, f"supply_chain.artifacts[{index}]")
        name = require_nonempty_string(artifact.get("name"), f"supply_chain.artifacts[{index}].name")
        require(name not in by_name, f"duplicate release artifact: {name}")
        by_name[name] = artifact
    require(set(by_name) == RELEASE_ARTIFACTS, f"supply_chain.artifacts must contain exactly: {', '.join(sorted(RELEASE_ARTIFACTS))}")
    for name, artifact in by_name.items():
        digest = require_nonempty_string(artifact.get("sha256"), f"supply_chain.artifacts[{name}].sha256")
        require(SHA256_RE.fullmatch(digest) is not None, f"supply_chain.artifacts[{name}].sha256 must be lowercase SHA-256")
        require(artifact.get("sigstore_verified") is True, f"supply_chain.artifacts[{name}].sigstore_verified must be true")
        require_nonempty_string(artifact.get("sigstore_bundle"), f"supply_chain.artifacts[{name}].sigstore_bundle")


def verify_signoffs(bundle: dict[str, Any], commit: str) -> None:
    signoffs = require_list(bundle.get("signoffs"), "signoffs")
    by_role: dict[str, dict[str, Any]] = {}
    for index, raw in enumerate(signoffs):
        signoff = require_object(raw, f"signoffs[{index}]")
        role = require_nonempty_string(signoff.get("role"), f"signoffs[{index}].role")
        require(role not in by_role, f"duplicate sign-off role: {role}")
        by_role[role] = signoff
    require(set(by_role) == SIGNOFF_ROLES, f"signoffs must contain exactly: {', '.join(sorted(SIGNOFF_ROLES))}")
    reviewers = []
    for role, signoff in by_role.items():
        reviewers.append(require_nonempty_string(signoff.get("reviewer"), f"signoffs[{role}].reviewer"))
        require_timestamp(signoff.get("approved_at"), f"signoffs[{role}].approved_at")
        require(signoff.get("conclusion") == "approved", f"signoffs[{role}].conclusion must be 'approved'")
        require(signoff.get("commit") == commit, f"signoffs[{role}].commit must equal release.commit")
        require_evidence_refs(signoff.get("evidence"), f"signoffs[{role}].evidence")
    require(len(set(reviewers)) == len(SIGNOFF_ROLES), "Security, Backend, SRE and Release reviewers must be distinct")


def verify_artifact_files(bundle: dict[str, Any], artifacts_dir: Path) -> None:
    artifacts = require_list(require_object(bundle.get("supply_chain"), "supply_chain").get("artifacts"), "supply_chain.artifacts")
    for index, raw in enumerate(artifacts):
        artifact = require_object(raw, f"supply_chain.artifacts[{index}]")
        name = require_nonempty_string(artifact.get("name"), f"supply_chain.artifacts[{index}].name")
        path = artifacts_dir / name
        require(path.is_file(), f"release artifact is missing: {name}")
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        require(digest == artifact.get("sha256"), f"release artifact digest does not match bundle: {name}")
        bundle_path = artifacts_dir / f"{name}.sigstore.json"
        require(bundle_path.is_file() and bundle_path.stat().st_size > 0, f"Sigstore bundle is missing: {name}.sigstore.json")


def verify(
    bundle: dict[str, Any],
    *,
    expected_commit: str | None = None,
    expected_tag: str | None = None,
    artifacts_dir: Path | None = None,
) -> None:
    require(bundle.get("schema_version") == 1, "schema_version must be 1")
    require(set(bundle) == TOP_LEVEL_FIELDS, f"bundle must contain exactly: {', '.join(sorted(TOP_LEVEL_FIELDS))}")
    reject_raw_secrets(bundle)
    commit, implementation_authors = verify_release(bundle)
    release = require_object(bundle.get("release"), "release")
    if expected_commit is not None:
        require(commit == expected_commit, "release.commit does not match the expected release commit")
    if expected_tag is not None:
        require(release.get("tag") == expected_tag, "release.tag does not match the expected release tag")
    verify_design_reviews(bundle)
    verify_aws_environment(bundle)
    verify_scenarios(bundle)
    verify_secret_canary(bundle)
    verify_deployments(bundle)
    verify_recovery(bundle, implementation_authors)
    verify_supply_chain(bundle)
    verify_signoffs(bundle, commit)
    if artifacts_dir is not None:
        verify_artifact_files(bundle, artifacts_dir)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("bundle", type=Path, help="sanitized M11 release evidence JSON")
    parser.add_argument("--expected-commit", required=True, help="full commit SHA selected by the signed release tag")
    parser.add_argument("--expected-tag", required=True, help="exact release tag")
    parser.add_argument("--artifacts-dir", required=True, type=Path, help="downloaded release artifact directory")
    args = parser.parse_args()
    try:
        bundle = json.loads(args.bundle.read_text(encoding="utf-8"))
        verify(
            require_object(bundle, "bundle"),
            expected_commit=args.expected_commit,
            expected_tag=args.expected_tag,
            artifacts_dir=args.artifacts_dir,
        )
    except (OSError, json.JSONDecodeError, EvidenceError) as exc:
        print(f"M11_RELEASE_EVIDENCE=FAIL: {exc}", file=sys.stderr)
        return 1
    print("M11_RELEASE_EVIDENCE=PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
