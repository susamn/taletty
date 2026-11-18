# Final-review key map

Status: proposed final sign-off for #2. These bindings supersede earlier unassigned
shortcuts on acceptance, not a claim that handlers exist. Lower/uppercase are distinct.

## Ownership first

Dispatch once: active editor/confirmation/menu → supporting view's local rules →
context action → root navigation. Never dispatch a consumed key again. Text input
owns Space, letters, punctuation, numbers and navigation/edit keys; none also act on
playback, favorites, focus or quit. Ctrl+C is **not** an app-global escape hatch from
an editor/dialog: that owner handles/ignores it locally. Esc is the explicit back key.

Read-only Downloads and Bookmarks permit current-audio transport and `b` capture;
Help permits transport only. Settings/account/menus/confirmations/filter editors
permit **no** transport passthrough. Background playback itself continues throughout.
Inside the Bookmarks note scroller, navigation scrolls text, not the bookmark cursor.
Tab in Downloads/Bookmarks cycles local list/details/actions and Player, not hidden
Library panes; numeric focus explicitly returns to root. Settings/Help own Tab
locally. Closing children restores parent focus/selection; Esc unwinds one layer.
Below minimum the dedicated resize/quit owner supersedes the hidden modal, with
unsaved-draft confirmation before quitting (see R1).

## Root / read-only content (unless owned above)

| Keys | Action / target |
|---|---|
| Tab / Shift+Tab | Cycle Library, Books, Details, Chapters, Player in that order; compact exposes the focused pane |
| 1 / 2 / 3 / 4 / 5 | Focus those same panes; supporting view closes/restores its origin before explicit root focus |
| j/k or Up/Down | Move focused list/scroll viewport; no selection of headings |
| g/G or Home/End | First/last selectable row; PageUp/Down page; Ctrl+u/d half-page |
| h/l or Left/Right | Pan wide text; in focused Player, choose previous/next control instead |
| Enter | Library sidebar: open destination; Books/Details: play/resume highlighted book; Chapters/Bookmarks: seek or cross-book confirmation; Player: activate focused control; Downloads: row action menu |
| Space | Toggle loaded current audio, never highlighted unrelated book; unavailable without a loaded session |
| comma / period | Seek current audio −15s / +15s, clamped to valid bounds; preserve paused state |
| minus / equals | Decrease/increase current volume by 5 percentage points (0–100); no legacy speed alias |
| [ / ] | Decrease/increase current playback speed by 0.1x within supported bounds; not a chapter or pan shortcut |
| b | Capture loaded current audio now, including paused; unavailable without it |
| B | Library context: highlighted book's manager; Player context: loaded book even paused; explicit named target stays fixed |
| / | Begin local filter in Library/Bookmarks/Downloads; unavailable in Player/Details; never invoke provider search |
| f | Toggle favorite for highlighted Library book (also named in Details/Chapters), never fall back to playing book |
| s | Library sort menu: title, author, duration, recent listening, available acquisition order/date; remember selection by BookID |
| d | Queue highlighted Library book if not Ready/already queued/active; otherwise explain status, never duplicate |
| D | Open Downloads; remember origin; does not enqueue anything |
| x | Context action menu for named book/job/bookmark: includes appropriate edit/delete/retry/completion actions |
| t | Current-audio sleep timer menu; explicit target and reset-on-switch/quit notice |
| a | Account menu: browser login, sync, doctor and explicit sign-out; explain busy auth operations |
| S | Settings (including theme); no font-family control inside Taletty |
| ? | Read-only Help with actual context hints; shortcuts for unavailable actions are labeled disabled |
| Esc | Cancel filter edit, else clear applied filter, else close child/return to origin; root with nothing to clear is no-op |
| q / Ctrl+C | At root/read-only content: orderly quit; stop/checkpoint/reap workers, persist waiting jobs; errors stay recoverable, no silent data loss |

`f/s/d` apply only in the Library book context; they are unavailable in other views,
not overloaded with hidden meanings. `B` in Downloads/Help/Settings is unavailable;
use a named-book action or Library/Player. In an open manager, B does not retarget.
Global bookmark search/export remain later features with no reserved MVP bindings.

## Local owners

| Owner | Keys / behavior |
|---|---|
| Filter | Type query; Enter applies **only** (never also plays row); Esc restores prior filter; all other keys belong to field |
| Bookmark editor | Enter/Ctrl+S save, Alt+Enter newline, Esc cancel; failed save retains input; Tab cycles local field/buttons |
| Action menu | j/k/arrows move, Enter chooses, Esc backs out; x in Bookmarks opens Jump/Edit/Delete, x in Downloads state-appropriate actions |
| Downloads actions | Queued: Move up/down or Remove; active: Cancel; failed/canceled: Retry; Ready: Show in Library or Delete audio; no alternate destructive letter shortcuts |
| Confirmation | Tab/Shift+Tab or Left/Right choose buttons; Enter activates; Esc cancels; initially Keep/Stay; no global q/Space transport |
| Settings | j/k/arrows select; Tab cycles local sections/actions; Enter applies item or opens its field/menu, Esc back; saved value remains authoritative on write failure |
| Help | Navigation scrolls; Esc closes; only transport whitelist passes through (not b/B/f/q or pane shortcuts) |

Switch confirmation is required for cross-book **chapter/bookmark** jumps; direct
Enter on a Library book is already an explicit play/resume action. Same-book seeks
preserve pause. Missing audio offers download, never deferred autoplay. For any open
action/dialog, background changes cannot replace target IDs; invalidate stale source
sessions or removed targets before execution. Actual Alt+Enter decoding and all
ownership combinations require PTY + real-model journey tests, not just this table.
