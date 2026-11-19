from contextlib import redirect_stderr, redirect_stdout
import importlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import audible
import httpx

session = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.sdk_session")
SNAPSHOT = {"locale_code": "us", "access_token": "Atna|synthetic-not-issued",
            "refresh_token": "Atnr|synthetic-not-issued", "expires": 1}
REFRESHED = {"access_token": "Atna|synthetic-refreshed-not-issued", "expires": 4102444800}


class SDKSessionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.root.chmod(0o700)
        self.fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        self.addCleanup(os.close, self.fd)
        self.target = self.root / "auth.json"
        session.auth_store.save(self.fd, json.dumps(SNAPSHOT).encode())
        self.clients = []
        self.sdk_client = audible.Client

    def factory(self, status=200, close_error=False, network=False):
        def response(request):
            self.assertEqual(request.headers["x-amz-access-token"], REFRESHED["access_token"])
            if network:
                raise httpx.ConnectError("fixture-private-network", request=request)
            return httpx.Response(status, json={"message": "fixture-private-provider", "fixture": True})
        def create(**kwargs):
            client = self.sdk_client(**kwargs, transport=httpx.MockTransport(response))
            self.clients.append(client)
            if close_error:
                close = client.close
                def fail_close():
                    close()
                    raise RuntimeError("fixture-private-close")
                client.close = fail_close
            return client
        return create

    def invoke(self, status=200, *, close_error=False, network=False):
        output, errors = io.StringIO(), io.StringIO()
        with (redirect_stdout(output), redirect_stderr(errors),
              patch.object(audible, "Client", side_effect=self.factory(status, close_error, network)),
              patch("audible.auth.refresh_access_token", return_value=REFRESHED) as refresh,
              patch.object(httpx.HTTPTransport, "handle_request", side_effect=AssertionError("network forbidden"))):
            try:
                with session.authenticated(self.fd) as client:
                    print("fixture-private-sdk-stdout")
                    print("fixture-private-sdk-stderr", file=sys.stderr)
                    result = client.get("library")
                return result
            finally:
                self.assertTrue(self.clients[-1].session.is_closed)
                refresh.assert_called_once()
                self.assertEqual(output.getvalue() + errors.getvalue(), "")

    def test_real_sdk_refresh_survives_success_and_provider_failures(self):
        for status, code in ((200, None), (401, "not_authenticated"), (403, "not_authenticated"),
                             (429, "throttled"), (500, "internal")):
            with self.subTest(status=status):
                session.auth_store.save(self.fd, json.dumps(SNAPSHOT).encode())
                if code is None:
                    self.assertTrue(self.invoke(status)["fixture"])
                else:
                    with self.assertRaises(session.ProviderError) as caught:
                        self.invoke(status)
                    self.assertEqual(caught.exception.code, code)
                    self.assertNotIn("fixture-private", str(caught.exception))
                saved = json.loads(session.auth_store.load(self.fd))
                self.assertEqual(saved["access_token"], REFRESHED["access_token"])
                self.assertFalse(os.get_inheritable(self.fd))
        session.auth_store.save(self.fd, json.dumps(SNAPSHOT).encode())
        with self.assertRaises(session.ProviderError) as caught:
            self.invoke(network=True)
        self.assertEqual(caught.exception.code, "network")

    def test_safe_failure_identity_close_precedence_and_constructor_failure(self):
        denied = session.ProviderError("entitlement_denied")
        with patch.object(audible, "Client", side_effect=self.factory(close_error=True)):
            with self.assertRaises(session.ProviderError) as caught:
                with session.authenticated(self.fd):
                    raise denied
        self.assertIs(caught.exception, denied)
        self.assertEqual(denied.retry, "check_entitlement")
        self.assertTrue(self.clients[-1].session.is_closed)
        with self.assertRaises(session.ProviderError) as caught:
            self.invoke(close_error=True)
        self.assertEqual(caught.exception.code, "internal")
        def broken(**kwargs):
            kwargs["auth"].access_token = REFRESHED["access_token"]
            raise RuntimeError("fixture-private-constructor")
        with patch.object(audible, "Client", side_effect=broken):
            with self.assertRaises(session.ProviderError) as caught:
                with session.authenticated(self.fd):
                    self.fail("constructor failed")
        self.assertEqual(caught.exception.code, "internal")
        self.assertEqual(json.loads(session.auth_store.load(self.fd))["access_token"], REFRESHED["access_token"])
        for error, code in ((KeyboardInterrupt(), "canceled"), (audible.exceptions.NotResponding(), "network"),
                            (audible.exceptions.AuthFlowError(), "not_authenticated"),
                            (audible.exceptions.NoRefreshToken(), "not_authenticated")):
            with patch.object(audible, "Client", side_effect=self.factory()):
                with self.assertRaises(session.ProviderError) as caught:
                    with session.authenticated(self.fd):
                        raise error
            self.assertEqual(caught.exception.code, code)

    def test_invalid_missing_unsafe_and_runtime_fail_before_client(self):
        malformed = [b"bad-json", b"\xff", b"[]", b'{"expires":NaN}', b'{"locale_code":"us","locale_code":"de"}',
                     b"{}", json.dumps(dict(SNAPSHOT, locale_code="de")).encode(),
                     b'{"locale_code":"us","website_cookies":{"fixture":"cookie-only"}}']
        with patch.object(audible, "Client") as client:
            for payload in malformed:
                session.auth_store.save(self.fd, payload)
                with self.assertRaises(session.ProviderError) as caught:
                    with session.authenticated(self.fd):
                        self.fail("invalid snapshot accepted")
                self.assertEqual(caught.exception.code, "not_authenticated")
                self.assertEqual(session.auth_store.load(self.fd), payload)
            self.target.unlink()
            with self.assertRaises(session.ProviderError):
                with session.authenticated(self.fd):
                    self.fail("missing auth accepted")
            session.auth_store.save(self.fd, json.dumps(SNAPSHOT).encode())
            self.root.chmod(0o755)
            with self.assertRaises(session.auth_store.StoreError):
                with session.authenticated(self.fd):
                    self.fail("unsafe root accepted")
            self.root.chmod(0o700)
            for python, sdk in (("runtime_incompatible", "ready"), ("ready", "runtime_missing")):
                with (patch.object(session.doctor, "python_status", return_value=python),
                      patch.object(session.doctor, "sdk_status", return_value=sdk)):
                    with self.assertRaises(session.ProviderError) as caught:
                        with session.authenticated(self.fd):
                            self.fail("bad runtime accepted")
                    self.assertEqual(caught.exception.code, python if python != "ready" else sdk)
            with patch.object(session.os, "set_inheritable", side_effect=OSError("fixture-private-fd")):
                with self.assertRaises(session.auth_store.StoreError):
                    with session.authenticated(self.fd):
                        self.fail("unsafe inheritance accepted")
            client.assert_not_called()

    def test_persistence_precommit_uncertainty_serialization_and_recovery(self):
        for published in (False, True):
            before = session.auth_store.load(self.fd)
            fsync = os.fsync
            def fail_dir(fd):
                if fd == self.fd:
                    raise OSError("fixture-private-directory-fsync")
                return fsync(fd)
            target = "fsync" if published else "replace"
            failure = fail_dir if published else OSError("fixture-private-replace")
            with (patch.object(audible, "Client", side_effect=self.factory()),
                  patch.object(session.auth_store.os, target, side_effect=failure)):
                with self.assertRaises(session.auth_store.StoreError) as caught:
                    with session.authenticated(self.fd) as client:
                        client.auth.access_token = REFRESHED["access_token"]
                        raise session.ProviderError("entitlement_denied")
            self.assertEqual(caught.exception.published, published)
            self.assertNotIn("fixture-private", str(caught.exception))
            after = session.auth_store.load(self.fd)
            self.assertEqual(after == before, not published)
            self.assertTrue(self.clients[-1].session.is_closed)
        before = session.auth_store.load(self.fd)
        with (patch.object(audible, "Client", side_effect=self.factory()),
              patch.object(audible.Authenticator, "to_dict", side_effect=ValueError("fixture-private-serialization"))):
            with self.assertRaises(session.auth_store.StoreError):
                with session.authenticated(self.fd):
                    pass
        self.assertEqual(session.auth_store.load(self.fd), before)
        session.auth_store.save(self.fd, json.dumps(SNAPSHOT).encode())
        self.assertTrue(self.invoke()["fixture"])
