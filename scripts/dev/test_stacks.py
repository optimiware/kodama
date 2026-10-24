"""DEV_STACK selection (unit_stack_selection) and endpoint files (unit_services_endpoints)."""
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import environment as dev

OWNER = "c" * 32
PORTS = {str(number) + "/tcp": [{"HostPort": str(number + 10000)}] for number in dev.SERVICES.values()}


class StackCase(unittest.TestCase):
    """A scope in a temporary state directory with a recording runtime."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        patcher = patch.object(dev, "STATE", Path(self.temp.name))
        patcher.start()
        self.addCleanup(patcher.stop)
        stdout = patch("sys.stdout", new_callable=io.StringIO)
        stdout.start()
        self.addCleanup(stdout.stop)
        self.calls = []
        self.network_labels = None  # None: the scoped network does not exist

    def runtime(self, args, **kwargs):
        """A stand-in for dev.run that records every command."""
        argv = [str(x) for x in args]
        self.calls.append(argv)
        if argv[1:3] == ["network", "inspect"]:
            if self.network_labels is None:
                return subprocess.CompletedProcess(args, 1, "", "Error: no such network")
            return subprocess.CompletedProcess(args, 0, json.dumps([{"Name": argv[3], "Labels": self.network_labels}]), "")
        if argv[1:3] == ["network", "create"]:
            self.network_labels = {dev.LABEL: OWNER}
        if argv[1:3] == ["network", "rm"]:
            self.network_labels = None
        if argv[1] == "ps":
            return subprocess.CompletedProcess(args, 0, "owned-1\nowned-2\n", "")
        return subprocess.CompletedProcess(args, 0, "", "")

    def verbs(self, *words):
        return [c for c in self.calls if c[1:1 + len(words)] == list(words)]

    def make(self, stack, state=True, requested=None):
        env = dev.Environment("t-091-test", "podman", requested or stack)
        env.directory.mkdir(parents=True, exist_ok=True)
        if state:
            cluster = "kodama-t-091-test-" + OWNER[:8]
            env.state = {"root": str(dev.ROOT), "scope": "t-091-test", "provider": "podman", "owner": OWNER,
                         "stack": stack, "cluster": cluster, "nodes": {}, "port": 5000, "local_image": "localhost:5000/t-091-test/scaffold:dev"}
            if stack == "services":
                env.state["network"] = cluster + "-net"
            env.stack = stack
            env.save()
        return env

    def start(self, env):
        """Run up() against the recording runtime with no real container engine."""
        def inspect(name):
            return {"Id": "id", "State": {"Running": True}, "NetworkSettings": {"Ports": PORTS, "Networks": {"kind": {"IPAddress": "10.0.0.1"}}}}

        def fixtures():
            directory = env.directory / "fixtures"
            directory.mkdir(exist_ok=True)
            for name in ("postgres-password", "nats-token"):
                (directory / name).write_text("fixture")
            return directory

        env.kind, env.kubectl = "kind", "kubectl"
        # Executable discovery is part of the simulated host: these tests must
        # not depend on which container engine the machine running them has.
        with patch.object(dev.shutil, "which", side_effect=lambda name: "/simulated/bin/" + name), \
                patch.object(dev, "run", side_effect=self.runtime), patch.object(dev, "install") as install, \
                patch.object(dev, "wait_for"), patch.object(dev.secrets, "token_hex", return_value=OWNER[:32]), \
                patch.object(env, "inspect", side_effect=inspect), patch.object(env, "build"), \
                patch.object(env, "fixtures", side_effect=fixtures), patch.object(env, "compose") as compose, \
                patch.object(env, "ensure_cluster", return_value=[]) as cluster, patch.object(env, "apply") as apply, \
                patch.object(env, "kube") as kube, patch.object(env, "manifests", return_value=[]):
            install.side_effect = lambda name: "/tools/" + name
            env.up()
        return {"install": install, "compose": compose, "cluster": cluster, "apply": apply, "kube": kube}


class TestUnitStackSelection(StackCase):
    def test_stacks_are_exactly_services_and_cluster(self):
        self.assertEqual(set(dev.STACKS), {"services", "cluster"})
        self.assertEqual(dev.DEFAULT_STACK, "cluster")

    def test_unknown_or_empty_values_are_rejected(self):
        for value in ("", "kind", "Services", "cluster ", "none", "services,cluster"):
            with self.subTest(value=value):
                with self.assertRaisesRegex(ValueError, "DEV_STACK must be one of services or cluster"):
                    dev.validate_stack(value)
                with self.assertRaises(ValueError):
                    dev.requested_stack(environ={"DEV_STACK": value})
                with self.assertRaises(ValueError):
                    dev.Environment("t-091-test", "podman", value)

    def test_selection_comes_from_the_argument_then_the_environment_then_the_cluster_default(self):
        self.assertIsNone(dev.requested_stack(environ={}))
        self.assertEqual(dev.requested_stack(environ={"DEV_STACK": "services"}), "services")
        self.assertEqual(dev.requested_stack("cluster", {"DEV_STACK": "services"}), "cluster")
        self.assertEqual(dev.Environment("t-091-test", "podman").stack, "cluster")
        self.assertEqual(dev.Environment("t-091-test", "podman", "services").stack, "services")

    def test_documented_resource_sets(self):
        services, cluster = dev.STACK_RESOURCES["services"], dev.STACK_RESOURCES["cluster"]
        self.assertEqual(services, {"registry", "postgres", "nats", "localstack", "git", "oauthsonas", "workload-network"})
        self.assertEqual(cluster, services | {"kind-cluster", "kubeconfig", "server-deployment"})
        for resource in ("kind-cluster", "kubeconfig", "server-deployment"):
            self.assertFalse(dev.Environment("t-091-test", "podman", "services").provides(resource))
            self.assertTrue(dev.Environment("t-091-test", "podman", "cluster").provides(resource))
        self.assertTrue({"postgres", "nats", "localstack", "git", "oauthsonas", "registry"} <= services)

    def test_services_never_creates_a_cluster_or_a_kubeconfig(self):
        env = self.make("services", state=False)
        tools = self.start(env)
        tools["cluster"].assert_not_called()
        tools["apply"].assert_not_called()
        tools["kube"].assert_not_called()
        self.assertFalse((env.directory / "kubeconfig").exists())
        for name in ("kind", "kubectl"):
            self.assertNotIn(name, [c.args[0] for c in tools["install"].call_args_list])
        self.assertFalse([c for c in self.calls if c[0] in ("kind", "kubectl") or "kind" in c[1:2]])
        self.assertEqual(env.state["stack"], "services")
        self.assertEqual(env.state["network"], env.state["cluster"] + "-net")
        (create,) = self.verbs("network", "create")
        self.assertIn(dev.LABEL + "=" + OWNER, create)
        self.assertEqual(create[-1], env.state["network"])

    def test_services_joins_compose_to_the_scoped_network_and_cluster_to_kind(self):
        services = self.make("services", state=False)
        self.assertEqual(self.start(services)["compose"].call_count, 2)
        self.assertEqual(services.network(), services.state["cluster"] + "-net")
        self.assertEqual(self.make("cluster").network(), "kind")
        compose = (dev.ROOT / "dev/compose.yaml").read_text()
        self.assertIn("name: ${KODAMA_NETWORK}", compose)

    def test_cluster_is_services_plus_the_kubernetes_resources(self):
        env = self.make("cluster", state=False)
        tools = self.start(env)
        tools["cluster"].assert_called_once()
        tools["apply"].assert_called_once()
        self.assertEqual(env.state["stack"], "cluster")
        self.assertNotIn("network", env.state)  # kind owns the shared network
        self.assertFalse(self.verbs("network", "create"))
        self.assertIn("kind", [c.args[0] for c in tools["install"].call_args_list])

    def test_default_scope_without_a_recorded_stack_is_a_cluster_scope(self):
        env = self.make("cluster")
        del env.state["stack"]
        env.save()
        legacy = dev.Environment("t-091-test", "podman")
        self.assertEqual(legacy.stack, "cluster")
        self.assertTrue(legacy.provides("kind-cluster"))

    def test_an_existing_scope_refuses_a_different_stack(self):
        self.make("services")
        env = dev.Environment("t-091-test", "podman", "cluster")
        self.assertEqual(env.stack, "services")
        with patch.object(dev, "run") as run, patch.object(env, "prepare"):
            with self.assertRaisesRegex(RuntimeError, "created with DEV_STACK=services; run make dev-down"):
                env.up()
            with self.assertRaisesRegex(RuntimeError, "created with DEV_STACK=services"):
                env.preflight()
            run.assert_not_called()

    def test_teardown_follows_the_recorded_stack_not_the_request(self):
        self.make("services")
        self.network_labels = {dev.LABEL: OWNER}
        env = dev.Environment("t-091-test", "podman", "cluster")
        with patch.object(dev, "run", side_effect=self.runtime), patch.object(env, "prepare"), patch.object(env, "compose"):
            env.down()
        self.assertEqual(len(self.verbs("network", "rm")), 1)
        self.assertFalse(env.directory.exists())

    def test_services_teardown_removes_exactly_the_labelled_containers_and_network(self):
        env = self.make("services")
        self.network_labels = {dev.LABEL: OWNER}
        network = env.state["network"]
        with patch.object(dev, "run", side_effect=self.runtime), patch.object(env, "prepare"), patch.object(env, "compose") as compose:
            env.down()
        compose.assert_called_once_with("down", "-v", check=False)
        selector = self.verbs("ps")[0]
        self.assertIn("label=" + dev.LABEL + "=" + OWNER, selector)
        self.assertEqual([c[-1] for c in self.verbs("rm", "-f")], ["owned-1", "owned-2"])
        self.assertEqual([c[-1] for c in self.verbs("network", "rm")], [network])
        self.assertFalse([c for c in self.calls if c[0] == "kind" or "kind" in c[1:2]])
        self.assertFalse(env.directory.exists())
        # Network removal follows container removal, so nothing is attached to it.
        self.assertGreater(self.calls.index(self.verbs("network", "rm")[0]), self.calls.index(self.verbs("rm", "-f")[-1]))

    def test_services_teardown_refuses_a_foreign_network_before_deleting_anything(self):
        env = self.make("services")
        self.network_labels = {dev.LABEL: "d" * 32}
        with patch.object(dev, "run", side_effect=self.runtime), patch.object(env, "prepare"), patch.object(env, "compose") as compose:
            with self.assertRaisesRegex(RuntimeError, "network ownership mismatch"):
                env.down()
        compose.assert_not_called()
        self.assertFalse(self.verbs("rm"))
        self.assertFalse(self.verbs("network", "rm"))
        self.assertTrue(env.directory.exists())

    def test_services_teardown_after_partial_creation_and_repeats(self):
        env = self.make("services")  # network never created: an interrupted dev-up
        with patch.object(dev, "run", side_effect=self.runtime), patch.object(env, "prepare"), patch.object(env, "compose"):
            env.down()
            env.down()  # the scope is gone: nothing left to do
        self.assertFalse(self.verbs("network", "rm"))
        self.assertFalse(env.directory.exists())

    def test_state_with_a_foreign_network_name_is_rejected(self):
        env = self.make("services")
        env.state["network"] = "podman"
        env.save()
        with self.assertRaisesRegex(ValueError, "network name"):
            dev.Environment("t-091-test", "podman")


class TestUnitServicesEndpoints(StackCase):
    def written(self, stack):
        env = self.make(stack, state=False)
        self.start(env)
        return (env, json.loads((env.directory / "host-env.json").read_text()), json.loads((env.directory / "endpoints.json").read_text()))

    def test_services_endpoint_files_name_only_what_the_stack_provides(self):
        env, host, workload = self.written("services")
        self.assertEqual(host["DEV_STACK"], "services")
        for key in ("KODAMA_TEST_KUBECONFIG", "KUBECTL"):
            self.assertNotIn(key, host)
        self.assertNotIn("kubeconfig", json.dumps([host, workload]).lower())
        self.assertNotIn("svc.cluster.local", json.dumps([host, workload]))
        self.assertNotIn("KODAMA_SERVER_URL", workload)  # the server under test is on the host
        self.assertEqual(host["KODAMA_TEST_PODMAN_NETWORK"], env.state["network"])
        self.assertEqual(host["KODAMA_TEST_SERVER_CALLBACK_HOST"], "host.containers.internal")
        self.assertIn("@127.0.0.1:15432/postgres", host["KODAMA_TEST_POSTGRES_ADMIN_URL"])
        self.assertTrue(all("127.0.0.1" in host[k] for k in ("KODAMA_NATS_URL", "KODAMA_S3_ENDPOINT", "KODAMA_REGISTRY_URL")))
        self.assertFalse((env.directory / "kubeconfig").exists())

    def test_services_workload_urls_use_container_names_on_the_scoped_network(self):
        env, host, workload = self.written("services")
        for service, key, scheme in (("postgres", "KODAMA_POSTGRES_URL", "postgres://"), ("nats", "KODAMA_NATS_URL", "nats://"),
                                     ("localstack", "KODAMA_S3_ENDPOINT", "http://"), ("git", "KODAMA_GIT_URL", "https://"),
                                     ("oauthsonas", "KODAMA_OIDC_ISSUER", "http://"), ("registry", "KODAMA_REGISTRY_URL", "http://")):
            self.assertTrue(workload[key].startswith(scheme + env.state["cluster"] + "-" + service + ":"), (key, workload[key]))
        self.assertEqual(workload["KODAMA_POSTGRES_URL"], "postgres://{}-postgres:5432/kodama_t_091_test".format(env.state["cluster"]))
        self.assertNotIn("@", workload["KODAMA_POSTGRES_URL"])  # credentials stay in the private fixtures
        self.assertEqual(workload["KODAMA_PODMAN_NETWORK"], env.state["network"])
        self.assertNotIn("127.0.0.1", json.dumps(workload))
        self.assertNotIn("localhost", json.dumps(workload))

    def test_cluster_endpoint_files_are_unchanged_pod_urls_and_a_kubeconfig(self):
        env, host, workload = self.written("cluster")
        self.assertEqual(host["KODAMA_TEST_KUBECONFIG"], str(env.directory / "kubeconfig"))
        self.assertIn("KUBECTL", host)
        self.assertNotIn("KODAMA_TEST_PODMAN_NETWORK", host)
        self.assertEqual(workload, dev.probe_env("t-091-test"))
        self.assertEqual(workload["KODAMA_SERVER_URL"], "http://kodama.kodama-deps.svc.cluster.local:8080")
        self.assertEqual(workload["KODAMA_OIDC_ISSUER"], "http://oauthsonas.kodama-deps.svc.cluster.local:8181")
        self.assertNotIn("KODAMA_PODMAN_NETWORK", workload)
        self.assertNotIn("svc.cluster.local", json.dumps(host))

    def test_docker_callback_host(self):
        env = dev.Environment("t-091-test", "docker", "services")
        env.state = {"network": "n", "cluster": "kodama-t-091-test-" + OWNER[:8]}
        self.assertEqual(env.workload_env()["KODAMA_SERVER_CALLBACK_HOST"], "host.docker.internal")

    def test_services_fixtures_address_services_by_workload_host(self):
        env = self.make("services")
        with patch.object(dev, "run") as run:
            directory = env.fixtures()
        self.assertIn(env.state["cluster"] + "-git", run.call_args[0][0])  # the Git certificate names the workload host
        url = (directory / "database-runtime-url").read_text()
        self.assertIn("@{}-postgres:5432/kodama_t_091_test".format(env.state["cluster"]), url)
        self.assertNotIn("svc.cluster.local", url)
        self.assertIn("http://{}-oauthsonas:8181".format(env.state["cluster"]), (directory / "oauthsonas.yaml").read_text())
        self.assertNotIn("svc.cluster.local", json.dumps({p.name: p.read_bytes().decode("latin-1") for p in directory.iterdir()}))


class ServicesPreflightTests(StackCase):
    def prepared(self):
        env = self.make("services", state=False)
        self.start(env)
        env.calls = self.calls
        return env

    def test_checks_endpoints_and_network_without_a_kubeconfig_or_kubectl(self):
        env = self.prepared()
        self.calls.clear()
        with patch.object(dev.socket, "create_connection") as connect, patch.object(dev, "run", side_effect=self.runtime), patch.object(dev, "install") as install:
            env.preflight()
        self.assertEqual(connect.call_count, 4)
        self.assertEqual([c[1:3] for c in self.calls], [["network", "inspect"]])
        install.assert_not_called()

    def test_missing_network_names_the_scope_and_the_recovery_command(self):
        env = self.prepared()
        self.network_labels = None
        with patch.object(dev.socket, "create_connection"), patch.object(dev, "run", side_effect=self.runtime):
            with self.assertRaisesRegex(RuntimeError, "network .* for .*t-091-test; run make dev-up DEV_SCOPE=t-091-test DEV_PROVIDER=podman DEV_STACK=services"):
                env.preflight()

    def test_foreign_network_fails_preflight(self):
        env = self.prepared()
        self.network_labels = {dev.LABEL: "e" * 32}
        with patch.object(dev.socket, "create_connection"), patch.object(dev, "run", side_effect=self.runtime):
            with self.assertRaisesRegex(RuntimeError, "fixture unavailable: network"):
                env.preflight()

    def test_unreachable_service_reports_key_not_credentials(self):
        env = self.prepared()
        with patch.object(dev.socket, "create_connection", side_effect=OSError("private")), patch.object(dev, "run", side_effect=self.runtime):
            with self.assertRaisesRegex(RuntimeError, "fixture unavailable: KODAMA_TEST_POSTGRES_ADMIN_URL") as caught:
                env.preflight()
        self.assertNotIn("private", str(caught.exception))

    def test_missing_scope_needs_no_runtime(self):
        env = dev.Environment("t-091-test", "podman", "services")
        with patch.object(dev, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "missing dev scope"):
                env.preflight()
            run.assert_not_called()


class ServicesProbeTests(StackCase):
    def test_probe_runs_as_a_labelled_podman_workload_on_the_scoped_network(self):
        env = self.make("services")
        (env.directory / "fixtures").mkdir()
        with patch.object(dev, "run", side_effect=self.runtime):
            env.services_probe()
        (run,) = self.verbs("run")
        self.assertEqual(run[run.index("--network") + 1], env.state["network"])
        self.assertIn(dev.LABEL + "=" + OWNER, run)
        self.assertEqual(run[run.index("--entrypoint") + 1], "/usr/local/bin/probe")
        self.assertIn("KODAMA_PROBE_CHECKS=" + dev.SERVICES_PROBE_CHECKS, run)
        self.assertNotIn("server", dev.SERVICES_PROBE_CHECKS.split(","))
        self.assertTrue(any(x.startswith("KODAMA_OIDC_ISSUER=http://" + env.state["cluster"] + "-oauthsonas") for x in run))
        mounts = [x for x in run if x.endswith(":ro")]
        self.assertEqual(len(mounts), len(dev.PROBE_SECRETS))
        self.assertFalse([m for m in mounts if "tls.key" in m or "master-key" in m or "postgres-password" in m])


class PrepareTests(StackCase):
    def test_prepare_requires_the_engine_and_installs_tools_only_for_the_cluster_stack(self):
        with patch.object(dev.shutil, "which", return_value=None), patch.object(dev, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "podman is missing from PATH"):
                self.make("services", state=False).prepare()
            run.assert_not_called()
        for stack, tools in (("services", []), ("cluster", ["kind", "kubectl"])):
            with self.subTest(stack=stack), patch.object(dev.shutil, "which", return_value="/simulated/bin/podman"), \
                    patch.object(dev, "run"), patch.object(dev, "install", side_effect=lambda name: name) as install:
                self.make(stack, state=False).prepare()
                self.assertEqual([c.args[0] for c in install.call_args_list], tools)


class RuntimeAcceptanceTests(StackCase):
    def test_runs_the_podman_runtime_harness_against_this_scope_only(self):
        env = self.make("services")
        env.state["image"] = "localhost:5000/t-091-test/scaffold@sha256:" + "a" * 64
        with patch.object(dev, "run", side_effect=self.runtime) as run:
            env.runtime_acceptance()
        args, kwargs = run.call_args[0][0], run.call_args[1]
        self.assertEqual(args[:2], ["go", "test"])
        self.assertIn("./dev/runtimeacceptance", args)
        self.assertEqual(kwargs["env"]["KODAMA_DEV_SCOPE_DIR"], str(env.directory))
        self.assertEqual(kwargs["env"]["KODAMA_REQUIRE_PODMAN"], "1")

    def test_refuses_cluster_scopes_missing_scopes_and_docker(self):
        with patch.object(dev, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "needs DEV_STACK=services"):
                self.make("cluster").runtime_acceptance()
            with self.assertRaisesRegex(RuntimeError, "make dev-up DEV_STACK=services"):
                dev.Environment("t-091-absent", "podman", "services").runtime_acceptance()
            docker = dev.Environment("t-091-docker", "docker", "services")
            docker.state = {"image": "x"}
            with self.assertRaisesRegex(RuntimeError, "DEV_PROVIDER=podman"):
                docker.runtime_acceptance()
            run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
