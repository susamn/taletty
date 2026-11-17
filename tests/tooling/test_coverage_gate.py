"""Synthetic reports exercise coverage policy, not application coverage."""

from contextlib import redirect_stderr, redirect_stdout
import io
import json
from pathlib import Path
import runpy
import sys
import tempfile
import unittest
from unittest.mock import patch

from tools import coverage_gate as gate


class CoverageGateTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def write(self, name, content):
        path = self.root / name
        path.write_text(content, encoding="utf-8")
        return path

    def python_report(self, covered=95, total=100):
        summary = {"covered_lines": covered, "num_statements": total,
                   "excluded_lines": 0}
        return {"files": {"bridge.py": {"summary": summary.copy()}},
                "totals": summary.copy()}

    def run_gate(self, *args):
        stdout, stderr = io.StringIO(), io.StringIO()
        with redirect_stdout(stdout), redirect_stderr(stderr):
            code = gate.main(list(args))
        return code, stdout.getvalue(), stderr.getvalue()

    def test_python_exact_thresholds_ignore_rounded_and_branch_percentages(self):
        for covered, total, expected in [(9499, 10000, 1), (95, 100, 0), (1, 1, 0)]:
            with self.subTest(covered=covered):
                report = self.python_report(covered, total)
                report["totals"]["percent_covered"] = 100
                report["totals"]["num_branches"] = 200
                path = self.write("python.json", json.dumps(report))
                code, out, error = self.run_gate("--require", "bridge", "--python",
                                                 f"bridge={path}")
                self.assertEqual(code, expected)
                self.assertIn("below 95%" if expected else "PASS bridge", error or out)

    def test_go_counts_weight_statements_not_lines_or_hits(self):
        for mode in ["set", "count", "atomic"]:
            path = self.write("go.out", f"mode: {mode}\nexample/main.go:1.1,2.5 95 1\n"
                              "example/main.go:3.1,5.2 5 0\n"
                              "example/empty.go:1.1,1.2 0 100\n")
            self.assertEqual(gate.go_counts(path), (95, 100))
            code, out, error = self.run_gate("--require", "engine", "--go", f"engine={path}")
            self.assertEqual((code, error), (0, ""))
            self.assertIn("95/100", out)
        path = self.write("go.out", "mode: atomic\nx.go:1.1,2.1 9499 100\nx.go:3.1,4.1 501 0\n")
        self.assertEqual(self.run_gate("--require", "engine", "--go", f"engine={path}")[0], 1)

    def test_independent_targets_cannot_pool_coverage(self):
        go = self.write("go.out", "mode: atomic\nx.go:1.1,2.1 10000 1\n")
        python = self.write("python.json", json.dumps(self.python_report(0, 1)))
        code, _, error = self.run_gate("--require", "engine", "--require", "bridge",
                                       "--go", f"engine={go}", "--python", f"bridge={python}")
        self.assertEqual(code, 1)
        self.assertIn("bridge: 0/1", error)

    def test_missing_empty_invalid_and_non_utf8_reports_fail(self):
        cases = [("missing", None), ("empty", b""), ("json", b"{"),
                 ("binary", b"\xff"), ("list", b"[]")]
        for name, data in cases:
            with self.subTest(name=name):
                path = self.root / name
                if data is not None:
                    path.write_bytes(data)
                code, _, error = self.run_gate("--require", "bridge", "--python", f"bridge={path}")
                self.assertEqual(code, 1)
                self.assertIn("FAIL coverage", error)
        code, _, error = self.run_gate("--require", "bridge", "--python", f"bridge={self.root}")
        self.assertEqual(code, 1)

    def test_no_executable_statements_is_not_success(self):
        for kind, data in [("python", json.dumps(self.python_report(0, 0))),
                           ("go", "mode: atomic\n")]:
            path = self.write(kind, data)
            code, _, error = self.run_gate("--require", "empty", f"--{kind}", f"empty={path}")
            self.assertEqual(code, 1)
            self.assertIn("no executable statements", error)

    def test_python_rejects_missing_mismatched_and_excluded_counts(self):
        invalid = [None, [], {}, {"covered_lines": 0},
                   {"covered_lines": True, "num_statements": 1, "excluded_lines": 0},
                   {"covered_lines": 2, "num_statements": 1, "excluded_lines": 0},
                   {"covered_lines": -1, "num_statements": 1, "excluded_lines": 0},
                   {"covered_lines": 1, "num_statements": 1, "excluded_lines": 1},
                   {"covered_lines": 1.0, "num_statements": 1, "excluded_lines": 0}]
        for summary in invalid:
            with self.subTest(summary=summary), self.assertRaises(ValueError):
                gate.counts(summary)
        for report in [{}, {"files": []}, {"files": {}},
                       {"files": {" ": {}}}, {"files": {"x.py": []}}]:
            path = self.write("python.json", json.dumps(report))
            with self.subTest(report=report), self.assertRaises(ValueError):
                gate.python_counts(path)
        report = self.python_report()
        report["totals"]["covered_lines"] = 96
        path = self.write("python.json", json.dumps(report))
        with self.assertRaisesRegex(ValueError, "totals do not match"):
            gate.python_counts(path)

    def test_python_sums_multiple_files_including_empty_file(self):
        report = self.python_report()
        report["files"]["empty.py"] = {"summary": self.python_report(0, 0)["totals"]}
        path = self.write("python.json", json.dumps(report))
        self.assertEqual(gate.python_counts(path), (95, 100))

    def test_malformed_go_blocks_rejected(self):
        for content in ["", "mode: bogus\n", "mode: atomic\n\n",
                        "mode: atomic\nnot a record\n",
                        "mode: atomic\nx.go:0.1,1.1 1 1\n",
                        "mode: atomic\nx.go:3.1,1.1 1 1\n",
                        "mode: atomic\nx.go:1.5,1.1 1 1\n",
                        "mode: atomic\nx.go:1.1,2.1 -1 1\n",
                        "mode: atomic\nx.go:1.1,2.1 1 -1\n",
                        "mode: atomic\nx.go:1.1,2.1 1 1\nx.go:1.1,2.1 1 0\n"]:
            path = self.write("go.out", content)
            with self.subTest(content=content), self.assertRaises(ValueError):
                gate.go_counts(path)

    def test_inventory_rejects_omitted_unexpected_duplicate_and_bad_targets(self):
        path = self.root / "go.out"
        cases = [([], [], ["engine"]), ([f"engine={path}"], [], ["app"]),
                 ([], [], ["bad/name"]), ([], [], ["engine", "engine"]),
                 ([f"engine={path}", f"engine={path}"], [], ["engine"]),
                 ([f"engine={path}"], [f"app={path}"], ["engine", "app"]),
                 (["no-equals"], [], ["engine"]), (["engine="], [], ["engine"]),
                 (["Bad=path"], [], ["engine"])]
        for go, python, required in cases:
            with self.subTest(args=(go, python, required)), self.assertRaises(ValueError):
                gate.targets(go, python, required)
        code, _, error = self.run_gate("--require", "engine")
        self.assertEqual(code, 1)
        self.assertIn("exactly match", error)

    def test_cli_requires_inventory_and_propagates_failure(self):
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as result:
            gate.main([])
        self.assertEqual(result.exception.code, 2)
        path = self.write("go.out", "mode: atomic\nx.go:1.1,2.1 1 1\n")
        argv = ["coverage_gate.py", "--require", "engine", "--go", f"engine={path}"]
        with patch.object(sys, "argv", argv), redirect_stdout(io.StringIO()):
            with self.assertRaises(SystemExit) as result:
                runpy.run_path(str(Path(gate.__file__)), run_name="__main__")
        self.assertEqual(result.exception.code, 0)
        argv[-1] = f"engine={self.root / 'missing'}"
        with patch.object(sys, "argv", argv), redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as result:
                runpy.run_path(str(Path(gate.__file__)), run_name="__main__")
        self.assertEqual(result.exception.code, 1)


if __name__ == "__main__":
    unittest.main()
