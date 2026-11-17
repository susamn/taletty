# Dependency-based delivery plan

GitHub issues are the execution record. Every blocker below is also a native
GitHub blocked-by dependency. Design/contract tasks can proceed in parallel;
implementation waits for merged blockers. Issue closure alone is not sufficient
without acceptance evidence. Split oversized tasks into linked child issues before coding.

All tasks inherit [PR policy](../CONTRIBUTING.md), [coverage/journeys](testing.md)
and [security](../SECURITY.md). The initial planning PR contains no runtime code.

| Issue | Work | Blocked by |
|---|---|---|
| [#1](https://github.com/susamn/taletty/issues/1) | Define provider-neutral engine API and versioned bridge contract | None |
| [#2](https://github.com/susamn/taletty/issues/2) | Approve wide/compact layouts, glyph vocabulary and interaction map | None |
| [#3](https://github.com/susamn/taletty/issues/3) | Define SQLite schema, data paths and non-destructive legacy migration | [#1](https://github.com/susamn/taletty/issues/1) |
| [#4](https://github.com/susamn/taletty/issues/4) | Bootstrap module scaffolds and enforce coverage/journey CI gates | [#1](https://github.com/susamn/taletty/issues/1) |
| [#5](https://github.com/susamn/taletty/issues/5) | Extract Audible login/sync adapter and headless commands without behavior changes | [#4](https://github.com/susamn/taletty/issues/4), [#1](https://github.com/susamn/taletty/issues/1) |
| [#6](https://github.com/susamn/taletty/issues/6) | Extract existing licensed download/remux lifecycle unchanged | [#5](https://github.com/susamn/taletty/issues/5) |
| [#7](https://github.com/susamn/taletty/issues/7) | Add structured transfer/preparation progress and cancellation events | [#6](https://github.com/susamn/taletty/issues/6) |
| [#8](https://github.com/susamn/taletty/issues/8) | Package and tag first independently consumable engine release | [#7](https://github.com/susamn/taletty/issues/7) |
| [#9](https://github.com/susamn/taletty/issues/9) | Implement application SQLite store and schema migration foundation | [#4](https://github.com/susamn/taletty/issues/4), [#3](https://github.com/susamn/taletty/issues/3) |
| [#10](https://github.com/susamn/taletty/issues/10) | Import legacy Audible data through explicit preview and verified migration | [#9](https://github.com/susamn/taletty/issues/9), [#5](https://github.com/susamn/taletty/issues/5) |
| [#11](https://github.com/susamn/taletty/issues/11) | Implement serialized mpv adapter and local playback lifecycle | [#4](https://github.com/susamn/taletty/issues/4), [#9](https://github.com/susamn/taletty/issues/9) |
| [#12](https://github.com/susamn/taletty/issues/12) | Build responsive pane shell, focus routing and fixed miniplayer | [#2](https://github.com/susamn/taletty/issues/2), [#4](https://github.com/susamn/taletty/issues/4) |
| [#13](https://github.com/susamn/taletty/issues/13) | Connect cached library, onboarding and offline session restoration | [#12](https://github.com/susamn/taletty/issues/12), [#9](https://github.com/susamn/taletty/issues/9), [#8](https://github.com/susamn/taletty/issues/8) |
| [#14](https://github.com/susamn/taletty/issues/14) | Add library views, stable sorting and contributor filtering | [#13](https://github.com/susamn/taletty/issues/13) |
| [#15](https://github.com/susamn/taletty/issues/15) | Connect book playback, inspected chapter browsing and miniplayer actions | [#13](https://github.com/susamn/taletty/issues/13), [#11](https://github.com/susamn/taletty/issues/11) |
| [#16](https://github.com/susamn/taletty/issues/16) | Implement persistent sequential download-job coordinator | [#7](https://github.com/susamn/taletty/issues/7), [#9](https://github.com/susamn/taletty/issues/9) |
| [#17](https://github.com/susamn/taletty/issues/17) | Build download manager with real progress and safe audio deletion | [#16](https://github.com/susamn/taletty/issues/16), [#14](https://github.com/susamn/taletty/issues/14), [#15](https://github.com/susamn/taletty/issues/15) |
| [#18](https://github.com/susamn/taletty/issues/18) | Implement persistent timestamped bookmarks and targeting policy | [#9](https://github.com/susamn/taletty/issues/9), [#11](https://github.com/susamn/taletty/issues/11) |
| [#19](https://github.com/susamn/taletty/issues/19) | Add bookmark editor and manager with safe modal keyboard flow | [#18](https://github.com/susamn/taletty/issues/18), [#15](https://github.com/susamn/taletty/issues/15), [#2](https://github.com/susamn/taletty/issues/2) |
| [#20](https://github.com/susamn/taletty/issues/20) | Add completion state, smart rewind and speed-adjusted remaining time | [#15](https://github.com/susamn/taletty/issues/15), [#14](https://github.com/susamn/taletty/issues/14) |
| [#21](https://github.com/susamn/taletty/issues/21) | Add sleep timer by duration and chapter end | [#20](https://github.com/susamn/taletty/issues/20) |
| [#22](https://github.com/susamn/taletty/issues/22) | Add local audiobook import using provider-neutral identities | [#15](https://github.com/susamn/taletty/issues/15), [#10](https://github.com/susamn/taletty/issues/10) |
| [#23](https://github.com/susamn/taletty/issues/23) | Add global bookmark search and safe Markdown export | [#19](https://github.com/susamn/taletty/issues/19), [#14](https://github.com/susamn/taletty/issues/14) |
| [#24](https://github.com/susamn/taletty/issues/24) | Complete release journey evidence and install/migration audit | [#10](https://github.com/susamn/taletty/issues/10), [#17](https://github.com/susamn/taletty/issues/17), [#19](https://github.com/susamn/taletty/issues/19), [#21](https://github.com/susamn/taletty/issues/21), [#8](https://github.com/susamn/taletty/issues/8) |

## Delivery tracks

- **Decisions:** API, design and storage ADRs; then real CI/coverage infrastructure.
- **Engine:** preserve login/sync → preserve downloads → progress events → independent release.
- **Application:** persistence/migration and playback can develop alongside the UI shell.
- **Vertical slices:** cached library → browsing/listening → downloads and bookmarks → listening policies/sleep.
- **Core release:** journey and install audit after the first-release slices pass.
- **Additional slices:** local import and global notes/export have explicit blockers, but do not block the first core release.

## Not yet scheduled

macOS, desktop media controls, direct streaming, cloud sync, parallel/resumable
downloads and automatic next-book playback require separate scoped issues when
prioritized. Exact visual design remains open until the design issue is accepted.
