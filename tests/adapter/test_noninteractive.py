from contextlib import nullcontext
import importlib
import io
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time
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
        self.invoke(invalid=True, broken=True)
        with (patch.object(launcher.os, "dup", return_value=17),
              patch.object(launcher.os, "fdopen", return_value=Mock(__enter__=Mock(return_value=io.StringIO()),
                                                                    __exit__=Mock(side_effect=BrokenPipeError("close")))),
              patch.object(launcher.os, "set_inheritable")):
            launcher.main({}, Mock(side_effect=RuntimeError("fixture-private")), Mock())
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

    def download_child(self, mode, *, content_length=None, close_on_progress=False, sigterm_group=False, sigint_group=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            root.chmod(0o700)
            fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
            try:
                snapshot = dict(
                    locale_code="us",
                    access_token="Atna|synthetic-never-issued",
                    refresh_token="Atnr|synthetic-never-issued",
                    expires=1,
                    device_info={"device_type": "fixture-type", "device_serial_number": "fixture-serial"},
                    customer_info={"user_id": "fixture-user"},
                )
                store.save(fd, json.dumps(snapshot).encode())
                names = ("doctor", "auth_store", "login", "sdk_session", "library",
                         "chapters", "licensing", "media_store", "remux", "composition", "reconciliation")
                sources = {name: (ADAPTER / (name + ".py")).read_text() for name in names}
                barrier_path = str(root / "barrier.sock")
                script = "import os\n"
                if fd != 3:
                    script += f"os.dup2({fd},3,inheritable=False)\nos.close({fd})\n"
                else:
                    script += "os.set_inheritable(3,False)\n"
                script += (
                    f"SOURCES={sources!r}\nMODE={mode!r}\n"
                    f"CONTENT_LENGTH={content_length!r}\nBARRIER={barrier_path!r}\n"
                    f"CLOSE_BARRIER={close_on_progress!r}\n"
                )
                script += (ADAPTER / "interactive.py").read_text() + "\n"
                script += (ADAPTER / "noninteractive.py").read_text() + "\n"
                script += r'''
import audible, audible.auth, base64, hashlib, httpx, json, socket, subprocess, sys
from audible.aescipher import aes_cbc_encrypt
from unittest.mock import Mock
original_load, original_client, original_http = load_sources, audible.Client, httpx.Client
URL = "https://media.example.invalid/secret-signed-url?token=fixture-private"
KEY, IV = "01" * 16, "02" * 16
PAYLOAD = b"synthetic-media-" + b"x" * (1024 * 1024)

def fixture_load(sources, names):
    package = original_load(sources, names)
    def license_response(request):
        if request.method == "POST":
            digest = hashlib.sha256(("fixture-type" + "fixture-serial" + "fixture-user" + "B000000001").encode()).digest()
            text = json.dumps({"key": KEY, "iv": IV})
            text += "\x00" * (-len(text) % 16)
            voucher = base64.b64encode(aes_cbc_encrypt(digest[:16], digest[16:], text, padding="none")).decode()
            return httpx.Response(200, json={"content_license": {
                "status_code": "Granted", "asin": "B000000001", "license_response": voucher,
                "content_metadata": {"content_url": {"offline_url": URL},
                                     "chapter_info": {"chapters": [{"title": "Ch1", "start_offset_ms": 0, "length_ms": 1000}]}}
            }})
        return httpx.Response(200, json={"content_metadata": {}})
    def client(**kwargs):
        return original_client(**kwargs, transport=httpx.MockTransport(license_response))
    audible.Client = client
    audible.auth.refresh_access_token = Mock(return_value=dict(access_token="Atna|synthetic-refreshed-never-issued", expires=4102444800))
    class Body(httpx.SyncByteStream):
        def __iter__(self):
            yield PAYLOAD
            if CLOSE_BARRIER:
                with socket.socket(socket.AF_UNIX) as s:
                    s.settimeout(8)
                    s.connect(BARRIER)
                    assert s.recv(1) == b"G"
            yield b"tail"
    def media_response(request):
        headers = {}
        if CONTENT_LENGTH is not None:
            headers["Content-Length"] = CONTENT_LENGTH
        return httpx.Response(200, headers=headers, stream=Body())
    def http(**kwargs):
        if "transport" not in kwargs:
            kwargs["transport"] = httpx.MockTransport(media_response)
        return original_http(**kwargs)
    httpx.Client = http
    httpx.HTTPTransport.handle_request = Mock(side_effect=AssertionError("network forbidden"))
    if MODE == "ffmpeg_group":
        def copy(source, output, media, info=None, plan=None, cancel_fd=None):
            child_code = (
                "import os, signal, socket\n"
                f"s = socket.socket(socket.AF_UNIX)\ns.connect({BARRIER!r})\n"
                "s.sendall((str(os.getpid()) + '\\n').encode())\ns.close()\n"
                "signal.pause()\n"
            )
            proc = subprocess.Popen([sys.executable, "-I", "-B", "-c", child_code],
                                    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                proc.wait()
            finally:
                if proc.poll() is None:
                    proc.kill()
                    proc.wait()
            raise package.remux.RemuxError("canceled")
        package.remux.copy = copy
    else:
        def copy(source, output, media, info=None, plan=None, cancel_fd=None):
            output.write(os.pread(source.fd, 64, 0))
            return package.chapters.prepare(info)[0]
        package.remux.copy = copy
        def probe(argv, fds, *args):
            if not os.pread(fds[0], 64, 0).startswith(b"synthetic-media-"):
                return b"{}"
            return json.dumps(dict(format=dict(format_name="mov,mp4", duration="1"),
                streams=[dict(codec_type="audio", codec_name="aac", nb_read_packets="1")],
                chapters=[dict(start_time="0", end_time="1", tags=dict(title="Ch1"))])).encode()
        package.remux._run = probe
    return package
main(SOURCES, fixture_load, null_stdio, action="download")
'''
                raw = json.dumps(dict(
                    protocol=1, operation_id="fixture-download", action="download",
                    book_id=dict(provider="audible", namespace="us", id="B000000001"),
                )).encode()
                if close_on_progress:
                    listener = socket.socket(socket.AF_UNIX)
                    listener.bind(barrier_path)
                    listener.listen(1)
                    listener.settimeout(8)
                    self.addCleanup(listener.close)
                    proc = subprocess.Popen(
                        [sys.executable, "-I", "-B", "-c", script],
                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                        pass_fds=(fd,), start_new_session=True,
                    )
                    try:
                        proc.stdin.write(raw)
                        proc.stdin.close()
                        seen = []
                        for _ in range(3):
                            line = proc.stdout.readline()
                            self.assertTrue(line)
                            seen.append(json.loads(line))
                        self.assertEqual([f["type"] for f in seen], ["stage", "stage", "progress"])
                        conn, _ = listener.accept()
                        proc.stdout.close()
                        with conn:
                            conn.sendall(b"G")
                        stderr = proc.stderr.read()
                        proc.stderr.close()
                        rc = proc.wait(timeout=10)
                    finally:
                        if proc.poll() is None:
                            os.killpg(proc.pid, signal.SIGKILL)
                            proc.wait()
                    self.assertEqual(rc, 0, stderr)
                    self.assertEqual(stderr, b"")
                    downloads = root / "downloads"
                    if downloads.exists():
                        self.assertEqual(list(downloads.iterdir()), [])
                    return seen
                if sigterm_group or sigint_group:
                    listener = socket.socket(socket.AF_UNIX)
                    listener.bind(barrier_path)
                    listener.listen(1)
                    listener.settimeout(8)
                    self.addCleanup(listener.close)
                    proc = subprocess.Popen(
                        [sys.executable, "-I", "-B", "-c", script],
                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                        pass_fds=(fd,), start_new_session=True,
                    )
                    try:
                        proc.stdin.write(raw)
                        proc.stdin.close()
                        conn, _ = listener.accept()
                        with conn:
                            ffmpeg_pid = int(conn.recv(64).decode().strip())
                        os.killpg(proc.pid, signal.SIGINT if sigint_group else signal.SIGTERM)
                        stdout = proc.stdout.read()
                        stderr = proc.stderr.read()
                        proc.stdout.close()
                        proc.stderr.close()
                        rc = proc.wait(timeout=10)
                    finally:
                        if proc.poll() is None:
                            os.killpg(proc.pid, signal.SIGKILL)
                            proc.wait()
                    if sigint_group:
                        self.assertEqual(rc, 0, stderr)
                        downloads = root / "downloads"
                        if downloads.exists():
                            self.assertEqual(list(downloads.iterdir()), [])
                    else:
                        self.assertNotEqual(rc, 0)
                    self.assertEqual(stderr, b"")
                    for _ in range(50):
                        try:
                            os.kill(ffmpeg_pid, 0)
                        except ProcessLookupError:
                            break
                        time.sleep(0.02)
                    else:
                        self.fail("ffmpeg group member survived signal")
                    frames = [json.loads(line) for line in stdout.splitlines()]
                    self.assertFalse(any(f.get("stage") == "publishing" for f in frames))
                    if sigint_group:
                        self.assertEqual(frames[-1]["type"], "result")
                        self.assertEqual(frames[-1]["outcome"]["media_code"], "canceled")
                        self.assertFalse(frames[-1]["outcome"]["ready"])
                    else:
                        self.assertFalse(any(f["type"] == "result" for f in frames))
                    return frames
                completed = subprocess.run(
                    [sys.executable, "-I", "-B", "-c", script], input=raw,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, pass_fds=(fd,), timeout=10, check=False,
                )
                self.assertEqual(completed.returncode, 0, completed.stderr)
                self.assertEqual(completed.stderr, b"")
                for secret in (b"fixture-private", b"secret-signed-url", b"Atna|", b"Atnr|", b"01" * 16, b"02" * 16):
                    self.assertNotIn(secret, completed.stdout)
                frames = [json.loads(line) for line in completed.stdout.splitlines()]
                self.assertEqual([f["seq"] for f in frames], list(range(1, len(frames) + 1)))
                self.assertTrue(all(f["protocol"] == 1 and f["operation_id"] == "fixture-download" for f in frames))
                # Second run on same root verifies ready reuse skips all stages/progress.
                reuse = subprocess.run(
                    [sys.executable, "-I", "-B", "-c", script], input=raw,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, pass_fds=(fd,), timeout=10, check=False,
                )
                self.assertEqual(reuse.returncode, 0, reuse.stderr)
                reuse_frames = [json.loads(line) for line in reuse.stdout.splitlines()]
                self.assertEqual(len(reuse_frames), 1)
                self.assertEqual((reuse_frames[0]["seq"], reuse_frames[0]["type"]), (1, "result"))
                self.assertTrue(reuse_frames[0]["outcome"]["reused"])
                return frames
            finally:
                os.close(fd)

    def test_download_progress_reuse_closed_pipe_and_sigterm_ffmpeg_group(self):
        total = (1024 * 1024) + len(b"synthetic-media-") + len(b"tail")
        known = self.download_child("success", content_length=str(total))
        self.assertEqual([(f["type"], f.get("stage")) for f in known], [
            ("stage", "licensing"),
            ("stage", "downloading"),
            ("progress", None),
            ("progress", None),
            ("stage", "preparing"),
            ("stage", "publishing"),
            ("result", None),
        ])
        self.assertEqual(known[2]["bytes"], 1024 * 1024)
        self.assertEqual(known[2]["total"], total)
        self.assertEqual(known[3]["bytes"], total)
        self.assertEqual(known[3]["total"], total)
        unknown = self.download_child("success", content_length=None)
        self.assertNotIn("total", unknown[2])
        self.assertNotIn("total", unknown[3])
        self.download_child("success", content_length=str(total), close_on_progress=True)
        self.download_child("ffmpeg_group", content_length=str(total), sigterm_group=True)
        self.download_child("ffmpeg_group", content_length=str(total), sigint_group=True)
        # Unit test: closed control pipe during progress never manufactures success.
        output = io.StringIO()
        orig_write = output.write
        writes = 0
        def write_fail(text):
            nonlocal writes
            writes += 1
            if writes == 3:
                raise BrokenPipeError("closed control")
            return orig_write(text)
        package = SimpleNamespace(
            doctor=SimpleNamespace(
                read_request=Mock(return_value={
                    "operation_id": "fixture",
                    "book_id": {"provider": "audible", "namespace": "us", "id": "B000000001"},
                }),
                require_action=Mock(),
            ),
            auth_store=store,
            sdk_session=sdk,
            media_store=SimpleNamespace(_leaf=Mock()),
            composition=SimpleNamespace(run=lambda fd, asin, emit=None: (
                emit("stage", stage="licensing"),
                emit("stage", stage="downloading"),
                emit("progress", bytes=10),
            )),
            reconciliation=Mock(),
        )
        with (patch.object(launcher.os, "dup", return_value=17),
              patch.object(launcher.os, "fdopen", return_value=nullcontext(output)),
              patch.object(launcher.os, "set_inheritable"),
              patch.object(output, "write", side_effect=write_fail)):
            launcher.main({}, Mock(return_value=package), Mock(), action="download")
        self.assertEqual(writes, 3)
        self.assertEqual(len(output.getvalue().splitlines()), 2)
