import errno
import importlib
import os
from pathlib import Path
import shutil
import signal
import select
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import patch

store = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.media_store")
ASIN = "B000000001"


def valid(fd):
    return os.pread(fd, 5, 0) == b"media"


class MediaStoreTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
        self.addCleanup(os.close, self.fd)

    def error(self, action, published=False, conflict=False):
        with self.assertRaises(store.MediaStoreError) as caught:
            action()
        self.assertEqual(caught.exception.published, published)
        self.assertEqual(caught.exception.conflict, conflict)
        self.assertIsNone(caught.exception.__cause__)
        self.assertNotIn("private-fixture", str(caught.exception))

    def test_private_owned_full_write_commit_and_reuse(self):
        write = os.write
        with store.opened(self.fd) as media:
            self.assertFalse(os.get_inheritable(media._directory))
            self.assertFalse(media.ready(ASIN, valid))
            with media.partial() as part:
                self.assertFalse(os.get_inheritable(part.fd))
                self.assertEqual(os.fstat(part.fd).st_mode & 0o777, 0o600)
                with patch.object(store.os, "write", side_effect=lambda fd, data: write(fd, data[:17003])):
                    part.write(b"media" * 500000)
                # Verify uses pread, independent of the writer's current offset.
                self.assertTrue(media.publish(part, ASIN, valid))
                self.assertTrue(part.published)
                self.error(lambda: media.publish(part, ASIN, valid), published=True)
            self.assertTrue(media.ready(ASIN, valid))
            with media.partial() as same:
                same.write(b"media" * 500000)
                self.assertFalse(media.publish(same, ASIN, valid))
                self.assertFalse(same.published)
            with media.partial() as other:
                other.write(b"media different")
                self.error(lambda: media.publish(other, ASIN, valid), conflict=True)
        self.assertEqual([p.name for p in (self.root / "downloads").iterdir()], [ASIN + ".m4b"])
        self.assertEqual((self.root / "downloads").stat().st_mode & 0o777, 0o700)

    def test_invalid_identity_and_cross_store_partial(self):
        with store.opened(self.fd) as media:
            for asin in (None, False, "../private-fixture", "B00000000", "X" * 11):
                self.error(lambda: media.ready(asin, valid))
            with media.partial() as part:
                part.write(b"media")
                self.error(lambda: store.Store(-1).publish(part, ASIN, valid))
                self.error(lambda: media.publish(part, "bad", valid))

    def test_unsafe_roots_and_directories_are_not_repaired(self):
        self.root.chmod(0o755)
        self.error(lambda: store.opened(self.fd).__enter__())
        self.root.chmod(0o700)
        downloads = self.root / "downloads"
        for kind in ("symlink", "file", "public"):
            with self.subTest(kind=kind):
                if kind == "symlink":
                    downloads.symlink_to(self.root, target_is_directory=True)
                elif kind == "file":
                    downloads.write_bytes(b"private-fixture")
                else:
                    downloads.mkdir(mode=0o755)
                self.error(lambda: store.opened(self.fd).__enter__())
                if kind == "public":
                    self.assertEqual(downloads.stat().st_mode & 0o777, 0o755)
                    downloads.rmdir()
                else:
                    downloads.unlink()
        close = os.close
        def close_fault(fd):
            close(fd)
            raise OSError("private-fixture")
        with patch.object(store.os, "fsync", side_effect=OSError("private-fixture")):
            with patch.object(store.os, "close", side_effect=close_fault):
                self.error(lambda: store.opened(self.fd).__enter__())
        with store.opened(self.fd):
            pass

    def test_unsafe_ready_leaves_no_follow_or_delete(self):
        with store.opened(self.fd) as media:
            target = self.root / "downloads" / (ASIN + ".m4b")
            outside = self.root / "sentinel"
            outside.write_bytes(b"media sentinel")
            outside.chmod(0o600)
            for kind in ("symlink", "hardlink", "fifo", "directory", "public", "empty", "invalid"):
                with self.subTest(kind=kind):
                    if kind == "symlink":
                        target.symlink_to(outside)
                    elif kind == "hardlink":
                        os.link(outside, target)
                    elif kind == "fifo":
                        os.mkfifo(target, 0o600)
                    elif kind == "directory":
                        target.mkdir(mode=0o700)
                    else:
                        target.write_bytes(b"" if kind == "empty" else b"media" if kind == "public" else b"private-fixture")
                        target.chmod(0o644 if kind == "public" else 0o600)
                    self.error(lambda: media.ready(ASIN, valid))
                    with media.partial() as part:
                        part.write(b"media")
                        self.error(lambda: media.publish(part, ASIN, valid))
                    self.assertTrue(target.exists())
                    target.rmdir() if kind == "directory" else target.unlink()
            self.assertEqual(outside.read_bytes(), b"media sentinel")

    def test_anchored_after_root_and_downloads_rename(self):
        with store.opened(self.fd) as media:
            moved = self.root / "retained"
            (self.root / "downloads").rename(moved)
            (self.root / "downloads").mkdir(mode=0o700)
            renamed = self.root.with_name(self.root.name + "-moved")
            self.root.rename(renamed)
            self.root.mkdir(mode=0o700)
            self.addCleanup(shutil.rmtree, renamed)
            with media.partial() as part:
                part.write(b"media")
                self.assertTrue(media.publish(part, ASIN, valid))
            self.assertTrue(media.ready(ASIN, valid))
            self.assertEqual((renamed / "retained" / (ASIN + ".m4b")).read_bytes(), b"media")
            self.assertEqual(list((renamed / "downloads").iterdir()), [])
            self.assertEqual(list(self.root.iterdir()), [])

    def test_only_own_inode_cleanup_exclusive_collision_and_no_repair(self):
        with store.opened(self.fd) as media:
            directory = self.root / "downloads"
            with patch.object(store.secrets, "token_hex", return_value="collision"):
                with media.partial() as first:
                    first.write(b"media")
                    self.error(lambda: media.partial().__enter__())
                    self.assertEqual((directory / first._name).read_bytes(), b"media")
            with media.partial() as part:
                name = directory / part._name
                name.unlink()
                name.write_bytes(b"other operation")
            self.assertEqual(name.read_bytes(), b"other operation")
            name.unlink()
            old_umask = os.umask(0o777)
            try:
                self.error(lambda: media.partial().__enter__())
            finally:
                os.umask(old_umask)
            self.assertEqual(list(directory.iterdir()), [])
            with patch.object(store.os, "fstat", side_effect=OSError("private-fixture")):
                self.error(lambda: media.partial().__enter__())
            # A failed initial fstat cannot prove inode ownership, so leaves a
            # private exclusive partial rather than deleting an unverified name.
            self.assertEqual(len(list(directory.iterdir())), 1)

    def test_canceled_acquisition_closes_owned_descriptors(self):
        opened, close, fstat = os.open, os.close, os.fstat
        acquired = []
        def record(*args, **kwargs):
            fd = opened(*args, **kwargs)
            acquired.append(fd)
            return fd
        with patch.object(store.os, "open", side_effect=record):
            with patch.object(store.os, "fsync", side_effect=KeyboardInterrupt):
                with self.assertRaises(KeyboardInterrupt):
                    store.opened(self.fd).__enter__()
        with self.assertRaises(OSError) as caught:
            fstat(acquired.pop())
        self.assertEqual(caught.exception.errno, errno.EBADF)
        with store.opened(self.fd) as media:
            for fault in (OSError, KeyboardInterrupt):
                def close_fault(fd):
                    close(fd)
                    raise OSError("private-fixture")
                with patch.object(store.os, "open", side_effect=record):
                    with patch.object(store.os, "fstat", side_effect=fault), patch.object(store.os, "close", side_effect=close_fault):
                        with self.assertRaises(store.MediaStoreError if fault is OSError else KeyboardInterrupt):
                            media.partial().__enter__()
                with self.assertRaises(OSError) as caught:
                    fstat(acquired.pop())
                self.assertEqual(caught.exception.errno, errno.EBADF)

    def test_write_failures_validation_and_precommit_preserve_old(self):
        with store.opened(self.fd) as media:
            with media.partial() as part:
                for payload in (None, "private-fixture"):
                    self.error(lambda: part.write(payload))
                with patch.object(store.os, "write", return_value=0):
                    self.error(lambda: part.write(b"media"))
                with patch.object(store.os, "write", side_effect=OSError("private-fixture")):
                    self.error(lambda: part.write(b"media"))
                self.error(lambda: media.publish(part, ASIN, valid))
                part.write(b"media")
                for validator in (lambda fd: False, lambda fd: 1,
                                  lambda fd: (_ for _ in ()).throw(ValueError("private-fixture"))):
                    self.error(lambda: media.publish(part, ASIN, validator))
                def mutate(fd):
                    os.write(fd, b"changed")
                    return True
                self.error(lambda: media.publish(part, ASIN, mutate))
                for fault in ("fsync", "_rename"):
                    owner = store.os if fault == "fsync" else store
                    with patch.object(owner, fault, side_effect=OSError("private-fixture")):
                        self.error(lambda: media.publish(part, ASIN, valid))
                    self.assertFalse(part.published)
                    self.assertFalse(media.ready(ASIN, valid))
                with patch.object(store.ctypes, "CDLL", side_effect=AttributeError("private-fixture")):
                    self.error(lambda: media.publish(part, ASIN, valid))
                self.assertTrue(media.publish(part, ASIN, valid))
            target = self.root / "downloads" / (ASIN + ".m4b")
            before = target.read_bytes()
            with media.partial() as part:
                part.write(b"media alternate")
                with patch.object(store.os, "fsync", side_effect=OSError()):
                    self.error(lambda: media.publish(part, ASIN, valid))
            self.assertEqual(target.read_bytes(), before)

    def test_publication_is_commit_even_when_directory_fsync_fails(self):
        with store.opened(self.fd) as media:
            with media.partial() as part:
                part.write(b"media")
                fsync = os.fsync
                def fail_directory(fd):
                    if fd == media._directory:
                        raise OSError("private-fixture")
                    fsync(fd)
                with patch.object(store.os, "fsync", side_effect=fail_directory):
                    self.error(lambda: media.publish(part, ASIN, valid), published=True)
                self.assertTrue(part.published)
                self.assertTrue(media.ready(ASIN, valid))
            self.assertEqual((self.root / "downloads" / (ASIN + ".m4b")).read_bytes(), b"media")

    def test_cleanup_failure_and_ordinary_exception_leave_no_foreign_deletion(self):
        with store.opened(self.fd) as media:
            with self.assertRaises(KeyboardInterrupt):
                with media.partial() as part:
                    part.write(b"media")
                    raise KeyboardInterrupt()
            self.assertEqual(list((self.root / "downloads").iterdir()), [])
            with self.assertRaises(store.MediaStoreError):
                with media.partial():
                    raise OSError("private-fixture")
            with patch.object(store.os, "unlink", side_effect=OSError()):
                with media.partial() as part:
                    part.write(b"media")
            self.assertEqual(len(list((self.root / "downloads").iterdir())), 1)

    def test_name_replacement_during_validation_is_rejected(self):
        with store.opened(self.fd) as media:
            with media.partial() as part:
                part.write(b"media")
                def replace(fd):
                    os.rename(part._name, "retained", src_dir_fd=media._directory, dst_dir_fd=media._directory)
                    Path(self.root / "downloads" / part._name).write_bytes(b"other operation")
                    return True
                self.error(lambda: media.publish(part, ASIN, replace))
            self.assertEqual((self.root / "downloads" / part._name).read_bytes(), b"other operation")

    def test_concurrent_publication_identical_reuse_and_conflict(self):
        # Barriers line up actual renameat2 calls, not guessed sleeps. Repeated
        # races cover both winners; the gate normally serializes these clients.
        for different in (False, True):
            for _ in range(10):
                with store.opened(self.fd) as media:
                    barrier, results = threading.Barrier(2), []
                    rename = store._rename
                    def synchronized(*args):
                        barrier.wait(timeout=5)
                        rename(*args)
                    def worker(payload):
                        try:
                            with media.partial() as part:
                                part.write(payload)
                                results.append(media.publish(part, ASIN, valid))
                        except store.MediaStoreError as error:
                            results.append(error.conflict)
                    with patch.object(store, "_rename", side_effect=synchronized):
                        threads = [threading.Thread(target=worker, args=(payload,))
                                   for payload in (b"media", b"media other" if different else b"media")]
                        for thread in threads:
                            thread.start()
                        for thread in threads:
                            thread.join(timeout=10)
                            self.assertFalse(thread.is_alive())
                    self.assertEqual(sorted(results), [True, True] if different else [False, True])
                    self.assertTrue(media.ready(ASIN, valid))
                    os.unlink(ASIN + ".m4b", dir_fd=media._directory)

    def test_cancel_at_syscall_commit_reconciles_instead_of_rolling_back(self):
        for committed in (False, True):
            for uncertain in (False, True):
                with store.opened(self.fd) as media:
                    with media.partial() as part:
                        part.write(b"media")
                        rename, fsync = store._rename, os.fsync
                        def interrupt(*args):
                            if committed:
                                rename(*args)
                            signal.raise_signal(signal.SIGINT)
                        def sync(fd):
                            if uncertain and fd == media._directory:
                                raise OSError("private-fixture")
                            fsync(fd)
                        with patch.object(store, "_rename", side_effect=interrupt), patch.object(store.os, "fsync", side_effect=sync):
                            if not committed:
                                with self.assertRaises(KeyboardInterrupt):
                                    media.publish(part, ASIN, valid)
                            elif uncertain:
                                self.error(lambda: media.publish(part, ASIN, valid), published=True)
                            else:
                                self.assertTrue(media.publish(part, ASIN, valid))
                        self.assertEqual(part.published, committed)
                        self.assertEqual(media.ready(ASIN, valid), committed)
                    if committed:
                        os.unlink(ASIN + ".m4b", dir_fd=media._directory)
        with store.opened(self.fd) as media:
            with media.partial() as part:
                part.write(b"media")
                rename = store._rename
                def ambiguous(*args):
                    rename(*args)
                    raise OSError("private-fixture")
                with patch.object(store, "_rename", side_effect=ambiguous):
                    self.error(lambda: media.publish(part, ASIN, valid), published=True)
                self.assertTrue(media.ready(ASIN, valid))

    def test_failed_commit_inspection_is_uncertain_not_rollback(self):
        for fault in (KeyboardInterrupt, OSError):
            with store.opened(self.fd) as media:
                with media.partial() as part:
                    part.write(b"media")
                    rename, stat = store._rename, os.stat
                    def interrupted(*args):
                        rename(*args)
                        raise fault()
                    def unreadable(name, *args, **kwargs):
                        if name == ASIN + ".m4b":
                            raise OSError(errno.EIO, "private-fixture")
                        return stat(name, *args, **kwargs)
                    with patch.object(store, "_rename", side_effect=interrupted), patch.object(store.os, "stat", side_effect=unreadable):
                        self.error(lambda: media.publish(part, ASIN, valid), published=None)
                    self.assertIsNone(part.published)
                    self.assertIsNone(media.published)
                    self.assertIn("could not be reconciled", str(store.MediaStoreError(None)))
                    self.error(lambda: media.publish(part, ASIN, valid), published=None)
                    self.assertTrue(media.ready(ASIN, valid))
                self.assertTrue((self.root / "downloads" / (ASIN + ".m4b")).exists())
                os.unlink(ASIN + ".m4b", dir_fd=media._directory)

    def test_close_faults_do_not_hide_committed_media(self):
        close = os.close
        with store.opened(self.fd) as media:
            def close_fault(fd):
                close(fd)
                raise OSError("private-fixture")
            with self.assertRaises(store.MediaStoreError) as caught:
                with patch.object(store.os, "close", side_effect=close_fault):
                    with media.partial() as part:
                        part.write(b"media")
                        self.assertTrue(media.publish(part, ASIN, valid))
            self.assertTrue(caught.exception.published)
            self.assertTrue(media.ready(ASIN, valid))
            context = media.partial()
            part = context.__enter__()
            part.published = None  # Reconciliation state must survive close faults.
            with patch.object(store.os, "close", side_effect=close_fault):
                self.error(lambda: context.__exit__(None, None, None), published=None)
        context = store.opened(self.fd)
        media = context.__enter__()
        with media.partial() as part:
            part.write(b"media second")
            media.publish(part, "B000000002", valid)
        def close_directory(fd):
            close(fd)
            raise OSError("private-fixture")
        with patch.object(store.os, "close", side_effect=close_directory):
            self.error(lambda: context.__exit__(None, None, None), published=True)

    def test_hard_kill_leaves_only_private_partial_without_finally_promise(self):
        read, write = os.pipe()
        pid = os.fork()
        if pid == 0:
            os.close(read)
            try:
                with store.opened(self.fd) as media:
                    with media.partial() as part:
                        part.write(b"media")
                        os.write(write, b"ready")
                        signal.pause()
            finally:
                os._exit(7)
        os.close(write)
        try:
            self.assertTrue(select.select([read], [], [], 5)[0])
            self.assertEqual(os.read(read, 5), b"ready")
        finally:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
            os.close(read)
        with store.opened(self.fd) as media:
            self.assertFalse(media.ready(ASIN, valid))
            before = list((self.root / "downloads").iterdir())
            self.assertEqual(len(before), 1)
            self.assertEqual(before[0].stat().st_mode & 0o777, 0o600)
            with media.partial() as part:
                part.write(b"media retry")
                self.assertTrue(media.publish(part, ASIN, valid))
            self.assertTrue(before[0].exists())  # Retry never sweeps a killed job.

    def test_destination_disappearing_at_conflict_is_not_success(self):
        with store.opened(self.fd) as media:
            with media.partial() as part:
                part.write(b"media")
                with patch.object(store, "_rename", side_effect=FileExistsError(errno.EEXIST, "fixture")):
                    self.error(lambda: media.publish(part, ASIN, valid), conflict=True)

    def test_generated_media_publication_with_real_probe(self):
        if not all(shutil.which(tool) for tool in ("ffmpeg", "ffprobe")):
            if os.environ.get("TALETTY_REQUIRE_MEDIA_TOOLS") == "1":
                self.fail("Mandatory generated-media tools missing")
            self.skipTest("Local ffmpeg/ffprobe absent; mandatory in CI")
        source = self.root / "silence.m4b"
        subprocess.run(["ffmpeg", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono",
                        "-t", "0.1", "-c:a", "aac", "-f", "mp4", str(source)], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        def probe(fd):
            result = subprocess.run(["ffprobe", "-v", "error", "-select_streams", "a:0",
                                     "-show_entries", "stream=codec_type", "-of", "csv=p=0",
                                     f"/proc/self/fd/{fd}"], pass_fds=(fd,), close_fds=True,
                                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=10)
            return result.returncode == 0 and result.stdout.strip() == b"audio"
        with store.opened(self.fd) as media:
            with media.partial() as part:
                part.write(source.read_bytes())
                self.assertTrue(media.publish(part, ASIN, probe))
            self.assertTrue(media.ready(ASIN, probe))
            self.assertEqual(source.read_bytes(), (self.root / "downloads" / (ASIN + ".m4b")).read_bytes())


if __name__ == "__main__":
    unittest.main()
