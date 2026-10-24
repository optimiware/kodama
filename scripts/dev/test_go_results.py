import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import go_results


class GoResultsTest(unittest.TestCase):
    def test_parse_go_test_json_collects_passed_and_excludes_skipped_subtests(self):
        stream = io.StringIO(
            "\n".join(
                [
                    json.dumps({"Package": "github.com/optimiware/kodama/internal/execution", "Action": "run", "Test": "Test_unit_dispatch_replay"}),
                    json.dumps({"Package": "github.com/optimiware/kodama/internal/execution", "Action": "pass", "Test": "Test_unit_dispatch_replay"}),
                    json.dumps({"Package": "github.com/optimiware/kodama/internal/execution", "Action": "pass", "Test": "TestParent/sub_ skipped"}),
                    json.dumps({"Package": "github.com/optimiware/kodama/internal/execution", "Action": "skip", "Test": "TestParent/sub_skipped"}),
                    json.dumps({"Package": "github.com/optimiware/kodama/internal/execution", "Action": "pass", "Test": "TestParent"}),
                    json.dumps({"Package": "github.com/optimiware/kodama/internal/execution", "Action": "pass"}),
                    json.dumps({"Package": "github.com/optimiware/kodama/internal/execution/executiontest", "Action": "output", "Output": "?   \tgithub.com/optimiware/kodama/internal/execution/executiontest\t[no test files]\n"}),
                    json.dumps({"Package": "github.com/optimiware/kodama/internal/execution/executiontest", "Action": "skip"}),
                ]
            )
            + "\n"
        )
        passed, failed, skipped, pkg_failed = go_results.parse_go_test_json(stream)
        self.assertFalse(pkg_failed)
        self.assertIn("unitdispatchreplay", passed)
        self.assertIn("Test_unit_dispatch_replay", passed)
        self.assertNotIn("parent", passed)
        self.assertIn("subskipped", skipped)
        self.assertEqual(failed, [])

    def test_prepare_env_returns_75_and_writes_infra_pause_when_preflight_fails(self):
        class Result:
            returncode = 1
            stdout = "podman down\n"
            stderr = ""

        with tempfile.TemporaryDirectory() as tmp:
            results_file = Path(tmp) / "results.json"
            with patch.dict("os.environ", {"FORGE_RESULTS": str(results_file)}):
                with patch("subprocess.run", return_value=Result()):
                    with self.assertRaises(SystemExit) as ctx:
                        go_results.prepare_env(True)
                    self.assertEqual(ctx.exception.code, go_results.EX_TEMPFAIL)
            payload = json.loads(results_file.read_text())
            self.assertEqual(payload["infra_pause"], "podman down")

    def test_prepare_env_provisions_a_missing_scope_before_preflight(self):
        calls = []

        class Result:
            returncode = 1
            stdout = "podman down\n"
            stderr = ""

        def fake_run(cmd, **kwargs):
            calls.append(cmd)
            return Result()

        with patch.dict("os.environ", {"DEV_SCOPE": "t-unit-missing", "DEV_STACK": "services"}):
            with patch("subprocess.run", side_effect=fake_run):
                with self.assertRaises(SystemExit):
                    go_results.prepare_env(True)
        self.assertIn("up", calls[0])
        self.assertEqual(calls[0][calls[0].index("--stack") + 1], "services")
        self.assertIn("preflight", calls[1])

    def test_prepare_env_does_not_provision_when_disabled(self):
        calls = []

        class Result:
            returncode = 1
            stdout = "x\n"
            stderr = ""

        with patch.dict("os.environ", {"DEV_SCOPE": "t-unit-missing", "KODAMA_AUTO_PROVISION": "0"}):
            with patch("subprocess.run", side_effect=lambda cmd, **kw: calls.append(cmd) or Result()):
                with self.assertRaises(SystemExit):
                    go_results.prepare_env(True)
        self.assertEqual(len(calls), 1)
        self.assertIn("preflight", calls[0])


if __name__ == "__main__":
    unittest.main()
