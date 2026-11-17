# Architecture and module boundaries

Status: accepted direction; [ADR 001](adr/001-engine-api-and-bridge-contract.md)
proposes the concrete API/protocol and bootstrap release strategy for review.

## Repository layout (target, not implemented)

```text
engine/                         independent Go module
  provider-neutral contracts    identity, books, chapters, operations, events
  Audible adapter               Python process + typed versioned protocol
  bridge/                       pinned Python dependencies and adapter tests
  cmd/taletty-engine/            headless login/sync/download/doctor
app/                            independent Go module
  cmd/taletty/                   user CLI and terminal application
  internal/app/                 coordination and asynchronous workers
  internal/ui/                  pure presentation and action contracts
  internal/playback/            serialized playback, timer and resume policies
  internal/player/              owned mpv process and IPC
  internal/store/               local history, notes, views, job metadata
```

Use separate `go.mod` files and module-scoped release tags (`engine/vX.Y.Z`,
`app/vX.Y.Z`). The app pins an engine version. A local workspace may connect
modules during development, but CI must also validate the pinned released-engine
path. Before the engine's first release, document the temporary bootstrap linkage;
never leave an unpublished local replace as the release strategy.

## Responsibilities

### Engine

Own provider authentication, token refresh, provider calls, entitlement/license
handling, media download/preparation and provider metadata conversion. Provide
headless operation through both a typed Go API and CLI. No Bubble Tea, Lip Gloss,
terminal widgets, user-history database, or mpv dependencies.

The Go engine owns a pinned Python bridge runtime contract. Keep the existing
unofficial `audible` integration; a Go rewrite is not a prerequisite. The adapter
is independently testable, but its distribution initially follows the engine.

### Application

Own user journeys, local persistence, presentation, artwork, playback, bookmarking,
sleep policy, filtering/sorting, and download-job presentation. It consumes typed
engine events and results, not Python stdout fragments or raw Audible JSON.

### Playback

Use one serialized authority for playback commands, state and checkpoints. UI,
future CLI control and desktop media integrations must share it. Keep mpv behind
an interface. Guarantee final checkpoint, orderly IPC closure/process reaping and
bounded forced cleanup. Parent crash must not leave orphan audio.

### Storage

Provider auth storage remains engine-owned and private. Application local metadata
belongs to the app, with schema migrations and stable provider-neutral book keys
(e.g. provider + provider ID, not title or downloaded path). Use SQLite for growing
relational state: bookmarks, history, completion and persistent jobs. Confirm schema,
transaction boundaries and Taletty-owned database upgrades in an ADR before coding
storage. All user data lives under `~/.config/taletty/`, with audio in `downloads/`.
Start fresh: never discover/import/reuse the prototype's auth, cache, history or media.
Completed media is engine output; deleting it is an explicit app request, never
implicit cleanup of user history. Do not force local imports to have Audible ASINs.

## Required engine contract

- Typed books, contributors, identity, optional metadata, chapters and capabilities.
- Context cancellation and per-operation IDs.
- Structured sanitized failures with retry guidance; do not blindly retry license
  refusals or throttling.
- Events for stage, bytes, optional total, completion, failure and cancellation.
- Versioned JSON-lines bridge protocol; stdout is machine output, diagnostics are
  sanitized. Separate interactive login from machine-readable noninteractive work.
- Event ordering and backpressure rules: stage/terminal events must not be dropped;
  progress samples may be coalesced under a documented policy.
- Explicit download lifecycle and atomic completed-file publication.
- Auth writes serialized across login/sync/download; do not remove the prototype's
  serialization until concurrency is designed and tested.
- No raw tokens, signed URLs, decryption keys or provider free-text IDs in events.

Bridge progress reporting extends the existing transfer/remux path; it does not
replace licensing or change audio encoding. Keep temporary and output permission
checks, process-group cancellation and token-refresh persistence on failure.

## Testability rules

Inject provider, clock, filesystem/process seams where needed. UI state transitions
must run without live Audible/MPD/mpv, network or real sleeps. Playback adapter tests
use fake IPC and real mpv with generated, null-output audio in a separate integration
job. Contract tests exercise real Go↔Python framing with a fake provider. No production
code exclusions merely to meet coverage.

## Decisions still needed

1. Review ADR 001: engine API/protocol and version compatibility policy.
2. Storage schema, Taletty-owned database upgrades and ownership within `~/.config/taletty/`.
3. Reviewed wide/compact layout, glyph vocabulary and exact key map.
4. Installation of Python runtime/dependencies and module release automation.

These are blocking design issues, not permission to couple the layers temporarily.
