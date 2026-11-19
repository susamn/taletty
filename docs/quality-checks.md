# CI checks and bootstrap acceptance

## Current real checks

| Check | Evidence / failure behavior |
|---|---|
| Coverage tooling | Discovered module inventory, Python compilation/unit tests, independent >=95% tooling coverage |
| Python adapter (3.12) | Fresh all-source independent coverage, exact discovered-source/report equality, real pinned SDK/media-tool probes and fake dependency/provider cases |
| Go app (1.26.x), Go app (1.27.x) | Formatting, tidy diff, vet/build, fresh race tests of all packages and independent >=95% app coverage |
| Go engine (1.26.x), Go engine (1.27.x) | Same gates independently for engine |
| Quality gate | Requires tooling, Python adapter and the entire discovered Go matrix to succeed; failed, canceled or skipped dependencies fail this check |

Require the stable **Quality gate** context from GitHub Actions (app ID 15368) for
merging into main. Matrix expansion then cannot bypass merge protection merely
because a new check name was not added to branch settings. It is a real status
aggregation gate, not a no-op UI/media job. Keep all checks on every PR, with no
path filter or continue-on-error bypass. A new adapter/job must be added to its
`needs` list in the same PR as implementation. #63 registers the real adapter
and tests all 64 tooling/matrix/adapter aggregation states. Inventory rejects Python
outside its registered location and reports omitting any discovered adapter source.

## Main-branch protection policy

- Require a pull request and passing Quality gate, with the branch up to date.
- Apply checks to administrators too; disallow force pushes and branch deletion.
- Resolve review conversations before merge.
- This is presently a single-maintainer repository: no mandatory second-person
  approval count, since the maintainer cannot approve their own PR. Human review
  and merge remain maintainer responsibilities; agents must not merge for them.
- Do not require a linear history; focused commits and normal reviewed merge PRs
  are compatible. Do not disable checks for a failing feature.

Repository settings are external state. Verify them through GitHub after the first
Quality gate run; describe any permission/plan restriction honestly rather than
claim protection was enabled. Change this policy through review, not a silent script.

## Parent #4 acceptance audit

- #36: fail-closed validator with exact threshold, malformed/empty/missing/omitted
  reports, exit-code and nonpooling regressions; tooling measured, not dummy code.
- #37: independent production-code-bearing modules, inventory-derived matrix,
  no unpublished local replaces, separate all-package race/coverage evidence.
- #38: real Bubble Tea model-command test seam with selected completion order,
  fake service/time, cancellation, finite budgets and explicit unsupported controls;
  stable required-check policy. No runnable user application claimed.

Parent #4 is complete only after these PRs are merged, actual CI checks succeed,
and protection is verified (or a documented maintainer-approved restriction exists).
A slice PR may close #38; it must not automatically close #4 before that audit.

## Deferred checks have explicit owners

| Check / journey | Delivery owner | Required before |
|---|---|---|
| Python adapter coverage, read-only bootstrap Go↔Python contracts | #63 under #57 | Delivered with first real source; named job + all-source equality + independent gate |
| Login/library/download Python/Go contracts | #58/#59/#7 | Claiming the corresponding actual provider operation |
| Full coordinator/key/focus/layout journeys, PTY startup/resize/quit | #12 and each following UI feature | Claiming that UI feature complete |
| Real mpv/ffmpeg generated-media tests with null audio | #11 | Claiming playback lifecycle complete |
| Clean install/upgrade/architecture and package lifecycle | #26–#29 | Claiming target/package supported |
| Safe private live-provider verification | #31/#24 | Claiming Audible download release readiness |

No skipped placeholder counts as a passing integration check. The production module
coverage floor remains independent >=95%; harness and high unit coverage do not
replace full user journeys, generated media, offline/recovery or manual release checks.
