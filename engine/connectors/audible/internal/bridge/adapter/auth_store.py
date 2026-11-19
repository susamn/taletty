"""Private opaque SDK snapshots; caller owns the root FD and serialized auth gate.

Never log/transport these bytes. This filesystem boundary does not validate SDK
credentials, acquire locks, discover/create roots or make provider calls.
"""

import os
import secrets
import stat

MAX_AUTH_BYTES = 4 << 20


class StoreError(Exception):
    def __init__(self, published=False):
        super().__init__("Credentials published; durability not confirmed." if published
                         else "Authentication storage failed.")
        self.published = published


class MissingAuth(Exception):
    pass


def _root(root_fd):
    info = os.fstat(root_fd)
    if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o700 or info.st_uid != os.geteuid():
        raise OSError()


def _private(info):
    return (stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600
            and info.st_uid == os.geteuid() and info.st_nlink == 1)


def _target(root_fd):
    try:
        info = os.stat("auth.json", dir_fd=root_fd, follow_symlinks=False)
    except FileNotFoundError:
        return
    if not _private(info):
        raise OSError()


def check(root_fd):
    """Check descriptor/leaf safety without reading credentials or creating files."""
    try:
        _root(root_fd)
        _target(root_fd)
    except OSError:
        raise StoreError() from None


def load(root_fd):
    """Read bounded bytes from a private leaf; do not close the borrowed root FD."""
    try:
        _root(root_fd)
        fd = os.open("auth.json", os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK, dir_fd=root_fd)
        try:
            if not _private(os.fstat(fd)):
                raise OSError()
            chunks, remaining = [], MAX_AUTH_BYTES + 1
            while remaining:
                chunk = os.read(fd, remaining)
                if not chunk:
                    break
                chunks.append(chunk)
                remaining -= len(chunk)
            payload = b"".join(chunks)
            if not payload or len(payload) > MAX_AUTH_BYTES:
                raise OSError()
            return payload
        finally:
            os.close(fd)
    except FileNotFoundError:
        raise MissingAuth("Not signed in.") from None
    except OSError:
        raise StoreError() from None


def save(root_fd, payload):
    """Publish immutable bytes atomically. Pre-commit failure preserves prior data.

    Directory-fsync failure is explicitly published=True: never undo an installed
    snapshot or claim the old one survived. Cleanup of an owned temp is best effort
    on filesystem failure/abrupt kill; unrelated temps are never removed.
    """
    if type(payload) is not bytes or not 0 < len(payload) <= MAX_AUTH_BYTES:
        raise StoreError()
    temporary, published = None, False
    try:
        check(root_fd)
        candidate = ".auth-" + secrets.token_hex(16) + ".tmp"
        fd = os.open(candidate, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=root_fd)
        temporary = candidate  # Only a successfully created name is ours to unlink.
        try:
            os.fchmod(fd, 0o600)  # New file only; restrictive umask cannot publish unreadable auth.
            view = memoryview(payload)
            while view:
                count = os.write(fd, view)
                if count <= 0:
                    raise OSError()
                view = view[count:]
            os.fsync(fd)
        finally:
            os.close(fd)
        _target(root_fd)
        os.replace(temporary, "auth.json", src_dir_fd=root_fd, dst_dir_fd=root_fd)
        temporary, published = None, True
        os.fsync(root_fd)
    except OSError:
        raise StoreError(published) from None
    finally:
        if temporary is not None:
            try:
                os.unlink(temporary, dir_fd=root_fd)
            except OSError:
                pass
