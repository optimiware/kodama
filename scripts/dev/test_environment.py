import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import environment as dev
from tools import command_path


class OwnershipTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.patch = patch.object(dev, "STATE", Path(self.temp.name))
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.env = dev.Environment("t-001-test", "podman")
        self.env.directory.mkdir(parents=True)
        owner = "a" * 32
        cluster = "kodama-t-001-test-" + owner[:8]
        self.env.state = {"root": str(dev.ROOT), "scope": "t-001-test", "provider": "podman", "owner": owner,
                          "cluster": cluster, "nodes": {cluster + "-control-plane": "original-id"}}
        self.env.save()

    def test_rejects_scope_traversal_and_unsafe_names(self):
        for scope in ("", "../outside", "default;exit", "a" * 40, "UPPER", "-prefix", "suffix-"):
            with self.subTest(scope=scope), self.assertRaises(ValueError):
                dev.Environment(scope, "podman")

    def test_rejects_foreign_worktree_or_provider(self):
        with self.assertRaises(ValueError):
            dev.Environment("t-001-test", "docker")
        self.env.state["root"] = "/another/worktree"
        self.env.save()
        with self.assertRaises(ValueError):
            dev.Environment("t-001-test", "podman")

    def test_rejects_resource_name_not_bound_to_owner(self):
        self.env.state["cluster"] = "unrelated-cluster"
        self.env.save()
        with self.assertRaises(ValueError):
            dev.Environment("t-001-test", "podman")

    def test_teardown_refuses_replaced_node_before_deleting_anything(self):
        node = next(iter(self.env.state["nodes"]))
        with patch.object(self.env, "prepare"), patch.object(self.env, "inspect", return_value={"Id": "replacement"}), \
                patch.object(dev, "run", return_value=subprocess.CompletedProcess([], 0, node + "\n", "")) as run:
            with self.assertRaisesRegex(RuntimeError, "ownership mismatch"):
                self.env.down()
            self.assertEqual(run.call_count, 1)  # only the node listing; no compose down, rm, or kind delete
            self.assertNotIn("delete", run.call_args[0][0])

    def test_teardown_removes_only_label_selected_containers(self):
        node = next(iter(self.env.state["nodes"]))
        calls = []

        def fake_run(args, **kwargs):
            calls.append([str(x) for x in args])
            listing = {"get": node + "\n", "ps": "owned-1\nowned-2\n"}
            return subprocess.CompletedProcess(args, 0, listing.get(str(args[1]), ""), "")

        with patch.object(self.env, "prepare"), patch.object(self.env, "inspect", return_value={"Id": "original-id", "Config": {"Labels": {"io.x-k8s.kind.cluster": self.env.state["cluster"]}}}), \
                patch.object(dev, "run", side_effect=fake_run):
            self.env.down()
        selector = next(c for c in calls if c[1] == "ps")
        self.assertIn("label=" + dev.LABEL + "=" + "a" * 32, selector)
        self.assertEqual([c[-1] for c in calls if c[1:3] == ["rm", "-f"]], ["owned-1", "owned-2"])
        self.assertFalse(self.env.directory.exists())

    def test_absent_scope_teardown_needs_no_runtime(self):
        with patch.object(dev, "run") as run:
            dev.Environment("already-absent", "podman").down()
            run.assert_not_called()

    def test_fixture_manifests_and_private_key_exclusion(self):
        self.env.state["image"] = "localhost:1234/t-001-test/scaffold@sha256:" + "b" * 64
        directory = self.env.directory / "fixtures"
        directory.mkdir()
        for key in ("ca.crt", "tls.key", "postgres-password", "master-key", "git-read-token"):
            (directory / key).write_text("fixture")
        manifests = self.env.manifests(directory, {name: "10.89.0.5" for name in dev.SERVICES})
        probe_secret = next(x for x in manifests if x["kind"] == "Secret")
        self.assertEqual(probe_secret["metadata"]["namespace"], dev.TESTS)
        self.assertEqual(set(probe_secret["data"]), {"ca.crt", "git-read-token"})
        services = {x["metadata"]["name"] for x in manifests if x["kind"] == "EndpointSlice"}
        self.assertEqual(services, set(dev.SERVICES))
        self.assertTrue(all(x["metadata"]["labels"][dev.LABEL] == "a" * 32 for x in manifests))
        self.assertNotIn("latest", json.dumps(manifests))

    def test_fixtures_are_stable_and_readable_by_container_users(self):
        with patch.object(dev, "run"):
            directory = self.env.fixtures()
            first = (directory / "git-write-token").read_text()
            self.env.fixtures()
        self.assertEqual((directory / "git-write-token").read_text(), first)
        self.assertEqual(directory.stat().st_mode & 0o777, 0o755)
        self.assertEqual((directory / "init.sql").stat().st_mode & 0o777, 0o644)
        self.assertIn("kodama_t_001_test", (directory / "init.sql").read_text())
        self.assertNotEqual((directory / "git-read-token").read_text(), (directory / "git-write-token").read_text())

    def test_probe_job_has_single_container_and_no_api_credentials(self):
        captured = []
        self.env.state["image"] = "fixture"
        with patch.object(self.env, "apply", side_effect=lambda items: captured.extend(copy.deepcopy(items))), \
                patch.object(self.env, "kube", side_effect=[subprocess.CompletedProcess([], 0, '{"status":{"succeeded":1}}', ""), subprocess.CompletedProcess([], 0, "PASS", "")]):
            self.env.job("smoke", "fixture", ["probe"], probe=True)
        job = captured[0]["spec"]
        self.assertEqual(job["backoffLimit"], 0)
        self.assertGreater(job["activeDeadlineSeconds"], 0)
        pod = job["template"]["spec"]
        self.assertEqual(len(pod["containers"]), 1)
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertTrue(pod["securityContext"]["runAsNonRoot"])


class ComposeTests(unittest.TestCase):
    def test_compose_labels_and_names_bind_every_service_to_the_scope(self):
        text = (dev.ROOT / "dev/compose.yaml").read_text()
        for service in dev.SERVICES:
            self.assertIn("container_name: ${KODAMA_PROJECT}-" + service, text)
        self.assertEqual(text.count("<<: *fixture"), len(dev.SERVICES))
        self.assertIn("dev.kodama.owner: ${KODAMA_OWNER}", text)
        self.assertNotIn(":latest", text)

    def test_host_services_publish_only_dynamic_loopback_ports(self):
        text = (dev.ROOT / "dev/compose.yaml").read_text()
        for port in (5432, 4222, 4566, 5000):
            self.assertIn('ports: ["127.0.0.1::{}"]'.format(port), text)

    def test_tools_have_stable_executable_names_without_replacing_unmanaged_files(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "kubectl-v1"
            target.write_text("pinned")
            path = Path(command_path("kubectl", target))
            self.assertEqual(path.name, "kubectl")
            self.assertEqual(path.resolve(), target.resolve())
            self.assertEqual(command_path("kubectl", target), str(path))
            path.unlink()
            path.write_text("unmanaged")
            with self.assertRaisesRegex(RuntimeError, "unmanaged tool"):
                command_path("kubectl", target)


class PreflightTests(unittest.TestCase):
    setUp = OwnershipTests.setUp

    def test_missing_scope_names_actual_directory_without_provisioning(self):
        with patch.object(self.env, "prepare") as prepare, patch.object(dev, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "missing dev scope .*t-001-test: run make dev-up DEV_SCOPE=t-001-test"):
                self.env.preflight()
            prepare.assert_not_called()
            run.assert_not_called()

    def write_host(self):
        with patch.object(dev, "run"):
            fixtures = self.env.fixtures()
        self.env.kubectl = str(self.env.directory / "kubectl")
        Path(self.env.kubectl).write_text("fixture executable")
        Path(self.env.kubectl).chmod(0o755)
        (self.env.directory / "kubeconfig").write_text("fixture kubeconfig")
        ports = {str(number) + "/tcp": [{"HostPort": str(number + 10000)}] for number in dev.SERVICES.values()}
        with patch.object(self.env, "inspect", return_value={"NetworkSettings": {"Ports": ports}}):
            self.env.write_host_environment(fixtures)
        return json.loads((self.env.directory / "host-env.json").read_text())

    def test_host_environment_is_private_and_distinct_from_pod_urls(self):
        values = self.write_host()
        self.assertEqual((self.env.directory / "host-env.json").stat().st_mode & 0o777, 0o600)
        self.assertIn("@127.0.0.1:15432/postgres", values["KODAMA_TEST_POSTGRES_ADMIN_URL"])
        self.assertEqual(values["KODAMA_TEST_KUBECONFIG"], str(self.env.directory / "kubeconfig"))
        self.assertEqual(values["DEV_PROVIDER"], "podman")
        self.assertNotIn("svc.cluster.local", json.dumps(values))

    def test_preflight_checks_endpoints_and_actual_cluster_without_installing(self):
        self.write_host()
        with patch.object(dev.socket, "create_connection") as connect, patch.object(dev, "run") as run, patch.object(dev, "install") as install:
            self.env.preflight()
            self.assertEqual(connect.call_count, 4)
            self.assertIn("--raw=/readyz", run.call_args[0][0])
            install.assert_not_called()

    def test_unreachable_endpoint_reports_key_and_scope_not_password(self):
        values = self.write_host()
        with patch.object(dev.socket, "create_connection", side_effect=OSError("private diagnostic")):
            with self.assertRaisesRegex(RuntimeError, "fixture unavailable: KODAMA_TEST_POSTGRES_ADMIN_URL in .*t-001-test") as caught:
                self.env.preflight()
        self.assertNotIn(values["KODAMA_TEST_POSTGRES_ADMIN_URL"], str(caught.exception))
        self.assertNotIn("private diagnostic", str(caught.exception))


if __name__ == "__main__":
    unittest.main()
