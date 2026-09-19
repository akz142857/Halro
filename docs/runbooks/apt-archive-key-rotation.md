# APT archive signing key: identity, expiry, and rotation

The APT channel at `https://packages.halro.ai/apt` is signed by one dedicated
online key. This runbook is what to do before it expires, and what changes if it
has to be replaced instead of extended. Its sibling is
[`model-catalog-publishing.md`](model-catalog-publishing.md); neither is an
incident procedure, so neither is embedded into the binary.

## The key

| | |
| --- | --- |
| Primary fingerprint | `5BE006E18867052367FECA92EEC7C041250D47E8` |
| Algorithm | RSA 4096, sign-only |
| Created | 2026-09-18, with the first APT publication (v0.8.4) |
| **Expires** | **2028-09-17** |
| Private half | `HALRO_APT_ARCHIVE_SIGNING_KEY`, an Environment secret in `apt-production` of `halro-ai/apt-repository` |
| Fingerprint | `HALRO_APT_ARCHIVE_SIGNING_FINGERPRINT`, an Environment variable beside it |

Three properties of that secret are load-bearing and were read from
`publish.yml`, not assumed:

- **No passphrase.** It is imported with `gpg --batch --import` and reprepro
  signs unattended.
- **Armored.** A GitHub secret is a text value; a binary export cannot survive
  it.
- **The variable holds the _primary_ key's fingerprint.** The check reads the
  first `fpr` from `gpg --with-colons --list-secret-keys`, which is the primary.
  A subkey fingerprint there fails closed after the environment is already
  provisioned.

RSA rather than Ed25519 is a compatibility choice: Debian's own archive keys are
RSA 4096, and an APT client older than the archive is the case that matters.

## Why expiry is not a quiet degradation

An expired key does not mean "no new versions". `apt update` fails signature
verification on **every host that already installed Halro**, because
`/etc/apt/keyrings/halro-archive-keyring.gpg` is a static file each host fetched
at install time and nothing updates it in place.

Nothing about the public half is manual: `publish.yml` exports the binary and
armored public key and hands them to the website, which serves them under
`/keys/`. `public-acceptance` then compares the SHA-256 of the served keyring
against the one it just exported. **That comparison is why rotation is a
coordinated change across two repositories rather than a key swap**: change the
key without republishing its public half and acceptance times out; republish
without the archive being re-signed and it times out the other way.

## Path 1 — extend the expiry (preferred)

Same key, same fingerprint, so an installed host keeps trusting what it already
has. Only the public key's bytes change, because the expiry is part of a
self-signature.

```bash
export GNUPGHOME=$(mktemp -d) && chmod 700 "$GNUPGHOME"
gpg --import <the operator's offline private key>
gpg --quick-set-expire 5BE006E18867052367FECA92EEC7C041250D47E8 2y
gpg --armor --export-secret-keys 5BE006E18867052367FECA92EEC7C041250D47E8 >archive-signing.asc

gh secret set HALRO_APT_ARCHIVE_SIGNING_KEY --repo halro-ai/apt-repository \
  --env apt-production <archive-signing.asc
rm -rf "$GNUPGHOME" archive-signing.asc
```

`HALRO_APT_ARCHIVE_SIGNING_FINGERPRINT` does not change. Then republish the
current version so the new public half reaches the website:

```bash
gh workflow run publish.yml --repo halro-ai/apt-repository \
  -f version=<the currently published version> -f commit=<that release's commit>
```

Keep the refreshed private key with the offline copy; the Environment secret
cannot be read back out of GitHub, so that copy stays the only readable one.

## Path 2 — replace the key (only when the private half is lost or leaked)

The fingerprint changes, so every installed host must fetch the new keyring
before `apt update` works again. That is a user-visible break: announce it on
`halro.ai` with a transition period before publishing under the new key, and
revoke the old key with its revocation certificate.

If the private half was merely lost, this is the only path left — which is why
the offline copy matters more than the GitHub secret.

## Verify, from outside CI

A green workflow is not the evidence; a clean host is. This is the check the
first publication was accepted on:

```bash
docker run --rm debian:13-slim bash -ceu '
  apt-get update -qq && apt-get install -y -qq --no-install-recommends ca-certificates curl
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://halro.ai/keys/halro-archive-keyring.gpg \
    -o /etc/apt/keyrings/halro-archive-keyring.gpg
  curl -fsSL https://halro.ai/packages/halro.sources \
    -o /etc/apt/sources.list.d/halro.sources
  apt-get update && apt-get install -y -qq --no-install-recommends halro && halro version'
```

`apt update` must complete with no `NO_PUBKEY` and no signature error.

## What the `apt-production` Environment does and does not enforce

It allows deployments from `main` only, through a branch policy. It has **no
required reviewer and no wait timer**: GitHub refuses both protection rules for
a private repository on the current billing plan, and setting
`protected_branches: true` instead would block every deployment, because no
branch in that repository carries branch-protection rules. The gate that remains
is that publishing requires write access and a deliberate dispatch.
