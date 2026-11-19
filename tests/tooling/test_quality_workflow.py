import itertools
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


class QualityWorkflowTests(unittest.TestCase):
    def test_media_setup_is_signed_invocation_local_and_fail_closed(self):
        workflow = (ROOT / ".github/workflows/quality.yml").read_text()
        self.assertEqual(workflow.count(".github/scripts/install-ci-media.sh"), 2)
        self.assertEqual(workflow.count("TALETTY_REQUIRE_MEDIA_TOOLS"), 2)
        self.assertNotIn("continue-on-error", workflow)
        for ffmpeg, ffprobe, apt_status in [(0, 0, 0), (1, 0, 0), (0, 1, 0), (1, 1, 17)]:
            with self.subTest(ffmpeg=ffmpeg, ffprobe=ffprobe, apt_status=apt_status), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                log, copy = root / "args", root / "sources"
                # Shell functions replace only executable discovery and privilege/
                # network operations. Execute the real setup script and cleanup.
                prefix = f'''
command() {{ if [[ "$2" == ffmpeg ]]; then return {ffmpeg}; else return {ffprobe}; fi; }}
sudo() {{
  printf '%s\\n' "$*" >> "$LOG"
  cp "${{3#Dir::Etc::sourcelist=}}" "$COPY"
  return {apt_status}
}}
source "$SCRIPT"
'''
                env = dict(os.environ, LOG=str(log), COPY=str(copy), SCRIPT=str(ROOT / ".github/scripts/install-ci-media.sh"))
                result = subprocess.run(["bash", "-c", prefix], env=env, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, apt_status, result.stderr)
                if ffmpeg == ffprobe == 0:
                    self.assertFalse(log.exists())
                    continue
                calls = log.read_text().splitlines()
                self.assertEqual(len(calls), 1 if apt_status else 2)
                for call in calls:
                    self.assertIn("Dir::Etc::sourceparts=-", call)
                    source = call.split("Dir::Etc::sourcelist=", 1)[1].split()[0]
                    self.assertFalse(Path(source).exists(), "temporary source list leaked")
                self.assertTrue(calls[0].endswith(" update"))
                if not apt_status:
                    self.assertTrue(calls[1].endswith(" install -y --no-install-recommends ffmpeg"))
                lines = copy.read_text().splitlines()
                self.assertEqual(len(lines), 3)
                for line in lines:
                    self.assertTrue(line.startswith("deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] https://"))
                    self.assertNotIn("ec2", line)
                self.assertIn("https://archive.ubuntu.com/ubuntu noble ", lines[0])
                self.assertIn("https://archive.ubuntu.com/ubuntu noble-updates ", lines[1])
                self.assertIn("https://security.ubuntu.com/ubuntu noble-security ", lines[2])

    def test_adapter_cannot_be_omitted_from_aggregate(self):
        workflow = (ROOT / ".github/workflows/quality.yml").read_text()
        self.assertIn("  python-adapter:\n", workflow)
        gate = workflow.split("  quality-gate:\n", 1)[1]
        self.assertIn("if: always()", gate)
        self.assertIn("needs: [coverage-tooling, module-coverage, python-adapter]", gate)
        self.assertIn("ADAPTER_RESULT: ${{ needs.python-adapter.result }}", gate)
        script = gate.split("        run: |\n", 1)[1]
        states = ["success", "failure", "cancelled", "skipped"]
        for values in itertools.product(states, repeat=3):
            env = dict(os.environ, **dict(zip(("TOOLING_RESULT", "MODULE_RESULT", "ADAPTER_RESULT"), values)))
            result = subprocess.run(["bash", "-e", "-c", script], env=env, capture_output=True, timeout=5)
            self.assertEqual(result.returncode == 0, all(value == "success" for value in values), values)
