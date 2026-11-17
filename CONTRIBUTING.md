# Contributing

**Before starting every issue, read [the global development guidelines](DEVELOPMENT.md)
in full.** They define the mandatory coverage, reuse/clean architecture and small
commit/PR policy for all contributors and coding agents.

## Work from dependencies

Every implementation task has a GitHub issue with scope, acceptance criteria,
required tests and explicit blockers. Use native blocked-by relationships and
repeat dependency links in the issue body. Start only when blockers are merged
and their acceptance criteria are satisfied. An open planning issue is not an
implementation approval. Keep unrelated refactors out of feature PRs.

## Small commits, bounded PRs

- One coherent issue or vertical slice per PR; split large issues into dependent
  follow-ups before implementation, rather than opening a mega-PR.
- Aim for **1–4 meaningful commits per PR**. More than 5 requires explanation and
  usually a split. This is a review guideline, not a reason for artificial commits.
- Each commit should express one understandable change and be buildable/testable
  once executable code exists. Ship regression tests with the fix; do not split
  them into a later unprotected PR.
- Use concise imperative messages: `feat(engine): ...`, `test(ui): ...`,
  `fix(playback): ...`, `docs: ...`.
- WIP/fixup commits should be cleaned up before review. Prefer retaining meaningful
  commits; squashing a tiny PR is fine. No force push to the default branch.
- Target roughly 100–400 hand-written changed lines per implementation PR where
  practical, excluding generated files/fixtures. This is guidance, not a quota.
  If safe review needs more, explain or split at a contract boundary.
- Do not merge this initial planning PR on behalf of the maintainer. Subsequent
  merge/branch-protection policy must be set up explicitly.

## Definition of done

- Blockers closed by merged work; PR references its issue and dependencies.
- Reviewed public contracts/ADRs updated before incompatible changes.
- Unit and relevant integration tests pass; regressions and failure paths covered.
- **95% statement coverage for each production Go module and the Python adapter**;
  never round 94.99% up, silently skip a module, or pool modules to hide a deficit.
- Relevant user journeys run at the UI event boundary, not only helper functions.
- Layout fits tested terminal sizes; selection/focus and keyboard ownership verified.
- No credentialed tests, personal accounts, protected media or secrets in CI.
- Offline and cancellation/recovery paths verified where applicable.
- Docs and migration/release notes describe user-visible changes.
- PR describes tests actually run and anything not run; no unsupported claims.

Before executable code lands, the quality-infrastructure issue must install and
prove the coverage/journey gates. Documentation-only PRs cannot demonstrate runtime
coverage. Do not add dummy code merely to produce a green percentage.

## Repository hygiene

Do not copy the prototype binary, user data, virtual environments or credentials.
Taletty is MIT licensed. Keep source attribution and dependency/license notices
when importing code; the repository license does not automatically relicense
third-party dependencies or prototype code. Verify provenance before extraction.
