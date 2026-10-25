"""Offline tests of the real shell runner with a disposable repository and fake Go/Kubernetes.

No cluster/provider credentials or real service processes are used.
"""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parent
PACKAGE = "github.com/optimiware/kodama/"


class RunnerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="kodama-runner-")
        self.addCleanup(self.tmp.cleanup)
        scratch = Path(self.tmp.name)
        self.repo = scratch / "repo"
        self.scripts = self.repo / "deploy/stage/scripts"
        self.scripts.mkdir(parents=True)
        for name in ("lib.sh", "readiness-cases.json"):
            shutil.copyfile(SOURCE / name, self.scripts / name)
        chart = self.repo / "deploy/helm/kodama"
        chart.mkdir(parents=True)
        (chart / "Chart.yaml").write_text("name: offline-fixture\n")
        for args in (["init", "-q"], ["add", "."], ["-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"]):
            subprocess.run(["git", *args], cwd=self.repo, check=True, capture_output=True)
        self.session = scratch / "state/qualification/fixture"
        self.session.mkdir(parents=True)
        self.contract = {
            "cloud": "scw", "installation_id": "fixture", "cluster": {"endpoint": "https://cluster.invalid"},
            "network": {"runtime_url": "https://fixture.invalid"}, "tenants": [],
            "object_storage": {"endpoint": "https://s3.invalid", "artifacts_bucket": "fixture", "artifacts_prefix": "fixture/"},
        }
        (self.session / "contract.json").write_text(json.dumps(self.contract))
        (self.session / "values.yaml").write_text("fixture: true\n")
        (self.session / "session.json").write_text(json.dumps({
            "name": "fixture", "namespace": "kodama-q-fixture", "installation_id": "fixture",
            "url": "https://fixture.invalid", "tenants": [],
        }))
        kc = scratch / "kubeconfig"
        kc.touch()
        bin_dir = scratch / "bin"
        bin_dir.mkdir()
        self.fake(bin_dir / "kubectl", "import sys\nprint('https://cluster.invalid' if 'config' in sys.argv else 'kodama-q-fixture', end='')\n")
        self.fake(bin_dir / "go", '''import json, os, pathlib, subprocess, sys
args = sys.argv[1:]
mapping = json.loads(pathlib.Path('deploy/stage/scripts/readiness-cases.json').read_text())
pkgs = sorted(set(x['package'] for x in mapping.values()))
chart = '-run' in args
if args[0] == 'list':
    for pkg in pkgs:
        if any(pkg.endswith(a.removeprefix('./').removesuffix('/...')) for a in args[1:]): print(pkg)
    sys.exit(0)
if os.environ.get('MOCK_COMMIT') and chart:
    pathlib.Path('deploy/helm/kodama/Chart.yaml').write_text('changed: true')
    subprocess.run(['git', 'add', '.'], check=True)
    subprocess.run(['git', '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '-qm', 'drift'], check=True)
if chart: pkgs = [x for x in pkgs if x.endswith('kube-full')]
if os.environ.get('MOCK_DIRTY'): pathlib.Path('unexpected-change').write_text('drift')
if os.environ.get('MOCK_VALUES_DRIFT'): pathlib.Path(os.environ['KODAMA_STAGE_VALUES']).write_text('drift')
def event(pkg, test, action): print(json.dumps({'Package':pkg, 'Test':test, 'Action':action}), flush=True)
for name,spec in mapping.items():
    if (name == 'kube_chart_install') != chart: continue
    event(spec['package'], spec['test'], 'pass')
    for case in spec['cases']:
        event(spec['package'], spec['test']+'/'+case, 'skip' if case == os.environ.get('MOCK_SKIP_CASE') else 'pass')
for pkg in pkgs:
    if os.environ.get('MOCK_TRUNCATE') and pkg.endswith('assembly-execution'): continue
    event(pkg, '', 'pass')
sys.exit(int(os.environ.get('MOCK_RUNNER_EXIT', '0')))
''')
        self.env = dict(os.environ)
        self.env.update({
            "PATH": str(bin_dir) + os.pathsep + os.environ["PATH"], "KUBECONFIG": str(kc),
            "KODAMA_QUAL_SESSION": "fixture", "KODAMA_QUAL_STATE_DIR": str(scratch / "state"),
            "KODAMA_STAGE_PROBE_IMAGE": "registry.invalid/probe@sha256:" + "a" * 64,
            "SCW_DEFAULT_PROJECT_ID": "11111111-2222-4333-8444-555555555555",
        })
        self.results = scratch / "results.json"

    def fake(self, path, code):
        path.write_text("#!" + sys.executable + "\n" + code)
        path.chmod(0o755)

    def run_suite(self, **extra):
        return subprocess.run([
            "bash", "-c", 'source "$1/lib.sh"; run_stage_go_suite "$2/contract.json" "$2/values.yaml" "$3" ./test/e2e/kube-full/... ./test/integration/assembly-execution/...',
            "fixture", str(self.scripts), str(self.session), str(self.results),
        ], env=self.env | extra, cwd=self.repo, text=True, capture_output=True, timeout=20)

    def verify(self):
        return subprocess.run([
            "bash", "-c", 'source "$1/lib.sh"; verify_forge_results "$3" "$2/contract.json" "$2/values.yaml" kube_chart_install kube_cancel_restart kube_secrets_kms execution_assembly_roundtrip',
            "fixture", str(self.scripts), str(self.session), str(self.results),
        ], env=self.env, cwd=self.repo, capture_output=True, text=True, timeout=20)

    def test_pass_has_chart_first_and_exact_cases(self):
        result = self.run_suite()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertLess(result.stdout.index("phase=chart-install"), result.stdout.index("phase=scenarios"))
        self.assertIn("runner_exit=0", result.stdout)
        self.assertEqual(self.verify().returncode, 0)
        data = json.loads(self.results.read_text())
        self.assertTrue(data["provenance"]["candidate_stable"])
        self.assertEqual(len(data["acceptances"]), 7)

    def test_skipped_required_child_never_counts_as_parent_pass(self):
        self.run_suite(MOCK_SKIP_CASE="lost_create_acknowledgment")
        self.assertNotEqual(self.verify().returncode, 0)

    def test_dirty_candidate_refused_before_tests(self):
        (self.repo / "uncommitted").write_text("dirty")
        result = self.run_suite()
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("phase=chart-install", result.stdout)

    def test_mutations_and_truncated_packages_fail(self):
        for flag in ("MOCK_DIRTY", "MOCK_VALUES_DRIFT", "MOCK_TRUNCATE", "MOCK_RUNNER_EXIT"):
            with self.subTest(flag=flag):
                result = self.run_suite(**{flag: "1"})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("phase=chart-install", result.stdout)
                data = json.loads(self.results.read_text())
                self.assertFalse(data["provenance"]["suite_passed"])
                (self.repo / "unexpected-change").unlink(missing_ok=True)
                (self.session / "values.yaml").write_text("fixture: true\n")

    def test_preflight_does_not_run_tests_or_issue_pass(self):
        result = self.run_suite(KODAMA_STAGE_PREFLIGHT_ONLY="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("phase=chart-install", result.stdout)
        self.assertFalse(json.loads(self.results.read_text())["provenance"]["suite_passed"])

    def test_commit_drift_preserves_actual_runner_exit(self):
        result = self.run_suite(MOCK_COMMIT="1")
        self.assertNotEqual(result.returncode, 0)
        data = json.loads(self.results.read_text())
        self.assertEqual(data["runner_exit"], 0)
        self.assertEqual(data["qualification_exit"], 1)
        self.assertFalse(data["provenance"]["candidate_stable"])

    def test_busy_session_fails_before_tests(self):
        import fcntl
        with (self.session / "qualification.lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.run_suite()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("already locked", result.stderr)
        self.assertNotIn("phase=chart-install", result.stdout)

    def test_mismatched_inputs_fail_before_tests(self):
        handle = json.loads((self.session / "session.json").read_text())
        handle["installation_id"] = "different"
        (self.session / "session.json").write_text(json.dumps(handle))
        result = self.run_suite()
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("phase=chart-install", result.stdout)


if __name__ == "__main__":
    unittest.main()
