# v0.6.0 → v0.7.0 upgrade and restore drill

Date: 2026-09-06 (Asia/Singapore)
Mode: isolated File-mode instance under a temporary directory; no KMS and no
real provider call.

## Source identities

- v0.6.0 binary: tag commit
  `381743f6613607dc256828f4776b52af8bdd232c`, built as version `v0.6.0`.
- candidate binary: parent commit
  `a3634d761d1b9364115ae009ee5e7a8189d6675e` plus the working-tree fixes,
  built as version `v0.7.0`.

The fixture used one project, enabled API key, priced deployment, active route,
authenticated Ledger, audit log, and Usage Parquet. Its upstream URL used the
reserved `.invalid` domain, so both gateway requests returned the expected
`provider_error` after authorization/routing rather than contacting a provider.

## Results

1. v0.6.0 initialized and bootstrapped the fixture, served the gateway, and
   produced eight authenticated Ledger frames plus two Usage rows.
2. Candidate `doctor` against schema 35 refused without modifying the data
   directory. A v0.6.0 pre-upgrade backup was then created and verified:
   `bkp_b944dd3f9a838a49dea00e68fb4a2a8e`, archive SHA-256
   `1ec46d28d2b09c7722da0612b1bf48c8abb9be0b3b7b9a9a83fc281200edf441`.
3. Candidate `start` migrated metadata 35 → 36, advanced the authenticated
   Ledger reader/writer epoch 4 → 5, rebuilt the checkpoint, and served the
   existing key/project/route. Post-upgrade verification is healthy:
   16 authenticated Ledger frames, four Ledger-derived Usage rows, four
   Parquet rows, and no missing, duplicate, or extra row.
4. The v0.6.0 reader then refused the upgraded directory on all three new
   boundaries: metadata schema 36, Ledger epoch 5, and Usage manifest/Parquet
   schema 6. Rollback therefore requires the pre-upgrade backup.
5. A post-upgrade backup was created and verified:
   `bkp_16acf33271e7c6ca3e6eb74ada95bf7d`, archive SHA-256
   `c94e441b15054a8f52b140fdf2598b56b96de2e8b04faaef42b7ee22fb138196`.
   It was restored into a fresh data directory; `doctor`, Ledger verification,
   Usage verification, and a clean start all passed.

The `.txt`, `.exit`, and gateway response files in this directory are the
sanitized command outputs. Backup archives, encryption keys, data directories,
and configuration are deliberately not committed.

## Boundary

This closes the populated File-mode upgrade, rollback-refusal, backup, restore,
and preserved-topology checks. It does not validate AWS KMS/key slots, every
supported architecture, a large retained WAL, or a billable real-provider
smoke. Those remain exact-candidate release-run/owner checks.
