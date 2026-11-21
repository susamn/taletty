"""Read-only media inspection after the original RunRoot child has been joined.

Caller retains the SAME borrowed root/gate and bounds this separate child's whole
lifetime. No root discovery, mkdir, auth, licensing, fsync, deletion or retry. A
ready artifact proves existence/validation, not this attempt's commit or durability.
"""
from contextlib import ExitStack
import errno
import os

from . import auth_store, media_store, remux

PROBE_SECONDS = 5


def _safe_directory(fd):
    try:
        auth_store._root(fd)
        return True
    except OSError as error:
        if error.errno is not None:
            raise
        return False


def inspect(root_fd, asin):
    """Return ready/absent/invalid/unsafe/unknown; never infer a chapter hierarchy.

    Unknown includes failed I/O, unavailable tools, timeout and changed media.
    Invalid means a safe empty file or a completed probe rejecting its contents;
    tool execution failure is NOT proof of absence or invalidity. All opens are
    no-follow/nonblocking and every descriptor is closed before returning.
    """
    try:
        name = media_store._leaf(asin)
        with ExitStack() as owned:
            if not _safe_directory(root_fd):
                return "unsafe"
            directory = os.open("downloads", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC,
                                dir_fd=root_fd)
            owned.callback(os.close, directory)
            if not _safe_directory(directory):
                return "unsafe"
            if not auth_store._private(os.stat(name, dir_fd=directory, follow_symlinks=False)):
                return "unsafe"
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC,
                         dir_fd=directory)
            owned.callback(os.close, fd)
            try:
                before = media_store._check(fd, directory, name)
            except OSError as error:
                return "unsafe" if error.errno is None else "unknown"
            if before.st_size == 0:
                state = "invalid"
            else:
                try:
                    state = "ready" if remux.validator(seconds=PROBE_SECONDS)(fd) is True else "invalid"
                except remux.RemuxError as error:
                    state = "invalid" if error.code == "unsupported" else "unknown"
            try:
                after = media_store._check(fd, directory, name)
            except OSError:
                return "unknown"
            if (before.st_size, before.st_mtime_ns) != (after.st_size, after.st_mtime_ns):
                state = "unknown"
        return state
    except OSError as error:
        if error.errno == errno.ENOENT:
            return "absent"
        if error.errno in (errno.ELOOP, errno.ENOTDIR):
            return "unsafe"
        return "unknown"
    except Exception:
        return "unknown"
