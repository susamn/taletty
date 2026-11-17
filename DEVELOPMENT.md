# Global application development guidelines

These guidelines apply to **every issue, contributor, coding agent and PR** across
the engine, Python adapter, application, UI, tests, packaging and documentation.
This is the canonical development policy. Feature-specific instructions supplement
it; they do not silently weaken it.

## Before starting any issue

1. Read this file in full, then [CONTRIBUTING.md](CONTRIBUTING.md).
2. Read the issue, its blockers and relevant architecture/interaction/testing docs.
   Confirm blocking work has been merged and accepted before implementation.
3. Inspect existing code and tests for reusable contracts, services and helpers.
4. Outline the smallest coherent slice, test/journey coverage and intended commits.
   Split oversized work into dependency-linked child issues before coding.
5. Acknowledge the guidelines and record the intended slice/test plan in the issue
   or PR. Re-read the current policy when starting another issue; do not rely only
   on memory from an earlier session.

## 1. At least 95% code coverage, with meaningful tests

- Require **>=95.00% statement coverage independently for each production Go module
  and the Python adapter**. Do not pool modules, round up a failing ratio, exclude
  hard-to-test code casually or omit packages/reports to pass.
- Include relevant user journeys through the real application event boundary,
  not just unit tests of helpers. Test failures, cancellation, offline operation,
  persistence, keyboard ownership, focus, resize and recovery where applicable.
- Tests accompany implementation, not a later cleanup PR. Fixes include regressions.
- Use deterministic clocks, fake providers, temporary private data and generated
  media. No credentials/protected audio in fixtures or CI artifacts.
- Follow [the test strategy](docs/testing.md). Coverage is a minimum gate, not proof
  of correctness. CI must fail on missing/invalid reports and unmet thresholds.
- Documentation/design-only changes report runtime coverage as N/A; never invent
  coverage for an empty module or claim checks ran when they did not.

## 2. Reuse, clean architecture and readable standard logic

- Read existing implementations before creating another helper/service. Reuse a
  correct existing abstraction; extract a small shared function when real duplicate
  behavior warrants it. Do not copy-paste business logic or introduce speculative
  frameworks solely for possible future reuse.
- Keep boundaries explicit: provider/auth/download logic in the engine; orchestration
  in application services; mpv and policies behind playback interfaces; persistence
  behind repositories; UI focused on presentation, input and typed actions.
- Do not put provider calls, database queries or subprocess coordination in rendering.
  Do not let presentation code own independent competing playback state machines.
- Prefer straightforward idiomatic Go/Python, descriptive names, small cohesive
  functions, explicit state transitions and predictable control flow. Use early
  returns to keep normal paths readable; avoid deep nesting, hidden side effects,
  magic strings, catch-all managers and unrelated responsibilities in giant files.
- Dependencies flow one way; no import cycles or leaking raw provider JSON/tokens
  through the app. Introduce interfaces at real boundaries, not for every function.
- Define ownership and cancellation for goroutines/processes/timers; sanitize errors
  at trust boundaries. Preserve context, resource cleanup and recoverable user input.
- Refactor only as needed for the slice and keep behavior changes explicit. Comments
  explain reasons and invariants; names and standard flow explain what the code does.
- Review readability, duplication and coupling alongside tests. Passing coverage
  does not justify spaghetti code, excessive abstraction or poor module boundaries.

## 3. Small commits and a sensible number per PR

- One coherent issue/slice per PR. Keep each commit focused, understandable and
  buildable/testable once executable code exists. Include the relevant tests.
- Target **1–4 meaningful commits per PR**. More than five needs an explanation and
  normally a split. Do not create artificial microcommits to hit a count.
- Aim for reviewable diffs (roughly 100–400 hand-written lines when practical,
  excluding generated artifacts). Explain larger slices; do not game the line count.
- Use descriptive imperative commits, clean up WIP/fixups before review, and keep
  unrelated refactors out. Never force-push the default branch.
- PRs identify the issue/blockers, reused components, verification actually run and
  limitations. Maintainers review before merge; do not auto-merge on their behalf.

## Completion check

- [ ] Guidelines and relevant docs read before implementation.
- [ ] Existing code inspected; reuse/architecture decisions explained where needed.
- [ ] Blockers satisfied; slice remains focused and code flow readable.
- [ ] Independent 95% gates and relevant journeys pass (or docs-only N/A explained).
- [ ] Security, cancellation and recovery requirements satisfied.
- [ ] Commit count/diff size are sensible; docs updated; actual test evidence supplied.
