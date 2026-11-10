# Website docs are part of every release

The release is not complete when only the download page advertises the new
version. Current Chinese and English docs, installation commands, API contracts,
and affected guides must describe the same Halro source that produced the assets.
Historical guides retain their original version and evidence.

## Prepare before dispatch

1. Complete the Halro release preparation PR, merge it, and wait for ordinary CI
   on the exact resulting main commit. Record that full SHA as `RELEASE_COMMIT`.
2. Create a companion PR in `akz142857/Halro-website` from its latest main. Keep
   unrelated local changes in their original checkout. Review the release delta
   for console behavior, Providers, configuration defaults, API compatibility,
   installation, upgrade and rollback instructions. Update both languages;
   record which affected topics changed and which were reviewed without changes.
3. Copy `docs/compatibility/endpoint-manifests.json` from that exact Halro commit
   into the website's `src/data/endpoint-manifests.json`. Set
   `contracts.meta.json` source ref, source commit and both manifest digests to
   the release identity. Derive `served-profile-state.json` from that source's
   `domain.AllProviderProfiles()` and `compatibility.BuiltinEndpointManifests()`, rather
   than carrying forward assumed serving capabilities. Regenerate both sets of
   API pages with `bun run generate:api`.
4. Set the website distribution's top-level version, release commit, release URL
   and planned date. Review both docs landing pages and quickstart-install pages;
   update their visible version, pinned install commands, `sourceRevision`,
   `verifiedAgainst`, and English `translatedFromRevision`. Do not merely stamp
   new provenance onto unreviewed content. Keep individual channel versions and
   digests at their last verified values until their new acceptance succeeds.
5. Run the website's build, release-content checks and CI. Commit the candidate
   and record its full SHA as `WEBSITE_DOCS_COMMIT`. **Keep this PR unmerged:** its
   new install links refer to assets that do not exist yet.

The website must target the merged Halro SHA, not the release PR's head. Do not
commit the website SHA back into the Halro release preparation record afterward:
that would change the Halro SHA and invalidate the candidate. Store the companion
PR, website SHA and run evidence outside the release source tree. If main moves
before dispatch, repin and review the website candidate against the new Halro
commit, then repeat its validation and the release rehearsal.

From the Halro checkout, validate the committed candidate locally:

```sh
python3 tools/release/verify_website_docs.py candidate \
  --version "$VERSION" --commit "$RELEASE_COMMIT" \
  --website-dir "$WEBSITE_DIR" --website-commit "$WEBSITE_DOCS_COMMIT"
```

## Rehearse and publish

Both dispatches require the same immutable website candidate:

```sh
gh workflow run release.yml --repo akz142857/Halro --ref main \
  -f version="$VERSION" -f website_docs_commit="$WEBSITE_DOCS_COMMIT" \
  -f dry_run=true
# After the rehearsal passes, with main still at RELEASE_COMMIT:
gh workflow run release.yml --repo akz142857/Halro --ref main \
  -f version="$VERSION" -f website_docs_commit="$WEBSITE_DOCS_COMMIT" \
  -f dry_run=false
```

The `website-docs` job checks out that website SHA, verifies release identity,
byte-identical API contracts and bilingual entry points, and builds the website
including its generated-content, translation, metadata, links and release-content
checks. `provenance` depends on it, so failure blocks signing and publication.
The existing release App supplies a website-scoped, contents-read token; this
read credential is required even for a rehearsal. No website merge or deployment
occurs in a rehearsal.

## Publish the companion and verify the public pages

After GitHub and GHCR publication, update the companion's GHCR digest to the
actual published manifest. Advertise new Homebrew/APT versions only after their
exact-version clean-host acceptance passes. Keep a delayed channel on its last
accepted version; it must not hold up accurate docs for the published release.

Coordinate with the package workflows' website PRs. An APT deployment-only PR
can proceed independently. A package advertisement PR that bumps the release
identity without the prepared docs will fail the website's docs gate: integrate
the companion and the accepted channel metadata instead of bypassing the gate.
Run website CI on the final combined diff, merge, and wait for deployment.
Metadata changes after preflight must preserve the validated docs; any content
changes require renewed review and website validation.

The release's `website-acceptance` job starts after `container-push` and waits up
to 20 minutes for six public pages: Chinese/English docs landing pages, pinned
quickstart install commands, and API reference source revisions. It also runs
for a GitHub-only release with package dispatch disabled. A successful build or
deployment run alone is not public-page acceptance.

If the wait expires, the workflow reports failure while the published assets
remain valid. Fix and deploy the website, then rerun the failed acceptance job
in the original release run. Never move the release tag, replace assets, or
start a new formal dispatch to repair docs. The check can also be run read-only:

```sh
python3 tools/release/verify_website_docs.py public \
  --version "$VERSION" --commit "$RELEASE_COMMIT"
```

Report publication and website state separately until acceptance succeeds.
The docs gate verifies identity and representative live pages; it does not
replace the release-delta review of every affected guide.

## Retain the evidence

Download `website-docs-candidate-RUN_ID-ATTEMPT` and
`website-docs-public-RUN_ID-ATTEMPT` from the run before their 90-day retention
expires. A retry can have a newer attempt for public acceptance than the original
candidate build; retain the successful artifact from each relevant attempt.
Record the final website PR/deployment run alongside them. These are separate
workflow artifacts, not additions to the immutable signed release asset set.
See [release run evidence](release-run-evidence.md) for the archive boundary.
