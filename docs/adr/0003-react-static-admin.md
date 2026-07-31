# ADR 0003: React static Admin UI

- Status: Accepted
- Date: 2026-07-31

## Decision

Use React, TypeScript, and Vite for the Admin UI. Production receives only static assets embedded into the Go binary.

No Node.js server, SSR runtime, public CDN, service worker, or client-side secret persistence is allowed.

## Rationale

Route editing, policy builders, redaction tests, usage filters, and one-time key handling have enough local interaction to justify a component UI. This does not change the Gateway data plane or deployment model.
