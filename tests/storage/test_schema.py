"""Execute app-owned SQL directly; stdlib sqlite3 is a test oracle, not an adapter."""
from pathlib import Path
import sqlite3
import sys
import tempfile
import unittest


SQL = (Path(__file__).resolve().parents[2] / "app/internal/store/schema/001_initial.sql").read_text()


class SchemaContractTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "contract.sqlite3"
        self.db = sqlite3.connect(self.path)
        self.addCleanup(lambda: self.db.close())
        self.db.executescript(SQL)

    def identity(self, provider="audible", namespace="us", identifier="B000000001"):
        return self.db.execute(
            "INSERT INTO book_identity(provider, namespace, id) VALUES (?, ?, ?) RETURNING book_key",
            (provider, namespace, identifier),
        ).fetchone()[0]

    def seed(self):
        key = self.identity()
        self.db.execute("INSERT INTO catalog_book VALUES (?, 'Original', NULL, NULL, NULL, NULL)", (key,))
        self.db.execute("INSERT INTO library_snapshot VALUES ('audible', 'us', 1)")
        self.db.execute("INSERT INTO listening_state VALUES (?, 123456789, 1250000, 'finished', 2)", (key,))
        self.db.execute("INSERT INTO favorite VALUES (?, 2)", (key,))
        self.db.execute("INSERT INTO bookmark VALUES ('note', ?, 123, 'Remember', NULL, '', 1, 2)", (key,))
        self.db.execute("INSERT INTO local_media VALUES (?, 'downloads/book.m4b', 'm4b', 2)", (key,))
        self.db.execute("INSERT INTO view_state VALUES (1, 'favorites', ?, 'details', '', 'title')", (key,))
        self.db.execute("INSERT INTO settings VALUES (1, 'plain', 80)")
        self.db.commit()
        return key

    def dump(self):
        return list(self.db.iterdump())

    def test_identity_collisions_and_model_syntax(self):
        keys = [self.identity(), self.identity(namespace="uk"),
                self.identity(provider="other"), self.identity("local", "user", "record-1")]
        self.assertEqual(len(set(keys)), 4)
        for values in [("audible", "us", "B000000001"), ("Audible", "us", "B000000002"),
                       ("audible", "", "B000000002"), ("audible", "us", "not-asin"),
                       ("local", "user", "../escape"), ("local", "user", ""),
                       ("local", "user", "has space"), ("", "user", "id"),
                       ("local\x00BAD", "user", "id"), ("local", "user\x00BAD", "id"),
                       ("local", "user", "id\x00BAD") ]:
            with self.subTest(values=values), self.assertRaises(sqlite3.IntegrityError):
                self.identity(*values)

    def test_optional_metadata_unknown_empty_zero_and_integer_bounds(self):
        unknown, known = self.identity(), self.identity("local", "user", "id")
        self.db.execute("INSERT INTO catalog_book VALUES (?, 'Unknown', NULL, NULL, NULL, NULL)", (unknown,))
        self.db.execute("INSERT INTO catalog_book VALUES (?, 'Known', '', '[]', '[]', 0)", (known,))
        rows = self.db.execute("SELECT subtitle, authors_json, narrators_json, duration_ns FROM catalog_book ORDER BY book_key").fetchall()
        self.assertEqual(rows, [(None, None, None, None), ("", "[]", "[]", 0)])
        self.db.execute("UPDATE catalog_book SET duration_ns = 9223372036854775807 WHERE book_key = ?", (known,))
        for column, value in [("duration_ns", -1), ("duration_ns", 1.5),
                              ("authors_json", "{}"), ("authors_json", "broken"),
                              ("narrators_json", "null"), ("title", " ")]:
            with self.subTest(column=column, value=value), self.assertRaises(sqlite3.IntegrityError):
                self.db.execute(f"UPDATE catalog_book SET {column} = ? WHERE book_key = ?", (value, known))

    def test_local_state_independence_and_reopen(self):
        key = self.seed()
        with self.db:
            self.db.execute("DELETE FROM catalog_book")
            self.db.execute("DELETE FROM local_media")
        for table in ["listening_state", "favorite", "bookmark", "view_state", "book_identity"]:
            self.assertEqual(self.db.execute(f"SELECT count(*) FROM {table}").fetchone()[0], 1)
        with self.assertRaises(sqlite3.IntegrityError):
            self.db.execute("DELETE FROM book_identity")
        self.db.rollback()
        with self.db:
            self.db.execute("DELETE FROM favorite")
        self.assertEqual(self.db.execute("SELECT completion FROM listening_state").fetchone(), ("finished",))
        self.assertEqual(self.db.execute("SELECT book_key FROM bookmark").fetchone(), (key,))
        before = self.dump()
        self.db.close()
        self.db = sqlite3.connect(self.path)
        self.db.execute("PRAGMA foreign_keys = ON")
        self.assertEqual(self.dump(), before)
        self.assertEqual(self.db.execute("PRAGMA application_id").fetchone(), (1413565524,))
        self.assertEqual(self.db.execute("PRAGMA user_version").fetchone(), (1,))
        self.assertEqual(self.db.execute("PRAGMA integrity_check").fetchall(), [("ok",)])
        self.assertEqual(self.db.execute("PRAGMA foreign_key_check").fetchall(), [])
        with self.db:
            self.db.execute("INSERT INTO favorite VALUES (?, 3)", (key,))
            self.db.execute("UPDATE listening_state SET completion = 'unfinished'")
            self.db.execute("DELETE FROM bookmark")
        self.assertEqual(self.db.execute("SELECT book_key FROM favorite").fetchone(), (key,))
        self.assertEqual(self.db.execute("SELECT position_ns FROM listening_state").fetchone(), (123456789,))

    def test_constraints_and_foreign_keys(self):
        self.seed()
        invalid = [
            "INSERT INTO favorite VALUES (999, 1)",
            "INSERT INTO listening_state VALUES (999, 0, 1000000, 'unfinished', 1)",
            "UPDATE listening_state SET position_ns = -1",
            "UPDATE listening_state SET speed_millionths = 0",
            "UPDATE listening_state SET completion = 'true'",
            "UPDATE listening_state SET updated_at_ms = -1",
            "UPDATE favorite SET created_at_ms = -1",
            "UPDATE bookmark SET position_ns = -1",
            "UPDATE bookmark SET note = ''",
            "UPDATE bookmark SET note = char(9)||char(10)",
            "UPDATE bookmark SET updated_at_ms = 0",
            "UPDATE local_media SET relative_path = '../book'",
            "UPDATE view_state SET selected_book_key = 999",
            "UPDATE view_state SET singleton = 2",
            "UPDATE view_state SET destination = 'unknown'",
            "UPDATE view_state SET focus = 'unknown'",
            "UPDATE view_state SET sort_order = 'unknown'",
            "UPDATE settings SET volume_percent = 101",
            "UPDATE settings SET glyph_mode = 'unknown'",
            "UPDATE library_snapshot SET refreshed_at_ms = -1",
            "INSERT INTO catalog_book VALUES (999, 'Missing', NULL, NULL, NULL, NULL)",
            "INSERT INTO bookmark VALUES ('other', 999, 0, 'note', NULL, NULL, 0, 0)",
            "INSERT INTO local_media VALUES (999, 'downloads/other.m4b', 'm4b', 0)",
        ]
        for statement in invalid:
            with self.subTest(statement=statement), self.assertRaises(sqlite3.IntegrityError):
                self.db.execute(statement)
        self.db.rollback()
        for focus in ['library', 'books', 'chapters', 'details', 'player']:
            self.db.execute("UPDATE view_state SET focus = ?", (focus,))
            self.assertEqual(self.db.execute("SELECT focus FROM view_state").fetchone(), (focus,))
        for order in ['title', 'author', 'duration', 'recent', 'acquisition']:
            self.db.execute("UPDATE view_state SET sort_order = ?", (order,))
            self.assertEqual(self.db.execute("SELECT sort_order FROM view_state").fetchone(), (order,))

    def test_persistent_jobs_deduplicate_live_attempts_not_terminal_history(self):
        key = self.identity()
        insert = "INSERT INTO download_job VALUES (?, ?, ?, ?, ?, ?, ?, 1, 2)"
        self.db.execute(insert, ("first", key, 0, "waiting", 0, None, None))
        for args in [("second", key, 1, "waiting", 0, 0, None),
                     ("bad", 999, None, "ready", 0, None, None),
                     ("bad", key, None, "unknown", 0, None, None),
                     ("bad", key, None, "failed", 0, None, None),
                     ("bad", key, None, "ready", 0, None, "internal"),
                     ("bad", key, None, "failed", 0, None, "raw body"),
                     ("bad", key, None, "ready", 2, 1, None),
                     ("bad", key, None, "ready", -1, None, None),
                     ("bad", key, 0, "ready", 0, None, None)]:
            with self.subTest(args=args), self.assertRaises(sqlite3.IntegrityError):
                self.db.execute(insert, args)
        self.db.execute("UPDATE download_job SET state = 'failed', failure_code = 'interrupted', queue_order = NULL")
        self.db.execute(insert, ("retry", key, 1, "waiting", 0, 0, None))
        another = self.identity("local", "user", "another")
        self.db.execute(insert, ("active", another, None, "preparing", 0, None, None))
        with self.assertRaises(sqlite3.IntegrityError):
            self.db.execute("UPDATE download_job SET state = 'downloading', queue_order = NULL WHERE job_id = 'retry'")
        self.db.execute("DELETE FROM download_job WHERE job_id = 'active'")
        self.db.commit()
        self.db.close()
        self.db = sqlite3.connect(self.path)
        self.assertEqual(self.db.execute("SELECT state, bytes_total FROM download_job ORDER BY job_id").fetchall(),
                         [("failed", None), ("waiting", 0)])

    def test_atomic_complete_refresh_and_injected_error_rollback(self):
        key = self.seed()
        other = self.identity(namespace="uk")
        self.db.execute("INSERT INTO catalog_book VALUES (?, 'Other marketplace', NULL, NULL, NULL, NULL)", (other,))
        self.db.commit()
        before = self.dump()
        # Simulate the future repository transaction, not a production helper.
        with self.assertRaises(sqlite3.IntegrityError):
            with self.db:
                self.db.execute("UPDATE catalog_book SET title = 'Provisional' WHERE book_key = ?", (key,))
                self.identity("local", "user", "provisional")
                self.db.execute("DELETE FROM catalog_book WHERE book_key = ?", (key,))
                self.db.execute("UPDATE library_snapshot SET refreshed_at_ms = 99")
                self.db.execute("INSERT INTO favorite VALUES (999, 1)")  # injected late error
        self.assertEqual(self.dump(), before)
        with self.db:
            replacement = self.identity(identifier="B000000002")
            self.db.execute("INSERT INTO catalog_book VALUES (?, 'Replacement', '', '[]', '[]', 0)", (replacement,))
            self.db.execute("DELETE FROM catalog_book WHERE book_key = ?", (key,))
            self.db.execute("UPDATE library_snapshot SET refreshed_at_ms = 2 WHERE provider = 'audible' AND namespace = 'us'")
        self.assertEqual(self.db.execute("SELECT book_key FROM catalog_book ORDER BY book_key").fetchall(), [(other,), (replacement,)])
        # Successful empty US snapshot removes US catalog, not identity/local state/UK.
        with self.db:
            self.db.execute("DELETE FROM catalog_book WHERE book_key IN (SELECT book_key FROM book_identity WHERE provider = 'audible' AND namespace = 'us')")
            self.db.execute("UPDATE library_snapshot SET refreshed_at_ms = 3 WHERE provider = 'audible' AND namespace = 'us'")
        self.assertEqual(self.db.execute("SELECT book_key FROM catalog_book").fetchall(), [(other,)])
        self.assertEqual(self.db.execute("SELECT refreshed_at_ms FROM library_snapshot").fetchone(), (3,))
        for table in ["favorite", "listening_state", "bookmark", "local_media"]:
            self.assertEqual(self.db.execute(f"SELECT book_key FROM {table}").fetchone(), (key,))

    def test_reapplication_refuses_and_preserves_existing_v1(self):
        self.seed()
        before = self.dump()
        with self.assertRaises(sqlite3.IntegrityError):
            self.db.executescript(SQL)
        self.assertTrue(self.db.in_transaction)
        self.db.rollback()  # mandatory caller obligation after any migration failure
        self.assertEqual(self.dump(), before)

    def test_fresh_guard_and_partial_migration_rollback(self):
        for preparation in ["PRAGMA user_version = 2", "PRAGMA application_id = 123",
                            "CREATE TABLE foreign_data (value TEXT)", ""]:
            with self.subTest(preparation=preparation), sqlite3.connect(":memory:") as db:
                db.executescript(preparation)
                before = list(db.iterdump())
                markers = [db.execute(f"PRAGMA {name}").fetchone() for name in ["application_id", "user_version"]]
                # Fresh case fails after all DDL and markers, before COMMIT.
                script = SQL if preparation else SQL.replace("COMMIT;", "INSERT INTO missing_table VALUES (1);\nCOMMIT;")
                with self.assertRaises(sqlite3.DatabaseError):
                    db.executescript(script)
                db.rollback()
                self.assertEqual(list(db.iterdump()), before)
                self.assertEqual([db.execute(f"PRAGMA {name}").fetchone() for name in ["application_id", "user_version"]], markers)


if __name__ == "__main__":
    print(f"SQLite contract oracle: SQLite {sqlite3.sqlite_version}; CPython {sys.version.split()[0]}", flush=True)
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(SchemaContractTests)
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    raise SystemExit(0 if result.wasSuccessful() and result.testsRun >= 8 and not result.skipped else 1)
