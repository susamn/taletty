import importlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import audible
import httpx

library = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.library")
session = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.sdk_session")


def item(index, **fields):
    return dict(asin=f"T{index:09d}", title=f"Fixture {index}", **fields)


def client(*responses, marketplace="us"):
    fake = Mock(marketplace=marketplace)
    fake.get.side_effect = list(responses)
    return fake


class LibraryTests(unittest.TestCase):
    def invalid(self, operation):
        with self.assertRaises(session.ProviderError) as caught:
            operation()
        self.assertEqual(caught.exception.code, "protocol_error")
        self.assertEqual(caught.exception.retry, "none")
        self.assertNotIn("fixture-private", str(caught.exception))

    def test_pages_dedup_null_optional_metadata_and_request_contract(self):
        first = [item(0), item(0)] + [item(i) for i in range(1, 99)]
        fake = client({"items": first}, {"items": [item(98), item(99)]})
        pages = list(library.pages(fake))
        self.assertEqual([len(page) for page in pages], [99, 1])
        self.assertEqual(len({book["id"]["id"] for page in pages for book in page}), 100)
        for number, call in enumerate(fake.get.call_args_list, 1):
            self.assertEqual(call.args, ("library",))
            self.assertEqual(call.kwargs, dict(page=number, num_results=100,
                response_groups="contributors,media,product_desc,product_attrs,relationships", sort_by="-PurchaseDate"))
        self.assertEqual(list(library.pages(client({"items": []}))), [])
        sparse = dict(asin="T000000001", title=None, subtitle=None, authors=None, narrators=None,
                      runtime_length_min=None, signed_url="fixture-private-not-exported", customer_id="fixture-private")
        self.assertEqual(library._book(sparse), dict(id=dict(provider="audible", namespace="us", id="T000000001"), title="Untitled"))
        rich = library._book(dict(sparse, title="\x1b[31mTitle\x1b[0m\nCJK中文\u202e",
            subtitle="\x1b]8;;fixture-private-link\x07Subtitle\x1b]8;;\x1b\\",
            authors=[{}, {"name": None}, {"name": ""}, {"name": "Author\x00Name"}],
            narrators=[], runtime_length_min=1.25))
        self.assertEqual(rich["title"], "Title CJK中文")
        self.assertEqual(rich["subtitle"], "Subtitle")
        self.assertEqual(rich["authors"], [{"name": "Author Name"}])
        self.assertEqual(rich["narrators"], [])
        self.assertEqual(rich["duration_ms"], 75000)
        self.assertNotIn("fixture-private", json.dumps(rich))
        self.assertEqual(library._book(dict(sparse, title="\x1b", subtitle="", runtime_length_min=0))["duration_ms"], 0)
        self.assertEqual(library._book(dict(sparse, runtime_length_min=library.MAX_DURATION_MS / 60000))["duration_ms"], library.MAX_DURATION_MS)

    def test_invalid_shapes_identity_metadata_duration_and_marketplace(self):
        for response in (None, [], {}, {"items": None}, {"items": "fixture-private"}, {"items": [item(i) for i in range(101)]}):
            self.invalid(lambda: list(library.pages(client(response))))
        base = item(1)
        malformed = [None, [], {}, dict(base, asin=None), dict(base, asin="bad"), dict(base, asin="é" * 10),
                     dict(base, title=False), dict(base, subtitle=[]), dict(base, authors="fixture-private"),
                     dict(base, authors=[None]), dict(base, narrators=[{"name": False}])]
        malformed += [dict(base, runtime_length_min=value) for value in (-1, True, "1", float("nan"),
                        float("inf"), -float("inf"), 10**400, library.MAX_DURATION_MS / 60000 + 1)]
        for value in malformed:
            self.invalid(lambda: list(library.pages(client({"items": [value]}))))
        fake = client(marketplace="de")
        with self.assertRaises(session.ProviderError) as caught:
            list(library.pages(fake))
        self.assertEqual(caught.exception.code, "unsupported")
        fake.get.assert_not_called()

    def test_repeated_partial_failure_and_exhaustion_are_not_completion(self):
        full = [item(i) for i in range(100)]
        for last in ({"items": full}, {"items": [item(0)]}):
            pending = library.pages(client({"items": full}, last))
            self.assertEqual(len(next(pending)), 100)
            self.invalid(lambda: next(pending))
        with patch.object(library, "MAX_PAGES", 2):
            fake = client({"items": full}, {"items": [item(i) for i in range(100, 200)]})
            self.invalid(lambda: list(library.pages(fake)))
            self.assertEqual(fake.get.call_count, 2)
        denied = session.ProviderError("entitlement_denied")
        pending = library.pages(client({"items": full}, denied))
        next(pending)
        with self.assertRaises(session.ProviderError) as caught:
            next(pending)
        self.assertIs(caught.exception, denied)
        # Closing a partially consumed iterator does not turn it into a snapshot.
        pending = library.pages(client({"items": full}))
        next(pending)
        pending.close()

    def test_exact_page_bound_reserves_full_bridge_envelope(self):
        base = item(1)
        overhead = len(json.dumps({"books": [library._book(dict(base, title="X"))]}, separators=(",", ":"))) - 1
        base["title"] = "X" * (library.MAX_PAGE_BYTES - overhead)
        page = next(library.pages(client({"items": [base]})))
        wire = dict(protocol=1, operation_id="X" * 64, seq=2**64 - 1, type="stage", stage="library_page", books=page)
        self.assertLessEqual(len(json.dumps(wire, separators=(",", ":")).encode()), 4 << 20)
        base["title"] += "X"
        self.invalid(lambda: list(library.pages(client({"items": [base]}))))

    def test_real_sdk_failure_persistence_and_no_partial_caller_replacement(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            root.chmod(0o700)
            fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
            self.addCleanup(os.close, fd)
            snapshot = dict(locale_code="us", access_token="Atna|synthetic-not-issued",
                            refresh_token="Atnr|synthetic-not-issued", expires=1)
            real_client = audible.Client
            for mode in ("storage", "recover", "throttled", "protocol", "canceled"):
                session.auth_store.save(fd, json.dumps(snapshot).encode())
                calls, clients = [], []
                cache = [{"fixture": "prior-cache"}]
                def response(request):
                    calls.append(int(request.url.params["page"]))
                    if calls[-1] == 1:
                        return httpx.Response(200, json={"items": [item(i) for i in range(100)]})
                    if mode == "canceled":
                        raise KeyboardInterrupt()
                    return httpx.Response(429 if mode == "throttled" else 200,
                                          json={"message": "fixture-private-provider", "items": None if mode == "protocol" else []})
                def factory(**kwargs):
                    value = real_client(**kwargs, transport=httpx.MockTransport(response))
                    clients.append(value)
                    return value
                refresh = dict(access_token="Atna|synthetic-refreshed-not-issued", expires=4102444800)
                with (patch.object(audible, "Client", side_effect=factory),
                      patch("audible.auth.refresh_access_token", return_value=refresh) as renewed,
                      patch.object(httpx.HTTPTransport, "handle_request", side_effect=AssertionError("provider network forbidden"))):
                    def apply():
                        with session.authenticated(fd) as sdk:
                            proposed = [book for page in library.pages(sdk) for book in page]
                        cache[:] = proposed  # Only after exhaustion AND successful finally persistence.
                    if mode == "storage":
                        with patch.object(session.auth_store.os, "replace", side_effect=OSError("fixture-private-storage")):
                            with self.assertRaises(session.auth_store.StoreError):
                                apply()
                    elif mode == "recover":
                        apply()
                    else:
                        with self.assertRaises(session.ProviderError) as caught:
                            apply()
                        self.assertEqual(caught.exception.code, "protocol_error" if mode == "protocol" else mode)
                    renewed.assert_called_once()
                self.assertTrue(clients[-1].session.is_closed)
                self.assertEqual(calls, [1, 2])
                if mode == "recover":
                    self.assertEqual(len(cache), 100)
                else:
                    self.assertEqual(cache, [{"fixture": "prior-cache"}])
                saved = json.loads(session.auth_store.load(fd))
                self.assertEqual(saved["access_token"], snapshot["access_token"] if mode == "storage" else refresh["access_token"])
            # A new operation after handled cancellation restores the persisted
            # refresh; no second refresh/network bootstrap or partial prior cache.
            def recovery(**kwargs):
                value = real_client(**kwargs, transport=httpx.MockTransport(
                    lambda request: httpx.Response(200, json={"items": []})))
                clients.append(value)
                return value
            with (patch.object(audible, "Client", side_effect=recovery),
                  patch("audible.auth.refresh_access_token", side_effect=AssertionError("refresh not reused")) as renewed,
                  patch.object(httpx.HTTPTransport, "handle_request", side_effect=AssertionError("network forbidden"))):
                with session.authenticated(fd) as sdk:
                    proposed = list(library.pages(sdk))
                self.assertEqual(proposed, [])
                renewed.assert_not_called()
            self.assertTrue(clients[-1].session.is_closed)
