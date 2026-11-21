# Provider-neutral library contracts

`BookID` is a comparable catalog key; `Book`/`Contributor` add provider display
metadata only. No auth/customer/content URLs, playback/history, favorites, cache
writes or guessed download/entitlement capabilities. Deferred artwork/acquisition/
description/capability contracts will land with their real consumers.

Call `Book.Validate()` at trust boundaries. It checks identity syntax, nonempty
UTF-8 title/contributor names, optional text and nonnegative native duration;
terminal controls and format characters are rejected, not silently transformed.
Provider adapters sanitize first. Errors never echo malformed metadata. A valid
catalog namespace is not proof that a particular connector supports that market.

Unknown fields are nil, not invented defaults. Public JSON omits nil metadata but
preserves known empty subtitle/contributor lists and zero duration. Standard Go
`omitzero` preserves the nil/empty slice distinction; no custom marshaler/global
JSON policy. `Duration` is `*time.Duration`; its explicitly named public JSON field
is **`duration_ns`**. The private Python DTO uses **`duration_ms`** instead: #85
must bound/validate integers before conversion, not reinterpret JSON as native
nanoseconds. No milliseconds converter or transport decoder is hidden here.

Book values include mutable slices/pointers: an owner must not share them for
concurrent mutation. Validate is read-only; ownership/defensive copies at event/API
boundaries belong to that implementation, not a generic clone framework.

`LibrarySnapshot`, `Failure` and `OperationEvent` are engine-created result/lifecycle
records used by [Client.Library](../connectors/audible/library.md). A stage count is
provisional, not a snapshot; the successful API return owns complete Books. Fixed
safe Failure values implement error; context identity may also be joined. Callers
must not substitute remote exception text in these records.

`Chapter`, `Media` and `DownloadResult` are consumed by the actual public
[Download lifecycle](../connectors/audible/download.md). ValidateChapters bounds
hierarchy/IDs/text/native times without flattening or inventing unknown ends;
Media.Validate checks safe absolute final M4B paths, not on-disk readiness.
DownloadResult keeps ready state/owned media, tri-state commit, media durability
and auth persistence/failures separate even on error. Its Clone gives terminal
callbacks independent pointers/slices. Nil provider hierarchy is explicitly unknown
on reuse/restart/reconciliation, not reconstructed from container leaf chapters.

Tests cover native/JSON optionality and invalid boundaries. The declarations alone
do not prove provider sync/cache persistence/full UI journeys; #85 adds the actual
owned transport/completion tests. CLI sync remains #86.
