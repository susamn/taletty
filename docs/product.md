# Product brief

## Product promise

Taletty is a keyboard-first, offline-first audiobook companion. Browsing, reading
notes, and managing downloads must not interrupt the current audio. It adopts
podmutt's bordered-pane/miniplayer language and mpdtui's timestamped-note workflow,
not their domain-specific targeting quirks.

## Accepted direction

- One repository with independently versioned engine and app Go modules.
- Preserve the working Audible login, license, download, and remux behavior before
  extending it. Existing audible-tui source is the behavioral baseline, not a
  binary dependency or source of credentials.
- Audible first, provider-neutral book identities. Local imports are a later slice.
- Linux first; no macOS support claim until its lifecycle/media paths are tested.
- Restore view, selection, sorting, and filters. Expose Continue Listening.
  Never autoplay at startup.
- Download-first playback. No direct Audible streaming in the initial release.
- Local progress and notes; no promise of Audible/Whispersync synchronization.
- Unicode default, optional Nerd Font mode, plain-text fallback.
- MIT license. Podmutt-style file palettes: default `colors`, Omarchy integration,
  and alternative themes in Settings; see [theme direction](design/theming.md).
- Wide/compact arrangement direction approved; [first-review wireframes](design/README.md)
  record it. Exact geometry, typography, detailed flows and key map still need approval.

## First release

### Library

Cached browsing without login/network; search title, subtitle, author, narrator;
explicit All / Continue Listening / Downloaded / Favorites / Finished views; stable selection
through filtering and refresh; sorting by title, author, duration, recent listening,
and acquisition date when actually available. Never invent unavailable metadata.
Keep acquisition order when the provider exposes no timestamp.

Mark/unmark favorite books locally, with a visible star/plain-text indicator and a
Favorites view. Favorite state survives restart, refresh, completion and downloaded
audio deletion; it is independent of timestamped bookmarks and never changes current
playback. No Audible-cloud favorite synchronization is promised (implementation #42).

### Downloads

One active job. Explicit Queued, Downloading, Preparing, Ready, Failed, and Canceled
states. Real bytes/total/speed when known; indeterminate display when totals are
unknown. Conversion progress must not be mislabeled as network progress.
Persist waiting jobs and allow cancellation, retry and reordering. Store media in
`~/.config/taletty/downloads/`. Remove local audio only after confirmation; preserve notes/history.
Initial canceled transfers restart from scratch, matching the prototype.

### Listening

Persistent player; local per-book resume/speed; configurable default speed;
volume; chapter and book time remaining adjusted for speed; finished/unfinished;
sleep timer by time or chapter end. Smart rewind is a separate, testable policy,
never applied to deliberate chapter/bookmark seeks.

### Chapters and notes

Keep chapter browsing and jumping. Permit inspecting another downloaded book's
chapters without replacing current audio. Clearly distinguish selected, inspected,
and playing books. A seek into a different book is an explicit playback switch.
Bookmarks save position, book identity, chapter context, and a note. Support
paused creation, configurable pre-roll, edit/delete/jump, global search, and
Markdown export as separate delivery slices.

## Later / out of initial scope

Direct streaming, cloud progress/bookmark synchronization, purchasing, multi-account
switching, resumable byte-range transfers, simultaneous downloads, automatic next-book
playback, desktop media integration, and macOS distribution. The engine boundary
must not prevent these, but speculative implementations are not required.
Local audiobook import follows the first Audible vertical slice.

## Data directory and security

Taletty starts fresh in **`~/.config/taletty/`**. Configuration, credentials, cached
library metadata, application database/progress and downloaded audio all live under
this root; audio lives in `downloads/`. Do not discover, import, copy, reuse or modify
`~/.config/audible-tui/` or any other app's files. No legacy-data migration is required.
Users sign in through Taletty's own account flow. Credentials and audio are private
runtime data, not fixtures. Engine/app ownership boundaries still apply within the
shared Taletty root. Taletty's own future database upgrades must preserve its data.

## Prototype inventory and known limits

The baseline contains Go CLI/UI/storage/mpv code and an embedded Python adapter.
It uses JSON caches, one authenticated-operation mutex, sequential downloads,
local `.m4b` files, one-second mpv snapshots, five-second checkpoints, and US login.
Chapters are flattened to leaves during remux and currently read only after mpv loads.
The bridge emits one final JSON result, not streaming progress. No bookmarks,
completion model, persistent download queue, or sleep timer exist yet.
The supplied checkout lacks `scripts/setup.sh` and the Python tests mentioned by its
README/Makefile. Restore these intentionally; do not claim the cited tests ran.
