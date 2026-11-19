from contextlib import redirect_stdout
import importlib.util
import io
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SOURCE = Path(__file__).resolve().parents[2] / "engine/connectors/audible/internal/bridge/adapter/doctor.py"
spec = importlib.util.spec_from_file_location("taletty_doctor", SOURCE)
doctor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(doctor)


class DoctorTests(unittest.TestCase):
    def invoke(self, raw, **kwargs):
        output = io.StringIO()
        with patch.object(sys, "stdin", SimpleNamespace(buffer=io.BytesIO(raw))), redirect_stdout(output):
            with patch.object(doctor, "diagnose", **kwargs) as inspect:
                doctor.main()
        self.assertEqual(len(output.getvalue().splitlines()), 1)
        return json.loads(output.getvalue()), inspect

    def test_request_validation_never_inspects_dependencies(self):
        good = {"protocol": 1, "operation_id": "fixture", "action": "doctor"}
        inputs = [b"", b"\xff", b"[]", b"null", b"{", b"{}",
                  b'{"protocol":1,"protocol":1}', b'{"extra":NaN}', b"x" * (doctor.MAX_REQUEST_BYTES + 2)]
        for key, value in [("protocol", True), ("protocol", 2), ("action", "library"),
                           ("operation_id", ""), ("operation_id", 1), ("operation_id", "a" * 65),
                           ("operation_id", "fixture-private/path")]:
            inputs.append(json.dumps(dict(good, **{key: value})).encode())
        for raw in inputs:
            with self.subTest(raw=raw[:50]):
                frame, inspect = self.invoke(raw)
                self.assertEqual(frame["type"], "error")
                self.assertEqual(frame["failure"]["code"], "invalid_input")
                self.assertNotIn("fixture-private", json.dumps(frame))
                inspect.assert_not_called()

    def test_terminal_and_exact_request_limit(self):
        raw = b'{"protocol":1,"operation_id":"fixture","action":"doctor","future":true}'
        result = dict.fromkeys(("python", "audible", "ffmpeg", "ffprobe"), doctor.READY)
        for data in [raw, raw + b"\n", b" " * (doctor.MAX_REQUEST_BYTES-len(raw)) + raw + b"\n"]:
            frame, inspect = self.invoke(data, return_value=result)
            self.assertEqual(frame, dict(protocol=1, operation_id="fixture", seq=1, type="result", **result))
            inspect.assert_called_once_with()
        frame, _ = self.invoke(raw, side_effect=RuntimeError("fixture-private-exception"))
        self.assertEqual(frame["failure"]["code"], "internal")
        self.assertNotIn("fixture-private", json.dumps(frame))

    def test_sdk_missing_incompatible_broken_and_importable(self):
        for failure, state in [(doctor.metadata.PackageNotFoundError(), doctor.MISSING),
                               (RuntimeError("fixture-private"), doctor.INCOMPATIBLE)]:
            with patch.object(doctor.metadata, "version", side_effect=failure):
                self.assertEqual(doctor.sdk_status(), state)
        with patch.object(doctor.metadata, "version", return_value="not-the-pin"):
            self.assertEqual(doctor.sdk_status(), doctor.INCOMPATIBLE)
        with patch.object(doctor.metadata, "version", return_value=doctor.AUDIBLE_VERSION):
            with patch.object(doctor.importlib, "import_module", side_effect=RuntimeError("private")):
                self.assertEqual(doctor.sdk_status(), doctor.INCOMPATIBLE)
            # Fake SDK fails if the probe constructs an authenticated client.
            class NoProvider:
                def __getattr__(self, name):
                    raise AssertionError("doctor attempted provider access")
            with patch.object(doctor.importlib, "import_module", return_value=NoProvider()):
                self.assertEqual(doctor.sdk_status(), doctor.READY)

    def test_tool_readiness_timeout_and_failed_execution(self):
        with patch.object(doctor.shutil, "which", return_value=None):
            self.assertEqual(doctor.tool_status("fixture"), doctor.MISSING)
        with patch.object(doctor.shutil, "which", return_value="/fixture/tool"):
            for failure in [OSError("private"), subprocess.TimeoutExpired("private", 2)]:
                with patch.object(doctor.subprocess, "run", side_effect=failure):
                    self.assertEqual(doctor.tool_status("fixture"), doctor.INCOMPATIBLE)
            for code, state in [(0, doctor.READY), (7, doctor.INCOMPATIBLE)]:
                with patch.object(doctor.subprocess, "run", return_value=SimpleNamespace(returncode=code)) as run:
                    self.assertEqual(doctor.tool_status("fixture"), state)
                    self.assertEqual(run.call_args.kwargs["timeout"], 2)
                    self.assertEqual(run.call_args.kwargs["stdout"], subprocess.DEVNULL)

    def test_python_compatibility_precedes_other_probes(self):
        for implementation, version in [("pypy", (3, 12)), ("cpython", (3, 11))]:
            with patch.object(doctor.sys, "implementation", SimpleNamespace(name=implementation)), patch.object(doctor.sys, "version_info", version), patch.object(doctor, "sdk_status") as sdk:
                result = doctor.diagnose()
                self.assertEqual(result["python"], doctor.INCOMPATIBLE)
                self.assertEqual(result["audible"], doctor.NOT_CHECKED)
                sdk.assert_not_called()
        with patch.object(doctor, "sdk_status", return_value=doctor.MISSING), patch.object(doctor, "tool_status", return_value=doctor.READY):
            self.assertEqual(doctor.diagnose(), dict(python=doctor.READY, audible=doctor.MISSING, ffmpeg=doctor.READY, ffprobe=doctor.READY))

    def test_entrypoint_read_error_and_closed_stdout_are_sanitized(self):
        raw = b'{"protocol":1,"operation_id":"fixture","action":"doctor"}'
        output = io.StringIO()
        with patch.object(sys, "stdin", SimpleNamespace(buffer=io.BytesIO(raw))), redirect_stdout(output):
            runpy.run_path(str(SOURCE), run_name="__main__")
        self.assertEqual(json.loads(output.getvalue())["type"], "result")
        broken = SimpleNamespace(read=lambda size: (_ for _ in ()).throw(OSError("private")))
        with patch.object(sys, "stdin", SimpleNamespace(buffer=broken)), redirect_stdout(output := io.StringIO()):
            doctor.main()
        self.assertEqual(json.loads(output.getvalue())["failure"]["code"], "internal")
        with patch.object(sys, "stdin", SimpleNamespace(buffer=io.BytesIO(raw))), patch.object(sys, "stdout") as out:
            out.write.side_effect = BrokenPipeError("private")
            doctor.main()

    def test_actual_pinned_sdk_probe_leaves_private_home_untouched(self):
        with tempfile.TemporaryDirectory() as home, patch.dict(os.environ, {"HOME": home, "XDG_CONFIG_HOME": home}):
            sentinel = Path(home) / "old-app-do-not-touch"
            sentinel.write_bytes(b"synthetic fixture, not credentials")
            result = doctor.diagnose()
            self.assertEqual(result["python"], doctor.READY)
            self.assertEqual(result["audible"], doctor.READY)
            if os.environ.get("TALETTY_REQUIRE_MEDIA_TOOLS") == "1":
                self.assertEqual(result["ffmpeg"], doctor.READY)
                self.assertEqual(result["ffprobe"], doctor.READY)
            self.assertEqual(list(Path(home).iterdir()), [sentinel])
            self.assertEqual(sentinel.read_bytes(), b"synthetic fixture, not credentials")
