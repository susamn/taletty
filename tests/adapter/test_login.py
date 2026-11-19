from contextlib import redirect_stderr, redirect_stdout
import importlib
import io
import json
import os
from pathlib import Path
import pty
import selectors
import subprocess
import sys
import tempfile
import termios
from types import SimpleNamespace
import unittest
from unittest.mock import patch

login = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.login")
ROOT = Path(__file__).resolve().parents[2]
SIGNIN = "https://www.amazon.com/ap/signin?fixture=public-challenge"
REDIRECT = "https://www.amazon.com/ap/maplanding?openid.oa2.authorization_code=fixture-never-issued"
# Deliberately invalid outside SDK shape tests; not issued by any provider/account.
SNAPSHOT = {"locale_code": "us", "access_token": "Atna|synthetic-not-issued", "expires": 4102444800}


class LoginTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.root.chmod(0o700)
        self.fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        self.addCleanup(os.close, self.fd)
        self.master, self.tty = pty.openpty()
        self.addCleanup(os.close, self.master)
        self.addCleanup(os.close, self.tty)
        os.set_blocking(self.master, False)
        self.target = self.root / "auth.json"
        login.auth_store.save(self.fd, b"prior-synthetic-snapshot")

    def fake_auth(self, data=None, modes=("bearer",), locale="us"):
        return SimpleNamespace(locale=SimpleNamespace(country_code=locale), available_auth_modes=modes,
                               to_dict=lambda: dict(SNAPSHOT if data is None else data))

    def invoke(self, *, auth=None, fail=None, browser_fail=False):
        import audible
        import httpx
        def provider(**kwargs):
            self.assertEqual(kwargs["locale"], "us")
            if fail is not None:
                raise fail
            print("fixture-private-sdk-banner")
            print("fixture-private-sdk-error", file=sys.stderr)
            self.assertEqual(kwargs["login_url_callback"](SIGNIN), REDIRECT)
            return auth or self.fake_auth()
        output, errors = io.StringIO(), io.StringIO()
        with (
            redirect_stdout(output), redirect_stderr(errors),
            patch.object(login.doctor, "sdk_status", return_value=login.doctor.READY),
            patch.object(login, "_read_redirect", return_value=REDIRECT),
            patch.object(login.webbrowser, "open", return_value=False,
                         side_effect=RuntimeError("fixture-private-browser") if browser_fail else None),
            patch.object(audible.Authenticator, "from_login_external", side_effect=provider),
            patch.object(httpx.Client, "send", side_effect=AssertionError("provider network forbidden")),
        ):
            try:
                result = login.login(self.fd, self.tty)
            finally:
                self.assertEqual(output.getvalue(), "")
                self.assertEqual(errors.getvalue(), "")
        return result

    def terminal_output(self):
        result = bytearray()
        while True:
            try:
                result.extend(os.read(self.master, 8192))
            except BlockingIOError:
                return bytes(result)

    def test_success_uses_real_sdk_restore_and_private_store_no_public_auth(self):
        for browser_fail in [False, True]:
            self.assertIsNone(self.invoke(browser_fail=browser_fail))
            self.assertEqual(json.loads(login.auth_store.load(self.fd)), SNAPSHOT)
            text = self.terminal_output()
            self.assertIn(SIGNIN.encode(), text)
            self.assertNotIn(b"fixture-never-issued", text)
            self.assertNotIn(b"fixture-private", text)
            self.assertEqual(self.target.stat().st_mode & 0o777, 0o600)
            termios.tcgetattr(self.tty)  # Borrowed terminal still owned by host.

    def test_provider_failure_cancel_and_invalid_snapshot_preserve_old_then_recover(self):
        for error, expected in [(RuntimeError("fixture-private-provider"), "login_failed"),
                                (KeyboardInterrupt(), "canceled"), (EOFError(), "canceled")]:
            with self.assertRaises(login.LoginError) as caught:
                self.invoke(fail=error)
            self.assertEqual(caught.exception.code, expected)
            self.assertNotIn("fixture-private", str(caught.exception))
            self.assertEqual(self.target.read_bytes(), b"prior-synthetic-snapshot")
        for auth in [self.fake_auth(locale="uk"), self.fake_auth(modes=("cookies",)),
                     self.fake_auth(data={"locale_code": "us"}),
                     self.fake_auth(data=dict(SNAPSHOT, future=float("nan"))),
                     self.fake_auth(data=dict(SNAPSHOT, future="x" * login.auth_store.MAX_AUTH_BYTES))]:
            with self.assertRaises(login.LoginError):
                self.invoke(auth=auth)
            self.assertEqual(self.target.read_bytes(), b"prior-synthetic-snapshot")
        self.assertIsNone(self.invoke())

    def test_preflight_starts_no_provider_on_unsafe_root_terminal_or_runtime(self):
        import audible
        with patch.object(audible.Authenticator, "from_login_external") as provider:
            self.root.chmod(0o755)
            with self.assertRaises(login.LoginError) as caught:
                login.login(self.fd, self.tty)
            self.assertEqual(caught.exception.code, "storage_failed")
            self.root.chmod(0o700)
            with self.assertRaises(login.LoginError) as caught:
                login.login(self.fd, self.fd)
            self.assertEqual(caught.exception.code, "terminal_required")
            for python, sdk, expected in [("runtime_incompatible", "ready", "runtime_incompatible"),
                                          ("ready", "runtime_missing", "runtime_missing"),
                                          ("ready", "runtime_incompatible", "runtime_incompatible")]:
                with patch.object(login.doctor, "python_status", return_value=python), patch.object(login.doctor, "sdk_status", return_value=sdk) as probe:
                    with self.assertRaises(login.LoginError) as caught:
                        login.login(self.fd, self.tty)
                    self.assertEqual(caught.exception.code, expected)
                    if python != "ready": probe.assert_not_called()
            provider.assert_not_called()

    def test_publication_failure_and_published_durability_uncertainty(self):
        with patch.object(login.auth_store.os, "replace", side_effect=OSError("fixture-private-path")):
            with self.assertRaises(login.LoginError) as caught:
                self.invoke()
            self.assertEqual(caught.exception.code, "storage_failed")
        self.assertEqual(self.target.read_bytes(), b"prior-synthetic-snapshot")
        sync = os.fsync
        def directory_failure(fd):
            if fd == self.fd: raise OSError("fixture-private-directory")
            sync(fd)
        with patch.object(login.auth_store.os, "fsync", side_effect=directory_failure):
            with self.assertRaises(login.LoginError) as caught:
                self.invoke()
            self.assertEqual(caught.exception.code, "storage_uncertain")
        self.assertEqual(json.loads(self.target.read_bytes()), SNAPSHOT)
        self.assertIsNone(self.invoke())

    def test_url_allowlist_bounds_and_no_unsafe_browser_launch(self):
        invalid = [None, "", "https://www.amazon.com/ap/signin#fragment", "https://evil.invalid/ap/signin",
                   "http://www.amazon.com/ap/signin", "https://www.amazon.com:443/ap/signin",
                   "https://user@www.amazon.com/ap/signin", SIGNIN + "\x1b[31m", SIGNIN + "x" * 8192]
        with patch.object(login.webbrowser, "open") as browser:
            for url in invalid:
                with self.subTest(url=url), self.assertRaises(login.LoginError):
                    login._browser_login(url, self.tty)
            browser.assert_not_called()
        self.assertEqual(self.terminal_output(), b"")
        for redirect in ["", REDIRECT.replace("https:", "http:"), REDIRECT.replace("maplanding", "signin"),
                         "https://www.amazon.com/ap/maplanding", REDIRECT + "&openid.oa2.authorization_code=other",
                         "https://www.amazon.com/ap/maplanding?openid.oa2.authorization_code=",
                         REDIRECT + "x" * login.MAX_REDIRECT_BYTES]:
            with patch.object(login.webbrowser, "open", return_value=False), patch.object(login, "_read_redirect", return_value=redirect):
                with self.assertRaises(login.LoginError):
                    login._browser_login(SIGNIN, self.tty)

    def test_hidden_setup_and_read_errors_never_fall_back_and_restore(self):
        saved = termios.tcgetattr(self.tty)
        for failure in [OSError("fixture-private-tty"), KeyboardInterrupt(), EOFError()]:
            with patch.object(login.os, "read", side_effect=failure):
                with self.assertRaises(type(failure)):
                    login._read_redirect(self.tty)
            self.assertEqual(termios.tcgetattr(self.tty), saved)
        setter = termios.tcsetattr
        count = 0
        def fail_setup(fd, action, attrs):
            nonlocal count
            count += 1
            if count == 1: raise OSError("fixture-private-echo")
            setter(fd, action, attrs)
        with patch.object(login.termios, "tcsetattr", side_effect=fail_setup), patch.object(login.os, "read") as read:
            with self.assertRaises(OSError): login._read_redirect(self.tty)
            read.assert_not_called()
        self.assertEqual(termios.tcgetattr(self.tty), saved)

    def test_bounded_hidden_reader_byte_handling_and_restoration(self):
        saved = termios.tcgetattr(self.tty)
        for content, expected in [(b"fixture-line\n", "fixture-line"), (b"", EOFError),
                                  (b"x" * (login.MAX_REDIRECT_BYTES + 1), login.LoginError)]:
            data = iter(bytes([value]) for value in content)
            def read(fd, count):
                self.assertEqual((fd, count), (self.tty, 1))
                self.assertFalse(termios.tcgetattr(fd)[3] & (termios.ECHO | termios.ECHONL))
                return next(data, b"")
            with patch.object(login.os, "read", side_effect=read):
                if isinstance(expected, str): self.assertEqual(login._read_redirect(self.tty), expected)
                else:
                    with self.assertRaises(expected): login._read_redirect(self.tty)
            self.assertEqual(termios.tcgetattr(self.tty), saved)

    def test_real_pty_hidden_input_eof_interrupt_overflow_and_exact_restore(self):
        script = '''
import fcntl, importlib, os, sys, termios
sys.path.insert(0, sys.argv[1])  # Explicit fixture-only source import, not runtime discovery.
module = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.login")
fd = int(sys.argv[2])
os.setsid()
fcntl.ioctl(fd, termios.TIOCSCTTY, 0)
saved = termios.tcgetattr(fd)
try:
    module._read_redirect(fd)
    print("success")
except EOFError: print("eof")
except KeyboardInterrupt: print("canceled")
except module.LoginError as error: print(error.code)
assert termios.tcgetattr(fd) == saved
'''
        for kind, expected in [("success", b"success"), ("eof", b"eof"),
                               ("interrupt", b"canceled"), ("overflow", b"invalid_input")]:
            with self.subTest(kind=kind):
                # Each child gets a fresh real controlling PTY; no live provider/browser.
                master, slave = pty.openpty()
                saved = termios.tcgetattr(slave)
                child = subprocess.Popen([sys.executable, "-I", "-B", "-c", script, str(ROOT), str(slave)],
                                         pass_fds=(slave,), stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                try:
                    text = bytearray()
                    with selectors.DefaultSelector() as ready:
                        ready.register(master, selectors.EVENT_READ)
                        while b"Final redirect URL" not in text:
                            self.assertTrue(ready.select(timeout=5), "hidden-input barrier missing")
                            text.extend(os.read(master, 4096))
                    flags = termios.tcgetattr(slave)[3]
                    self.assertFalse(flags & (termios.ECHO | termios.ECHONL))
                    if kind == "interrupt":
                        os.write(master, b"\x03")
                    else:
                        if kind == "success":
                            content = REDIRECT.encode() + b"\n"
                        elif kind == "eof":
                            content = b"\x04"
                        else:
                            content = b"x" * (login.MAX_REDIRECT_BYTES + 1) + b"\n"
                        os.write(master, content)
                    output, errors = child.communicate(timeout=5)
                    self.assertEqual((child.returncode, output.strip(), errors), (0, expected, b""))
                    self.assertEqual(termios.tcgetattr(slave), saved)
                    os.set_blocking(master, False)
                    try: text.extend(os.read(master, 4096))
                    except BlockingIOError: pass
                    self.assertNotIn(b"fixture-never-issued", text)
                finally:
                    if child.poll() is None: child.kill()
                    child.communicate(timeout=5)
                    os.close(master)
                    os.close(slave)
