import itertools
import os
from pathlib import Path
import subprocess
import unittest


class QualityWorkflowTests(unittest.TestCase):
    def test_adapter_cannot_be_omitted_from_aggregate(self):
        workflow = (Path(__file__).resolve().parents[2] / ".github/workflows/quality.yml").read_text()
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
