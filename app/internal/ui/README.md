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
  margins; at 80×20 its maximum is 76×11. Modal behavior is still #49.
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
Modal/other supporting-view ownership, draft recovery and PTY integration remain #49.

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
Help itself owns no unsaved draft. Modal draft warnings belong to #53.

This is the first #49 slice: #52 → #53 (modal/draft ownership) → #54 (real PTY and
shell acceptance audit). No domain editor/download/bookmark workflow is implied.

## Evidence

[`shell_test.go`](shell_test.go) drives real Init/Update/Cmd through the existing
[runner](../testkit/README.md): browse B while A plays; explicit action targets and
source sessions; completion-driven repaint; refresh identity/isolation; sticky error
and dismissal; focus/resize/recovery; safe long metadata; named states and glyphs.
[`help_test.go`](help_test.go) adds real model/command journeys for exclusive Help
ownership from every pane, transport/session whitelisting, blocked quit/focus keys,
scroll bounds, long safe errors, resize/repaint and failed-shutdown recovery.
Pure geometry tests include the boundary grid and extreme integer dimensions;
actual rendered views are checked at supported/minimum/tiny sizes in terminal cells.
This is partial J13 (and the browse-target portion of J07), not complete app journeys.
There is no persistence, live media, actual terminal key decoding/restoration or PTY
coverage claimed until those implementations land.

The reviewed [80×20 plain fixture](testdata/shell-minimum-plain.golden) supplements,
not replaces, cell/semantic assertions. From app/ deliberately regenerate with:

```sh
GOWORK=off go test ./internal/ui -run TestShellMinimumPlainGolden -update-shell-golden
```

Review the text fixture diff; trailing padding is normalized only for the golden.
Separate per-line cell-width assertions still enforce the full 80-column budget.
