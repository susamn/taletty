"""Read-only bridge-v1 readiness probe; no auth paths or provider operations."""

from contextlib import redirect_stderr, redirect_stdout
import importlib
from importlib import metadata
import json
import os
import re
import shutil
import subprocess
import sys

PROTOCOL = 1
MAX_REQUEST_BYTES = 4 << 20
AUDIBLE_VERSION = "0.13.1"
READY, MISSING, INCOMPATIBLE, NOT_CHECKED = (
    "ready", "runtime_missing", "runtime_incompatible", "not_checked")


def sdk_status():
    try:
        if metadata.version("audible") != AUDIBLE_VERSION:
            return INCOMPATIBLE
        # -B prevents bytecode writes; suppress any import-time banners/logging.
        # Import checks dependencies without constructing auth/client objects.
        with open(os.devnull, "w") as sink, redirect_stdout(sink), redirect_stderr(sink):
            importlib.import_module("audible")
        return READY
    except metadata.PackageNotFoundError:
        return MISSING
    except Exception:
        return INCOMPATIBLE  # Never echo arbitrary module exceptions/paths.


def tool_status(name):
    path = shutil.which(name)
    if path is None:
        return MISSING
    try:
        result = subprocess.run([path, "-version"], stdin=subprocess.DEVNULL,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                timeout=2, check=False)
        return READY if result.returncode == 0 else INCOMPATIBLE
    except (OSError, subprocess.TimeoutExpired):
        return INCOMPATIBLE


def python_status():
    return READY if sys.implementation.name == "cpython" and sys.version_info[:2] == (3, 12) else INCOMPATIBLE


def diagnose():
    result = dict.fromkeys(("python", "audible", "ffmpeg", "ffprobe"), NOT_CHECKED)
    # This slice has evidence for CPython 3.12 only, not a guessed support matrix.
    if python_status() != READY:
        result["python"] = INCOMPATIBLE
        return result
    result.update(python=READY, audible=sdk_status(),
                  ffmpeg=tool_status("ffmpeg"), ffprobe=tool_status("ffprobe"))
    return result


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def reject_constant(value):
    raise ValueError("non-finite JSON constant")


def read_request():
    raw = sys.stdin.buffer.read(MAX_REQUEST_BYTES + 2)
    content = raw[:-1] if raw.endswith(b"\n") else raw
    if len(content) > MAX_REQUEST_BYTES:
        raise ValueError("request too large")
    request = json.loads(content.decode("utf-8"), object_pairs_hook=unique_object, parse_constant=reject_constant)
    if not isinstance(request, dict):
        raise ValueError("request must be an object")
    candidate = request.get("operation_id")
    if not isinstance(candidate, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", candidate):
        raise ValueError("invalid operation ID")
    return request


def require_action(request, action):
    if type(request.get("protocol")) is not int or request["protocol"] != PROTOCOL or request.get("action") != action:
        raise ValueError("unsupported request")


def main():
    operation_id = "invalid"
    frame = dict(protocol=PROTOCOL, operation_id=operation_id, seq=1, type="error",
                 failure={"code": "invalid_input", "message": "Invalid doctor request.",
                          "retry": "none"})
    try:
        request = read_request()
        candidate = request["operation_id"]
        frame["operation_id"] = candidate
        require_action(request, "doctor")
        frame = dict(protocol=PROTOCOL, operation_id=candidate, seq=1, type="result", **diagnose())
    except (ValueError, UnicodeError):
        pass
    except Exception:
        frame["failure"] = {"code": "internal", "message": "Runtime inspection failed.", "retry": "none"}
    try:
        sys.stdout.write(json.dumps(frame, separators=(",", ":")) + "\n")
        sys.stdout.flush()
    except OSError:
        pass  # Closed stdout during cancellation must not emit a traceback.


if __name__ == "__main__":
    main()
