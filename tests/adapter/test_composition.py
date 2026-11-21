"""Private composition journeys: real SDK/store, fake provider, generated clear media.

The lightweight copy/validator seam used in fault tests is NOT media validation
proof; the separate mandatory generated-media test uses production ffmpeg/probe.
"""
from contextlib import ExitStack, contextmanager
from dataclasses import asdict
import errno
import importlib
import json
import os
from pathlib import Path
import shutil
import stat as statmode
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import audible
import httpx

from test_licensing import ASIN, URL, KEY, IV, granted
from test_sdk_session import SNAPSHOT, REFRESHED

composition = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.composition")
license, store, remux = composition.licensing, composition.media_store, composition.remux
session = composition.sdk_session
INFO = {"chapters": [{"title": "Generated chapter", "start_offset_ms": 0, "length_ms": 100}]}
PAYLOAD = b"clear-synthetic-fixture"


class CompositionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.root.chmod(0o700)
        self.fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        self.addCleanup(os.close, self.fd)
        snapshot = dict(SNAPSHOT, device_info={"device_type": "fixture-type", "device_serial_number": "fixture-serial"},
                        customer_info={"user_id": "fixture-user"})
        session.auth_store.save(self.fd, json.dumps(snapshot).encode())
        self.target = self.root / "downloads" / (ASIN + ".m4b")
        self.calls, self.streams, self.clients, self.plans = [], [], [], []
        self.real_client, self.real_plan = audible.Client, license._Plan

    def safe(self, outcome, **facts):
        actual = asdict(outcome)
        chapters = actual.pop("chapters")
        self.assertEqual(actual, dict(ready=False, reused=False, published=False,
                                     media_code=None, auth_code=None) | facts)
        if chapters is not None:
            self.assertEqual(chapters, remux.chapters.prepare(INFO)[0])
            self.assertTrue(outcome.ready)
            self.assertFalse(outcome.reused)
        for secret in (URL, KEY, IV, "fixture-user", "fixture-serial", REFRESHED["access_token"]):
            self.assertNotIn(secret, repr(outcome))
        for plan in self.plans:
            self.assertEqual((plan._url, plan._key, plan._iv, plan._chapter_info), (None, None, None, None))
        self.assertTrue(all(client.session.is_closed for client in self.clients))
        self.assertTrue(all(body.closed for body in self.streams))
        downloads = self.root / "downloads"
        if downloads.exists():
            self.assertFalse(any(path.suffix == ".part" for path in downloads.iterdir()))

    def copied(self, source, output, media, info=None, plan=None, cancel_fd=None):
        self.assertEqual((plan._key, plan._iv), (KEY, IV) if plan._key else (None, None))
        tree, _ = remux.chapters.prepare(info)
        output.write(os.pread(source.fd, os.fstat(source.fd).st_size, 0))
        return tree

    @contextmanager
    def provider(self, response=None, *, license_status=200, metadata_status=200,
                 body_status=200, body_error=False, payload=PAYLOAD, chunks=None,
                 body_headers=None, body_hook=None, real_media=False,
                 close_error=False, metadata_interrupt=False):
        response = granted() if response is None else response
        owner = self
        class Body(httpx.SyncByteStream):
            closed = False
            def __iter__(self):
                if body_hook:
                    body_hook()
                for item in (chunks if chunks is not None else (payload,)):
                    yield item
                if body_error:
                    raise httpx.ReadError(URL)
            def close(self):
                self.closed = True
        def sdk_http(request):
            self.calls.append(request)
            self.assertEqual(request.headers["x-amz-access-token"], REFRESHED["access_token"])
            if request.method == "POST":
                self.assertEqual(request.url.path, "/1.0/content/" + ASIN + "/licenserequest")
                self.assertEqual(json.loads(request.content)["consumption_type"], "Download")
                return httpx.Response(license_status, json=response)
            self.assertEqual(request.url.path, "/1.0/content/" + ASIN + "/metadata")
            if metadata_interrupt:
                raise KeyboardInterrupt()
            self.assertEqual(request.url.params["chapter_titles_type"], "Tree")
            return httpx.Response(metadata_status, json={"content_metadata": {"chapter_info": INFO}})
        def sdk_client(**kwargs):
            client = self.real_client(**kwargs, transport=httpx.MockTransport(sdk_http))
            self.clients.append(client)
            if close_error:
                close = client.close
                def fail_close():
                    close()
                    raise RuntimeError(URL)
                client.close = fail_close
            return client
        def media_http(request):
            self.assertEqual(str(request.url), URL)
            for header in ("authorization", "x-amz-access-token", "cookie", "proxy-authorization"):
                self.assertNotIn(header, request.headers)
            body = Body()
            owner.streams.append(body)
            headers = {"location": "https://other.example.test/secret"}
            if body_headers:
                headers.update(body_headers)
            return httpx.Response(body_status, headers=headers, stream=body)
        def plan(*args):
            value = self.real_plan(*args)
            self.plans.append(value)
            return value
        with ExitStack() as stack:
            stack.enter_context(patch.object(audible, "Client", side_effect=sdk_client))
            stack.enter_context(patch("audible.auth.refresh_access_token", return_value=REFRESHED))
            # Real signed HTTP Client and its default transport, intercepted before
            # any provider/network access. SDK traffic uses separate MockTransport.
            stack.enter_context(patch.object(httpx.HTTPTransport, "handle_request", side_effect=media_http))
            stack.enter_context(patch.object(license, "_Plan", side_effect=plan))
            if not real_media:
                stack.enter_context(patch.object(remux, "copy", side_effect=self.copied))
                stack.enter_context(patch.object(remux, "validator", side_effect=lambda *a, **kw:
                    lambda fd: os.pread(fd, len(PAYLOAD), 0).startswith(PAYLOAD)))
            yield

    def test_sdk_refresh_transfer_publish_reuse_without_auth_or_license(self):
        with self.provider(granted(True)):
            outcome = composition.run(self.fd, ASIN)
            self.safe(outcome, ready=True, published=True)
            self.assertEqual(outcome.chapters, remux.chapters.prepare(INFO)[0])
        self.assertEqual(self.target.read_bytes(), PAYLOAD)
        self.assertEqual(json.loads(session.auth_store.load(self.fd))["access_token"], REFRESHED["access_token"])
        self.assertEqual(len(self.calls), 2)
        self.assertEqual(len(self.streams), 1)
        (self.root / "auth.json").unlink()
        with self.provider(), patch.object(audible, "Client") as client:
            outcome = composition.run(self.fd, ASIN)
            self.safe(outcome, ready=True, reused=True)
            self.assertIsNone(outcome.chapters)
            client.assert_not_called()
        self.assertEqual(self.target.stat().st_mode & 0o777, 0o600)

    def test_embedded_and_unavailable_optional_chapters(self):
        response = granted()
        response["content_license"]["content_metadata"]["chapter_info"] = INFO
        with self.provider(response):
            self.safe(composition.run(self.fd, ASIN), ready=True, published=True)
        self.assertEqual(len(self.calls), 1)
        self.target.unlink()
        with self.provider(metadata_status=500):
            self.safe(composition.run(self.fd, ASIN), ready=True, published=True)
        self.assertEqual(len(self.calls), 3)

    def test_refusal_throttle_bad_plan_stream_failure_and_recovery(self):
        denied = {"content_license": {"status_code": "Denied", "license_denial_reasons": []}}
        throttled = {"content_license": {"status_code": "Denied", "license_denial_reasons": [
            {"rejectionReason": "CustomerThrottled", "message": URL}]}}
        for response, kwargs, code in ((denied, {}, "entitlement_denied"), (throttled, {}, "throttled"),
                                      ({}, {}, "protocol_error"), (granted(), {"license_status": 429}, "throttled"),
                                      (granted(), {"body_error": True}, "network"),
                                      (granted(), {"body_status": 302}, "network"),
                                      (granted(), {"body_status": 429}, "throttled")):
            with self.subTest(code=code, kwargs=kwargs), self.provider(response, **kwargs):
                before = len(self.calls)
                self.safe(composition.run(self.fd, ASIN), media_code=code)
                self.assertEqual(len(self.calls) - before, 1 if response != granted() or kwargs.get("license_status") else 2)
                self.assertFalse(self.target.exists())
        with self.provider():
            self.safe(composition.run(self.fd, ASIN), ready=True, published=True)

    def test_invalid_input_and_unsafe_ready_never_reach_provider(self):
        with self.provider(), patch.object(audible, "Client") as client:
            self.safe(composition.run(self.fd, "../invalid"), media_code="invalid_input")
            self.target.parent.mkdir(mode=0o700, exist_ok=True)
            self.target.write_bytes(b"unverified")
            self.target.chmod(0o600)
            self.safe(composition.run(self.fd, ASIN), media_code="storage_failed")
            client.assert_not_called()
        self.assertEqual(self.target.read_bytes(), b"unverified")

    def test_write_remux_and_unknown_faults_are_safe_and_recoverable(self):
        for owner, method, error, code in ((store.Partial, "write", store.MediaStoreError(), "storage_failed"),
                                          (remux, "copy", remux.RemuxError("unsupported"), "unsupported"),
                                          (remux, "copy", RuntimeError(URL), "internal")):
            with self.subTest(code=code), self.provider(), patch.object(owner, method, side_effect=error):
                self.safe(composition.run(self.fd, ASIN), media_code=code)
                self.assertFalse(self.target.exists())
        with self.provider():
            self.safe(composition.run(self.fd, ASIN), ready=True, published=True)

    def test_real_source_short_writes_and_disk_error_preserve_sdk_persistence(self):
        write = os.write
        for disk_full in (False, True):
            calls = []
            def media_write(fd, data):
                if "/.media-" in os.readlink("/proc/self/fd/" + str(fd)):
                    calls.append(fd)
                    if disk_full:
                        raise OSError(errno.ENOSPC, URL)
                    return write(fd, data[:3])
                return write(fd, data)
            with self.provider(), patch.object(store.os, "write", side_effect=media_write):
                if disk_full:
                    self.safe(composition.run(self.fd, ASIN), media_code="storage_failed")
                else:
                    self.safe(composition.run(self.fd, ASIN), ready=True, published=True)
                    self.assertEqual(self.target.read_bytes(), PAYLOAD)
            self.assertGreater(len(calls), 0 if disk_full else 2)
            self.assertEqual(json.loads(session.auth_store.load(self.fd))["access_token"], REFRESHED["access_token"])
            if not disk_full:
                self.target.unlink()
                state = json.loads(session.auth_store.load(self.fd)) | SNAPSHOT
                session.auth_store.save(self.fd, json.dumps(state).encode())
        self.assertFalse(self.target.exists())

    def test_malformed_chapters_and_production_remux_tool_failures(self):
        response = granted()
        response["content_license"]["content_metadata"]["chapter_info"] = {
            "chapters": [{"start_offset_ms": -1, "title": URL}]}
        with self.provider(response):
            self.safe(composition.run(self.fd, ASIN), media_code="protocol_error")
        # Exercise production copy's FD/chapter/validator integration. These
        # deterministic tool-output faults are not actual ffmpeg/media evidence.
        with self.provider(real_media=True), patch.object(remux, "_run", side_effect=remux.RemuxError("preparation_failed")):
            self.safe(composition.run(self.fd, ASIN), media_code="preparation_failed")
        with self.provider(real_media=True), patch.object(remux, "_run", return_value=b"malformed-probe"):
            self.safe(composition.run(self.fd, ASIN), media_code="unsupported")
        self.assertFalse(self.target.exists())

    def test_auth_persistence_precommit_and_postreplace_uncertainty_keep_media(self):
        for uncertain in (False, True):
            with self.subTest(uncertain=uncertain):
                before = session.auth_store.load(self.fd)
                fsync = os.fsync
                def sync(fd):
                    if fd == self.fd and self.plans:
                        raise OSError(URL)
                    fsync(fd)
                owner, method = (store.os, "fsync") if uncertain else (session.auth_store.os, "replace")
                fault = sync if uncertain else OSError(URL)
                self.plans.clear()
                with self.provider(), patch.object(owner, method, side_effect=fault):
                    self.safe(composition.run(self.fd, ASIN), ready=True, published=True,
                              auth_code="storage_uncertain" if uncertain else "storage_failed")
                self.assertTrue(self.target.exists())
                self.assertEqual(session.auth_store.load(self.fd) == before, not uncertain)
                self.target.unlink()
                # Force refresh/change for the next iteration's persistence check.
                state = json.loads(session.auth_store.load(self.fd))
                state.update(SNAPSHOT)
                session.auth_store.save(self.fd, json.dumps(state).encode())

    def test_media_precommit_postcommit_and_unknown_reconciliation(self):
        for commit, unknown in ((False, False), (True, False), (True, True)):
            with self.subTest(commit=commit, unknown=unknown):
                renamed = False
                rename, stat, fsync = store._rename, os.stat, os.fsync
                def boundary(*args):
                    nonlocal renamed
                    if commit:
                        rename(*args)
                        renamed = True
                    if unknown:
                        raise KeyboardInterrupt()
                    if not commit:
                        raise OSError(URL)
                def inspect(*args, **kwargs):
                    if unknown and renamed and args[0] == ASIN + ".m4b":
                        raise OSError(errno.EIO, URL)
                    return stat(*args, **kwargs)
                def sync(fd):
                    if renamed and fd != self.fd and statmode.S_ISDIR(os.fstat(fd).st_mode):
                        raise OSError(URL)
                    return fsync(fd)
                with (self.provider(), patch.object(store, "_rename", side_effect=boundary),
                      patch.object(store.os, "stat", side_effect=inspect), patch.object(store.os, "fsync", side_effect=sync)):
                    self.safe(composition.run(self.fd, ASIN), ready=commit and not unknown,
                              published=None if unknown else commit,
                              media_code="storage_uncertain" if commit else "storage_failed")
                self.assertEqual(self.target.exists(), commit)
                if commit:
                    self.target.unlink()

    def test_media_and_auth_failures_remain_independent(self):
        with (self.provider(), patch.object(remux, "copy", side_effect=remux.RemuxError("preparation_failed")),
              patch.object(session.auth_store.os, "replace", side_effect=OSError(URL))):
            self.safe(composition.run(self.fd, ASIN), media_code="preparation_failed", auth_code="storage_failed")
        self.assertFalse(self.target.exists())
        with self.provider():
            self.safe(composition.run(self.fd, ASIN), ready=True, published=True)

    def test_media_publication_uncertainty_survives_auth_storage_failure(self):
        rename, committed = store._rename, store.Store._committed
        for unknown in (False, True):
            def ambiguous(*args):
                rename(*args)
                raise OSError(errno.EIO, URL)
            def reconcile(media, part, name):
                if unknown:
                    raise OSError(errno.EIO, URL)
                return committed(media, part, name)
            with (self.provider(), patch.object(store, "_rename", side_effect=ambiguous),
                  patch.object(store.Store, "_committed", new=reconcile),
                  patch.object(session.auth_store.os, "replace", side_effect=OSError(URL))):
                self.safe(composition.run(self.fd, ASIN), ready=not unknown,
                          published=None if unknown else True,
                          media_code="storage_uncertain", auth_code="storage_failed")
            self.assertEqual(self.target.read_bytes(), PAYLOAD)
            self.target.unlink()

    def test_cancellation_before_during_transfer_and_after_commit(self):
        read, write = os.pipe()
        self.addCleanup(os.close, read)
        self.addCleanup(os.close, write)
        os.write(write, b"x")
        with self.provider(), patch.object(audible, "Client") as client:
            self.safe(composition.run(self.fd, ASIN, read), media_code="canceled")
            client.assert_not_called()
        os.read(read, 1)
        with self.provider(body_hook=lambda: os.write(write, b"x")):
            self.safe(composition.run(self.fd, ASIN, read), media_code="canceled")
        os.read(read, 1)
        publish = store.Store.publish
        def after(media, *args):
            result = publish(media, *args)
            raise KeyboardInterrupt()
        with self.provider(), patch.object(store.Store, "publish", new=after):
            self.safe(composition.run(self.fd, ASIN, read), ready=True, published=True)
        self.assertTrue(self.target.exists())

    def test_concurrent_verified_identical_reuse_and_conflict(self):
        publish = store.Store.publish
        for identical in (True, False):
            def concurrent(media, part, asin, validate):
                self.target.write_bytes(PAYLOAD if identical else PAYLOAD + b"different")
                self.target.chmod(0o600)
                return publish(media, part, asin, validate)
            with self.provider(), patch.object(store.Store, "publish", new=concurrent):
                self.safe(composition.run(self.fd, ASIN), ready=identical, reused=identical,
                          media_code=None if identical else "conflict")
            self.assertEqual(self.target.read_bytes(), PAYLOAD if identical else PAYLOAD + b"different")
            self.target.unlink()

    def test_client_close_failure_preserves_committed_or_primary_media_fact(self):
        with self.provider(close_error=True):
            self.safe(composition.run(self.fd, ASIN), ready=True, published=True, media_code="internal")
        self.target.unlink()
        with (self.provider(close_error=True),
              patch.object(remux, "copy", side_effect=remux.RemuxError("preparation_failed"))):
            self.safe(composition.run(self.fd, ASIN), media_code="preparation_failed")
        plan_count = len(self.plans)
        with self.provider(metadata_interrupt=True):
            self.safe(composition.run(self.fd, ASIN), media_code="canceled")
        self.assertEqual(len(self.plans), plan_count)
        self.assertEqual(json.loads(session.auth_store.load(self.fd))["access_token"], REFRESHED["access_token"])
        with self.provider():
            self.safe(composition.run(self.fd, ASIN), ready=True, published=True)

    def test_outer_faults_and_cancellation_swallowed_by_store_validator(self):
        for error, code in ((KeyboardInterrupt(), "canceled"), (RuntimeError(URL), "internal"),
                            (remux.RemuxError("runtime_missing"), "runtime_missing")):
            with patch.object(store, "opened", side_effect=error):
                self.safe(composition.run(self.fd, ASIN), media_code=code)
        read, write = os.pipe()
        self.addCleanup(os.close, read)
        self.addCleanup(os.close, write)
        self.target.parent.mkdir(mode=0o700, exist_ok=True)
        self.target.write_bytes(PAYLOAD)
        self.target.chmod(0o600)
        def canceled(fd):
            os.write(write, b"x")
            raise remux.RemuxError("canceled")
        with self.provider(), patch.object(remux, "validator", return_value=canceled):
            self.safe(composition.run(self.fd, ASIN, read), media_code="canceled")
        os.read(read, 1)
        self.target.unlink()
        publish = store.Store.publish
        def cancel_validation(media, part, asin, validate):
            return publish(media, part, asin, canceled)
        with self.provider(), patch.object(store.Store, "publish", new=cancel_validation):
            self.safe(composition.run(self.fd, ASIN, read), media_code="canceled")
        self.assertFalse(self.target.exists())

    def test_generated_clear_audio_actual_remux_probe_publication_and_ready_reuse(self):
        if not all(shutil.which(tool) for tool in ("ffmpeg", "ffprobe")):
            if os.environ.get("TALETTY_REQUIRE_MEDIA_TOOLS") == "1":
                self.fail("Mandatory generated-media tools missing")
            self.skipTest("Local ffmpeg/ffprobe absent; mandatory actual media in CI")
        source = self.root / "generated.m4b"
        subprocess.run(["ffmpeg", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono",
                        "-t", "0.3", "-c:a", "aac", "-f", "mp4", str(source)], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
        def hashes(path):
            result = subprocess.run(["ffprobe", "-v", "error", "-select_streams", "a:0", "-show_packets",
                                     "-show_entries", "packet=data_hash", "-show_data_hash", "sha256", "-of", "json", str(path)],
                                    check=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=20)
            return [packet["data_hash"] for packet in json.loads(result.stdout)["packets"]]
        with self.provider(payload=b"generated-invalid-container", real_media=True):
            self.safe(composition.run(self.fd, ASIN), media_code="preparation_failed")
        events = []
        raw = source.read_bytes()
        with self.provider(payload=raw, body_headers={"Content-Length": str(len(raw))}, real_media=True):
            self.safe(composition.run(self.fd, ASIN, emit=lambda kind, **kw: events.append((kind, kw))),
                      ready=True, published=True)
        self.assertEqual(events, [
            ("stage", {"stage": "licensing"}),
            ("stage", {"stage": "downloading"}),
            ("progress", {"bytes": len(raw), "total": len(raw)}),
            ("stage", {"stage": "preparing"}),
            ("stage", {"stage": "publishing"}),
        ])
        self.assertEqual(hashes(source), hashes(self.target))
        (self.root / "auth.json").unlink()
        reuse_events = []
        with self.provider(real_media=True), patch.object(audible, "Client") as client:
            self.safe(composition.run(self.fd, ASIN, emit=lambda kind, **kw: reuse_events.append((kind, kw))),
                      ready=True, reused=True)
            client.assert_not_called()
        self.assertEqual(reuse_events, [])
        self.target.write_bytes(b"generated-invalid-container")
        with self.provider(real_media=True), patch.object(audible, "Client") as client:
            self.safe(composition.run(self.fd, ASIN), media_code="storage_failed")
            client.assert_not_called()
        self.assertEqual(self.target.read_bytes(), b"generated-invalid-container")

    def test_stage_and_progress_emission_content_length_and_emit_faults(self):
        chunk1 = PAYLOAD + b"x" * ((1024 * 1024) - len(PAYLOAD))
        chunk2 = b"y" * 100
        chunks = (chunk1, chunk2)
        total = len(chunk1) + len(chunk2)
        for name, headers, expected_progress in (
            ("known", {"Content-Length": str(total)}, [
                ("progress", {"bytes": len(chunk1), "total": total}),
                ("progress", {"bytes": total, "total": total}),
            ]),
            ("unknown", None, [
                ("progress", {"bytes": len(chunk1)}),
                ("progress", {"bytes": total}),
            ]),
            ("invalid", {"Content-Length": "-5"}, [
                ("progress", {"bytes": len(chunk1)}),
                ("progress", {"bytes": total}),
            ]),
            ("smaller", {"Content-Length": str(len(chunk1) + 50)}, [
                ("progress", {"bytes": len(chunk1), "total": len(chunk1) + 50}),
                ("progress", {"bytes": total}),
            ]),
            ("zero", {"Content-Length": "0"}, [
                ("progress", {"bytes": len(chunk1)}),
                ("progress", {"bytes": total}),
            ]),
        ):
            with self.subTest(name=name):
                if self.target.exists():
                    self.target.unlink()
                events = []
                with self.provider(chunks=chunks, body_headers=headers):
                    self.safe(composition.run(self.fd, ASIN, emit=lambda kind, **kw: events.append((kind, kw))),
                              ready=True, published=True)
                self.assertEqual(events, [
                    ("stage", {"stage": "licensing"}),
                    ("stage", {"stage": "downloading"}),
                    *expected_progress,
                    ("stage", {"stage": "preparing"}),
                    ("stage", {"stage": "publishing"}),
                ])
                self.assertEqual(self.target.read_bytes(), chunk1 + chunk2)
        # Empty chunks yielded by a stream do not emit duplicate progress frames.
        self.target.unlink()
        empty_chunk_events = []
        with (self.provider(),
              patch.object(license, "signed_stream", return_value=composition.licensing.contextmanager(lambda p: (yield iter((b"", PAYLOAD))))(None))):
            self.safe(composition.run(self.fd, ASIN, emit=lambda kind, **kw: empty_chunk_events.append((kind, kw))),
                      ready=True, published=True)
        self.assertEqual(empty_chunk_events, [
            ("stage", {"stage": "licensing"}),
            ("stage", {"stage": "downloading"}),
            ("progress", {"bytes": len(PAYLOAD)}),
            ("stage", {"stage": "preparing"}),
            ("stage", {"stage": "publishing"}),
        ])
        # Verified ready reuse skips all stages and progress.
        reuse_events = []
        with self.provider(), patch.object(audible, "Client") as client:
            self.safe(composition.run(self.fd, ASIN, emit=lambda kind, **kw: reuse_events.append((kind, kw))),
                      ready=True, reused=True)
            client.assert_not_called()
        self.assertEqual(reuse_events, [])
        self.target.unlink()
        # Closed control pipe (OSError) during progress aborts cleanly, cleans partials, and persists auth.
        def fail_on_progress(kind, **kw):
            if kind == "progress":
                raise BrokenPipeError("closed control")
        with self.provider(chunks=chunks, body_headers={"Content-Length": str(total)}):
            self.safe(composition.run(self.fd, ASIN, emit=fail_on_progress), media_code="internal")
        self.assertFalse(self.target.exists())
        self.assertEqual(json.loads(session.auth_store.load(self.fd))["access_token"], REFRESHED["access_token"])
        # Cancellation during remux emits preparing but never publishing and cleans partials.
        read, write = os.pipe()
        self.addCleanup(os.close, read)
        self.addCleanup(os.close, write)
        cancel_events = []
        def cancel_remux(source, output, media, info=None, plan=None, cancel_fd=None):
            os.write(write, b"x")
            raise remux.RemuxError("canceled")
        with self.provider(), patch.object(remux, "copy", side_effect=cancel_remux):
            self.safe(composition.run(self.fd, ASIN, read, emit=lambda kind, **kw: cancel_events.append((kind, kw))),
                      media_code="canceled")
        os.read(read, 1)
        self.assertEqual(cancel_events, [
            ("stage", {"stage": "licensing"}),
            ("stage", {"stage": "downloading"}),
            ("progress", {"bytes": len(PAYLOAD)}),
            ("stage", {"stage": "preparing"}),
        ])
        self.assertFalse(self.target.exists())
