"""Offline tests of the real shell runner with a disposable repository and fake Go/Kubernetes.

No cluster/provider credentials or real service processes are used.
"""

import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest


SOURCE = Path(__file__).resolve().parent
PACKAGE = "github.com/optimiware/kodama/"


class FakeRegistry:
    """A loopback registry serving two digest-pinned images whose config carries a revision label."""

    def __init__(self, revisions, tamper=(), token=False):
        self.blobs = {}
        self.refs = {}
        self.token = token
        for name, revision in revisions.items():
            labels = {"org.opencontainers.image.revision": revision} if revision is not None else {}
            config = json.dumps({"config": {"Labels": labels}}).encode()
            config_digest = "sha256:" + hashlib.sha256(config).hexdigest()
            manifest = json.dumps({"schemaVersion": 2, "config": {"digest": config_digest}, "layers": []}).encode()
            digest = "sha256:" + hashlib.sha256(manifest).hexdigest()
            served = manifest + b" " if name in tamper else manifest
            self.blobs[f"/v2/fixture/{name}/manifests/{digest}"] = served
            self.blobs[f"/v2/fixture/{name}/blobs/{config_digest}"] = config
            self.refs[name] = digest
        registry = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                path = self.path.split("?")[0]
                if path == "/token":
                    body = json.dumps({"token": "anonymous-pull"}).encode()
                elif registry.token and self.headers.get("Authorization") != "Bearer anonymous-pull":
                    self.send_response(401)
                    self.send_header("WWW-Authenticate", f'Bearer realm="http://{self.headers["Host"]}/token",service="fixture",scope="repository:fixture:pull"')
                    self.end_headers()
                    return
                elif path in registry.blobs:
                    body = registry.blobs[path]
                else:
                    self.send_response(404)
                    self.end_headers()
                    return
                self.send_response(200)
                self.end_headers()
                self.wfile.write(body)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.host = f"127.0.0.1:{self.server.server_port}"

    def ref(self, name):
        return f"{self.host}/fixture/{name}@{self.refs[name]}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class RunnerBase(unittest.TestCase):
    registry_revision = None
    registry_options = {}

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="kodama-runner-")
        self.addCleanup(self.tmp.cleanup)
        scratch = Path(self.tmp.name)
        self.repo = scratch / "repo"
        self.scripts = self.repo / "deploy/stage/scripts"
        self.scripts.mkdir(parents=True)
        for name in ("lib.sh", "readiness-cases.json", "verify_image_revision.py"):
            shutil.copyfile(SOURCE / name, self.scripts / name)
        chart = self.repo / "deploy/charts/kodama"
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
        head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=self.repo, check=True, capture_output=True, text=True).stdout.strip()
        self.head = head
        revision = head if self.registry_revision is None else self.registry_revision
        revisions = {"kodama": revision, "probe": revision}
        if revision == "unlabelled":
            revisions = {"kodama": None, "probe": None}
        self.registry = FakeRegistry(revisions, **self.registry_options)
        self.addCleanup(self.registry.close)
        repository, _, digest = self.registry.ref("kodama").partition("@")
        self.values_text = f'image:\n  repository: {repository}\n  tag: "t"\n  digest: "{digest}"\n  pullPolicy: IfNotPresent\nfixture: true\n'
        (self.session / "contract.json").write_text(json.dumps(self.contract))
        (self.session / "values.yaml").write_text(self.values_text)
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
gate = 'staging' if any('test/e2e/staging' in a for a in args) else 'readiness'
mapping = {k: v for k, v in json.loads(pathlib.Path('deploy/stage/scripts/readiness-cases.json').read_text()).items() if v.get('gate', 'readiness') == gate}
pkgs = sorted(set(x['package'] for x in mapping.values()))
chart = '-run' in args
if args[0] == 'list':
    for pkg in pkgs:
        if any(pkg.endswith(a.removeprefix('./').removesuffix('/...')) for a in args[1:]): print(pkg)
    sys.exit(0)
if os.environ.get('MOCK_COMMIT') and chart:
    pathlib.Path('deploy/charts/kodama/Chart.yaml').write_text('changed: true')
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
        if case == 'lost_create_acknowledgment': print(json.dumps({'Action':'output','Output':'FAULT_CONFIG PX-A02 HTTPS loopback proxy; both TLS hops verified; test-owned only\\n'}), flush=True)
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
            "KODAMA_STAGE_PROBE_IMAGE": self.registry.ref("probe"),
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

class RunnerTests(RunnerBase):
    def run_staging(self, **extra):
        return subprocess.run([
            "bash", "-c", 'source "$1/lib.sh"; run_stage_go_suite "$2/contract.json" "$2/values.yaml" "$3" ./test/e2e/staging/...',
            "fixture", str(self.scripts), str(self.session), str(self.results),
        ], env=self.env | extra, cwd=self.repo, text=True, capture_output=True, timeout=20)

    def verify_staging(self):
        names = [n for n, spec in json.loads((SOURCE / "readiness-cases.json").read_text()).items() if spec.get("gate") == "staging"]
        return subprocess.run([
            "bash", "-c", 'source "$1/lib.sh"; verify_forge_results "$3" "$2/contract.json" "$2/values.yaml" ' + " ".join(names),
            "fixture", str(self.scripts), str(self.session), str(self.results),
        ], env=self.env, cwd=self.repo, capture_output=True, text=True, timeout=20)

    def test_a_staging_run_ends_in_a_receipt_the_gate_accepts(self):
        # The staging path of the runner itself (not only the verifier): no chart phase, the release image checked
        # at the registry, no readiness-only evidence, and a results file the staging gate verifies.
        result = self.run_staging()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("phase=chart-install", result.stdout)
        data = json.loads(self.results.read_text())
        self.assertTrue(data["provenance"]["suite_passed"])
        self.assertEqual([i["revision"] for i in data["provenance"]["image_revisions"]], [self.head])
        self.assertEqual(self.verify_staging().returncode, 0)
        self.assertEqual(len(data["acceptances"]), 6)

    def test_a_staging_run_with_a_stale_release_image_is_refused_before_tests(self):
        (self.session / "values.yaml").write_text(self.values_text.replace(self.registry.refs["kodama"], "sha256:" + "0" * 64))
        result = self.run_staging()
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("phase=scenarios", result.stdout)

    def test_pass_has_chart_first_and_exact_cases(self):
        result = self.run_suite()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertLess(result.stdout.index("phase=chart-install"), result.stdout.index("phase=scenarios"))
        self.assertIn("runner_exit=0", result.stdout)
        self.assertEqual(self.verify().returncode, 0)
        data = json.loads(self.results.read_text())
        self.assertTrue(data["provenance"]["candidate_stable"])
        self.assertEqual(len(data["acceptances"]), 7)

    def test_verified_image_revisions_are_recorded(self):
        self.assertEqual(self.run_suite().returncode, 0)
        recorded = json.loads(self.results.read_text())["provenance"]["image_revisions"]
        self.assertEqual({r["revision"] for r in recorded}, {self.head})
        self.assertEqual(len(recorded), 2)

    def test_a_results_file_without_the_runners_proof_is_refused(self):
        self.assertEqual(self.run_suite().returncode, 0)
        good = json.loads(self.results.read_text())
        mutations = {
            "candidate not stable": lambda p: p.update(candidate_stable=False),
            "no stability proof": lambda p: p.pop("candidate_stable"),
            "fault never applied": lambda p: p["fault_config"].update(applied=False),
            "no image revisions": lambda p: p.pop("image_revisions"),
            "one image only": lambda p: p.update(image_revisions=p["image_revisions"][:1]),
            "image from another revision": lambda p: p["image_revisions"][1].update(revision="dirty-debug"),
            "helper not among the verified images": lambda p: p.update(probe_image="registry.invalid/other@sha256:" + "c" * 64),
        }
        for name, mutate in mutations.items():
            with self.subTest(name):
                forged = json.loads(json.dumps(good))
                mutate(forged["provenance"])
                self.results.write_text(json.dumps(forged))
                self.assertNotEqual(self.verify().returncode, 0)
        self.results.write_text(json.dumps(good))
        self.assertEqual(self.verify().returncode, 0)

    def test_staging_receipt_needs_the_runners_proof(self):
        cases = json.loads((SOURCE / "readiness-cases.json").read_text())
        names = [n for n, spec in cases.items() if spec.get("gate") == "staging"]
        self.assertEqual(len(names), 6)
        contract = self.contract
        sha = lambda p: "sha256:" + hashlib.sha256(Path(p).read_bytes()).hexdigest()
        records = [{"name": n, "package": cases[n]["package"], "test": cases[n]["test"], "backend": cases[n]["backend"], "status": "pass",
                    "cases": {c: "pass" for c in cases[n]["cases"]}} for n in names]
        prov = {
            "cloud": contract["cloud"], "installation_id": contract["installation_id"], "git_commit": self.head[:12],
            "contract_sha256": sha(self.session / "contract.json"), "values_sha256": sha(self.session / "values.yaml"),
            "cluster_endpoint": contract["cluster"]["endpoint"], "suite_passed": True, "candidate_stable": True,
            "image_revisions": [{"image": self.registry.ref("kodama"), "revision": self.head}],
        }
        base = {"passed": [cases[n]["test"] for n in names], "failed": [], "skipped": [], "acceptances": records, "truncated": False,
                "runner_exit": 0, "packages": [{"package": cases[names[0]]["package"], "action": "pass"}], "provenance": prov}
        def check(mutate):
            doc = json.loads(json.dumps(base))
            mutate(doc)
            self.results.write_text(json.dumps(doc))
            return subprocess.run(["bash", "-c", 'source "$1/lib.sh"; verify_forge_results "$3" "$2/contract.json" "$2/values.yaml" ' + " ".join(names), "fixture", str(self.scripts), str(self.session), str(self.results)],
                                  env=self.env, cwd=self.repo, capture_output=True, text=True, timeout=20).returncode
        self.assertEqual(check(lambda d: None), 0)
        self.assertNotEqual(check(lambda d: d["provenance"].update(candidate_stable=False)), 0)
        self.assertNotEqual(check(lambda d: d["provenance"].pop("image_revisions")), 0)
        self.assertNotEqual(check(lambda d: d["provenance"]["image_revisions"][0].update(revision="dirty-debug")), 0)
        self.assertNotEqual(check(lambda d: d.update(runner_exit=1)), 0)
        self.assertNotEqual(check(lambda d: d.update(packages=[])), 0)
        self.assertNotEqual(check(lambda d: d["acceptances"][4]["cases"].update({"Process/Podman_runtime": "missing"})), 0)

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
                (self.session / "values.yaml").write_text(self.values_text)

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


class StaleHelperRevisionTests(RunnerBase):
    registry_revision = "dirty-debug"

    def test_image_built_from_another_revision_is_refused_before_tests(self):
        result = self.run_suite()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("image provenance", result.stderr)
        self.assertIn("dirty-debug", result.stderr)
        self.assertNotIn("phase=chart-install", result.stdout)



class UnlabelledImageTests(StaleHelperRevisionTests):
    registry_revision = "unlabelled"

    def test_image_built_from_another_revision_is_refused_before_tests(self):
        result = self.run_suite(KODAMA_STAGE_PREFLIGHT_ONLY="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("carries revision none", result.stderr)


class TamperedManifestTests(StaleHelperRevisionTests):
    registry_options = {"tamper": ("kodama",)}

    def test_image_built_from_another_revision_is_refused_before_tests(self):
        result = self.run_suite(KODAMA_STAGE_PREFLIGHT_ONLY="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not match the pinned digest", result.stderr)


class TokenRegistryTests(RunnerBase):
    registry_options = {"token": True}

    def test_anonymous_registry_token_flow_is_supported(self):
        result = self.run_suite(KODAMA_STAGE_PREFLIGHT_ONLY="1")
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
