"""Private FD-only copy-remux. Caller retains the RunRoot group and store gate.

Never log argv: inherited audible key/IV argv exposure is not solved here. Tools
are trusted local executables, not a sandbox for hostile codecs or escaped children.
"""
import errno
import json
import math
import os
import selectors
import subprocess
import time

from . import chapters, media_store

MAX_PROBE_BYTES = 4 << 20
PROBE_SECONDS, COPY_SECONDS = 60, 3600


class RemuxError(Exception):
    def __init__(self, code):
        self.code = code
        super().__init__("Private media preparation failed (" + code + ").")


def _run(argv, fds, seconds, cancel_fd=None):
    """Bound stdout without buffering stderr; inherit the existing owner's group.

    cancel_fd is a borrowed readable cancellation pipe/socket, never inherited.
    Go cancellation also kills this entire group. Only the direct tool is waitable
    here; RunRoot owns residual descendants. No preexec hook/new session/group.
    """
    child, failure, output = None, None, bytearray()
    try:
        with selectors.DefaultSelector() as selector:
            child = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                     stderr=subprocess.DEVNULL, close_fds=True, pass_fds=tuple(fds))
            selector.register(child.stdout, selectors.EVENT_READ)
            if cancel_fd is not None:
                selector.register(cancel_fd, selectors.EVENT_READ)
            deadline = time.monotonic() + seconds
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError()
                events = selector.select(remaining)
                if any(key.fileobj == cancel_fd for key, _ in events):
                    failure = "canceled"
                    break
                if not events:
                    raise TimeoutError()
                chunk = os.read(child.stdout.fileno(), 65536)
                if not chunk:
                    if child.wait(timeout=max(0.001, deadline - time.monotonic())):
                        failure = "preparation_failed"
                    break
                output.extend(chunk)
                if len(output) > MAX_PROBE_BYTES:
                    raise ValueError()
    except FileNotFoundError:
        failure = "runtime_missing"
    except OSError as error:
        failure = "runtime_incompatible" if isinstance(error, PermissionError) or error.errno == errno.ENOEXEC else "preparation_failed"
    except KeyboardInterrupt:
        failure = "canceled"
    except Exception:
        failure = "preparation_failed"
    finally:
        if child is not None:
            if child.poll() is None:
                child.kill()
            child.wait()  # Sole direct-child reaper, including timeout/interrupt.
            child.stdout.close()
    # Raise outside handlers: do not retain raw tool/argv exceptions as context.
    if failure is not None:
        raise RemuxError(failure)
    return bytes(output)


def _path(fd):
    return "/proc/self/fd/" + str(fd)


def _probe(fd, cancel_fd=None, seconds=PROBE_SECONDS):
    payload = _run(["ffprobe", "-v", "error", "-protocol_whitelist", "file",
                    "-format_whitelist", "mov,mp3,aac", "-count_packets",
                    "-show_entries", "format=format_name,duration:stream=codec_type,codec_name,nb_read_packets:chapter=start_time,end_time:chapter_tags=title",
                    "-of", "json", _path(fd)], (fd,), seconds, cancel_fd)
    result = None
    try:
        result = json.loads(payload)
        audio = [s for s in result["streams"] if s["codec_type"] == "audio"]
        duration = float(result["format"]["duration"])
        if ("mp4" not in result["format"]["format_name"].split(",") or len(audio) != 1
                or audio[0]["codec_name"] not in ("aac", "mp3", "alac", "ac3", "eac3")
                or int(audio[0]["nb_read_packets"]) <= 0 or not math.isfinite(duration) or duration <= 0
                or any(s["codec_type"] not in ("audio", "data") for s in result["streams"])):
            result = None
        else:
            for chapter in result.get("chapters", []):
                start, end = float(chapter["start_time"]), float(chapter["end_time"])
                if not (math.isfinite(start) and math.isfinite(end) and 0 <= start < end <= duration + 0.1):
                    result = None
                    break
    except Exception:
        result = None
    if result is None:
        raise RemuxError("unsupported")
    return result


def validator(tree=None, cancel_fd=None, seconds=PROBE_SECONDS):
    """Store callback: full bounded packet scan, audio/container and leaf checks.

    tree must be an owned chapters.prepare result, not untrusted provider JSON.
    None/no positive leaves retains optional embedded chapters. Does not mutate,
    close or retain the borrowed FD; store verifies inode/size/mtime around it.
    """
    leaves = []
    def visit(nodes):
        for node in nodes:
            if node["children"]:
                visit(node["children"])
            elif node.get("end_ms", node["start_ms"]) > node["start_ms"]:
                leaves.append(node)
    visit(tree or [])
    leaves.sort(key=lambda node: node["start_ms"])

    def validate(fd):
        result = _probe(fd, cancel_fd, seconds)
        if leaves:
            actual = result.get("chapters", [])
            if len(actual) != len(leaves):
                return False
            for want, got in zip(leaves, actual):
                if (got.get("tags", {}).get("title") != want["title"]
                        or abs(float(got["start_time"]) * 1000 - want["start_ms"]) > 1
                        or abs(float(got["end_time"]) * 1000 - want["end_ms"]) > 1):
                    return False
        return True
    return validate


def copy(source, output, store, chapter_info=None, plan=None, cancel_fd=None):
    """Borrow owned store partials, prepare chapters, copy first audio, validate.

    No transfer/license composition, publication, progress or public outcome. The
    caller chooses publish/reuse with validator(tree) and reconciles commit facts.
    """
    tree, metadata = chapters.prepare(chapter_info)
    failure = False
    try:
        before = media_store._check(source.fd, source._directory, source._name)
        after = media_store._check(output.fd, output._directory, output._name)
        if (media_store._same(before, after) or source.published is not False or output.published is not False
                or source._directory != store._directory or output._directory != store._directory):
            raise ValueError()
    except Exception:
        failure = True
    if failure:
        raise RemuxError("invalid_input")
    with store.partial() as meta:
        args = ["ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y"]
        if plan is not None and plan._key is not None:
            args += ["-audible_key", plan._key, "-audible_iv", plan._iv]
        args += ["-protocol_whitelist", "file", "-format_whitelist", "mov,mp3,aac", "-i", _path(source.fd)]
        fds = [source.fd, output.fd]
        if metadata is not None:
            meta.write(metadata.encode("utf-8"))
            args += ["-f", "ffmetadata", "-i", _path(meta.fd)]
            fds.append(meta.fd)
        args += ["-map", "0:a:0", "-map_metadata", "0", "-map_chapters", "1" if metadata else "0",
                 "-c:a", "copy", "-f", "mp4", _path(output.fd)]
        _run(args, fds, COPY_SECONDS, cancel_fd)
    if not validator(tree, cancel_fd)(output.fd):
        raise RemuxError("unsupported")
    return tree
