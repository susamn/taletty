"""Private Linux media capabilities; caller holds the serialized root/process gate.

No provider calls or path discovery. Validators borrow a read/write media FD,
return exactly True for complete media and must not modify/retain/close it. Root
and directory names may move: every lookup remains relative to retained FDs.
Same-UID hostile mutation is outside the cooperative auth gate's threat model.
"""
from contextlib import contextmanager
import ctypes
import errno
import hashlib
import os
import re
import secrets

from . import auth_store


class MediaStoreError(Exception):
    def __init__(self, published=False, conflict=False):
        self.published, self.conflict = published, conflict
        super().__init__("Media publication could not be reconciled." if published is None else
                         "Media published; durability not confirmed." if published else
                         "Completed media conflicts." if conflict else "Private media storage failed.")


def _same(left, right):
    return (left.st_dev, left.st_ino) == (right.st_dev, right.st_ino)


def _leaf(asin):
    if not isinstance(asin, str) or re.fullmatch(r"[A-Za-z0-9]{10}", asin) is None:
        raise MediaStoreError()
    return asin + ".m4b"


def _check(fd, directory, name):
    info = os.fstat(fd)
    if (not auth_store._private(info) or not _same(info, os.stat(
            name, dir_fd=directory, follow_symlinks=False))):
        raise OSError()
    return info


def _verified(fd, directory, name, validate):
    before = _check(fd, directory, name)
    try:
        valid = validate(fd) is True
    except Exception:
        valid = False
    after = _check(fd, directory, name)
    if not valid or before.st_size <= 0 or (before.st_size, before.st_mtime_ns) != (after.st_size, after.st_mtime_ns):
        raise OSError()


def _digest(fd):
    digest, offset = hashlib.sha256(), 0
    while True:
        chunk = os.pread(fd, 1024 * 1024, offset)
        if not chunk:
            return digest.digest()
        digest.update(chunk)
        offset += len(chunk)


def _rename(directory, source, target):
    # Linux renameat2 avoids link/unlink's transient nlink=2 ready inode. No
    # fallback to replace: unsupported filesystems/runtimes must fail closed.
    libc = ctypes.CDLL(None, use_errno=True)
    rename = libc.renameat2
    rename.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint)
    rename.restype = ctypes.c_int
    if rename(directory, source.encode(), directory, target.encode(), 1) != 0:
        raise OSError(ctypes.get_errno(), "Private media rename failed.")


class Partial:
    """One exclusively created inode; never sweep other operations' partials."""
    def __init__(self, directory, name, fd):
        self._directory, self._name, self.fd = directory, name, fd
        try:
            self._identity = os.fstat(fd)
        except BaseException:
            try:
                os.close(fd)
            except OSError:
                pass
            raise
        self.published = False

    def write(self, payload):
        try:
            _check(self.fd, self._directory, self._name)
            view = memoryview(payload)
            while view:
                count = os.write(self.fd, view)
                if count <= 0:
                    raise OSError()
                view = view[count:]
        except (OSError, TypeError, ValueError):
            raise MediaStoreError() from None

    def _cleanup(self):
        try:
            if _same(self._identity, os.stat(self._name, dir_fd=self._directory, follow_symlinks=False)):
                os.unlink(self._name, dir_fd=self._directory)
        except OSError:
            pass  # Best effort; no finally/cleanup guarantee after hard kill.
        finally:
            try:
                os.close(self.fd)
            except OSError:
                raise MediaStoreError(self.published) from None


class Store:
    def __init__(self, directory):
        self._directory = directory
        self.published = False

    @contextmanager
    def partial(self):
        partial = None
        try:
            name = ".media-" + secrets.token_hex(16) + ".part"
            fd = os.open(name, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC,
                         0o600, dir_fd=self._directory)
            partial = Partial(self._directory, name, fd)
            # Validate, never repair even restrictive-umask or preexisting files.
            _check(fd, self._directory, name)
            yield partial
        except OSError:
            raise MediaStoreError(partial.published if partial else False) from None
        finally:
            if partial is not None:
                partial._cleanup()

    @contextmanager
    def _ready(self, name, validate):
        fd = None
        try:
            try:
                fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK,
                             dir_fd=self._directory)
            except FileNotFoundError:
                yield None
                return
            _verified(fd, self._directory, name, validate)
            yield fd
        finally:
            if fd is not None:
                os.close(fd)

    def ready(self, asin, validate):
        """Verify existing ready media, not mere existence. Unsafe data is an error."""
        try:
            with self._ready(_leaf(asin), validate) as fd:
                return fd is not None
        except OSError:
            raise MediaStoreError() from None

    def _committed(self, partial, name):
        try:
            info = os.stat(name, dir_fd=self._directory, follow_symlinks=False)
            return auth_store._private(info) and _same(info, partial._identity)
        except FileNotFoundError:
            return False

    def _reconcile(self, partial, name):
        if partial.published is True:
            return True
        try:
            committed = self._committed(partial, name)
        except OSError:
            partial.published = self.published = None
            raise MediaStoreError(None) from None
        if committed:
            partial.published = self.published = True
        return committed

    def publish(self, partial, asin, validate):
        """Return True for commit, False for verified byte-identical concurrent reuse.

        Successful rename is the irrevocable commit point. A later fsync error
        carries published=True and partial.published stays True. Never delete or
        pretend rollback of final media. Callers reconcile this fact even when
        auth persistence or cancellation later overrides the operation outcome.
        published=None means commit inspection failed: reconcile before retry,
        never interpret unknown as False/rollback.
        """
        name = _leaf(asin)
        if partial._directory != self._directory or partial.published is not False:
            raise MediaStoreError(partial.published)
        try:
            _verified(partial.fd, self._directory, partial._name, validate)
            os.fsync(partial.fd)
            try:
                _rename(self._directory, partial._name, name)
            except OSError as error:
                if error.errno != errno.EEXIST:
                    raise
                with self._ready(name, validate) as fd:
                    if fd is None or _digest(fd) != _digest(partial.fd):
                        raise MediaStoreError(conflict=True)
                return False
            partial.published = self.published = True
            os.fsync(self._directory)
            return True
        except KeyboardInterrupt:
            # A signal may arrive between the syscall commit and Python's flag
            # assignment. Reconcile the inode, not just the in-memory flag.
            if not self._reconcile(partial, name):
                raise
            try:
                os.fsync(self._directory)
            except OSError:
                raise MediaStoreError(True) from None
            return True
        except (OSError, AttributeError):
            self._reconcile(partial, name)
            raise MediaStoreError(partial.published) from None


@contextmanager
def opened(root_fd):
    """Borrow verified root; own only a noninheritable downloads directory FD."""
    directory = None
    try:
        auth_store._root(root_fd)
        try:
            os.mkdir("downloads", 0o700, dir_fd=root_fd)
        except FileExistsError:
            pass
        directory = os.open("downloads", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC,
                            dir_fd=root_fd)
        auth_store._root(directory)
        os.fsync(root_fd)
    except BaseException as error:
        if directory is not None:
            try:
                os.close(directory)
            except OSError:
                pass
        if isinstance(error, OSError):
            raise MediaStoreError() from None
        raise
    media = Store(directory)
    try:
        yield media
    finally:
        try:
            os.close(directory)
        except OSError:
            raise MediaStoreError(media.published) from None
