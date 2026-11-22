"""Deterministic producer/recovery contracts; fake probe output is NOT media proof."""
from contextlib import nullcontext
import errno
import importlib
import io
import json
import os
from pathlib import Path
import stat
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import test_composition as fixtures

prefix = "engine.connectors.audible.internal.bridge.adapter."
launcher = importlib.import_module(prefix + "noninteractive")
recovery = importlib.import_module(prefix + "reconciliation")


class DownloadProducerTests(unittest.TestCase):
    def test_launcher_consumes_real_composition_and_preserves_independent_facts(self):
        fixture = fixtures.CompositionTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        request = dict(operation_id="download-fixture", book_id=dict(provider="audible", namespace="us", id=fixtures.ASIN))
        package = SimpleNamespace(doctor=SimpleNamespace(read_request=Mock(return_value=request), require_action=Mock()),
                                  auth_store=fixtures.session.auth_store, sdk_session=fixtures.session,
                                  media_store=fixtures.store, composition=fixtures.composition, reconciliation=recovery)
        for mode in ("success", "reuse", "auth_failed", "auth_uncertain", "media_uncertain", "denied", "reconcile"):
            with self.subTest(mode=mode):
                if mode not in ("reuse", "reconcile") and fixture.target.exists():
                    fixture.target.unlink()
                output = io.StringIO()
                control = Mock(__enter__=Mock(return_value=output), __exit__=Mock(return_value=False))
                save_fault = fixtures.session.auth_store.StoreError(mode == "auth_uncertain")
                real_save = fixtures.session.auth_store.save
                real_sync = os.fsync
                def sync(fd):
                    if mode == "media_uncertain" and fd != fixture.fd and stat.S_ISDIR(os.fstat(fd).st_mode):
                        raise OSError(errno.EIO, "private")
                    real_sync(fd)
                denied = {"content_license": {"status_code": "Denied"}}
                response = denied if mode == "denied" else None
                def run(fd, asin, cancel_fd=None, emit=None):
                    return original_run(fixture.fd, asin, cancel_fd=cancel_fd, emit=emit)
                original_run = fixtures.composition.run
                with (fixture.provider(response), patch.object(launcher.os, "dup", return_value=17),
                      patch.object(launcher.os, "fdopen", return_value=control), patch.object(launcher.os, "set_inheritable"),
                      patch.object(fixtures.composition, "run", side_effect=run), patch.object(os, "fsync", side_effect=sync),
                      patch.object(fixtures.session.auth_store, "save", side_effect=save_fault) if mode.startswith("auth_") else nullcontext(),
                      patch.object(recovery, "inspect", return_value="absent")):
                    loader = Mock(return_value=package)
                    launcher.main({}, loader, Mock(), action="reconcile" if mode == "reconcile" else "download")
                frames = [json.loads(line) for line in output.getvalue().splitlines()]
                self.assertEqual([f["seq"] for f in frames], list(range(1, len(frames) + 1)))
                self.assertNotIn("private", output.getvalue())
                frame = frames[-1]
                self.assertEqual(frame["type"], "result")
                if mode == "reconcile":
                    self.assertEqual(len(frames), 1)
                    self.assertEqual(frame["state"], "absent")
                    continue
                if mode == "reuse":
                    self.assertEqual(len(frames), 1)
                elif mode == "denied":
                    self.assertEqual([(f["type"], f.get("stage")) for f in frames[:-1]], [("stage", "licensing")])
                else:
                    self.assertEqual([(f["type"], f.get("stage")) for f in frames[:-1]], [
                        ("stage", "licensing"),
                        ("stage", "downloading"),
                        ("progress", None),
                        ("stage", "preparing"),
                        ("stage", "publishing"),
                    ])
                    self.assertEqual(frames[2]["bytes"], len(fixtures.PAYLOAD))
                    self.assertNotIn("total", frames[2])
                outcome = frame["outcome"]
                self.assertEqual(outcome["ready"], mode != "denied")
                self.assertEqual(outcome["reused"], mode == "reuse")
                self.assertEqual(outcome["published"], mode not in ("reuse", "denied"))
                self.assertEqual(outcome["auth_code"], {"auth_failed": "storage_failed", "auth_uncertain": "storage_uncertain"}.get(mode))
                self.assertEqual(outcome["media_code"], {"denied": "entitlement_denied", "media_uncertain": "storage_uncertain"}.get(mode))
                if mode == "reuse":
                    self.assertIsNone(outcome["chapters"])
                if mode == "success":
                    self.assertEqual(outcome["chapters"], fixtures.remux.chapters.prepare(fixtures.INFO)[0])
                # Reset refreshed auth without test fault injection.
                real_save(fixture.fd, json.dumps(fixtures.SNAPSHOT).encode())

    def test_bad_book_schema_never_composes_or_inspects(self):
        package = SimpleNamespace(doctor=SimpleNamespace(read_request=Mock(), require_action=Mock()),
                                  media_store=fixtures.store, composition=Mock(), reconciliation=Mock(),
                                  auth_store=fixtures.session.auth_store, sdk_session=fixtures.session)
        for book in (None, [], {}, {"provider": "local", "namespace": "us", "id": fixtures.ASIN},
                     {"provider": "audible", "namespace": "de", "id": fixtures.ASIN},
                     {"provider": "audible", "namespace": "us", "id": "../unsafe"}):
            package.doctor.read_request.return_value = dict(operation_id="fixture", book_id=book)
            output = io.StringIO()
            with (patch.object(launcher.os, "dup", return_value=17), patch.object(launcher.os, "set_inheritable"),
                  patch.object(launcher.os, "fdopen", return_value=nullcontext(output))):
                launcher.main({}, Mock(return_value=package), Mock(), action="download")
            self.assertEqual(json.loads(output.getvalue())["failure"]["code"], "invalid_input")
        package.composition.run.assert_not_called()
        package.reconciliation.inspect.assert_not_called()


class ReconciliationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.root.chmod(0o700)
        self.fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
        self.addCleanup(os.close, self.fd)
        self.downloads = self.root / "downloads"
        self.target = self.downloads / (fixtures.ASIN + ".m4b")

    def seeded(self, payload=b"synthetic"):
        self.downloads.mkdir(mode=0o700, exist_ok=True)
        self.target.write_bytes(payload)
        self.target.chmod(0o600)

    def test_readonly_states_no_provider_or_writes_and_anchored_root(self):
        with (patch.object(fixtures.license, "entitled", side_effect=AssertionError("must not license")),
              patch.object(os, "mkdir", side_effect=AssertionError("must not write"))):
            self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "absent")
        self.assertFalse(self.downloads.exists())
        self.seeded(b"")
        self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "invalid")
        self.seeded()
        for result, expected in ((True, "ready"), (False, "invalid"),
                                 (fixtures.remux.RemuxError("unsupported"), "invalid"),
                                 (fixtures.remux.RemuxError("runtime_missing"), "unknown"),
                                 (fixtures.remux.RemuxError("preparation_failed"), "unknown"),
                                 (RuntimeError("private"), "unknown")):
            with self.subTest(expected=expected), patch.object(recovery.remux, "validator") as validator:
                validator.return_value = Mock(side_effect=result) if isinstance(result, Exception) else Mock(return_value=result)
                self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), expected)
                validator.assert_called_once_with(seconds=5)
        moved = self.root.with_name(self.root.name + "-moved")
        self.root.rename(moved)
        self.addCleanup(lambda: moved.rename(self.root))
        with patch.object(recovery.remux, "validator", return_value=lambda fd: True):
            self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "ready")
        self.assertFalse(self.root.exists())

    def test_unsafe_files_and_directories_and_mutation(self):
        self.seeded()
        self.target.chmod(0o644)
        self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unsafe")
        self.target.unlink()
        self.target.symlink_to("/dev/null")
        self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unsafe")
        self.target.unlink()
        os.mkfifo(self.target, 0o600)
        self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unsafe")
        self.target.unlink()
        self.seeded()
        os.link(self.target, self.downloads / "other")
        self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unsafe")
        (self.downloads / "other").unlink()
        def mutate(fd):
            self.target.write_bytes(b"changed-size")
            return True
        with patch.object(recovery.remux, "validator", return_value=mutate):
            self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unknown")
        self.downloads.chmod(0o755)
        self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unsafe")
        self.downloads.chmod(0o700)
        self.root.chmod(0o755)
        self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unsafe")
        self.root.chmod(0o700)
        self.target.unlink()
        self.downloads.rmdir()
        self.downloads.symlink_to("/dev/null")
        self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unsafe")

    def test_io_tool_and_close_failures_are_unknown_not_absent(self):
        self.seeded()
        self.assertEqual(recovery.inspect(self.fd, "../invalid"), "unknown")
        with patch.object(recovery.os, "fstat", side_effect=OSError(errno.EIO, "private")):
            self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unknown")
        for calls in ((OSError(),), (OSError(errno.EIO, "private"),),
                      (os.stat(self.target), OSError()), (os.stat(self.target), FileNotFoundError())):
            with patch.object(recovery.media_store, "_check", side_effect=calls), patch.object(recovery.remux, "validator", return_value=lambda fd: True):
                self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unsafe" if len(calls) == 1 and calls[0].errno is None else "unknown")
        close = os.close
        def failed_close(fd):
            close(fd)
            raise OSError(errno.EIO, "private")
        with patch.object(recovery.os, "close", side_effect=failed_close), patch.object(recovery.remux, "validator", return_value=lambda fd: True):
            self.assertEqual(recovery.inspect(self.fd, fixtures.ASIN), "unknown")
