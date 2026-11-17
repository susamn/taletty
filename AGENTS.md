# Instructions for coding agents

Before starting **any issue or implementation task**, read `DEVELOPMENT.md` in full
and then `CONTRIBUTING.md`. Read the issue's blockers and relevant docs before code.
Treat the global guidelines as mandatory, not optional suggestions:

1. >=95% statement coverage independently per production Go module and Python
   adapter, plus meaningful automated UI journeys and failure/recovery tests.
2. Inspect and reuse existing code; preserve clean module boundaries and readable,
   idiomatic, straightforward logic. No copy-paste business logic or spaghetti code.
3. Small focused commits, normally 1–4 meaningful commits per PR; split oversized
   work rather than accumulating unrelated changes.

Acknowledge the policy and slice/test plan in the issue or PR. Do not implement
blocked issues or claim tests/coverage that have not run. Documentation-only tasks
report runtime coverage N/A. Do not merge without maintainer approval.
