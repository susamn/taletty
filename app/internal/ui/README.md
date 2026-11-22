# Terminal presentation

#47 supplies shared terminal-cell geometry/text; #48 adds a real Bubble Tea v2
shell Model, not a standalone player or provider coordinator. The approved
[R1 contract](../../../docs/design/final-review.md) is authoritative.

## Geometry and text

- Wide requires both 144 columns and 42 rows: 24-column Library, 42-column Chapters,
  remaining middle width; Books uses floor(2/3 content height), Details the remainder.
- Wide player owns six full-width rows, compact five. Activity and hints own one
  row each; all borders/padding fit inside their allocations.
- Compact starts at 80×20 with one active content pane. Player focus keeps the last
  content pane visible instead of duplicating the player. Below minimum, the resize
  owner hides content while retaining focus, highlighted identity and current audio.
- Dialog geometry centers inside content with two-column side/one-row vertical
  margins; at 80×20 its maximum is 76×11. #53 uses this for the note editor and
  named confirmation components, without covering the persistent player.
- ClipLine/PadLine strip untrusted ANSI and flatten controls before measuring with
  the existing ANSI/grapheme library. Pass raw metadata, not styled renderer output.
  Borders/styles are added only after fitting raw text to its allocated cells.

## Coordinator contract

`NewModel(SnapshotMsg, dispatch)` accepts injected display data. Book.Key is a unique,
nonempty opaque identity supplied by the coordinator; this projection does not
validate provider IDs, discover files or duplicate engine models. The app remains
independent of the unpublished engine module. Snapshots clone book/chapter slices;
callers may reuse them after Update returns, not mutate concurrently during delivery.

The coordinator serializes authoritative snapshots and rejects stale service replies
**before** sending them to this shell; SnapshotMsg is not an unordered provider reply.
Selection survives snapshot reordering by key; removal falls back to the first row.
Details/Chapters follow that highlighted book, not the independent current player.

Dispatch creates a nonblocking `tea.Cmd` for a typed Action. Book Enter requests its
explicit highlighted target; transport requests carry the current key **and source
session**, including across same-book reloads. The coordinator must validate the
session/capability, offer missing-audio download or required switch confirmation,
and manage cancellation/processes/checkpoints. UI request emission is not successful
playback: no optimistic state, synthetic clock advance or provider calls in View.
Quit also requests coordinator shutdown, never bypasses cleanup with a direct Quit.

Only playing/paused snapshots with a key/session enable transport; nil dispatch is
explicitly disabled. Playback state/duration/speed/volume are displayed truthfully;
unknown duration has no determinate marker. AppearanceMsg injects semantic palette
colors and Unicode/plain mode without changing focus/audio. Nil colors inherit the
terminal. File loading, glyph Settings/Nerd opt-in and SIGUSR1 integration remain #34.

Errors from snapshots/NoticeMsg remain sticky through unrelated success. Only an
explicit DismissFailureMsg acknowledges them. The coordinator supplies redacted
errors; terminal-control stripping cannot redact secrets from arbitrary text.

Implemented keys: root Tab/Shift+Tab, 1–5, Books j/k/arrows, Enter in Books/Details/
Player, Player Left/Right, current-audio Space/comma/period/volume/speed and shutdown
requests, plus `?` for scrollable Help. Library destinations, filtering/sorting/panning, detailed chapter navigation
and domain actions ship with their owning features; hints do not advertise them as
working. There is no executable demo, mpv, filesystem/config initialization or artwork.
The coordinator-opened modal components below supply draft ownership/recovery;
actual feature entry points remain separate work. #54 adds a real production
[terminal lifecycle boundary](../terminal/run_linux.go) and controlling-PTY tests,
not an application/domain coordinator.

## Help ownership (#52)

Help replaces only the content region; the player/activity/error/hints rows remain.
It is the sole visible focus owner, even when opened from Player, and preserves the
underlying root pane, highlighted book and player-control selection. Esc restores
that origin. j/k/arrows, g/G/Home/End, paging and Ctrl+u/d scroll Help locally.
Tab, numbers, q/Ctrl+C, b/B and context actions are consumed, not redispatched to
root. Only the same seven explicit current-audio transport keys pass through once;
completion updates the authoritative player snapshot without closing Help.

Context rows distinguish origin bindings (usable only after Esc), disabled requests
and unimplemented features. Sticky error details wrap after control stripping and
remain fully scrollable without acknowledging the error. Reflow clamps the visible
page but preserves the stored reading offset. Below minimum the resize/quit owner
supersedes hidden Help: no hidden navigation/transport, q/Ctrl+C requests orderly
shutdown; recovery, including shutdown failure, restores Help and its root origin.
Help itself owns no unsaved draft. A nested note editor uses #53's draft warning.

This is the first #49 slice: #52 → #53 (modal/draft ownership) → #54 (real PTY and
shell acceptance audit). No domain editor/download/bookmark workflow is implied.

## Bounded note and confirmation components (#53)

`OpenNoteEditorMsg` opens the real Bubbles v2 textarea, not a simulated text field.
The coordinator supplies a unique opening ID and frozen `NoteTarget`: book identity,
optional existing bookmark identity, title/chapter, captured position and already
computed absolute jump position. The component never recaptures, computes pre-roll,
reads a repository or claims it saved a bookmark. `b/B` and feature-service wiring
remain #18/#19; there is no new root shortcut or standalone runnable app.

Enter/Ctrl+S requests `SaveNote`; Alt+Enter inserts a newline. Tab cycles the field,
Save and Cancel; Esc/Cancel returns to the exact parent. The field owns Space, q,
numbers, navigation letters and Ctrl+C. Bracketed paste is sanitized as plain text
with newlines retained; clipboard subprocess shortcuts are disabled. The textarea
supplies editing, wrapping, a four-row scrolled viewport and a real terminal cursor.
An existing note that cannot round-trip through the widget's logical-line limit is
rejected visibly rather than opening a truncated draft that could overwrite it.
The shell supplies semantic colors and bounds, not a second text-editing engine.

`OpenConfirmationMsg` freezes a named `Action` and explanatory Keep/Confirm labels.
It defaults to Keep/Stay, owns all input, scrolls long explanations with Up/Down,
and activates only the focused choice with Enter. Its body includes full button
labels even when the bounded button row ellipsizes. It can be a child of an editor;
closing it restores the editor's exact local focus/draft. Both components can sit
above Help; no hidden Help/root transport or focus key is redispatched.

Submission carries `RequestID` and an incrementing `Attempt`. Only matching pending
`ModalResultMsg` closes a layer on success; failure retains target/input and enables
retry. The coordinator supplies sanitized errors and sends success **only after**
the authoritative operation completes. Duplicate submits are suppressed. While
pending, keys (including Esc) are held: cancellation cannot promise to undo a write
that already committed. The coordinator must validate stale sessions/removed
identities before executing confirmations and reconcile late domain results.
Opening IDs must be unique across canceled/reopened components; an active component
cannot be silently replaced, and pending work cannot be hidden by a new child.

Snapshots, palette changes and resize retain the field, local focus and frozen
identities. Unsupported sizes hide the entire owner stack, suppress typing/paste
and transport, and remove the cursor. With a note open, q/Ctrl+C first warns;
q after that warning requests orderly quit, while Esc retains the draft. Returning
to supported geometry disarms the warning and restores the modal; failed shutdown
also retains it. The warning is cell-clipped even at one-column/zero-sized terminals.

### Supporting-view transport contract

These are the approved integration rules, **not implemented domain destinations**:

| Owner | Current-audio passthrough | Other ownership |
|---|---|---|
| Downloads / read-only Bookmarks | Seven transport keys (Space, comma, period, minus, equals, brackets) and b capture | Tab cycles local areas/Player; numbers explicitly return to root; note scroller owns navigation |
| Help (implemented) | Same seven transport keys only | All other keys local/blocked; Esc returns to origin |
| Settings / account / action menus / confirmations / filter editors | None | Local navigation/actions only, Esc one layer back |
| Note editor (implemented) | None | Text/local buttons own input; Ctrl+C is not global quit |
| Below-minimum resize owner (implemented) | None | Resize/quit warning/recovery supersede hidden owners |

Background audio continues in every case. Feature coordinators must integrate these
rules without inventing account/download/bookmark services in the presentation layer.

## Evidence

[`shell_test.go`](shell_test.go) drives real Init/Update/Cmd through the existing
[runner](../testkit/README.md): browse B while A plays; explicit action targets and
source sessions; completion-driven repaint; refresh identity/isolation; sticky error
and dismissal; focus/resize/recovery; safe long metadata; named states and glyphs.
[`help_test.go`](help_test.go) adds real model/command journeys for exclusive Help
ownership from every pane, transport/session whitelisting, blocked quit/focus keys,
scroll bounds, long safe errors, resize/repaint and failed-shutdown recovery.
[`modal_test.go`](modal_test.go) exercises actual field input, bracketed paste,
local buttons, default-safe/nested confirmations, frozen submissions, pending and
stale replies, failed-submit/retry, one-layer close, theme/snapshot preservation,
long Unicode scrolling, cell bounds and two-step tiny-terminal quit recovery.
[`bookmarks_test.go`](bookmarks_test.go) exercises `b`/`B` explicit targeting,
the dedicated Bookmarks Manager view, multiline note scrolling, local `/`
filtering, row action menus with frozen IDs and stale-target invalidation,
query-failure retry, and `Tab`/`1-5`/`Esc` focus restoration.
[`favorites_test.go`](favorites_test.go) exercises the `Favorites` library
destination, `f` highlighted-book targeting (unavailable in `PlayerPane`),
accent-tinted `★` / `[Fav]` indicators in `Books` and `Details`, empty
`Favorites` guidance, and `RestoreSessionMsg` restoration.
Pure geometry tests include the boundary grid and extreme integer dimensions;
actual rendered views are checked at supported/minimum/tiny sizes in terminal cells.
This is partial J13 (and the browse-target portion of J07), not complete app journeys.
[terminal_linux_test.go](terminal_linux_test.go) now runs the actual model inside
Bubble Tea on controlling PTYs, including real key/paste decoding, resize, nested
Back, draft/save failure/retry, quit recovery, cancellation/SIGINT, writer failure,
exact termios/foreground/file-status restoration and bounded child cleanup. The
same journeys repeat three times in each app CI Go version and cannot be skipped
by an integration environment flag. See the [complete shell acceptance audit](../../../docs/shell-acceptance.md)
for evidence and explicit deferred requirements. There is still no persistence,
live media, complete domain J13 or usable audiobook application claim.

The reviewed [80×20 plain fixture](testdata/shell-minimum-plain.golden) supplements,
not replaces, cell/semantic assertions. From app/ deliberately regenerate with:

```sh
GOWORK=off go test ./internal/ui -run TestShellMinimumPlainGolden -update-shell-golden
```

Review the text fixture diff; trailing padding is normalized only for the golden.
Separate per-line cell-width assertions still enforce the full 80-column budget.
