from contextlib import nullcontext
import importlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

prefix = "engine.connectors.audible.internal.bridge.adapter."
launcher = importlib.import_module(prefix + "noninteractive")
interactive = importlib.import_module(prefix + "interactive")
doctor = importlib.import_module(prefix + "doctor")
store = importlib.import_module(prefix + "auth_store")
sdk = importlib.import_module(prefix + "sdk_session")
ADAPTER = Path(launcher.__file__).parent


class LauncherTests(unittest.TestCase):
    def invoke(self, *, error=None, invalid=False, broken=False, loader_error=False):
        package = SimpleNamespace(doctor=SimpleNamespace(read_request=Mock(return_value={"operation_id": "fixture"}),
            require_action=Mock(side_effect=ValueError("fixture-private") if invalid else None)), auth_store=store,
            sdk_session=SimpleNamespace(authenticated=Mock(return_value=nullcontext("client")), ProviderError=sdk.ProviderError),
            library=SimpleNamespace(pages=Mock(return_value=[[{"fixture": "book"}]], side_effect=error)))
        output = io.StringIO()
        control = Mock(__enter__=Mock(return_value=output), __exit__=Mock(return_value=False))
        loader = Mock(return_value=package, side_effect=RuntimeError("fixture-private-loader") if loader_error else None)
        null = Mock()
        with (patch.object(launcher.os, "dup", return_value=17),
              patch.object(launcher.os, "fdopen", return_value=control),
              patch.object(launcher.os, "set_inheritable") as inherit):
            if broken:
                with patch.object(output, "write", side_effect=OSError("fixture-private-pipe")):
                    launcher.main({}, loader, null)
                return
            with patch.object(output, "flush", wraps=output.flush) as flushed:
                launcher.main({}, loader, null)
            self.assertEqual(flushed.call_count, len(output.getvalue().splitlines()))
        inherit.assert_called_once_with(3, False)
        null.assert_any_call((1, 2))
        if invalid:
            package.sdk_session.authenticated.assert_not_called()
        self.assertNotIn("fixture-private", output.getvalue())
        frames = [json.loads(line) for line in output.getvalue().splitlines()]
        self.assertEqual([frame["seq"] for frame in frames], list(range(1, len(frames) + 1)))
        return frames

    def test_policy_errors_and_flush_boundaries(self):
        self.assertEqual(self.invoke()[-1], dict(protocol=1, operation_id="fixture", seq=2, type="result", count=1))
        self.assertEqual(self.invoke(invalid=True)[0]["failure"], {"code": "invalid_input"})
        self.assertEqual(self.invoke(loader_error=True)[0]["operation_id"], "invalid")
        failures = [(KeyboardInterrupt(), "canceled"), (EOFError(), "canceled"),
                    (RuntimeError("fixture-private"), "internal"), (store.StoreError(), "storage_failed"),
                    (store.StoreError(True), "storage_uncertain")]
        failures += [(sdk.ProviderError(code), code) for code in sdk._FAILURES]
        for error, code in failures:
            self.assertEqual(self.invoke(error=error)[-1]["failure"], {"code": code})
        self.invoke(broken=True)
        output = io.StringIO()
        launcher.write_frame(output, {"x": "a" * ((4 << 20) - 8)})
        self.assertEqual(len(output.getvalue()), (4 << 20) + 1)
        with self.assertRaises(ValueError):
            launcher.write_frame(output, {"x": "a" * ((4 << 20) - 7)})

    def child(self, mode, request=None):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            root.chmod(0o700)
            fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
            try:
                snapshot = dict(locale_code="us", access_token="Atna|synthetic-never-issued",
                                refresh_token="Atnr|synthetic-never-issued", expires=1)
                if mode != "missing":
                    store.save(fd, json.dumps(snapshot).encode())
                sources = {name: (ADAPTER / (name + ".py")).read_text()
                           for name in ("doctor", "auth_store", "login", "sdk_session", "library")}
                # Test-only transport remapping emulates RunRoot's FD3; no path/auth
                # goes in the request. All production modules come from actual source.
                script = "import os\n"
                if fd != 3:
                    script += f"os.dup2({fd},3,inheritable=False)\nos.close({fd})\n"
                else:
                    script += "os.set_inheritable(3,False)\n"
                script += "SOURCES=" + repr(sources) + "\nMODE=" + repr(mode) + "\n"
                script += (ADAPTER / "interactive.py").read_text() + "\n"
                script += (ADAPTER / "noninteractive.py").read_text() + "\n"
                script += r'''
import audible, httpx, stat, subprocess, sys
from unittest.mock import Mock
original_load, original_client = load_sources, audible.Client

def fixture_load(sources, names):
    package = original_load(sources, names)
    def item(i):
        return dict(asin=f"T{i:09d}", title="Fixture", authors=None)
    def response(request):
        assert request.headers["x-amz-access-token"] == "Atna|synthetic-refreshed-never-issued"
        page = int(request.url.params["page"])
        if page == 1:
            return httpx.Response(200, json={"items": [item(i) for i in range(100)]})
        if MODE == "cancel":
            raise KeyboardInterrupt()
        status = 429 if MODE in ("denied", "storage_denied") else 200
        items = None if MODE == "malformed" else ([item(i) for i in range(100)] if MODE == "repeat" else [item(0), item(100)])
        return httpx.Response(status, json={"items": items, "message": "fixture-private-provider"})
    def client(**kwargs):
        if MODE == "invalid":
            raise AssertionError("SDK must not start on invalid request")
        os.write(1, b"fixture-private-stdout")
        os.write(2, b"fixture-private-stderr")
        assert os.read(0,1) == b""
        check = "import os\nfor fd in (3,4):\n try: os.fstat(fd)\n except OSError: pass\n else: raise SystemExit(2)"
        subprocess.run([sys.executable,"-I","-B","-c",check], close_fds=False, check=True)
        result = original_client(**kwargs, transport=httpx.MockTransport(response))
        if MODE == "close":
            original_close = result.close
            def close():
                original_close()
                raise RuntimeError("fixture-private-close")
            result.close = close
        return result
    audible.Client = client
    audible.auth.refresh_access_token = Mock(return_value=dict(access_token="Atna|synthetic-refreshed-never-issued", expires=4102444800))
    httpx.HTTPTransport.handle_request = Mock(side_effect=AssertionError("network forbidden"))
    if MODE in ("storage", "storage_denied"):
        package.auth_store.os.replace = Mock(side_effect=OSError("fixture-private-replace"))
    if MODE == "uncertain":
        original_fsync = os.fsync
        def fsync(fd):
            if stat.S_ISDIR(os.fstat(fd).st_mode):
                raise OSError("fixture-private-dir-fsync")
            original_fsync(fd)
        os.fsync = fsync
    return package
main(SOURCES, fixture_load, null_stdio)
'''
                raw = json.dumps(dict(protocol=1, operation_id="fixture-library", action="library")).encode() if request is None else request
                completed = subprocess.run([sys.executable, "-I", "-B", "-c", script], input=raw,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, pass_fds=(fd,), timeout=10, check=False)
                self.assertEqual(completed.returncode, 0, completed.stderr)
                self.assertEqual(completed.stderr, b"")
                self.assertNotIn(b"fixture-private", completed.stdout)
                self.assertNotIn(b"Atna|", completed.stdout)
                self.assertNotIn(b"Atnr|", completed.stdout)
                frames = [json.loads(line) for line in completed.stdout.splitlines()]
                self.assertEqual([f["seq"] for f in frames], list(range(1, len(frames) + 1)))
                self.assertTrue(all(f["protocol"] == 1 for f in frames))
                if mode != "missing":
                    saved = json.loads(store.load(fd))
                    unchanged = mode in ("storage", "storage_denied", "invalid")
                    self.assertEqual(saved["access_token"], snapshot["access_token"] if unchanged else "Atna|synthetic-refreshed-never-issued")
                return frames
            finally:
                os.close(fd)

    def test_actual_fileless_sdk_stream_persistence_and_fd_isolation(self):
        success = self.child("success")
        self.assertEqual([len(frame["books"]) for frame in success[:-1]], [100, 1])
        self.assertEqual(success[-1]["count"], 101)
        self.assertEqual(success[-1]["type"], "result")
        for mode, code in (("denied", "throttled"), ("repeat", "protocol_error"), ("malformed", "protocol_error"),
                           ("cancel", "canceled"), ("storage", "storage_failed"), ("storage_denied", "storage_failed"),
                           ("uncertain", "storage_uncertain"), ("close", "internal"), ("missing", "not_authenticated")):
            frames = self.child(mode)
            self.assertEqual(frames[-1]["failure"], {"code": code})
            self.assertFalse(any(frame["type"] == "result" for frame in frames))
            self.assertTrue(all(frame["type"] == "stage" for frame in frames[:-1]))

    def test_invalid_requests_never_construct_sdk(self):
        for raw in (b"{}", b"[]", b"\xff", b'{"operation_id":"bad ID"}',
                    b'{"protocol":1,"operation_id":"fixture","action":"login"}',
                    b'{"protocol":true,"operation_id":"fixture","action":"library"}',
                    b'{"protocol":1,"protocol":1,"operation_id":"fixture","action":"library"}',
                    b" " * (doctor.MAX_REQUEST_BYTES + 1)):
            frames = self.child("invalid", raw)
            self.assertEqual(len(frames), 1)
            self.assertEqual(frames[0]["failure"], {"code": "invalid_input"})
