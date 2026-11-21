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
        for ffmpeg, ffprobe, mpv, apt_status in [(0, 0, 0, 0), (1, 0, 0, 0), (0, 1, 0, 0), (0, 0, 1, 0), (1, 1, 1, 17)]:
            with self.subTest(ffmpeg=ffmpeg, ffprobe=ffprobe, mpv=mpv, apt_status=apt_status), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                log, copy = root / "args", root / "sources"
                # Shell functions replace only executable discovery and privilege/
                # network operations. Execute the real setup script and cleanup.
                prefix = f'''
command() {{
  case "$2" in
    ffmpeg) return {ffmpeg} ;;
    ffprobe) return {ffprobe} ;;
    mpv) return {mpv} ;;
    *) return 1 ;;
  esac
}}
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
                if ffmpeg == ffprobe == mpv == 0:
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
                    self.assertTrue(calls[1].endswith(" install -y --no-install-recommends ffmpeg mpv"))
                lines = copy.read_text().splitlines()
                self.assertEqual(len(lines), 3)
                for line in lines:
                    self.assertTrue(line.startswith("deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] https://"))
                    self.assertNotIn("ec2", line)
                self.assertIn("https://archive.ubuntu.com/ubuntu noble ", lines[0])
                self.assertIn("https://archive.ubuntu.com/ubuntu noble-updates ", lines[1])
                self.assertIn("https://security.ubuntu.com/ubuntu noble-security ", lines[2])

    def test_real_terminal_journeys_are_mandatory_in_app_matrix(self):
        workflow = (ROOT / ".github/workflows/quality.yml").read_text()
        step = workflow.split("      - name: Repeat real controlling-PTY shell journeys\n", 1)[1].split("      - name:", 1)[0]
        self.assertIn("if: matrix.module == 'app'", step)
        self.assertIn("go test -count=3 -race -timeout=60s -run '^TestControllingTerminalJourney$' -v ./internal/ui", step)
        self.assertNotIn("continue-on-error", workflow)
        self.assertNotIn("TALETTY_TERMINAL_FIXTURE", workflow)
        source = (ROOT / "app/internal/ui/terminal_linux_test.go").read_text()
        journey = source.split("func TestControllingTerminalJourney", 1)[1].split("type failingTerminalWriter", 1)[0]
        self.assertNotIn("t.Skip", journey)
        self.assertNotIn("os.Getenv", journey)

    def test_real_mpv_playback_journeys_repeat_with_required_real_media(self):
        workflow = (ROOT / ".github/workflows/quality.yml").read_text()
        media_step = workflow.split("      - name: Install actual CI media tools\n", 1)[1].split("      - name:", 1)[0]
        self.assertNotIn("if:", media_step)
        self.assertIn("bash ../.github/scripts/install-ci-media.sh", media_step)
        self.assertIn("echo 'TALETTY_REQUIRE_MEDIA_TOOLS=1' >>", media_step)
        step = workflow.split("      - name: Repeat real null-output mpv playback journeys\n", 1)[1].split("      - name:", 1)[0]
        self.assertIn("if: matrix.module == 'app'", step)
        self.assertIn("go test -count=3 -race -timeout=60s -run '^TestRealMPV' -v ./internal/playback", step)
        self.assertNotIn("continue-on-error", step)
        source = (ROOT / "app/internal/playback/mpv_integration_linux_test.go").read_text()
        self.assertIn('os.Getenv("TALETTY_REQUIRE_MEDIA_TOOLS") == "1"', source)
        self.assertIn('t.Fatalf("mandatory playback media tool %s missing", tool)', source)

    def test_private_composition_journeys_repeat_with_required_real_media(self):
        workflow = (ROOT / ".github/workflows/quality.yml").read_text()
        step = workflow.split("      - name: Repeat private licensed composition process journeys\n", 1)[1].split("      - name:", 1)[0]
        self.assertIn("if: matrix.module == 'engine'", step)
        self.assertIn("go test -count=3 -race -timeout=60s -run '^TestComposition' -v ./connectors/audible", step)
        self.assertNotIn("continue-on-error", step)
        self.assertIn("echo 'TALETTY_REQUIRE_MEDIA_TOOLS=1' >>", workflow)
        source = (ROOT / "engine/connectors/audible/composition_process_linux_test.go").read_text()
        self.assertIn('os.Getenv("TALETTY_REQUIRE_MEDIA_TOOLS") == "1"', source)
        self.assertIn('t.Fatal("mandatory composition process media tool missing")', source)

    def test_sql_contract_is_mandatory_in_tooling_dependency(self):
        workflow = (ROOT / ".github/workflows/quality.yml").read_text()
        tooling = workflow.split("  coverage-tooling:\n", 1)[1].split("  python-adapter:\n", 1)[0]
        step = tooling.split("      - name: Execute app-owned SQLite schema contract\n", 1)[1].split("      - name:", 1)[0]
        self.assertEqual(step.strip(), "run: python tests/storage/test_schema.py")
        self.assertNotIn("continue-on-error", workflow)
        # Execute the same entry point; the runner rejects zero tests and skips.
        result = subprocess.run([os.sys.executable, "tests/storage/test_schema.py"], cwd=ROOT,
                                capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("SQLite contract oracle: SQLite", result.stdout)
        self.assertIn("Ran 8 tests", result.stderr)

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
