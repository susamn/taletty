import base64
import hashlib
import importlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import audible
from audible.aescipher import aes_cbc_encrypt
import httpx

from test_sdk_session import SNAPSHOT, REFRESHED

license = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.licensing")
session = license.sdk_session
ASIN = "B000000001"
URL = "https://media.example.test/book?signature=synthetic-private"
KEY, IV = "01" * 16, "02" * 16


def granted(encrypted=False):
    result = {"content_license": {"status_code": "Granted", "asin": ASIN,
              "content_metadata": {"content_url": {"offline_url": URL}}}}
    if encrypted:
        # Synthetic never-issued SDK-format voucher, not a provider fixture.
        digest = hashlib.sha256(("fixture-type" + "fixture-serial" + "fixture-user" + ASIN).encode()).digest()
        text = json.dumps({"key": KEY, "iv": IV})
        text += "\x00" * (-len(text) % 16)
        result["content_license"]["license_response"] = base64.b64encode(
            aes_cbc_encrypt(digest[:16], digest[16:], text, padding="none")).decode()
    return result


class LicenseTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.root.chmod(0o700)
        self.fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
        self.addCleanup(os.close, self.fd)
        snapshot = dict(SNAPSHOT, device_info={"device_type": "fixture-type", "device_serial_number": "fixture-serial"},
                        customer_info={"user_id": "fixture-user"})
        session.auth_store.save(self.fd, json.dumps(snapshot).encode())
        self.clients, self.requests = [], []
        self.real_client = audible.Client

    def factory(self, response, status=200):
        def handle(request):
            self.requests.append(request)
            self.assertEqual(request.url.path, "/1.0/content/" + ASIN + "/licenserequest")
            self.assertEqual(request.headers["x-amz-access-token"], REFRESHED["access_token"])
            self.assertEqual(json.loads(request.content), {
                "supported_drm_types": ["Mpeg", "Adrm"], "quality": "High", "consumption_type": "Download",
                "response_groups": "content_reference,chapter_info"})
            return httpx.Response(status, json=response)
        def create(**kwargs):
            client = self.real_client(**kwargs, transport=httpx.MockTransport(handle))
            self.clients.append(client)
            return client
        return create

    def invoke(self, response, status=200):
        with (patch.object(audible, "Client", side_effect=self.factory(response, status)),
              patch("audible.auth.refresh_access_token", return_value=REFRESHED),
              patch.object(httpx.HTTPTransport, "handle_request", side_effect=AssertionError("external HTTP forbidden"))):
            with license.entitled(self.fd, ASIN) as plan:
                self.assertEqual(plan._url, URL)
                self.assertNotIn(URL, repr(plan))
                self.assertNotIn(KEY, repr(plan))
                self.assertFalse(hasattr(plan, "__dict__"))
                values = plan._key, plan._iv
            self.assertEqual((plan._url, plan._key, plan._iv), (None, None, None))
            return values

    def test_real_sdk_synthetic_voucher_refresh_persist_and_recovery(self):
        self.assertEqual(self.invoke(granted(True)), (KEY, IV))
        self.assertEqual(self.invoke(granted()), (None, None))
        for status, code in ((401, "not_authenticated"), (429, "throttled"), (500, "internal")):
            with self.assertRaises(session.ProviderError) as caught:
                self.invoke({"message": URL}, status)
            self.assertEqual(caught.exception.code, code)
            self.assertNotIn(URL, repr(caught.exception))
            self.assertIsNone(caught.exception.__cause__)
        self.assertTrue(all(c.session.is_closed for c in self.clients))
        self.assertEqual(json.loads(session.auth_store.load(self.fd))["access_token"], REFRESHED["access_token"])
        self.assertEqual(self.invoke(granted()), (None, None))

    def test_denied_throttled_no_retry_and_malformed_plan(self):
        for reasons, code in (([], "entitlement_denied"), ([{"rejectionReason": "CustomerThrottled", "message": URL}], "throttled")):
            before = len(self.requests)
            with self.assertRaises(session.ProviderError) as caught:
                self.invoke({"content_license": {"status_code": "Denied", "license_denial_reasons": reasons}})
            self.assertEqual(caught.exception.code, code)
            self.assertEqual(len(self.requests), before + 1)
        bad = [None, {}, {"content_license": []}]
        for field, value in (("asin", "B000000002"), ("status_code", "Unknown"), ("license_response", "bad-voucher")):
            response = granted()
            response["content_license"][field] = value
            bad.append(response)
        for value in bad:
            with self.assertRaises(session.ProviderError) as caught:
                self.invoke(value)
            self.assertEqual(caught.exception.code, "protocol_error")
        with patch.object(audible, "Client") as client:
            for asin in (None, "", "../../private", "B000000001\n"):
                with self.assertRaises(license.LicenseError):
                    with license.entitled(self.fd, asin):
                        self.fail("invalid input")
            client.assert_not_called()

    def test_malformed_voucher_and_persistence_failure_cleanup(self):
        with patch("audible.aescipher.decrypt_voucher_from_licenserequest", return_value={"key": URL, "iv": IV}):
            with self.assertRaises(session.ProviderError):
                self.invoke(granted(True))
        retained = []
        with (patch.object(audible, "Client", side_effect=self.factory(granted(True))),
              patch.object(session.auth_store, "save", side_effect=session.auth_store.StoreError()),
              patch("audible.auth.refresh_access_token", return_value=REFRESHED)):
            with self.assertRaises(session.auth_store.StoreError):
                with license.entitled(self.fd, ASIN) as plan:
                    retained.append(plan)
                    raise RuntimeError(URL)
        self.assertIsNone(retained[0]._key)
        self.assertTrue(self.clients[-1].session.is_closed)
        self.assertEqual(self.invoke(granted()), (None, None))

    def test_signed_failure_persists_and_next_operation_recovers(self):
        def fail(request):
            self.assertNotIn("x-amz-access-token", request.headers)
            raise httpx.ConnectError(URL, request=request)
        with (patch.object(audible, "Client", side_effect=self.factory(granted(True))),
              patch("audible.auth.refresh_access_token", return_value=REFRESHED),
              patch.object(httpx.HTTPTransport, "handle_request", side_effect=fail)):
            with self.assertRaises(session.ProviderError) as caught:
                with license.entitled(self.fd, ASIN) as plan:
                    with license.signed_stream(plan) as chunks:
                        list(chunks)
        self.assertEqual(caught.exception.code, "network")
        self.assertIsNone(plan._url)
        self.assertIsNone(plan._key)
        self.assertEqual(json.loads(session.auth_store.load(self.fd))["access_token"], REFRESHED["access_token"])
        self.assertEqual(self.invoke(granted()), (None, None))

    def test_url_validation(self):
        for value in (None, "", "x" * 16385, URL + "\n", "https://é.example/a", "http://media.example/a",
                      "https://user:pass@media.example/a", "https://media.example:444/a", "https://media.example/#secret",
                      "https://127.0.0.1/a", "https://[::1]/a", "https://localhost/a", "https://a.localhost/a",
                      "https://media.example./a", "https://media.example\\evil/a", "https://a%20.example/a",
                      "https://media.example:bad/a"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                license._url(value)
        self.assertEqual(license._url(URL), URL)


class SignedStreamTests(unittest.TestCase):
    def run_stream(self, status=200, headers=None, body_error=False, consumer_error=False):
        requests, clients, streams = [], [], []
        real_client = httpx.Client
        class Body(httpx.SyncByteStream):
            closed = False
            def __iter__(self):
                if body_error:
                    raise RuntimeError(URL)
                yield b"synthetic-unprotected-bytes"
            def close(self):
                self.closed = True
        def handle(request):
            requests.append(request)
            body = Body()
            streams.append(body)
            return httpx.Response(status, headers=headers, stream=body)
        def factory(**kwargs):
            self.assertEqual(kwargs, dict(auth=None, trust_env=False, follow_redirects=False, timeout=120))
            client = real_client(**kwargs, transport=httpx.MockTransport(handle))
            clients.append(client)
            return client
        try:
            with patch.object(httpx, "Client", side_effect=factory):
                with license.signed_stream(license._Plan(URL, KEY, IV)) as chunks:
                    if consumer_error:
                        raise KeyboardInterrupt()
                    self.assertEqual(b"".join(chunks), b"synthetic-unprotected-bytes")
        finally:
            self.assertEqual(len(requests), 1)
            self.assertEqual(str(requests[0].url), URL)
            for name in ("authorization", "x-amz-access-token", "cookie", "proxy-authorization"):
                self.assertNotIn(name, requests[0].headers)
            self.assertTrue(all(c.is_closed for c in clients))
            self.assertTrue(all(s.closed for s in streams))

    def test_fresh_transport_and_cleanup(self):
        self.run_stream()
        for kwargs, code in (({"status": 302, "headers": {"location": "https://other.example/private"}}, "network"),
                             ({"status": 429}, "throttled"), ({"body_error": True}, "network"),
                             ({"consumer_error": True}, "canceled")):
            with self.assertRaises(session.ProviderError) as caught:
                self.run_stream(**kwargs)
            self.assertEqual(caught.exception.code, code)
            self.assertNotIn(URL, repr(caught.exception))
            self.assertIsNone(caught.exception.__cause__)
            self.assertTrue(caught.exception.__suppress_context__)
        with self.assertRaises(session.ProviderError):
            with license.signed_stream(license._Plan(None, None, None)):
                self.fail("cleared plan used")
