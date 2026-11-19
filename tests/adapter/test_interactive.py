import importlib
import io
import json
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

interactive = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.interactive")
login = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.login")
ADAPTER = Path(login.__file__).parent


class InteractiveTests(unittest.TestCase):
    def package(self):
        return SimpleNamespace(
            doctor=SimpleNamespace(read_request=Mock(return_value={"operation_id": "fixture"}),
                                   require_action=Mock()),
            login=SimpleNamespace(login=Mock(), _validate=Mock(), LoginError=login.LoginError),
            auth_store=SimpleNamespace(load=Mock(return_value=b'{"fixture":"not-credentials"}')))

    def invoke(self, package=None, error=None, *, failed_write=False, failed_load=False):
        package = package or self.package()
        package.login.login.side_effect = error
        output = io.StringIO()
        control = Mock()
        control.__enter__ = Mock(return_value=output)
        control.__exit__ = Mock(return_value=False)
        sdk = SimpleNamespace(Authenticator=SimpleNamespace(from_dict=Mock(return_value="restored")))
        with (patch.object(interactive.os, "dup", return_value=17) as dup,
              patch.object(interactive.os, "fdopen", return_value=control),
              patch.object(interactive.os, "set_inheritable") as inherit,
              patch.object(interactive, "null_stdio") as null,
              patch.object(interactive, "load_sources", return_value=package,
                           side_effect=RuntimeError("fixture-private-loader") if failed_load else None),
              patch.object(interactive.importlib, "import_module", return_value=sdk)):
            if failed_write:
                with patch.object(output, "write", side_effect=OSError("fixture-private-pipe")):
                    interactive.main({})
                return
            interactive.main({})
        dup.assert_called_once_with(1)
        self.assertEqual(inherit.call_args_list[0].args, (3, False))
        self.assertEqual(inherit.call_args_list[1].args, (4, False))
        null.assert_any_call((1, 2))
        self.assertNotIn("fixture-private", output.getvalue())
        frame = json.loads(output.getvalue())
        self.assertEqual(frame["seq"], 1)
        self.assertEqual(frame["protocol"], 1)
        if frame["type"] == "result":
            null.assert_any_call((0,))
            package.login.login.assert_called_once_with(3, 4)
            package.auth_store.load.assert_called_once_with(3)
            package.login._validate.assert_called_once_with("restored")
            self.assertEqual(frame["status"], "signed_in")
            self.assertNotIn("failure", frame)
        return frame

    def test_success_and_fixed_failures(self):
        self.assertEqual(self.invoke()["type"], "result")
        for code in login._MESSAGES:
            with self.subTest(code=code):
                self.assertEqual(self.invoke(error=login.LoginError(code))["failure"], {"code": code})
        for error, code in ((RuntimeError("fixture-private-sdk"), "internal"),
                            (KeyboardInterrupt(), "canceled"), (EOFError(), "canceled"),
                            (ValueError("fixture-private-json"), "invalid_input"),
                            (UnicodeError("fixture-private-unicode"), "invalid_input")):
            self.assertEqual(self.invoke(error=error)["failure"], {"code": code})
        self.assertEqual(self.invoke(failed_load=True)["failure"], {"code": "internal"})
        self.invoke(failed_write=True)

    def test_invalid_request_and_snapshot_never_succeed(self):
        package = self.package()
        package.doctor.read_request.side_effect = ValueError("fixture-private-request")
        self.assertEqual(self.invoke(package)["operation_id"], "invalid")
        package.login.login.assert_not_called()
        package = self.package()
        package.doctor.require_action.side_effect = ValueError("bad version/action")
        self.assertEqual(self.invoke(package)["failure"], {"code": "invalid_input"})
        package.login.login.assert_not_called()
        package = self.package()
        package.auth_store.load.side_effect = OSError("fixture-private-snapshot")
        self.assertEqual(self.invoke(package)["failure"], {"code": "internal"})
        package = self.package()
        package.login._validate.side_effect = login.LoginError("login_failed")
        self.assertEqual(self.invoke(package)["failure"], {"code": "login_failed"})

    def test_actual_fileless_relative_imports(self):
        try:
            package = interactive.load_sources({name: (ADAPTER / (name + ".py")).read_text()
                                                for name in ("doctor", "auth_store", "login", "sdk_session")})
            self.assertIs(package.login.doctor, package.doctor)
            self.assertIs(package.login.auth_store, package.auth_store)
            self.assertIs(package.sdk_session.login, package.login)
            self.assertEqual(package.doctor.AUDIBLE_VERSION, "0.13.1")
        finally:
            for name in list(sys.modules):
                if name == "_taletty_login" or name.startswith("_taletty_login."):
                    del sys.modules[name]

    def test_stdio_redirects_actual_descriptor_numbers(self):
        with patch.object(interactive.os, "dup2") as dup:
            interactive.null_stdio((0, 1, 2))
        self.assertEqual([call.args[1] for call in dup.call_args_list], [0, 1, 2])
