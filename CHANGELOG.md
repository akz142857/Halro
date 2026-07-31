# Changelog

All notable user-visible changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and releases use
semantic versioning.

## [Unreleased]

### Added

- Single-binary OpenAI-compatible LLM Gateway with embedded Admin console.
- Encrypted Provider credential vault and hash-only internal Gateway keys.
- Project budget, RPM, TPM, concurrency, model, CIDR, Token Guard, and
  redaction controls.
- Durable local accounting, Parquet analytics, audit integrity, backup/restore,
  Prometheus metrics, alerts, and operational diagnostics.
- OpenAI, Azure OpenAI, DeepSeek, and generic compatible GA adapters; Gemini
  and Bedrock Beta adapters.

### Fixed

- Pin the Dashboard trend to the actual seven-day window when only one data
  point exists.
- Separate Provider-reported Token usage from conservative estimates recorded
  for ambiguous failed attempts.

[Unreleased]: https://github.com/akz142857/Heimdall/commits/main
