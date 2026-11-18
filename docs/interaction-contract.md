# Interaction contract

Status: product rules and wide/compact arrangement direction accepted;
[first-review wireframes](design/README.md) record the proposal. Exact geometry,
remaining detailed flows and key map await final approval in design issue #2.

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
No synthetic spectrum should imply a real audio visualization.

## Identity and targeting

- Selected: the cursor in a list.
- Inspected: book pinned for details/chapter inspection.
- Playing: authoritative loaded playback session, including paused audio.

Browsing never changes audio. Space targets current audio. Enter on a book
plays/resumes that book; avoid using Enter as a second implicit pause toggle.
Enter on a current-book chapter seeks without changing pause state. A chapter or
bookmark in another book explicitly loads that book at the requested position;
label the action as a switch before execution, with the final confirmation policy
settled in the design issue. Missing local audio offers download, never a fake seek
of the unrelated current book. Preserve the prior book's checkpoint on switches.

## Navigation conventions

Tab/Shift+Tab cycle content panes; numbers focus panes; j/k or arrows move;
g/G or Home/End reach boundaries; PageUp/Down page; Ctrl+u/d half-page; h/l and
Left/Right pan wide lists, except arrows in a focused player select controls.
Supporting panes have explicit back behavior and a remembered origin.

`/` filters locally. Enter applies; Esc restores the previous filter while editing;
outside editing, Esc clears an applied filter before navigating back. Keep selection
by identity through asynchronous updates; decorative headings are not selectable.

Proposed transport: Space pause/resume, comma/period seek, minus/equals volume.
Speed, chapter, sort, account and download keys must be assigned without collision
in the design issue. The prototype's h/l focus, o downloaded-only and minus/equals
speed bindings are intentionally not frozen compatibility requirements.

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
context. Resolve the manager target in the design issue rather than copying
mpdtui's surprising paused-target fallback.

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
