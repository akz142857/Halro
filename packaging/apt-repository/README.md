# Halro APT repository control plane

This directory is the source for the private control-plane repository
`halro-ai/apt-repository`. Its generated public tree is published at
`https://packages.halro.ai/apt`; `conf/`, the reprepro database, and all private
key material are never copied to the public bucket.

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

`scripts/verify-release.sh VERSION DIRECTORY` downloads and independently
verifies the Debian release assets. Only its successful output directory may be
passed to `scripts/import-release.sh DIRECTORY`. The final object-store upload
is intentionally deployment-specific: configure it only after the storage
provider, atomic publication strategy, and rollback snapshot location have been
approved.

Do not commit the reprepro `db/` directory or any exported private key.
