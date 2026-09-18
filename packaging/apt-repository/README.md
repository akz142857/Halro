# Halro APT repository control plane

`halro-ai/apt-repository` is the control plane that publishes
`https://packages.halro.ai/apt`, and it is **authoritative**: the files here are
a copy of what runs there, kept so the verification procedure is publicly
readable. That repository is private, so this copy is the only public record of
how a Halro release is checked before it reaches an APT archive. `conf/`, the
reprepro database, and all private key material are never copied to the public
bucket.

**A change to one of these files must land in both places in the same change.**
This copy has already drifted once, in both directions at once: the control
plane gained the release-commit argument and the `dpkg-deb` field matrix while
this copy gained the `checksums.txt` membership assertion and the prerelease
tilde fix, and neither reached the other for eleven days. Nothing detects that
automatically — the control plane is private, so no check in this repository can
read it — which is why it is written down here instead.

What this directory carries: `scripts/verify-release.sh`,
`scripts/import-release.sh`, the reprepro `conf/`, and the control plane's
`ci.yml`. What it does not carry: the publication workflow itself, the public
repository smoke test, the publication contract test, and the snapshot image
definition. Those live only in the private repository, and `ci.yml` here names
them because it is a copy of the real one.

Before first publication:

1. replace `HALRO_ARCHIVE_SIGNING_KEY_FINGERPRINT` with the dedicated online
   archive-signing subkey fingerprint;
2. configure a protected publishing environment and secret manager;
3. import only `.deb` files whose checksums, GitHub attestation, and Sigstore
   bundles have been verified against the immutable Halro GitHub Release;
4. upload immutable pool/index objects first and publish `dists/stable/InRelease`
   last;
5. complete clean-host amd64 and arm64 `apt update`, install, upgrade, remove,
   and retained-data acceptance before advertising the channel on halro.ai.

`scripts/verify-release.sh VERSION EXPECTED_COMMIT DIRECTORY` downloads and
independently verifies the Debian release assets: it refuses a tag that resolves
to a different commit, a package that is absent from `checksums.txt`, a package
whose Debian version is not the one the tag implies, and any set that is not
exactly `halro` and `halro-deadman` for amd64 and arm64. Only its successful
output directory may be passed to `scripts/import-release.sh DIRECTORY`. The
final object-store upload is intentionally deployment-specific: configure it only
after the storage provider, atomic publication strategy, and rollback snapshot
location have been approved.

Do not commit the reprepro `db/` directory or any exported private key.
