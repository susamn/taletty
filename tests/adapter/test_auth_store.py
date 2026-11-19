import importlib.util
import json
import os
from pathlib import Path
import selectors
import socket
import stat
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parents[2] / "engine/connectors/audible/internal/bridge/adapter/auth_store.py"
spec = importlib.util.spec_from_file_location("taletty_auth_store", SOURCE)
store = importlib.util.module_from_spec(spec)
spec.loader.exec_module(store)


class AuthStoreTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.root = self.base / "private"
        self.root.mkdir(mode=0o700)
        self.fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        self.addCleanup(os.close, self.fd)
        self.target = self.root / "auth.json"

    def put(self, value=b"synthetic-before"):
        self.target.write_bytes(value)
        self.target.chmod(0o600)

    def failure(self, operation, *, published=False):
        with self.assertRaises(store.StoreError) as caught:
            operation()
        self.assertEqual(caught.exception.published, published)
        self.assertNotIn("fixture-private", str(caught.exception))
        self.assertTrue(caught.exception.__suppress_context__ or caught.exception.__context__ is None)
        return caught.exception

    def test_private_atomic_replace_short_io_and_borrowed_descriptor(self):
        self.put()
        unrelated = self.root / ".auth-unrelated.tmp"
        unrelated.write_bytes(b"unrelated")
        write, read, sync = os.write, os.read, os.fsync
        events = []

        def partial_write(fd, data):
            info = os.fstat(fd)
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
            self.assertEqual(info.st_nlink, 1)
            self.assertFalse(os.get_inheritable(fd))
            self.assertEqual(self.target.read_bytes(), b"synthetic-before")
            return write(fd, data[:2])

        def synced(fd):
            events.append("directory" if fd == self.fd else "file")
            if fd == self.fd:
                self.assertEqual(self.target.read_bytes(), b"synthetic-after")
            sync(fd)

        previous = os.umask(0o777)
        try:
            with patch.object(store.os, "write", side_effect=partial_write), patch.object(store.os, "fsync", side_effect=synced):
                store.save(self.fd, b"synthetic-after")
        finally:
            os.umask(previous)
        with patch.object(store.os, "read", side_effect=lambda fd, count: read(fd, min(count, 2))):
            self.assertEqual(store.load(self.fd), b"synthetic-after")
        self.assertEqual(events, ["file", "directory"])
        self.assertEqual(set(self.root.iterdir()), {self.target, unrelated})
        self.assertEqual(unrelated.read_bytes(), b"unrelated")
        self.assertEqual(stat.S_IMODE(self.target.stat().st_mode), 0o600)
        self.assertEqual(os.fstat(self.fd).st_ino, self.root.stat().st_ino)

    def test_descriptor_anchors_renamed_directory_and_no_legacy_discovery(self):
        self.put()
        moved = self.base / "moved"
        self.root.rename(moved)
        self.root.mkdir(mode=0o700)
        self.target.write_bytes(b"other-root-sentinel")
        store.save(self.fd, b"anchored")
        self.assertEqual(store.load(self.fd), b"anchored")
        self.assertEqual((moved / "auth.json").read_bytes(), b"anchored")
        self.assertEqual(self.target.read_bytes(), b"other-root-sentinel")

    def test_missing_and_payload_bounds_are_not_sign_in_validation(self):
        with self.assertRaisesRegex(store.MissingAuth, "^Not signed in\\.$"):
            store.load(self.fd)
        for value in [b"", "text", bytearray(b"mutable"), b"x" * (store.MAX_AUTH_BYTES + 1)]:
            self.failure(lambda: store.save(self.fd, value))
            self.assertEqual(list(self.root.iterdir()), [])
        data = b"x" * store.MAX_AUTH_BYTES
        store.save(self.fd, data)
        self.assertEqual(store.load(self.fd), data)
        for value in [b"", data + b"x"]:
            self.put(value)
            self.failure(lambda: store.load(self.fd))
        # Opaque bytes are deliberately not a valid-credentials/signed-in claim.
        store.save(self.fd, b"not-json")
        self.assertEqual(store.load(self.fd), b"not-json")

    def test_unsafe_roots_and_leaves_are_not_repaired(self):
        self.put()
        for mode in [0o755, 0o1700]:
            self.root.chmod(mode)
            self.failure(lambda: store.load(self.fd))
            self.failure(lambda: store.save(self.fd, b"after"))
            self.assertEqual(stat.S_IMODE(self.root.stat().st_mode), mode)
        self.root.chmod(0o700)
        for mode in [0o644, 0o1600]:
            self.target.chmod(mode)
            self.failure(lambda: store.load(self.fd))
            self.failure(lambda: store.save(self.fd, b"after"))
            self.assertEqual(stat.S_IMODE(self.target.stat().st_mode), mode)
        self.target.chmod(0o600)
        with self.target.open("rb") as regular:
            self.failure(lambda: store.load(regular.fileno()))
            self.failure(lambda: store.save(regular.fileno(), b"after"))
        with patch.object(store.os, "geteuid", return_value=os.geteuid() + 1):
            self.failure(lambda: store.load(self.fd))
            self.failure(lambda: store.save(self.fd, b"after"))
        self.assertEqual(self.target.read_bytes(), b"synthetic-before")

    def test_symlink_hardlink_fifo_directory_and_socket_leaves(self):
        outside = self.base / "outside"
        outside.write_bytes(b"unrelated")
        outside.chmod(0o600)
        sock = socket.socket(socket.AF_UNIX)
        self.addCleanup(sock.close)
        creators = [lambda: self.target.symlink_to(outside),
                    lambda: os.link(outside, self.target),
                    lambda: os.mkfifo(self.target, 0o600),
                    lambda: self.target.mkdir(mode=0o700),
                    lambda: sock.bind(str(self.target))]
        for create in creators:
            create()
            self.failure(lambda: store.load(self.fd))
            self.failure(lambda: store.save(self.fd, b"after"))
            if self.target.is_dir():
                self.target.rmdir()
            else:
                self.target.unlink()
            self.assertEqual(outside.read_bytes(), b"unrelated")

    def test_wrong_leaf_owner_and_unexpected_io_are_sanitized_without_fd_leaks(self):
        self.put()
        inspect = os.fstat

        def wrong_owner(fd):
            info = inspect(fd)
            return info if fd == self.fd else SimpleNamespace(st_mode=info.st_mode, st_nlink=1, st_uid=info.st_uid + 1)

        before = len(os.listdir("/proc/self/fd"))
        with patch.object(store.os, "fstat", side_effect=wrong_owner):
            self.failure(lambda: store.load(self.fd))
        info = self.target.stat()
        foreign = SimpleNamespace(st_mode=info.st_mode, st_nlink=1, st_uid=info.st_uid + 1)
        with patch.object(store.os, "stat", return_value=foreign):
            self.failure(lambda: store.save(self.fd, b"after"))
        for method in ["open", "read", "fstat"]:
            with patch.object(store.os, method, side_effect=OSError("fixture-private-path")):
                self.failure(lambda: store.load(self.fd))
        self.assertEqual(len(os.listdir("/proc/self/fd")), before)

    def test_exclusive_temp_collision_never_deletes_someone_elses_file(self):
        self.put()
        collision = self.root / (".auth-" + "a" * 32 + ".tmp")
        collision.symlink_to(self.target)
        with patch.object(store.secrets, "token_hex", return_value="a" * 32):
            self.failure(lambda: store.save(self.fd, b"after"))
        self.assertTrue(collision.is_symlink())
        self.assertEqual(self.target.read_bytes(), b"synthetic-before")

    def test_precommit_failure_preserves_previous_snapshot_and_recovers(self):
        self.put()
        before = len(os.listdir("/proc/self/fd"))
        for method in ["fchmod", "write", "fsync", "replace", "stat"]:
            with self.subTest(method=method):
                with patch.object(store.os, method, side_effect=OSError("fixture-private-disk")):
                    self.failure(lambda: store.save(self.fd, b"after"))
                self.assertEqual(self.target.read_bytes(), b"synthetic-before")
                self.assertEqual(list(self.root.iterdir()), [self.target])
        with patch.object(store.os, "write", return_value=0):
            self.failure(lambda: store.save(self.fd, b"after"))
        inspect, calls = os.stat, 0

        def fail_recheck(*args, **kwargs):
            nonlocal calls
            calls += 1
            if calls == 2:
                raise OSError("fixture-private-before-publication")
            return inspect(*args, **kwargs)

        with patch.object(store.os, "stat", side_effect=fail_recheck):
            self.failure(lambda: store.save(self.fd, b"after"))
        self.assertEqual(calls, 2)
        self.assertEqual(self.target.read_bytes(), b"synthetic-before")
        self.assertEqual(list(self.root.iterdir()), [self.target])
        self.assertEqual(len(os.listdir("/proc/self/fd")), before)
        store.save(self.fd, b"recovered")
        self.assertEqual(store.load(self.fd), b"recovered")

    def test_postcommit_durability_failure_does_not_undo_published_data(self):
        self.put()
        sync = os.fsync

        def fail_directory(fd):
            if fd == self.fd:
                raise OSError("fixture-private-directory")
            sync(fd)

        with patch.object(store.os, "fsync", side_effect=fail_directory):
            self.failure(lambda: store.save(self.fd, b"published"), published=True)
        self.assertEqual(store.load(self.fd), b"published")
        self.assertEqual(list(self.root.iterdir()), [self.target])
        store.save(self.fd, b"recovered")
        self.assertEqual(store.load(self.fd), b"recovered")

    def test_owned_cleanup_failure_is_private_and_never_sweeps_other_temps(self):
        self.put()
        with patch.object(store.os, "write", side_effect=OSError("fixture-private")), patch.object(store.os, "unlink", side_effect=PermissionError("fixture-private")):
            self.failure(lambda: store.save(self.fd, b"after"))
        leftover, = self.root.glob(".auth-*.tmp")
        self.assertEqual(stat.S_IMODE(leftover.stat().st_mode), 0o600)
        self.assertEqual(self.target.read_bytes(), b"synthetic-before")
        store.save(self.fd, b"recovered")
        self.assertTrue(leftover.exists())

    def test_killed_writer_before_replace_preserves_prior_file(self):
        self.put()
        script = '''
import os, sys
scope = {}
with open(sys.argv[1]) as source:
    exec(compile(source.read(), sys.argv[1], "exec"), scope)
def stop_before_commit(*args, **kwargs):
    print("before-replace", flush=True)
    sys.stdin.buffer.read(1)
    raise AssertionError("fixture resumed")
os.replace = stop_before_commit
scope["save"](int(sys.argv[2]), b"synthetic-after")
'''
        child = subprocess.Popen([sys.executable, "-I", "-B", "-c", script, str(SOURCE), str(self.fd)],
                                 pass_fds=(self.fd,), stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            with selectors.DefaultSelector() as ready:
                ready.register(child.stdout, selectors.EVENT_READ)
                self.assertTrue(ready.select(timeout=5), "writer did not reach commit barrier")
            self.assertEqual(child.stdout.readline(), b"before-replace\n")
            child.kill()
            _, errors = child.communicate(timeout=5)
            self.assertEqual(errors, b"")
            self.assertEqual(child.returncode, -9)
        finally:
            if child.poll() is None:
                child.kill()
            child.communicate(timeout=5)
        self.assertEqual(store.load(self.fd), b"synthetic-before")
        leftover, = self.root.glob(".auth-*.tmp")
        self.assertEqual(leftover.read_bytes(), b"synthetic-after")
        self.assertEqual(stat.S_IMODE(leftover.stat().st_mode), 0o600)
        store.save(self.fd, b"retry")
        self.assertEqual(store.load(self.fd), b"retry")
        self.assertTrue(leftover.exists(), "retry must not sweep another operation's temp")

    def test_real_pinned_sdk_synthetic_round_trip_without_provider_access(self):
        import audible
        import httpx
        with patch.object(httpx.Client, "send", side_effect=AssertionError("provider access forbidden")):
            auth = audible.Authenticator.from_dict({"locale_code": "us"})
            snapshot = auth.to_dict()
            self.assertIsNone(snapshot["access_token"])
            payload = json.dumps(snapshot, allow_nan=False).encode("utf-8")
            store.save(self.fd, payload)
            restored = audible.Authenticator.from_dict(json.loads(store.load(self.fd)))
            self.assertEqual(restored.to_dict(), snapshot)
        self.assertEqual(list(self.root.iterdir()), [self.target])
