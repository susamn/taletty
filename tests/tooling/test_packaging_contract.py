"""Verify reproducible adapter lockfile, version pins, and ADR 003 cross-references."""

from pathlib import Path
import re
import unittest

from tools import check_inventory as inventory


ROOT = Path(__file__).resolve().parents[2]
ADAPTER_DIR = ROOT / "engine/connectors/audible/internal/bridge/adapter"
EXACT_PIN = re.compile(r"^([A-Za-z0-9][A-Za-z0-9._-]*)==([A-Za-z0-9.+!_-]+)$")
MARKDOWN_LINK = re.compile(r"\[[^\]]+\]\(([^)]+)\)")


def normalize_name(name):
    return re.sub(r"[-_.]+", "-", name).lower()


def parse_exact_pins(path):
    pins = {}
    for lineno, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if "#" in line:
            line = line.split("#", 1)[0].strip()
        match = EXACT_PIN.fullmatch(line)
        if not match:
            raise AssertionError(f"{path}:{lineno}: expected exact NAME==VERSION pin, got {raw!r}")
        name, version = normalize_name(match.group(1)), match.group(2)
        if name in pins:
            raise AssertionError(f"{path}:{lineno}: duplicate package {name!r}")
        pins[name] = version
    if not pins:
        raise AssertionError(f"{path}: expected at least one pinned package")
    return pins


class PackagingContractTests(unittest.TestCase):
    def test_adapter_lockfile_pins_exact_runtime_dependencies(self):
        lock_path = ADAPTER_DIR / "requirements.lock"
        raw = lock_path.read_text(encoding="utf-8")
        for forbidden in ("git+", "http://", "https://", "file://", "/tmp/", "/home/", " -e ", "--index-url"):
            self.assertNotIn(forbidden, raw)

        pins = parse_exact_pins(lock_path)
        self.assertEqual(pins.get("audible"), "0.13.1")
        for excluded in ("coverage", "pip", "setuptools", "wheel"):
            self.assertNotIn(excluded, pins)

        expected_runtime = {
            "anyio",
            "audible",
            "beautifulsoup4",
            "certifi",
            "h11",
            "httpcore",
            "httpx",
            "idna",
            "pbkdf2",
            "pillow",
            "pyaes",
            "pyasn1",
            "rsa",
            "soupsieve",
            "typing-extensions",
        }
        self.assertEqual(set(pins), expected_runtime)

    def test_tools_and_adapter_requirements_match_pins(self):
        tool_pins = parse_exact_pins(ROOT / "tools/requirements.txt")
        self.assertEqual(tool_pins, {"coverage": "7.6.1"})

        adapter_req_path = ADAPTER_DIR / "requirements.txt"
        adapter_pins = parse_exact_pins(adapter_req_path)
        self.assertEqual(adapter_pins, {"audible": "0.13.1"})
        self.assertIn("requirements.lock", adapter_req_path.read_text(encoding="utf-8"))

    def test_go_python_and_sqlite_contract_pins_agree(self):
        doctor_src = (ADAPTER_DIR / "doctor.py").read_text(encoding="utf-8")
        self.assertIn("PROTOCOL = 1", doctor_src)
        self.assertIn('AUDIBLE_VERSION = "0.13.1"', doctor_src)
        self.assertIn("(3, 12)", doctor_src)

        stream_src = (ROOT / "engine/connectors/audible/internal/bridge/stream.go").read_text(encoding="utf-8")
        self.assertRegex(stream_src, r"(?m)^\s*Version\s*=\s*1\s*$")

        version_src = (ROOT / "engine/connectors/audible/version.go").read_text(encoding="utf-8")
        self.assertIn('ContractVersion = "v0.1.0"', version_src)
        self.assertIn("BridgeProtocolVersion = bridge.Version", version_src)
        self.assertIn('PinnedPythonVersion = "3.12"', version_src)
        self.assertIn('PinnedAudibleVersion = "0.13.1"', version_src)

        for mod in ("app/go.mod", "engine/go.mod"):
            self.assertRegex((ROOT / mod).read_text(encoding="utf-8"), r"(?m)^go 1\.26\.0$")

        store_src = (ROOT / "app/internal/store/validated_linux.go").read_text(encoding="utf-8")
        self.assertIn("major == 3 && minor >= 38", store_src)

    def test_inventory_accepts_lockfile_and_packaging_tests(self):
        self.assertEqual(inventory.inventory(ROOT), ["app", "engine"])

    def test_adr_003_and_docs_cross_references_and_links(self):
        adr_path = ROOT / "docs/adr/003-runtime-and-platform-matrix.md"
        adr = adr_path.read_text(encoding="utf-8")
        for required in (
            "linux/amd64",
            "linux/arm64",
            "GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...",
            "darwin/amd64",
            "darwin/arm64",
            "Windows",
            "Go 1.26.0+",
            "go1.26.11",
            "go1.27.4",
            "CPython 3.12",
            "3.12.16",
            "3.12.3",
            "glibc 2.39",
            ">=3.38",
            "3.51.3",
            "3.45.1",
            "7:6.1.1",
            "0.37.0",
            "audible==0.13.1",
            "requirements.lock",
            "taletty",
            "taletty-engine",
            "~/.config/taletty/",
            "~/.config/audible-tui/",
            "SHA256SUMS",
            "SPDX",
            "CycloneDX",
            "#27",
            "#28",
            "#29",
            "../distribution.md",
        ):
            self.assertIn(required, adr)

        doc_expectations = {
            ROOT / "docs/distribution.md": ("adr/003-runtime-and-platform-matrix.md", "requirements.lock", "taletty-engine"),
            ROOT / "engine/README.md": ("../docs/adr/003-runtime-and-platform-matrix.md", "requirements.lock"),
            ROOT / "README.md": ("docs/adr/003-runtime-and-platform-matrix.md", "docs/distribution.md"),
            ROOT / "docs/testing.md": ("adr/003-runtime-and-platform-matrix.md", "requirements.lock", "test_packaging_contract.py"),
        }
        for doc_path, needles in doc_expectations.items():
            text = doc_path.read_text(encoding="utf-8")
            for needle in needles:
                self.assertIn(needle, text, f"{doc_path} missing {needle!r}")

        for doc_path in (adr_path, *doc_expectations):
            text = doc_path.read_text(encoding="utf-8")
            for target in MARKDOWN_LINK.findall(text):
                if target.startswith(("http://", "https://", "#")):
                    continue
                rel = target.split("#", 1)[0]
                resolved = (doc_path.parent / rel).resolve()
                self.assertTrue(resolved.exists(), f"{doc_path}: broken relative link {target!r} -> {resolved}")
