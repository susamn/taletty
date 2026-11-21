import importlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

remux = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.remux")
store = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.media_store")
chapters = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.chapters")


def probe(**fields):
    result = dict(format=dict(format_name="mov,mp4,m4a,3gp,3g2,mj2", duration="2"),
                  streams=[dict(codec_type="audio", codec_name="aac", nb_read_packets="17")], chapters=[])
    result.update(fields)
    return json.dumps(result).encode()


class RemuxTests(unittest.TestCase):
    def error(self, code, call):
        with self.assertRaises(remux.RemuxError) as caught:
            call()
        self.assertEqual(caught.exception.code, code)
        self.assertIsNone(caught.exception.__context__)
        self.assertIsNone(caught.exception.__cause__)
        self.assertNotIn("fixture-private", repr(caught.exception))

    def test_bounded_tools_errors_and_no_exception_context(self):
        for exception, code in ((FileNotFoundError("fixture-private"), "runtime_missing"),
                                (PermissionError("fixture-private"), "runtime_incompatible"),
                                (OSError("fixture-private"), "preparation_failed"),
                                (KeyboardInterrupt(), "canceled")):
            with patch.object(remux.subprocess, "Popen", side_effect=exception):
                self.error(code, lambda: remux._run(["fixture-private"], (), 1))
        with tempfile.TemporaryDirectory() as root:
            executable = Path(root) / "incompatible"
            executable.write_bytes(b"fixture-private not an executable format")
            executable.chmod(0o700)
            self.error("runtime_incompatible", lambda: remux._run([str(executable)], (), 5))
        self.error("preparation_failed", lambda: remux._run([sys.executable, "-c", "raise SystemExit(7)"], (), 5))
        self.error("preparation_failed", lambda: remux._run([sys.executable, "-c", "pass"], (), 0))
        with patch.object(remux, "MAX_PROBE_BYTES", 1):
            self.error("preparation_failed", lambda: remux._run([sys.executable, "-c", "print('xx')"], (), 5))
        self.assertEqual(remux._run([sys.executable, "-c", "print('ok')"], (), 5), b"ok\n")
        # Real selector timeout, child blocked on a pipe rather than a sleep.
        read, write = os.pipe()
        try:
            self.error("preparation_failed", lambda: remux._run(
                [sys.executable, "-c", f"import os; os.read({read}, 1)"], (read,), .05))
        finally:
            os.close(read)
            os.close(write)

    def test_explicit_fds_only_same_process_group_and_cancel_wait(self):
        with tempfile.TemporaryFile() as private, tempfile.TemporaryFile() as control:
            os.set_inheritable(private.fileno(), True)
            os.set_inheritable(control.fileno(), True)
            script = "import os,json; print(json.dumps(dict(pgid=os.getpgrp(),fds=[os.readlink('/proc/self/fd/'+f) for f in os.listdir('/proc/self/fd') if f.isdigit() and int(f)>2 and os.path.exists('/proc/self/fd/'+f)])))"
            result = json.loads(remux._run([sys.executable, "-c", script], (), 5))
            self.assertEqual(result["pgid"], os.getpgrp())
            self.assertEqual(result["fds"], [])
        for _ in range(5):
            parent, child = socket.socketpair()
            cancel, trigger = socket.socketpair()
            result = []
            def work():
                try:
                    remux._run([sys.executable, "-c", "import os,socket; s=socket.socket(fileno=" + str(child.fileno()) + "); s.sendall(str(os.getpid()).encode()); s.recv(1)"],
                               (child.fileno(),), 5, cancel.fileno())
                except remux.RemuxError as error:
                    result.append(error.code)
            thread = threading.Thread(target=work)
            try:
                thread.start()
                parent.settimeout(5)
                pid = int(parent.recv(40))  # Child barrier, not a guessed delay.
                trigger.sendall(b"cancel")
                thread.join(5)
                self.assertFalse(thread.is_alive())
                self.assertEqual(result, ["canceled"])
                with self.assertRaises(ProcessLookupError):
                    os.kill(pid, 0)
                with self.assertRaises(ChildProcessError):
                    os.waitpid(pid, os.WNOHANG)
            finally:
                trigger.sendall(b"x")
                thread.join(5)
                for sock in (parent, child, cancel, trigger):
                    sock.close()

    def test_probe_requires_packets_audio_container_and_sane_chapters(self):
        invalid = [b"fixture-private", b"[]", b"{}", probe(format=dict(format_name="mp3", duration="2")),
                   probe(format=dict(format_name="mp4", duration="nan")),
                   probe(format=dict(format_name="mp4", duration="0")), probe(streams=[]),
                   probe(streams=[dict(codec_type="audio", codec_name="vorbis", nb_read_packets="2")]),
                   probe(streams=[dict(codec_type="audio", codec_name="aac", nb_read_packets="0")]),
                   probe(streams=[dict(codec_type="audio", codec_name="aac", nb_read_packets="2"), dict(codec_type="video")]),
                   probe(chapters=[dict(start_time="0", end_time="nan")]),
                   probe(chapters=[dict(start_time="1", end_time="0")]),
                   probe(chapters=[dict(start_time="0", end_time="3")])]
        for payload in invalid:
            with patch.object(remux, "_run", return_value=payload):
                self.error("unsupported", lambda: remux.validator()(17))
        with patch.object(remux, "_run", return_value=probe(chapters=[dict(start_time="0", end_time="2")])):
            self.assertTrue(remux.validator()(17))
        tree, _ = chapters.prepare(dict(chapters=[dict(start_offset_ms=0, length_ms=2000, title="One")]))
        for actual, want in (([], False), ([dict(start_time="0", end_time="2", tags=dict(title="One"))], True),
                             ([dict(start_time="0", end_time="2", tags=dict(title="Wrong"))], False),
                             ([dict(start_time="0.1", end_time="2", tags=dict(title="One"))], False)):
            with patch.object(remux, "_run", return_value=probe(chapters=actual)):
                self.assertEqual(remux.validator(tree)(17), want)
        nested = [dict(children=tree, start_ms=0)]
        with patch.object(remux, "_run", return_value=probe()):
            self.assertFalse(remux.validator(nested)(17))

    def test_copy_capabilities_exact_map_keys_and_validation(self):
        with tempfile.TemporaryDirectory() as root:
            os.chmod(root, 0o700)
            fd = os.open(root, os.O_DIRECTORY)
            try:
                with store.opened(fd) as media, media.partial() as source, media.partial() as output:
                    source.write(b"fixture-private")
                    self.error("invalid_input", lambda: remux.copy(source, source, media))
                    with patch.object(remux.media_store, "_check", side_effect=OSError("fixture-private")):
                        self.error("invalid_input", lambda: remux.copy(source, output, media))
                    class Plan:
                        _key, _iv = "0" * 32, "1" * 32
                    info = dict(chapters=[dict(length_ms=1000, title="One")])
                    for metadata in (None, info):
                        with patch.object(remux, "_run", return_value=b"") as run, patch.object(remux, "validator", return_value=lambda fd: True):
                            remux.copy(source, output, media, metadata, Plan())
                            args, inherited, *_ = run.call_args.args
                            self.assertEqual(args[args.index("-map") + 1], "0:a:0")
                            self.assertEqual(args[args.index("-c:a") + 1], "copy")
                            self.assertEqual(args[args.index("-map_chapters") + 1], "1" if metadata else "0")
                            self.assertEqual(args[args.index("-audible_key") + 1], Plan._key)
                            self.assertEqual(len(inherited), 3 if metadata else 2)
                            self.assertNotIn(fd, inherited)
                            self.assertNotIn(media._directory, inherited)
                    with patch.object(remux, "_run"), patch.object(remux, "validator", return_value=lambda fd: False):
                        self.error("unsupported", lambda: remux.copy(source, output, media))
                    with patch.object(remux, "_run", side_effect=remux.RemuxError("preparation_failed")):
                        self.error("preparation_failed", lambda: remux.copy(source, output, media))
                    self.assertEqual(len(os.listdir(Path(root) / "downloads")), 2)
            finally:
                os.close(fd)

    def test_generated_media_copy_packets_chapters_publication_and_reuse(self):
        if not all(shutil.which(tool) for tool in ("ffmpeg", "ffprobe")):
            if os.environ.get("TALETTY_REQUIRE_MEDIA_TOOLS") == "1":
                self.fail("Mandatory real remux requires ffmpeg/ffprobe")
            self.skipTest("Local ffmpeg/ffprobe absent; mandatory real media CI")
        # Real ffmpeg progress is the barrier: it is executing generated audio
        # before cancellation, not merely a Popen mocked into existence.
        for _ in range(3):
            progress, writer = socket.socketpair()
            cancel, trigger = socket.socketpair()
            result = []
            def convert():
                try:
                    remux._run(["ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i",
                                "anullsrc=r=8000:cl=mono", "-progress", f"pipe:{writer.fileno()}",
                                "-f", "null", "-"], (writer.fileno(),), 15, cancel.fileno())
                except remux.RemuxError as error:
                    result.append(error.code)
            thread = threading.Thread(target=convert)
            try:
                thread.start()
                progress.settimeout(15)
                self.assertTrue(progress.recv(4096))
                trigger.sendall(b"cancel")
                thread.join(5)
                self.assertFalse(thread.is_alive())
                self.assertEqual(result, ["canceled"])
            finally:
                trigger.sendall(b"cancel")
                thread.join(5)
                for sock in (progress, writer, cancel, trigger):
                    sock.close()
        def run(*args):
            return subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15, check=True).stdout
        def digest(fd):
            return subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-i", remux._path(fd), "-map", "0:a:0", "-c:a", "copy", "-f", "hash", "-hash", "sha256", "-"],
                                  pass_fds=(fd,), stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15, check=True).stdout
        with tempfile.TemporaryDirectory() as root:
            os.chmod(root, 0o700)
            generated = Path(root) / "silence.m4b"
            run("ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono", "-t", "2", "-c:a", "aac", "-f", "mp4", str(generated))
            fd = os.open(root, os.O_DIRECTORY)
            try:
                with store.opened(fd) as media, media.partial() as source:
                    source.write(generated.read_bytes())
                    info = dict(chapters=[dict(length_ms=1000, title="One=;#\\ 中文"), dict(start_offset_ms=1000, length_ms=1000, title="Second")])
                    with media.partial() as output:
                        tree = remux.copy(source, output, media, info)
                        self.assertEqual(digest(source.fd), digest(output.fd))
                        self.assertTrue(media.publish(output, "B000000001", remux.validator(tree)))
                    self.assertTrue(media.ready("B000000001", remux.validator(tree)))
                    # Copy a chaptered artifact with no positive supplied leaves:
                    # embedded chapters survive, not an empty metadata replacement.
                    with media.partial() as chaptered:
                        chaptered.write((Path(root) / "downloads/B000000001.m4b").read_bytes())
                        for supplied in (None, dict(chapters=[dict(length_ms=0)])):
                            with media.partial() as output:
                                remux.copy(chaptered, output, media, supplied)
                                self.assertTrue(remux.validator(tree)(output.fd))
                                self.assertEqual(digest(chaptered.fd), digest(output.fd))
                    with media.partial() as output:
                        remux.copy(source, output, media)
                        self.assertEqual(remux._probe(output.fd).get("chapters", []), [])
                    with media.partial() as invalid:
                        invalid.write(b"not audio fixture-private")
                        self.error("preparation_failed", lambda: remux.copy(invalid, source, media))
                        self.error("preparation_failed", lambda: remux.validator()(invalid.fd))
            finally:
                os.close(fd)
