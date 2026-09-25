#!/usr/bin/env python3

"""A binary `make` leaves in place must not keep an older build's identity.

Found while re-freezing G0 on 2026-09-25. `make build` on a clean checkout of
the candidate produced a `bin/halro` reporting the candidate SHA and a
`bin/halro-deadman` still reporting `v0.8.3-12-g42e48090-dirty` — a describe
string from 2026-09-18, `-dirty` suffix and all, from a tree that was not
clean. Nothing failed. The binary was simply never relinked.

The cause is that the stamp is not a prerequisite. `bin/halro-deadman` depends
on the deadman's sources, and those do not move when HEAD does, so make judged
a binary carrying week-old provenance to be up to date. `bin/halro` had the
same defect and hid it, because its sources happened to change in the same
commits.

G0's pass criterion is that every artifact traces back to the candidate SHA.
This is how that answer becomes no with nothing on the way reporting a
problem, which is why it is worth a gate rather than a one-line fix.

What this asserts is narrow on purpose: that both binaries take the identity
as a prerequisite, and that the stamp does not carry a value which moves on
every invocation — that would make every `make build` relink the tree and
teach people to stop running it.
"""

from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[2]
MAKEFILE = ROOT / "Makefile"

# The stamp holding what the binaries are linked with.
IDENTITY_VARIABLE = "RELEASE_IDENTITY"

# The two artifacts `make build` produces, and which a release, an operator and
# G0 all read a version back from.
STAMPED_BINARIES = ("bin/halro", "bin/halro-deadman")

# Moves on every invocation; in the stamp it would force a relink every time.
REBUILD_ALWAYS_VARIABLE = "RELEASE_DATE"


def prerequisites(makefile: str, target: str) -> list[str]:
    match = re.search(rf"^{re.escape(target)}:(.*)$", makefile, re.MULTILINE)
    return match.group(1).split() if match else []


def recipe(makefile: str, target: str) -> str:
    """The recipe lines of one rule, which are the tab-indented lines after it."""
    match = re.search(
        rf"^{re.escape(target)}:.*\n((?:\t.*\n)*)", makefile, re.MULTILINE
    )
    return match.group(1) if match else ""


class BuildIdentityContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.makefile = MAKEFILE.read_text(encoding="utf-8")

    def test_the_identity_stamp_exists(self):
        # assertRegex does not apply MULTILINE, so the flag is in the pattern.
        self.assertRegex(
            self.makefile,
            rf"(?m)^{IDENTITY_VARIABLE}\s*:?=",
            f"{IDENTITY_VARIABLE} is what makes the stamped version a build input; "
            "without it a binary keeps whatever identity it was first linked with",
        )

    def test_every_stamped_binary_takes_the_identity_as_a_prerequisite(self):
        for binary in STAMPED_BINARIES:
            with self.subTest(binary=binary):
                needs = prerequisites(self.makefile, binary)
                self.assertTrue(
                    needs, f"{binary} has no rule in the Makefile any more"
                )
                self.assertIn(
                    f"$({IDENTITY_VARIABLE})",
                    needs,
                    f"{binary} does not rebuild when the version it is stamped with "
                    "changes, so it can go on reporting an older commit — and, as on "
                    "2026-09-18, an older commit's -dirty tree",
                )

    def test_the_stamp_does_not_carry_a_value_that_moves_every_invocation(self):
        stamp_recipe = recipe(self.makefile, f"$({IDENTITY_VARIABLE})")
        self.assertTrue(
            stamp_recipe, f"$({IDENTITY_VARIABLE}) has no recipe to inspect"
        )
        self.assertNotIn(
            f"$({REBUILD_ALWAYS_VARIABLE})",
            stamp_recipe,
            f"{REBUILD_ALWAYS_VARIABLE} changes on every invocation; in the stamp it "
            "would relink both binaries every time and the gate would be turned off "
            "rather than obeyed",
        )

    def test_the_stamp_is_rewritten_only_when_it_changes(self):
        # Writing it unconditionally would move its mtime on every invocation,
        # which is the same rebuild-always failure by a different route.
        self.assertIn(
            "cmp -s",
            recipe(self.makefile, f"$({IDENTITY_VARIABLE})"),
            "the stamp must be compared before it is replaced, or its timestamp "
            "moves on every invocation and both binaries relink every time",
        )


if __name__ == "__main__":
    unittest.main()
