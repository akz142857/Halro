# Security Policy

Heimdall handles Provider credentials, internal Gateway keys, prompts, usage,
and budget decisions. Please report vulnerabilities privately and responsibly.

## Supported versions

| Version | Security support |
|---|---|
| Latest stable release | Yes |
| Current `main` | Best effort before the next release |
| Older releases | No, unless a maintainer announces otherwise |

Before the first stable release, only the current `main` branch is supported.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting flow:

<https://github.com/akz142857/Heimdall/security/advisories/new>

Do not open a public issue and do not include a real Provider key, Gateway key,
Master Key, prompt, response, backup, database, WAL, or production endpoint.
Use synthetic reproductions and redact headers and identifiers.

Please include:

- affected version or commit;
- deployment assumptions and reachable interface;
- reproduction steps or a minimal proof of concept;
- expected impact and suggested severity;
- whether the issue is already public or under active exploitation;
- a safe way to coordinate follow-up.

If private reporting is unavailable, open a public issue containing only a
request for private security contact. Do not disclose technical details there.

## Response targets

These are targets, not contractual guarantees:

- acknowledgement within 3 business days;
- initial triage within 7 business days;
- coordinated remediation and disclosure timeline based on severity.

Maintainers may request validation on a private patch or release candidate.
Please allow reasonable time for users to update before public disclosure.

## Security boundaries

Reports are especially valuable for credential disclosure, authentication or
authorization bypass, budget/accounting bypass, durable ledger corruption,
redaction bypass, SSRF/DNS rebinding, unsafe restore or path handling, session
or CSRF weaknesses, unbounded resource consumption, and release supply-chain
issues.

Third-party Provider outages, unsupported Provider behavior, social engineering
without a product vulnerability, and findings requiring an already-compromised
host are normally outside scope, but may still be reported privately when the
impact is unclear.
