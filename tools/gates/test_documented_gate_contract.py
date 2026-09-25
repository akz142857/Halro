#!/usr/bin/env python3

"""The gate CLAUDE.md names has to be the gate the Makefile runs.

260918-PV-F-14: CLAUDE.md called `make check` "the full local gate" while the
block directly above it listed a typecheck, a production build and a
bundle-drift check that `check` does not run. Anyone following the file ran a
weaker gate than they believed, and a stale `internal/webui/dist` passed
locally and failed CI.

Correcting the sentence once is not the fix. The two drifted apart because
nothing compared them, and they will drift again for the same reason — the
Makefile is edited by people changing targets and CLAUDE.md by people writing
prose. So this compares them.

What it asserts is deliberately narrow: that the target CLAUDE.md names as the
gate exists, that it actually reaches the frontend production evidence, and
that the target it describes as the shorter loop does not silently become the
same thing. It does not try to parse every step of either — a test that
mirrored the Makefile line for line would fail on every reordering and teach
people to edit it without reading it.
"""

from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[2]
MAKEFILE = ROOT / "Makefile"
GUIDE = ROOT / "CLAUDE.md"

# The target that must carry the frontend production evidence: typecheck,
# production build, and the committed-bundle comparison.
PRODUCTION_EVIDENCE_TARGET = "frontend-production-check"


def make_prerequisites(makefile: str, target: str) -> list[str]:
    """The direct prerequisites of one target, in order."""
    match = re.search(rf"^{re.escape(target)}:(.*)$", makefile, re.MULTILINE)
    if match is None:
        return []
    return match.group(1).split()


def reaches(makefile: str, target: str, wanted: str, seen: set[str] | None = None) -> bool:
    """Whether `target` reaches `wanted` through its prerequisites."""
    seen = seen if seen is not None else set()
    if target in seen:
        return False
    seen.add(target)
    for prerequisite in make_prerequisites(makefile, target):
        if prerequisite == wanted or reaches(makefile, prerequisite, wanted, seen):
            return True
    return False


class DocumentedGateContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.makefile = MAKEFILE.read_text(encoding="utf-8")
        cls.guide = GUIDE.read_text(encoding="utf-8")

    def test_the_named_gate_target_exists(self):
        named = self._named_gate_target()
        self.assertTrue(
            make_prerequisites(self.makefile, named),
            f"CLAUDE.md names `make {named}` as the gate and the Makefile has no such target",
        )

    def test_the_named_gate_reaches_the_frontend_production_evidence(self):
        """The distinction that made F-14 a finding rather than a typo.

        Without this the gate runs no typecheck, no production build and no
        bundle-drift comparison, so a stale committed bundle passes locally and
        fails in CI — which is the failure the file was steering people into.
        """
        named = self._named_gate_target()
        self.assertTrue(
            reaches(self.makefile, named, PRODUCTION_EVIDENCE_TARGET),
            f"`make {named}` is documented as the full gate but does not reach "
            f"`{PRODUCTION_EVIDENCE_TARGET}`; it would pass on a stale committed bundle",
        )

    def test_the_shorter_loop_is_not_quietly_the_same_target(self):
        """`check` may grow, but if it becomes the gate then the guide has to
        say so rather than leaving two names for one thing — the pre-1.0.0 rule
        against a wrong construct surviving beside its replacement."""
        named = self._named_gate_target()
        if named == "check":
            return
        self.assertFalse(
            reaches(self.makefile, "check", PRODUCTION_EVIDENCE_TARGET),
            "`make check` now reaches the frontend production evidence, so it is the "
            "full gate; update CLAUDE.md to name it and retire the second target",
        )

    def test_the_guide_does_not_call_the_shorter_loop_the_full_gate(self):
        """The exact sentence that was wrong."""
        # An explicit fail rather than assertNotRegex: the helpers print the
        # whole haystack on failure, and the haystack here is CLAUDE.md.
        if re.search(r"full local gate: `make check`", self.guide):
            self.fail(
                "CLAUDE.md calls `make check` the full local gate again; it runs no "
                "typecheck, no production build and no bundle-drift check"
            )

    def test_the_guide_names_node_22_for_the_gate(self):
        """The production gate exits 2 on any other major version, so a reader
        who does not know that meets a failure with no obvious cause."""
        if "Node 22" not in self.guide:
            self.fail("CLAUDE.md does not mention Node 22, which the frontend production gate requires")

    def _named_gate_target(self) -> str:
        """The target CLAUDE.md points at as the gate to run before pushing."""
        match = re.search(r"run it as one target: `make ([a-z-]+)`", self.guide)
        self.assertIsNotNone(
            match,
            "CLAUDE.md no longer names a single gate target; this contract has nothing to check",
        )
        return match.group(1)


if __name__ == "__main__":
    unittest.main()
