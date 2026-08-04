# Admin Design System and Appearance QA Evidence

Date: 2026-08-04
Scope: `docs/prd-admin-design-system-appearance.zh-CN.md` implementation and
code-level release gates.

## Automated evidence

- `go test ./...`: passed outside the restricted sandbox; the first sandboxed
  run failed only where existing tests attempted to bind loopback test ports.
- `cd web && npm run typecheck && npm test -- --run && npm run build`: passed.
- Frontend result: 19 test files and 90 tests passed.
- Production bundle: 224,247 bytes gzip across the five initial files, below
  the 500 KiB gate.
- Browser artifact scan: eight files clean.
- Design-system gates verify exact Dark/Light semantic-token parity, documented
  WCAG contrast thresholds, and zero business-source color literals or direct
  primitive-token consumption.

The automated cases include default/normalization/persistence, Preferences
complete-resource validation, CSRF/ETag/revision conflicts, stable error codes,
Audit metadata and rollback, ordinary and MFA session restoration, logout
reset, serialized rapid switching, failed-save rollback/retry, reduced motion,
forced colors, localization parity, and browser-persistence artifact scanning.

## In-app browser evidence

An isolated local instance was exercised with a real admin session and server
persistence. Light and Dark selections applied immediately, displayed the saved
confirmation, survived navigation/reload, and returned the expected semantic
token value. A cookie-isolated login page independently rendered Dark.

Both appearances were checked on all of these areas:

- Dashboard
- Providers and Credentials
- Deployments
- Routes
- Policies
- Projects and Keys
- Usage
- Operations and Audit
- Master Key
- Settings / General

Each area was checked at 320, 768, and 1440 CSS pixels. All 60 combinations had
the requested appearance, a rendered main region and page heading, and no
document-level horizontal overflow. Desktop Light and mobile Dark were also
visually inspected.

## Release boundary

This evidence closes automated and in-app-browser code acceptance. The PRD's
separate human sign-off across current Safari and Firefox, a desktop screen
reader, native autofill, and a real second browser/device remains a release
operator gate because those environments are not available in the automated
workspace. It is intentionally not represented as completed by this record.
