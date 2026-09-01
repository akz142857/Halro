# Agent Working Instructions

Applies to every coding agent working in this repository, not one vendor's.
`CLAUDE.md` points here for the verification policy below.

## Validation policy

The rule behind all of it: run what the change can affect. Re-running a suite that
cannot see the change is not thoroughness, it is dead waiting.

### Frontend

- During iterative UI work, do not run the full test suite after every edit.
- For CSS-only, copy-only, spacing, or visual hierarchy changes, use a focused visual check; run tests only when behavior, markup semantics, or component logic changes.
- For localized frontend behavior changes, run the directly affected test file first, for example `cd web && npx vitest run src/pages/DeploymentsPage.test.tsx`.
- Design-system and stylesheet edits are answered by `npx vitest run src/design-system.test.ts` alone; it is the only test that reads `styles.css` and the theme layer.
- Run type checking when TypeScript types or component logic change. Do not repeat it when subsequent edits only change CSS or copy.
- Run the full frontend gate (`typecheck`, all tests, and production build) once before pushing, at final handoff, or when the user explicitly requests it — not once per commit.

### Go

- While iterating, run the narrowest thing that covers the change: `go test ./internal/<pkg>/ -run TestName`, then the package, and only then `./...`. `internal/app` alone is ~80s and the whole suite is minutes, so the difference is not marginal.
- Use `-count=1` whenever a pass is being read as evidence; a cached `ok` proves nothing.
- `-race` is for changes that touch concurrency, goroutines, shared state or lifecycle, and then only for the affected package — `go test -race ./internal/app/` is ~215s. Threading a parameter or editing pure logic does not earn it.
- A full `go test ./...` is warranted once before pushing, and whenever the question itself needs it — deciding whether a failure elsewhere was caused by this change is a legitimate reason to run it.

### Both

- Do not repeat a previously successful full gate unless code changed afterward in a way that could affect the result.
- Never run billable real-provider smoke tests unless the user explicitly requests them.
- Do not read an exit code through a pipe: `cmd | grep ...` leaves `$?` and `PIPESTATUS[0]` describing `grep`, so a suite gets re-run for no reason. Redirect to a file and grep that.

## The full gate runs once before pushing, not once per commit

A series of commits gets one full gate, immediately before the push that publishes
it. What has to be protected is the branch other people and CI read, and that is
what a push changes; a local commit changes nothing anyone else can see.

For each individual commit, run what that commit can affect — the packages and test
files it touches, plus the conditional checks its content earns (`-race`, the
embedded-bundle drift check after `web/` edits).

The cost this avoids is real: a four-commit series used to mean four full Go
suites and, where the frontend was touched, four full frontend gates. The price
paid for it is that an intermediate commit in a series has not been independently
verified, so a `git bisect` landing on one cannot assume a green gate. That is an
accepted trade in this repository.

Two things this does not license. A push still gets a genuine full gate, including
`git diff --exit-code -- internal/webui/dist` when `web/` changed. And a docs-only
commit added on top of already-gated code does not require re-running anything —
markdown cannot change a test result, and re-running it is exactly the dead
waiting this policy exists to prevent.

## The embedded bundle is generated, and is treated as generated

`internal/webui/dist` is the built `web/` bundle compiled into the Go binary. It
is committed, so it looks like source in every `git` view, and it is not. Two
rules follow from that, and both have cost real time in this repository.

**Commit it in the same commit as the `web/src` change that produced it.** It is
easy to miss because the obvious command misses it: `git add -A -- web` stages
nothing under `internal/`, so a frontend change commits cleanly, passes review,
and fails CI on drift. Stage it by name, or use `git add -A` from the repository
root. `git diff --exit-code -- internal/webui/dist` before the push is the check
that catches it.

**Never hand-merge a conflict in it — delete it and rebuild.** Two branches that
both touch the console produce different content-hashed filenames for the same
sources, so a merge presents dozens of conflicts between files that are not
different versions of each other but different builds of the same thing. There is
no correct resolution to pick, and a spliced bundle corresponds to no source tree
at all. Resolve by taking either side wholesale, rebuilding from the merged
sources, and committing that:

```bash
rm -rf internal/webui/dist
git checkout origin/main -- internal/webui/dist
cd web && npm run build && cd ..
git add -A internal/webui/dist
```

The sources themselves are ordinary text and merge ordinarily; check that they
carry both branches' changes before rebuilding, because the rebuild will happily
produce a bundle from a bad merge too.

## An adapter's silence is not the upstream's answer

Before writing a hardcoded list of anything an upstream could be asked for —
models, regions, deployments, versions — go and check whether it serves a route
that answers. "Halro does not enumerate this profile" is a fact about Halro. It
is not evidence about the provider, and the two get confused because the code
reads the same either way: an empty list, a missing Refresh button, a bundled
catalog quietly standing in.

This has already cost a release. MiniMax's Anthropic-faced profile shipped with a
bundled model list and no Refresh control, on the stated grounds that the profile
cannot enumerate. MiniMax serves `GET /v1/models` on the same host with the same
bearer key, and Halro's own OpenAI-faced MiniMax profiles call it. What could not
read it was one adapter's decoder, which expects Anthropic's response shape —
a Halro limitation described in the code as if it were an upstream one.

The second half is worse, because it looked like a reason rather than a gap. The
code justified not enumerating by saying the list would credit the account's
speech and video models with chat. Nobody had read the list. When it was finally
read, it held eight entries and every one was a chat model — and the reasoning
had the wrong subject anyway, since what turns an identifier into a capability
claim is the metadata mapper, not enumeration. An inference stated as a finding
survives review, because it reads exactly like one.

The rule that follows:

- **Where the upstream serves a list, the upstream's list is the model list.** Ask
  it, and put the answer behind a control the operator can press. A bundled
  catalog is the answer only for a provider that genuinely has no route to ask,
  and "genuinely" means somebody looked.
- **The bundled catalog still supplies capabilities, and only capabilities.** An
  OpenAI-shaped list carries an identifier and an owner, so it says who exists
  and nothing about what they do. Enumeration and capability evidence are
  separate questions; answering the first from the upstream does not license
  answering the second from it. A target enumerated without capability evidence
  resolves unknown and is declared or detected — never credited by construction.
- **Keep "who exists" and "what they do" apart, and check who is answering.** A
  claim sourced `provider_metadata` asserts the upstream said something, so it may
  only be built from a target the upstream actually described. The Anthropic
  metadata mapper claimed chat and streaming from the endpoint rather than from a
  field — correct for a model that endpoint listed, a fabrication for an
  identifier somebody typed, and the mapper cannot tell them apart because every
  field it reads looks identical. An invented model name resolved as a working
  chat deployment on evidence labelled `provider_metadata`. Mappers that build
  claims out of a field the provider populated were safe by construction;
  claiming from the absence of a field is the shape that needs a gate.
- **Read the real response before writing the decoder for it.** A decoder written
  against a shape inferred from documentation is a fixture of your own mental
  model. Capture the body first — the MiniMax smoke has a subtest that does
  exactly this and nothing else — and write against what came back.
