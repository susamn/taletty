"""Private US browser login; host owns serialized root/TTY and child lifetime.

Use in a dedicated child with stdout/stderr discarded by the host. Only explicit
TTY writes are presentation; no final redirect or SDK data crosses public output.
"""

from contextlib import redirect_stderr, redirect_stdout
import json
import os
import termios
from urllib.parse import parse_qs, urlsplit
import webbrowser

from . import auth_store, doctor

MAX_REDIRECT_BYTES = 2048  # Below Linux canonical-line truncation; reject, never truncate.
_MESSAGES = {
    "canceled": "Sign-in canceled.",
    "terminal_required": "Sign-in requires an owned terminal.",
    "invalid_input": "Invalid sign-in URL or redirect.",
    "runtime_missing": "The Audible SDK is missing.",
    "runtime_incompatible": "The sign-in runtime is incompatible.",
    "login_failed": "Audible sign-in failed.",
    "storage_failed": "Credentials could not be saved.",
    "storage_uncertain": "Credentials published; durability not confirmed.",
}


class LoginError(Exception):
    def __init__(self, code):
        super().__init__(_MESSAGES[code])
        self.code = code


def _say(tty_fd, message):
    # Borrow the TTY; only the duplicate is closed, never the host descriptor.
    with os.fdopen(os.dup(tty_fd), "w", encoding="utf-8") as terminal:
        print(message, file=terminal, flush=True)


def _url(value, path, maximum):
    if not isinstance(value, str) or not 0 < len(value) <= maximum or any(not 33 <= ord(c) <= 126 for c in value):
        raise LoginError("invalid_input")
    parsed = urlsplit(value)
    if parsed.scheme != "https" or parsed.netloc != "www.amazon.com" or parsed.path != path or parsed.fragment:
        raise LoginError("invalid_input")
    return parsed


def _read_redirect(tty_fd):
    saved = termios.tcgetattr(tty_fd)
    hidden = saved[:]
    hidden[0] = (hidden[0] | termios.ICRNL) & ~(termios.IGNCR | termios.INLCR)
    hidden[3] = (hidden[3] | termios.ICANON | termios.ISIG) & ~(termios.ECHO | termios.ECHONL)
    try:
        termios.tcsetattr(tty_fd, termios.TCSAFLUSH, hidden)
        _say(tty_fd, "Final redirect URL (hidden; empty cancels):")
        content = bytearray()
        while len(content) <= MAX_REDIRECT_BYTES:
            character = os.read(tty_fd, 1)
            if not character:
                raise EOFError()
            if character == b"\n":
                return content.decode("ascii").strip()
            content.extend(character)
        raise LoginError("invalid_input")
    finally:
        # Parent must additionally restore on SIGKILL/TERM; finally is not a kill guarantee.
        termios.tcsetattr(tty_fd, termios.TCSAFLUSH, saved)


def _browser_login(url, tty_fd):
    _url(url, "/ap/signin", 8192)
    _say(tty_fd, "Open Amazon sign-in in your browser (fallback URL):\n" + url)
    try:
        webbrowser.open(url, new=1)
    except Exception:
        pass  # Fallback URL is already shown; never echo browser exceptions.
    _say(tty_fd, "After sign-in an error page is expected. Copy the entire final URL; treat it as a secret.")
    redirect = _read_redirect(tty_fd)
    if not redirect:
        raise LoginError("canceled")
    parsed = _url(redirect, "/ap/maplanding", MAX_REDIRECT_BYTES)
    codes = parse_qs(parsed.query, keep_blank_values=True, max_num_fields=64).get("openid.oa2.authorization_code", [])
    if len(codes) != 1 or not codes[0]:
        raise LoginError("invalid_input")
    return redirect


def _validate(auth):
    if auth.locale.country_code != "us" or not {"signing", "bearer"}.intersection(auth.available_auth_modes):
        raise LoginError("login_failed")


def login(root_fd, tty_fd):
    """Persist a validated SDK snapshot or raise a fixed LoginError; return no auth data.

    No CLI entry point. The host must retain both borrowed FDs, supply foreground
    terminal ownership and discard child stdio; process cancellation/restore is #73.
    """
    try:
        auth_store.check(root_fd)
        if not os.isatty(tty_fd):
            raise LoginError("terminal_required")
        state = doctor.python_status()
        if state == doctor.READY:
            state = doctor.sdk_status()
        if state != doctor.READY:
            raise LoginError(state)
        with open(os.devnull, "w") as sink, redirect_stdout(sink), redirect_stderr(sink):
            import audible
            auth = audible.Authenticator.from_login_external(
                locale="us", login_url_callback=lambda url: _browser_login(url, tty_fd))
            _validate(auth)
            payload = json.dumps(auth.to_dict(), allow_nan=False).encode("utf-8")
            # Verify the serialized representation too; SDK from_dict mutates its input.
            restored = audible.Authenticator.from_dict(json.loads(payload))
            _validate(restored)
        auth_store.save(root_fd, payload)
    except (KeyboardInterrupt, EOFError):
        raise LoginError("canceled") from None
    except auth_store.StoreError as error:
        raise LoginError("storage_uncertain" if error.published else "storage_failed") from None
    except LoginError:
        raise
    except Exception:
        raise LoginError("login_failed") from None
