from contextlib import redirect_stderr, redirect_stdout
import io
import json
from pathlib import Path
import runpy
import sys
import tempfile
import unittest
from unittest.mock import patch

from tools import check_inventory as inventory


class InventoryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.write((inventory.ADAPTER / "doctor.py").as_posix(), "pass\n")
        for directory, module in inventory.MODULES.items():
            self.write(f"{directory}/go.mod", f"module {module}\n\ngo 1.26.0\n")

    def write(self, filename, content=""):
        path = self.root / filename
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")
        return path

    def test_registered_sources_and_generated_directories(self):
        for filename in ["engine/model/book.go", "app/internal/profile/paths.go",
                         "tools/check.py", "tests/tooling/test_check.py"]:
            self.write(filename)
        for directory in inventory.IGNORED:
            self.write(f"{directory}/nested/go.mod", "module ignored\n")
            self.write(f"{directory}/extra.py")
        self.assertEqual(inventory.inventory(self.root), ["app", "engine"])

    def test_missing_unregistered_and_nested_modules_fail(self):
        path = self.root / "app/go.mod"
        original = path.read_text()
        path.unlink()
        with self.assertRaisesRegex(ValueError, "registered"):
            inventory.inventory(self.root)
        path.write_text(original)
        for filename in ["go.mod", "extra/go.mod", "engine/nested/go.mod"]:
            path = self.write(filename, "module unregistered\n")
            with self.subTest(filename=filename), self.assertRaisesRegex(ValueError, "registered"):
                inventory.inventory(self.root)
            path.unlink()

    def test_bad_or_missing_module_identity_fails(self):
        for content in ["", "go 1.26.0\n", "module wrong/path\n"]:
            self.write("engine/go.mod", content)
            with self.subTest(content=content), self.assertRaisesRegex(ValueError, "identity"):
                inventory.inventory(self.root)

    def test_omitted_adapter_and_unowned_source_fail(self):
        for filename in ["engine/bridge/adapter.py", "app/adapter.py", "other/helper.py",
                         "other/package.go"]:
            path = self.write(filename)
            with self.subTest(filename=filename), self.assertRaises(ValueError):
                inventory.inventory(self.root)
            path.unlink()

    def test_adapter_report_cannot_omit_add_or_remove_sources(self):
        source = (inventory.ADAPTER / "doctor.py").as_posix()
        report = self.write("coverage/adapter.json", json.dumps({"files": {source: {}}}))
        self.assertEqual(inventory.inventory(self.root, report), ["app", "engine"])
        for data in [[], {}, {"files": []}, {"files": {}}, {"files": {"wrong.py": {}}}]:
            report.write_text(json.dumps(data))
            with self.assertRaises(ValueError):
                inventory.inventory(self.root, report)
        report.write_text(json.dumps({"files": {source: {}}}))
        extra = self.write((inventory.ADAPTER / "new.py").as_posix(), "uncovered = True\n")
        with self.assertRaisesRegex(ValueError, "every discovered"):
            inventory.inventory(self.root, report)
        extra.unlink()
        (self.root / source).unlink()
        with self.assertRaisesRegex(ValueError, "source is missing"):
            inventory.inventory(self.root)

    def test_adapter_report_cli_rejects_missing_and_malformed(self):
        for content in [None, "not JSON"]:
            report = self.root / "report.json"
            if content is not None:
                report.write_text(content)
            with redirect_stderr(io.StringIO()):
                self.assertEqual(inventory.main(["--root", str(self.root), "--adapter-report", str(report)]), 1)

    def test_cli_success_and_failure(self):
        out, error = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(error):
            self.assertEqual(inventory.main(["--root", str(self.root)]), 0)
        self.assertEqual(out.getvalue(), 'modules=["app", "engine"]\n')
        self.assertEqual(error.getvalue(), "")
        with redirect_stderr(error):
            self.assertEqual(inventory.main(["--root", str(self.root / "missing")]), 1)
        self.assertIn("FAIL inventory", error.getvalue())

    def test_cli_read_errors_are_failures_not_success(self):
        for exception in [OSError("read failed"), UnicodeError("bad encoding")]:
            with patch.object(inventory, "inventory", side_effect=exception):
                with redirect_stderr(io.StringIO()):
                    self.assertEqual(inventory.main([]), 1)

    def test_entrypoint_preserves_exit_status(self):
        args = ["check_inventory.py", "--root", str(self.root)]
        with patch.object(sys, "argv", args), redirect_stdout(io.StringIO()):
            with self.assertRaises(SystemExit) as result:
                runpy.run_path(str(Path(inventory.__file__)), run_name="__main__")
        self.assertEqual(result.exception.code, 0)
