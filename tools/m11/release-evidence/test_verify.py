#!/usr/bin/env python3

import copy
import importlib.util
import unittest
from pathlib import Path


MODULE_PATH = Path(__file__).with_name("verify.py")
SPEC = importlib.util.spec_from_file_location("m11_release_evidence_verify", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
VERIFY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VERIFY)


def valid_bundle():
    timestamp = "2026-08-03T12:00:00Z"
    commit = "a" * 40
    scenarios = []
    for scenario_id in sorted(VERIFY.SCENARIOS):
        not_applicable = scenario_id in VERIFY.CLOUDTRAIL_NOT_APPLICABLE
        item = {
            "id": scenario_id,
            "status": "pass",
            "performed_at": timestamp,
            "operator": "aws-test-operator",
            "evidence": [f"evidence/{scenario_id}.json"],
            "cloudtrail_correlated": not not_applicable,
        }
        if not_applicable:
            item["cloudtrail_not_applicable_reason"] = "no KMS API request is expected"
        scenarios.append(item)
    artifacts = [
        {
            "name": name,
            "sha256": "b" * 64,
            "sigstore_verified": True,
            "sigstore_bundle": f"release/{name}.sigstore.json",
        }
        for name in sorted(VERIFY.RELEASE_ARTIFACTS)
    ]
    return {
        "schema_version": 1,
        "release": {
            "commit": commit,
            "tag": "v1.0.0-rc.1",
            "ci_run": "https://github.example.invalid/actions/runs/1",
            "test": "pass",
            "race": "pass",
            "vet": "pass",
            "vulnerability_scan": "pass",
            "reachable_vulnerabilities": 0,
            "kms_boundary": "pass",
            "kill_point_matrix": "pass",
            "implementation_authors": ["implementation-author"],
        },
        "design_reviews": {
            "kms_wrapper_engineering": {
                "status": "pass",
                "reviewer": "engineering-reviewer",
                "approved_at": timestamp,
                "evidence": ["https://github.example.invalid/pull/66#engineering-review"],
            },
            "error_taxonomy_security": {
                "status": "pass",
                "reviewer": "security-reviewer",
                "approved_at": timestamp,
                "evidence": ["https://github.example.invalid/pull/66#security-review"],
            },
            "adr_0010_accepted": {
                "status": "pass",
                "reviewer": "engineering-reviewer",
                "approved_at": timestamp,
                "evidence": ["docs/adr/0010-kms-sdk-dependency-isolation.md"],
            },
        },
        "aws_environment": {
            "identity_source": "CredentialsEndpointProvider",
            "region": "us-east-1",
            "account_sha256": "1" * 64,
            "key_sha256": {
                "primary": "2" * 64,
                "recovery": "3" * 64,
                "replacement_primary": "4" * 64,
            },
            "failure_domain_review": "pass",
            "evidence": ["evidence/aws-environment.json"],
        },
        "aws_scenarios": scenarios,
        "secret_canary": {domain: "pass" for domain in VERIFY.SECRET_CANARY_DOMAINS},
        "deployments": {
            "eks": {
                "status": "pass",
                "performed_at": timestamp,
                "evidence": ["evidence/eks.json"],
                "crashloop": {
                    "identity_not_ready": "pass",
                    "permission_denied": "pass",
                    "primary_disabled": "pass",
                },
            },
            "vm_systemd": {
                "status": "pass",
                "performed_at": timestamp,
                "evidence": ["evidence/vm.json"],
            },
        },
        "recovery_drill": {
            "primary_restore": "pass",
            "recovery_restore": "pass",
            "disabled_or_deleted_primary": "pass",
            "temporary_permission_revoked": "pass",
            "independent_operator": "independent-operator",
            "performed_at": timestamp,
            "permission_revoked_at": timestamp,
            "evidence": ["evidence/recovery.json"],
        },
        "supply_chain": {
            "sbom_review": "pass",
            "checksums_verified": "pass",
            "artifacts": artifacts,
        },
        "signoffs": [
            {
                "role": role,
                "reviewer": f"{role.lower()}-reviewer",
                "approved_at": timestamp,
                "conclusion": "approved",
                "commit": commit,
                "evidence": [f"evidence/{role.lower()}-approval.json"],
            }
            for role in sorted(VERIFY.SIGNOFF_ROLES)
        ],
    }


class VerifyTests(unittest.TestCase):
    def assert_invalid(self, mutate):
        bundle = valid_bundle()
        mutate(bundle)
        with self.assertRaises(VERIFY.EvidenceError):
            VERIFY.verify(bundle)

    def test_complete_bundle_passes(self):
        VERIFY.verify(valid_bundle())

    def test_missing_scenario_fails(self):
        self.assert_invalid(lambda bundle: bundle["aws_scenarios"].pop())

    def test_static_identity_source_fails(self):
        self.assert_invalid(lambda bundle: bundle["aws_environment"].update(identity_source="EnvConfigCredentials"))

    def test_missing_security_design_review_fails(self):
        self.assert_invalid(lambda bundle: bundle["design_reviews"].pop("error_taxonomy_security"))

    def test_duplicate_kms_key_fails(self):
        def mutate(bundle):
            bundle["aws_environment"]["key_sha256"]["recovery"] = bundle["aws_environment"]["key_sha256"]["primary"]

        self.assert_invalid(mutate)

    def test_pending_scenario_fails(self):
        self.assert_invalid(lambda bundle: bundle["aws_scenarios"][0].update(status="pending"))

    def test_missing_cloudtrail_correlation_fails(self):
        def mutate(bundle):
            scenario = next(item for item in bundle["aws_scenarios"] if item["id"] == "primary_unwrap")
            scenario["cloudtrail_correlated"] = False

        self.assert_invalid(mutate)

    def test_incomplete_secret_canary_fails(self):
        self.assert_invalid(lambda bundle: bundle["secret_canary"].pop("heap"))

    def test_implementation_author_cannot_sign_independent_drill(self):
        self.assert_invalid(
            lambda bundle: bundle["recovery_drill"].update(independent_operator="implementation-author")
        )

    def test_missing_artifact_fails(self):
        self.assert_invalid(lambda bundle: bundle["supply_chain"]["artifacts"].pop())

    def test_unsigned_artifact_fails(self):
        self.assert_invalid(
            lambda bundle: bundle["supply_chain"]["artifacts"][0].update(sigstore_verified=False)
        )

    def test_signoff_for_different_commit_fails(self):
        self.assert_invalid(lambda bundle: bundle["signoffs"][0].update(commit="c" * 40))

    def test_duplicate_signoff_reviewer_fails(self):
        def mutate(bundle):
            bundle["signoffs"][1]["reviewer"] = bundle["signoffs"][0]["reviewer"]

        self.assert_invalid(mutate)

    def test_raw_arn_fails(self):
        self.assert_invalid(
            lambda bundle: bundle["aws_scenarios"][0]["evidence"].append(
                "arn:aws:kms:us-east-1:111122223333:key/example"
            )
        )

    def test_placeholder_reviewer_fails(self):
        self.assert_invalid(lambda bundle: bundle["signoffs"][0].update(reviewer="pending"))


if __name__ == "__main__":
    unittest.main()
