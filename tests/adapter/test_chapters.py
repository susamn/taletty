import copy
import importlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

chapters = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.chapters")
session = importlib.import_module("engine.connectors.audible.internal.bridge.adapter.sdk_session")


def leaf(start=0, length=1000, title="Chapter", **fields):
    return dict(start_offset_ms=start, length_ms=length, title=title, **fields)


class ChapterTests(unittest.TestCase):
    def invalid(self, info):
        with self.assertRaises(session.ProviderError) as caught:
            chapters.prepare(info)
        self.assertEqual(caught.exception.code, "protocol_error")
        self.assertEqual(caught.exception.retry, "none")
        self.assertNotIn("fixture-private", str(caught.exception))

    def test_owned_tree_positive_leaves_stable_sort_and_escaping(self):
        source = dict(chapters=[leaf(2000, 1000, "Parent", chapters=[
            leaf(1000, 1000, "\x1b[31mOne=;#\\\x1b[0m\n中文\u202e"),
            leaf(0, 1000, "First"), leaf(0, 500, "Tied"), leaf(0, 0, "Zero"),
            dict(title=None), dict(title=" \x1b ", chapters=None)]),
            leaf(3000, 1000, "Last", customer_id="fixture-private", signed_url="fixture-private")])
        before = copy.deepcopy(source)
        tree, metadata = chapters.prepare(source)
        self.assertEqual(source, before)
        self.assertEqual(tree[0]["title"], "Parent")
        self.assertEqual([node["id"] for node in tree[0]["children"]], [f"0.{i}" for i in range(6)])
        self.assertEqual(tree[0]["children"][0]["title"], "One=;#\\ 中文")
        self.assertNotIn("end_ms", tree[0]["children"][4])
        self.assertEqual(tree[0]["children"][4]["title"], "Chapter")
        self.assertNotIn("fixture-private", json.dumps(tree))
        self.assertEqual(metadata, ";FFMETADATA1\n"
            "[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1000\ntitle=First\n"
            "[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=500\ntitle=Tied\n"
            "[CHAPTER]\nTIMEBASE=1/1000\nSTART=1000\nEND=2000\ntitle=One\\=\\;\\#\\\\ 中文\n"
            "[CHAPTER]\nTIMEBASE=1/1000\nSTART=3000\nEND=4000\ntitle=Last\n")
        tree[0]["children"][0]["title"] = "Changed"
        self.assertEqual(chapters.prepare(source)[0][0]["children"][0]["title"], "One=;#\\ 中文")
        self.assertEqual(source, before)

    def test_missing_empty_unknown_zero_and_maximum_times(self):
        for info in (None, {}, {"chapters": None}, {"chapters": []}):
            self.assertEqual(chapters.prepare(info), ([], None))
        tree, metadata = chapters.prepare({"chapters": [{}, leaf(length=0),
            leaf(chapters.MAX_DURATION_MS - 1, 1, "Boundary"),
            dict(length_ms=1, title="Implicit zero start")]})
        self.assertNotIn("end_ms", tree[0])
        self.assertEqual(tree[1]["end_ms"], 0)
        self.assertEqual(tree[2]["end_ms"], chapters.MAX_DURATION_MS)
        self.assertEqual(tree[3]["start_ms"], 0)
        self.assertIsNotNone(metadata)
        self.assertEqual(chapters.prepare({"chapters": [{}, leaf(length=0)]})[1], None)
        self.assertEqual(chapters.prepare({"chapters": [leaf(title="é" * 2048)]})[0][0]["title"], "é" * 2048)

    def test_invalid_shapes_times_titles_and_no_raw_errors(self):
        for info in ([], False, "fixture-private", {"chapters": {}}, {"chapters": [None]}, {"chapters": [True]}):
            self.invalid(info)
        for field, values in {
            "start_offset_ms": [None, True, -1, 0.5, "fixture-private", chapters.MAX_DURATION_MS + 1],
            "length_ms": [True, -1, 0.5, float("nan"), float("inf"), "fixture-private", chapters.MAX_DURATION_MS + 1],
            "title": [True, {}, "X" * 4097, "é" * 2049],
            "chapters": [{}, "fixture-private", False],
        }.items():
            for value in values:
                self.invalid({"chapters": [dict(leaf(), **{field: value})]})
        self.invalid({"chapters": [leaf(1, chapters.MAX_DURATION_MS)]})

    def test_depth_node_tree_and_ffmetadata_byte_bounds(self):
        node = leaf()
        for _ in range(chapters.MAX_DEPTH - 1):
            node = leaf(chapters=[node])
        info = {"chapters": [node]}
        chapters.prepare(info)
        self.invalid({"chapters": [leaf(chapters=[node])]})
        cycle = leaf()
        cycle["chapters"] = [cycle]
        self.invalid({"chapters": [cycle]})
        chapters.prepare({"chapters": [leaf(length=0)] * chapters.MAX_NODES})
        self.invalid({"chapters": [leaf(length=0)] * (chapters.MAX_NODES + 1)})
        info = {"chapters": [leaf(title="X")]}
        tree, _ = chapters.prepare(info)
        size = len(json.dumps(tree, ensure_ascii=True, separators=(",", ":")).encode())
        with patch.object(chapters, "MAX_PAGE_BYTES", size):
            chapters.prepare(info)
        with patch.object(chapters, "MAX_PAGE_BYTES", size - 1):
            self.invalid(info)
        info = {"chapters": [leaf(title="#" * 1000)]}
        tree, metadata = chapters.prepare(info)
        size = len(metadata.encode())
        with patch.object(chapters, "MAX_PAGE_BYTES", size):
            chapters.prepare(info)
        with patch.object(chapters, "MAX_PAGE_BYTES", size - 1):
            self.invalid(info)

    def test_actual_four_mib_minus_envelope_boundaries(self):
        # Fill real production budgets, not only patched small-limit fixtures.
        for wire_tree in (True, False):
            info = {"chapters": [leaf(length=0 if wire_tree else 1, title="X") for _ in range(1100)]}
            tree, metadata = chapters.prepare(info)
            used = len(json.dumps(tree, ensure_ascii=True, separators=(",", ":")).encode()) if wire_tree else len(metadata.encode())
            remaining = chapters.MAX_PAGE_BYTES - used
            for item in info["chapters"]:
                width = 1 if wire_tree else 2  # '#' doubles only in ffmetadata.
                added = min(4095, remaining // width)
                item["title"] += ("X" if wire_tree else "#") * added
                remaining -= added * width
                if remaining == 1 and len(item["title"]) < 4096:
                    item["title"] += "X"
                    remaining -= 1
            self.assertEqual(remaining, 0)
            tree, metadata = chapters.prepare(info)
            actual = len(json.dumps(tree, ensure_ascii=True, separators=(",", ":")).encode()) if wire_tree else len(metadata.encode())
            self.assertEqual(actual, (4 << 20) - 512)
            info["chapters"][-1]["title"] += "X"
            self.invalid(info)

    def test_generated_media_copy_remux_roundtrip(self):
        ffmpeg, ffprobe = shutil.which("ffmpeg"), shutil.which("ffprobe")
        if not ffmpeg or not ffprobe:
            if os.environ.get("TALETTY_REQUIRE_MEDIA_TOOLS") == "1":
                self.fail("Mandatory generated-media chapter test requires ffmpeg and ffprobe")
            self.skipTest("Local media tools absent; mandatory CI runs generated-media roundtrip")
        title = "One=;#\\ 中文"
        _, metadata = chapters.prepare({"chapters": [leaf(0, 1000, title), leaf(1000, 1000, "Second")]})
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            meta, source, output = root / "chapters.ffmetadata", root / "source.m4b", root / "copy.m4b"
            meta.write_text(metadata, encoding="utf-8")
            def run(*args):
                return subprocess.run(args, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15).stdout
            run(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono",
                "-t", "2", "-c:a", "aac", "-f", "mp4", str(source))
            run(ffmpeg, "-nostdin", "-v", "error", "-i", str(source), "-f", "ffmetadata", "-i", str(meta),
                "-map", "0:a:0", "-map_metadata", "0", "-map_chapters", "1", "-c:a", "copy", "-f", "mp4", str(output))
            result = json.loads(run(ffprobe, "-v", "error", "-show_chapters", "-of", "json", str(output)))
            self.assertEqual([c["tags"]["title"] for c in result["chapters"]], [title, "Second"])
            self.assertEqual([round(float(c["start_time"]) * 1000) for c in result["chapters"]], [0, 1000])
            self.assertEqual([round(float(c["end_time"]) * 1000) for c in result["chapters"]], [1000, 2000])
            # Demuxed packet bytes match: this extraction must not change encoding.
            def audio_hash(path):
                return run(ffmpeg, "-nostdin", "-v", "error", "-i", str(path), "-map", "0:a:0",
                           "-c:a", "copy", "-f", "hash", "-hash", "sha256", "-")
            self.assertEqual(audio_hash(source), audio_hash(output))
