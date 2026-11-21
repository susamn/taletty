-- Taletty v1: fresh databases only; caller rolls back on ANY script error.
-- Requires SQLite >=3.38 (STRICT tables and built-in JSON), foreign_keys enabled.
PRAGMA foreign_keys = ON;
BEGIN IMMEDIATE;
CREATE TEMP TABLE taletty_initial_guard (
    valid INTEGER NOT NULL CHECK (valid = 1)
) STRICT;
INSERT INTO taletty_initial_guard
SELECT application_id = 0 AND user_version = 0
    AND NOT EXISTS (SELECT 1 FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%')
FROM pragma_application_id, pragma_user_version;
DROP TABLE taletty_initial_guard;

-- ASCII syntax matches engine/model.BookID.Validate; comparison is case-sensitive.
CREATE TABLE book_identity (
    book_key INTEGER PRIMARY KEY,
    provider TEXT NOT NULL CHECK (provider GLOB '[a-z]*' AND provider NOT GLOB '*[^a-z0-9_-]*'),
    namespace TEXT NOT NULL CHECK (namespace GLOB '[a-z]*' AND namespace NOT GLOB '*[^a-z0-9_-]*'),
    id TEXT NOT NULL CHECK (id GLOB '[A-Za-z0-9]*' AND id NOT GLOB '*[^A-Za-z0-9._-]*'),
    UNIQUE (provider, namespace, id),
    CHECK (instr(provider, char(0)) = 0 AND instr(namespace, char(0)) = 0 AND instr(id, char(0)) = 0),
    CHECK (provider != 'audible' OR (length(id) = 10 AND id NOT GLOB '*[^A-Za-z0-9]*'))
) STRICT;

-- Only a complete validated snapshot may replace a provider/namespace's rows.
CREATE TABLE library_snapshot (
    provider TEXT NOT NULL,
    namespace TEXT NOT NULL,
    refreshed_at_ms INTEGER NOT NULL CHECK (refreshed_at_ms >= 0),
    PRIMARY KEY (provider, namespace)
) STRICT;
CREATE TABLE catalog_book (
    book_key INTEGER PRIMARY KEY REFERENCES book_identity ON DELETE RESTRICT,
    title TEXT NOT NULL CHECK (length(trim(title)) > 0),
    subtitle TEXT,
    authors_json TEXT CHECK (CASE WHEN json_valid(authors_json) THEN json_type(authors_json) = 'array' ELSE authors_json IS NULL END),
    narrators_json TEXT CHECK (CASE WHEN json_valid(narrators_json) THEN json_type(narrators_json) = 'array' ELSE narrators_json IS NULL END),
    duration_ns INTEGER CHECK (duration_ns >= 0)
) STRICT;

CREATE TABLE listening_state (
    book_key INTEGER PRIMARY KEY REFERENCES book_identity ON DELETE RESTRICT,
    position_ns INTEGER NOT NULL CHECK (position_ns >= 0),
    speed_millionths INTEGER NOT NULL CHECK (speed_millionths > 0),
    completion TEXT NOT NULL CHECK (completion IN ('unfinished', 'finished')),
    updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= 0)
) STRICT;
CREATE TABLE favorite (
    book_key INTEGER PRIMARY KEY REFERENCES book_identity ON DELETE RESTRICT,
    created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0)
) STRICT;
CREATE TABLE bookmark (
    bookmark_id TEXT PRIMARY KEY NOT NULL CHECK (length(bookmark_id) > 0),
    book_key INTEGER NOT NULL REFERENCES book_identity ON DELETE RESTRICT,
    position_ns INTEGER NOT NULL CHECK (position_ns >= 0),
    note TEXT NOT NULL CHECK (length(trim(note, char(9)||char(10)||char(11)||char(12)||char(13)||' ')) > 0),
    chapter_id TEXT,
    chapter_title TEXT,
    created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
    updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms)
) STRICT;
CREATE INDEX bookmark_by_book ON bookmark(book_key, position_ns, bookmark_id);

-- Inventory is not proof that a file still exists or passed current validation.
CREATE TABLE local_media (
    book_key INTEGER PRIMARY KEY REFERENCES book_identity ON DELETE RESTRICT,
    relative_path TEXT NOT NULL UNIQUE CHECK (relative_path GLOB 'downloads/?*' AND relative_path NOT GLOB '*..*'),
    format TEXT NOT NULL CHECK (length(format) > 0),
    published_at_ms INTEGER NOT NULL CHECK (published_at_ms >= 0)
) STRICT;
CREATE TABLE download_job (
    job_id TEXT PRIMARY KEY NOT NULL CHECK (length(job_id) > 0),
    book_key INTEGER NOT NULL REFERENCES book_identity ON DELETE RESTRICT,
    queue_order INTEGER UNIQUE CHECK (queue_order >= 0),
    state TEXT NOT NULL CHECK (state IN ('waiting', 'licensing', 'downloading', 'preparing', 'publishing', 'ready', 'failed', 'canceled')),
    bytes_received INTEGER NOT NULL CHECK (bytes_received >= 0),
    bytes_total INTEGER CHECK (bytes_total >= bytes_received),
    failure_code TEXT CHECK (failure_code IN ('network', 'throttled', 'entitlement_denied', 'disk_full', 'permission_denied', 'preparation_failed', 'interrupted', 'internal')),
    created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
    updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
    CHECK ((state = 'failed') = (failure_code IS NOT NULL)),
    CHECK ((state = 'waiting') = (queue_order IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX one_live_download_per_book ON download_job(book_key)
    WHERE state IN ('waiting', 'licensing', 'downloading', 'preparing', 'publishing');

CREATE UNIQUE INDEX one_active_download ON download_job((1))
    WHERE state IN ('licensing', 'downloading', 'preparing', 'publishing');

CREATE TABLE view_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    destination TEXT NOT NULL CHECK (destination IN ('all', 'continue', 'downloaded', 'finished', 'favorites', 'downloads', 'bookmarks')),
    selected_book_key INTEGER REFERENCES book_identity ON DELETE RESTRICT,
    focus TEXT NOT NULL CHECK (focus IN ('library', 'books', 'chapters', 'details', 'player')),
    filter_text TEXT NOT NULL,
    sort_order TEXT NOT NULL CHECK (sort_order IN ('title', 'author', 'duration', 'recent', 'acquisition'))
) STRICT;
CREATE TABLE settings (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    glyph_mode TEXT NOT NULL CHECK (glyph_mode IN ('unicode', 'plain')),
    volume_percent INTEGER NOT NULL CHECK (volume_percent BETWEEN 0 AND 100)
) STRICT;

-- ASCII 'TALT'; distinct from user_version and any engine/bridge version.
PRAGMA application_id = 1413565524;
PRAGMA user_version = 1;
COMMIT;
