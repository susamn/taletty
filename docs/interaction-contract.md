# Interaction contract

Status: W1/C1/F1, Downloads and Bookmarks direction accepted via #43–#45. The
[final #2 sign-off](design/final-review.md) consolidates responsive geometry,
remaining states and the [complete key map](design/keys.md). Until that review is
approved/merged, #2 gates UI implementation; these docs do not claim runtime features.

## Shared language

Bordered numbered panes, focus-colored borders, selected-row highlighting,
contextual bottom hints, a fixed activity/error row, and centered themed dialogs.
Text must remain readable without Nerd Fonts or color. All glyph states have text
or plain-mode equivalents. Sanitize remote metadata, ANSI/control sequences and
exported filenames. Measure terminal-cell width, not bytes or rune counts.

## Responsive composition

Wide mode should expose Library, Chapters and Details around a persistent player.
Downloads and bookmarks use dedicated views/overlays rather than overcrowding it.
Compact mode shows the active content pane above the player. Artwork, decoration
and whitespace yield before identity, controls or actionable errors. Set exact
breakpoints and minimum dimensions in reviewed mockups, then share geometry among
rendering, scrolling, paging, artwork and tests. Do not inherit arbitrary breakpoints
from podmutt without validating audiobook content.

The player always represents current audio, not the highlighted library book.
Show title, chapter, progress, speed, remaining time and transport as space permits.
Use the panel's full inner width, like podmutt: identity/chapter row, elapsed time
and a width-filling book progress slider with duration/remaining time, then transport
and settings. Distribute controls and secondary status across the bottom row rather
than clustering every item at the left. Chapter ticks may mark the book slider;
label chapter timing separately so book and chapter progress cannot be confused.
Compact mode keeps the width-filling slider and sacrifices secondary timing first.
No synthetic spectrum should imply a real audio visualization.

Typography recommendation: [Iosevka Slab](design/typography.md), controlled by an
optional terminal profile, never silently installed or switched by the application.

## Identity and targeting

- Selected: the cursor in a list.
- Details/Chapters target: follows the highlighted library book; no separate Inspect
  action or pinned inspection mode. Keep that book stable while focus moves into
  its chapters/details. These panes show its title, not an "Inspected" state label.
- Playing: authoritative loaded playback session, including paused audio.

The UI uses a cursor/highlight for selection and a Playing/Paused state for current
audio; do not display technical "Selected / Inspected" badges. Late chapter/detail
replies must match the current highlighted BookID, never overwrite a newer choice.

Browsing never changes audio. Space targets current audio. Enter on a book
plays/resumes that book; avoid using Enter as a second implicit pause toggle.
Enter on a current-book chapter seeks without changing pause state. A chapter or
bookmark in another book explicitly loads that book at the requested position;
use the named Stay / Switch and play confirmation from B3 before cross-book
chapter/bookmark activation. Missing local audio offers download, never a fake seek
of the unrelated current book. Preserve the prior book's checkpoint on switches.

## Navigation conventions

Tab/Shift+Tab cycle content panes; numbers focus panes; j/k or arrows move;
g/G or Home/End reach boundaries; PageUp/Down page; Ctrl+u/d half-page; h/l and
Left/Right pan wide lists, except arrows in a focused player select controls.
Supporting panes have explicit back behavior and a remembered origin.

`/` filters locally. Enter applies; Esc restores the previous filter while editing;
outside editing, Esc clears an applied filter before navigating back. Keep selection
by identity through asynchronous updates; decorative headings are not selectable.

The [scoped key map](design/keys.md) assigns all root/view/editor bindings: Space
pause/resume, comma/period seek, minus/equals volume, brackets speed, f favorites,
s sort, d enqueue, D Downloads, x context actions, t timer, a account and S Settings.
No prototype h/l focus, o downloaded-only or minus/equals speed compatibility aliases.
Editors/menus own keys first; unavailable context actions never fall back silently.

## Modal precedence

1. Active application dialog/editor/confirmation owns its keys.
2. Presentation overlays, filters, help and settings own their keys.
3. Contextual actions.
4. Default navigation.

Typing must not trigger transport, focus, delete or quit shortcuts. Read-only
overlays may deliberately retain transport, but this is an explicit per-overlay
policy. Esc from add/edit/confirmation returns to its parent view; closing the
parent restores prior focus. Playback changes cannot steal focus from an editor.

## Bookmarks

Lowercase b captures the loaded current book's position immediately, including
paused playback; uppercase B opens a clearly labeled manager for its explicit
context: highlighted book from Library/Details/Chapters, loaded current book from
the focused Player, including paused audio. Manager target stays fixed until closed;
no paused-versus-playing fallback. [B1–B3](design/bookmarks.md) records frozen capture,
editor/error recovery and default-safe chapter/bookmark switch confirmation.

Store absolute position, book identity, note, chapter context and timestamps.
Optional two-second pre-roll is clamped at zero. Do not apply a second smart rewind
when jumping. Enter/Ctrl+S save, Alt+Enter inserts newline, Esc cancels; empty notes
are not saved. Edit changes text, deletion confirms, and errors retain recoverable
input. Global search/export are later slices, not hidden behind MVP completion.

## Downloads and failures

Never conflate download queue with listening queue. Show downloaded, queued,
active transfer, preparing and failed distinctly. Known totals permit determinate
progress; unknown totals remain indeterminate. Separate stage and total/job progress.
Blocking account operations explain why an action must wait. Failures remain
inspectable and do not disappear behind unrelated background success. Retry is
explicit for entitlement/throttling failures. Audio deletion confirms and preserves
local metadata. No network is needed to browse cached data or play completed files.
