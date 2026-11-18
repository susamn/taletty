# Taletty design reference — #2

**Final sign-off requested:** [responsive/supporting-state contract](final-review.md)
and [complete key map](keys.md). Accepting the final-review PR closes #2; until then
production UI remains gated. No runtime feature is implemented by these documents.

**Already accepted:** W1/C1/F1 in #43, Downloads in #44, Bookmarks in #45, full-width
podmutt-style player, Details/Chapters following the highlighted book, Iosevka Slab
recommendation, MIT and `colors`/Omarchy direction. Final review consolidates exact
geometry, minimum dialogs, Settings, recovery, glyphs and all remaining bindings.

These are editable, self-contained **SVG wireframes** with exported PNG previews,
not screenshots of a working application. Open them directly in a browser or SVG editor; their text and shapes
are the source, so no hosted drawing account, build step or generated-image service
is required. All metadata is fictional. The dark palette is illustrative only;
implementation uses [semantic themes](theming.md), not these literal colors.

## Follow-up design reviews

- [Downloads — D1 view, D2 stage/confirmation alternatives](downloads.md): direction
  accepted via merged PR #44; queue/progress/recovery/safe audio deletion. Final
  compact/minimum dialog bounds and shortcuts are consolidated in R1/keys below.
- [Bookmarks — B1 editor, B2 manager, B3 action alternatives](bookmarks.md): accepted
  via merged PR #45; frozen capture, stable b/B targets and safe switches.
- [Final review — R1 minimum, S1 Settings, S2 supporting states](final-review.md):
  consolidates remaining decisions; [key map](keys.md) defines scoped input ownership.
  These supersede earlier candidate geometry/unassigned bindings on acceptance.

## W1 — Wide library/listening

![W1: wide library, selected Rivers and playing Orchard](w1-wide.png)

[Open editable W1](w1-wide.svg)

- Candidate terminal: **144 columns × 42 rows**. Sidebar 24 columns, middle 78,
  chapters 42, including each region's borders; shared-edge accounting belongs in
  implementation geometry. These are review candidates, not inherited breakpoints.
- Top content: 34 rows. Middle stacks Books (22) above Details (12). Player: 6
  rows, then one activity/error row and one contextual-hints row. Borders/padding
  are included, not extra rows. SVG pixels illustrate hierarchy, not a cell-width
  correctness proof.
- Focus is **2 Books**, identified by `[FOCUS]` as well as an accent border. The
  selected row has a `>` marker and highlight. Star has plain equivalent `[Fav]`.
- **Rivers** is highlighted; **Orchard** is playing. Details and Chapters follow the
  highlighted book automatically and retain that target while focus moves into them.
  Show book titles, not technical Selected/Inspected badges. No separate Inspect
  action. A chapter cursor does not change the highlighted library book.
- Library views: All, Continue Listening, Downloaded, Favorites and Finished.
  Downloads, Bookmarks and Settings/Help open dedicated views/overlays, not new
  permanent columns. The download count is a download queue, never a listening queue.
- `Enter` in Books plays/resumes that book; `Space` addresses loaded audio, unless
  an editor owns input. Highlighting a book updates its details/chapters, never audio.
  Late replies for an older highlighted book cannot replace the new book's content.
- Favorite action targets the highlighted book, also named in Details. It never
  falls back to current audio. Stars do not replace progress or local-download state.
- Now Playing uses its full inner width, following podmutt: book/author/chapter left,
  chapter timing right; then elapsed book time, a stretching book slider with chapter
  ticks, and duration/speed-adjusted remaining time. Controls sit left below, speed
  and volume centrally, sleep status right. The bar shows book progress (~5%), not
  the chapter's ~36%; chapter timing is explicitly labeled. No decorative spectrum.

## C1 — Compact active content above player

![C1: Favorites list over a persistent Orchard player](c1-compact.png)

[Open editable C1](c1-compact.svg)

- Candidate: **80 × 28**. Active content: 21 rows; player: 5; activity: 1; hints: 1.
  Candidate wide entry threshold is 144 columns **and** 42 rows; other usable
  sizes use compact mode. Validate intermediate widths/heights before approval.
- Keep one active content pane, with its title, filter/view and focus visible.
  Tab/Shift+Tab and numbered focus move to other panes; they do not discard state.
  Avoid a second independent navigation model just for narrow terminals.
- Preserve focus identity, highlighted book and its chapter target, scroll origin and editor
  input when resizing. Resizing never starts/stops audio or abandons a modal.
- Player uses the same width-filling book slider between elapsed and duration stamps.
  Identity/chapter sits above it; transport left, speed center and volume/sleep right
  below it. Omit author/adjusted remaining time before hiding identity or transport.
  Activity and hints remain separate rows.
- The SVG intentionally shows a cached offline Favorites view. Toggling a local
  favorite needs no provider call. An empty Favorites view offers a way back to
  All rather than looking like login or download failure.
- **80 × 20** and bounded dialogs are specified in the [R1 final review](final-review.md).
  Below minimum show a safe resize/quit view; preserve audio and modal input.

## F1 — Continue, browse, explicitly switch

![F1: launch, resume Orchard, highlight Rivers, explicitly switch and return](f1-main-flow.png)

[Open editable F1](f1-main-flow.svg)

| Step | Input intent | Required state / observation |
|---|---|---|
| F1.1 | Launch, including offline | Restore view/selection, never autoplay; no old-app discovery |
| F1.2 | Play/resume Orchard explicitly | Current audio becomes Orchard at saved position/speed |
| F1.3 | Highlight Rivers; favorite it | Details/chapters follow Rivers; Orchard audio remains unchanged |
| F1.4 | Activate Rivers chapter | B3 Stay / Switch and play confirmation; no switch until confirmed |
| F1.4a | No completed local audio | Offer download; never seek Orchard or falsely report success |
| F1.5 | Commit switch | Preserve Orchard checkpoint; load Rivers at chapter, no smart rewind |
| F1.6 | Return to Orchard and explicitly play | Resume its checkpoint; merely highlighting it does not switch |

A failed load must be truthful, preserve the prior checkpoint and expose recovery.
Do not promise the old process/audio can always be transactionally restored after
a real player failure. Cancel before committing a switch leaves current audio alone.

## Final review, then implementation

Review [R1/S1/S2 and the acceptance handoff](final-review.md) together with the
[full scoped key map](keys.md). They settle all remaining #2 design decisions in
one PR; diagrams marked candidate retain historical context, not competing rules.
The [interaction contract](../interaction-contract.md) remains the shared behavior
boundary. On final approval/merge, implementation proceeds through existing blocked-by
relationships with real tests. No additional planning-only design PR is scheduled.

## Typography / preview export

Use the recommended [Iosevka Slab terminal profile](typography.md) for a bookish
monospace appearance; the application never changes terminal fonts. PNGs were
exported using optional local `resvg-py 0.5.0` with Iosevka Slab v34.9.1 regular/bold
explicitly supplied (this environment's system-font discovery is incomplete).
Font files were downloaded for local preview only, not bundled in this repository.
SVGs request Iosevka Slab with a monospace fallback; PNGs preserve the actual preview.
This is not an application/CI dependency. Edit the SVG sources, then export each
at its declared viewBox size using an SVG renderer with an available monospace font.
Review the PNGs after text or geometry changes; never accept an empty-text render.
CairoSVG could not run here because native libcairo is absent; resvg exports were
used instead. Native terminal-cell tests still belong to implementation.

## Test handoff (requirements, not implemented tests)

| Journeys | Design assertions to carry into implementation |
|---|---|
| J01/J02 | No startup autoplay; cached views and local audio work offline |
| J03 | Keep selected BookID through filter/sort/refresh, including Favorites |
| J04–J06 | [D1/D2 proposal](downloads.md): distinguish transfer/preparation/errors; no fake Ready |
| J07/J08 | Highlighting updates details/chapters, never audio; stale replies ignored; explicit switch checkpoints; pause-aware seeks |
| J09 | [B1–B3 proposal](bookmarks.md): frozen capture/manager target, input ownership, recovery, confirmed switches |
| J10 | [S2 timer policy](final-review.md): fake-clock expiry, pause/seek/switch/quit behavior |
| J11/J12 | Completion/deleting audio does not remove favorites, bookmarks or history |
| J13 | Every pane fits terminal-cell bounds; player visible; resize/back restores focus |
| J17 (#42) | Favorite Rivers while Orchard plays; restart offline; unfavorite final visible row safely |

J17 is a newly planned journey for #42, not existing executable coverage. Unicode,
CJK/combining widths, long or absent metadata, terminal escapes, unknown durations,
no chapters and plain-text mode must be tested at actual terminal-cell boundaries.
The SVG cannot establish those properties. Runtime coverage for this design slice
is **N/A**; implementation keeps independent ≥95% coverage and full journey gates.
