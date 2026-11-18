# Audible and audiobook player feature map

Status: planned Taletty behavior. Prototype behavior is reference evidence, not a
claim that Taletty implements it. #31 audits this map before core release #24.

## First-release capability stories

| Capability | Story | User-journey evidence |
|---|---|---|
| US browser sign-in/MFA handoff, private tokens and refresh | #5, #13 | J01; fake-provider tests, private live validation |
| Cached library, safe pagination, offline browsing | #5, #13 | J01/J02 |
| Contributor filtering, views, stable sorting and selection | #14 | J03 |
| Persistent local favorites and Favorites library view | #42 | J17 |
| Entitled download, license refusals, chapter-preserving remux | #6 | J04/J06; generated-media remux + private live validation |
| Honest byte progress, preparing stage and terminal outcomes | #7, #17 | J04–J06 |
| Persistent sequential jobs, cancel/retry/reorder | #16, #17 | J05/J06 |
| Downloaded-file status and safe audio deletion | #14, #17 | J04/J12 |
| Owned mpv playback, cleanup and local offline resume | #11, #15 | J02/J07/J08 plus real null-output mpv |
| Chapters, paused seeking and explicit cross-book targeting | #15 | J07/J08 |
| Responsive fixed miniplayer, transport, speed and volume | #12, #15 | J08/J13 |
| Per-book speed, completion and speed-adjusted time remaining | #20 | J08/J11 |
| Smart rewind with no extra rewind on explicit seeks | #20 | J08/J11 fake-clock policy |
| Timestamped notes/bookmarks while playing or paused | #18, #19 | J09 |
| Sleep timer by duration or chapter end | #21 | J10 |
| Fresh private `~/.config/taletty/` setup, persistence and safe upgrades | #10 | J14 |

Persist local state by stable book identity, not display title/path. Inspecting a
book must not replace current audio. UI actions and future integrations use one
playback authority. Bookmark timestamp is captured when invoked, not after typing.
Keep the pause state for seeks within the loaded book. Exact keys, target labels
and cross-book confirmation are recorded in the [final #2 design review](design/final-review.md)
and [key map](design/keys.md), not inferred from prototype bindings.

## Additional explicitly tracked features

| Capability | Story | Scope |
|---|---|---|
| Local m4b/mp3/folder import | #22 | Later slice; ownership/duplicate rules reviewed before implementation |
| Global bookmark search and Markdown export | #23 | Later slice; correct book target and safe output |
| Linux MPRIS, later macOS desktop media controls | #30 | Later platform-specific child tasks; same playback authority |

## Audible limitations and uncommitted extensions

- **Local progress, completion, speed, notes and bookmarks are not Audible/
  Whispersync state.** No cloud synchronization or provider-rating writes promised.
- US is the initial marketplace. Other marketplaces require explicit locale/login/
  entitlement coverage; multi-account switching is also separate future work.
- Download-first, no direct Audible network streaming in the initial release.
- Podcast/series expansion and multipart audiobook stitching are unsupported until
  separately scoped and verified. Surface limitations rather than hiding missing parts.
- Do not purchase titles, bypass entitlement, or automatically retry throttling.
- Parallel/resumable transfers and automatic next-book playback are not committed.
- Cover art/richer descriptions depend on safe provider metadata and reviewed design;
  no unsupported API fields or invented acquisition timestamps.

#31 must turn any newly identified release-essential gap into a scoped dependent
issue, or explicitly record it as unsupported. It must not silently grow into a
mega-PR implementing every possible Audible feature.

## Release validation

Generated audio and fake-provider tests run publicly, without protected media or
credentials. Live login/library/download checks are manual/private and redacted.
Successful login or library sync does not prove a title can be downloaded; license
and updated device registration behavior must be validated before release claims.
Every supported feature needs test links in the journey evidence matrix, independent
>=95% module/adapter coverage, and failure/offline/cancellation behavior where relevant.
