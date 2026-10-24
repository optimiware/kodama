"""Scoped development environment: compose dependencies, optionally a kind cluster.

Python 3.9+, standard library only. Compose (dev/compose.yaml) runs the
dependency containers. DEV_STACK selects what is built (ADR 0009):

  services  the dependency containers on one scoped Podman network, which the
            Podman runtime's workloads join; no cluster and no kubeconfig
  cluster   (default) services plus a scoped kind cluster whose Pods reach them
"""
import argparse
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import secrets
import shutil
import socket
import subprocess
import sys
import time
import urllib.request
from urllib.parse import quote, urlparse

from tools import LOCK, ROOT, install

LABEL = "dev.kodama.owner"
DEPS, TESTS = "kodama-deps", "kodama-test"
STATE = ROOT / "dev/.state"
STACKS = ("services", "cluster")
DEFAULT_STACK = "cluster"
# Every resource a stack owns. "cluster" is "services" plus the Kubernetes
# resources; teardown removes exactly the resources the recorded stack lists.
SERVICES_RESOURCES = frozenset({"registry", "postgres", "nats", "localstack", "git", "oauthsonas", "workload-network"})
STACK_RESOURCES = {
    "services": SERVICES_RESOURCES,
    "cluster": SERVICES_RESOURCES | {"kind-cluster", "kubeconfig", "server-deployment"},
}
# Probe checks that make sense without a server Deployment (the "services" stack).
SERVICES_PROBE_CHECKS = "postgres,jetstream,s3,https_git,oidc_pkce,registry"
# How a container on the workload network reaches the host-side server callback.
CALLBACK_HOSTS = {"podman": "host.containers.internal", "docker": "host.docker.internal"}
# Compose services that Pods reach as <name>.kodama-deps.svc.cluster.local.
SERVICES = {"postgres": 5432, "nats": 4222, "localstack": 4566, "git": 8443, "oauthsonas": 8181, "registry": 5000}
MANIFEST_TYPES = ("application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, "
                  "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")
PROBE_SECRETS = ["database-runtime-url", "database-migration-url", "nats-token", "git-read-token", "git-write-token",
                 "s3-access-key", "s3-secret-key", "ca.crt"]


def run(args, *, capture=False, data=None, env=None, check=True, timeout=300):
    pipe = subprocess.PIPE if capture else None
    result = subprocess.run([str(x) for x in args], cwd=ROOT, input=data, text=True, stdout=pipe, stderr=pipe,
                            env=env, check=False, timeout=timeout)
    if check and result.returncode:
        detail = ": " + result.stderr.strip() if capture and result.stderr else ""
        raise RuntimeError("{} failed with exit {}{}".format(" ".join(str(x) for x in args[:4]), result.returncode, detail))
    return result


def validate_scope(scope):
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,29}[a-z0-9]", scope):
        raise ValueError("DEV_SCOPE must be 2-31 lowercase letters/digits/hyphens, starting with a letter")
    return scope


def validate_stack(stack):
    if stack not in STACKS:
        raise ValueError("DEV_STACK must be one of {}, not {!r}".format(" or ".join(STACKS), stack))
    return stack


def requested_stack(argument=None, environ=None):
    """The explicit selection (--stack, else DEV_STACK), or None when unset."""
    value = argument if argument is not None else (os.environ if environ is None else environ).get("DEV_STACK")
    return None if value is None else validate_stack(value)


def write_json(path, value):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.chmod(0o600)
    temporary.replace(path)


def wait_for(url, seconds=90):
    deadline = time.monotonic() + seconds
    while True:
        try:
            with urllib.request.urlopen(url, timeout=5):
                return
        except OSError:
            if time.monotonic() > deadline:
                raise RuntimeError("timed out waiting for " + url)
            time.sleep(1)


def probe_env(scope, host=None):
    """Workload URLs. host(service) names the service as workloads resolve it.

    Without a host function these are the Pod URLs of the cluster stack; the
    "services" stack passes container names and has no server Deployment.
    """
    if host is None:
        dns = "." + DEPS + ".svc.cluster.local"

        def host(service):
            return service + dns
    values = {"KODAMA_TEST_SCOPE": scope, "KODAMA_SERVER_URL": "http://" + host("kodama") + ":8080",
              "KODAMA_NATS_URL": "nats://" + host("nats") + ":4222", "KODAMA_S3_ENDPOINT": "http://" + host("localstack") + ":4566",
              "KODAMA_S3_BUCKET": "kodama-" + scope, "KODAMA_S3_PREFIX": scope + "/",
              "KODAMA_GIT_URL": "https://" + host("git") + ":8443/fixture.git",
              "KODAMA_OIDC_ISSUER": "http://" + host("oauthsonas") + ":8181", "KODAMA_OIDC_AUDIENCE": "https://kodama.example.test/api",
              "KODAMA_REGISTRY_URL": "http://" + host("registry") + ":5000", "KODAMA_REGISTRY_SOURCE": scope + "/scaffold"}
    return values


HARDENED = {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}}


class Environment:
    def __init__(self, scope, provider, stack=None):
        self.scope = validate_scope(scope)
        if provider not in ("podman", "docker"):
            raise ValueError("DEV_PROVIDER must be podman or docker")
        self.provider = provider
        self.requested = None if stack is None else validate_stack(stack)
        self.directory = STATE / "environments" / scope
        self.state_path = self.directory / "state.json"
        self.state = json.loads(self.state_path.read_text()) if self.state_path.exists() else None
        if self.state:
            self.validate_state()
        # A scope keeps the stack it was created with; scopes that predate the
        # split have no record and are cluster scopes.
        self.stack = self.state.get("stack", "cluster") if self.state else self.requested or DEFAULT_STACK
        self.env = dict(os.environ, KIND_EXPERIMENTAL_PROVIDER=provider, PODMAN_COMPOSE_WARNING_LOGS="false")
        self.kind = self.kubectl = None

    def validate_state(self):
        s = self.state
        if (s.get("root") != str(ROOT) or s.get("provider") != self.provider
                or s.get("scope") != self.scope or not re.fullmatch(r"[0-9a-f]{32}", s.get("owner", ""))):
            raise ValueError("state owner/worktree/provider mismatch; refusing resource mutation")
        if s.get("cluster") != "kodama-" + self.scope + "-" + s["owner"][:8]:
            raise ValueError("state resource names do not match ownership token")
        if validate_stack(s.get("stack", "cluster")) == "services" and s.get("network") != s["cluster"] + "-net":
            raise ValueError("state network name does not match ownership token")

    def provides(self, resource):
        return resource in STACK_RESOURCES[self.stack]

    def select(self):
        """Refuse to build a different stack into an existing scope."""
        if self.requested and self.state and self.requested != self.stack:
            raise RuntimeError("scope {} was created with DEV_STACK={}; run make dev-down first to select {}".format(
                self.scope, self.stack, self.requested))

    def workload_host(self, service):
        """The name a workload (Pod or Podman container) uses to reach a service."""
        if self.stack == "cluster":
            return service + "." + DEPS + ".svc.cluster.local"
        return self.container(service)

    def workload_env(self):
        values = probe_env(self.scope, self.workload_host)
        if self.stack == "services":
            del values["KODAMA_SERVER_URL"]  # the server under test runs on the host, not in the stack
            values.update(KODAMA_POSTGRES_URL="postgres://{}:5432/{}".format(self.workload_host("postgres"), self.database()),
                          KODAMA_PODMAN_NETWORK=self.state["network"], KODAMA_SERVER_CALLBACK_HOST=CALLBACK_HOSTS[self.provider])
        return values

    def database(self):
        return "kodama_" + self.scope.replace("-", "_")

    def save(self):
        write_json(self.state_path, self.state)

    def prepare(self):
        if not shutil.which(self.provider):
            raise RuntimeError(self.provider + " is missing from PATH")
        run([self.provider, "info"], capture=True)
        run([self.provider, "compose", "version"], capture=True, env=self.env)
        if self.provides("kind-cluster"):
            self.kind, self.kubectl = install("kind"), install("kubectl")

    # -- runtime helpers ---------------------------------------------------

    def compose(self, *args, **kwargs):
        """Run compose for this scope; the project name is the cluster name."""
        env = dict(self.env, KODAMA_PROJECT=self.state["cluster"], KODAMA_OWNER=self.state["owner"],
                   KODAMA_FIXTURES=str(self.directory / "fixtures"), KODAMA_IMAGE=self.state.get("local_image", "scaffold:dev"),
                   KODAMA_NETWORK=self.network())
        return run([self.provider, "compose", "-p", self.state["cluster"], "-f", ROOT / "dev/compose.yaml", *args], env=env, **kwargs)

    def network(self):
        """The network every service joins: kind's own, or the scoped Podman network."""
        return "kind" if self.stack == "cluster" else self.state["network"]

    def kube(self, *args, **kwargs):
        # Never read or modify the caller's current Kubernetes context.
        return run([self.kubectl, "--kubeconfig", self.directory / "kubeconfig", "--context", "kind-" + self.state["cluster"], *args], **kwargs)

    def container(self, service):
        return self.state["cluster"] + "-" + service

    def inspect(self, name):
        result = run([self.provider, "container", "inspect", name], capture=True, check=False)
        if result.returncode:
            if "no such" in result.stderr.lower() or "not found" in result.stderr.lower():
                return None
            raise RuntimeError("container inspection failed: " + result.stderr)
        return json.loads(result.stdout)[0]

    def owned_containers(self):
        """Containers carrying this scope's random ownership label."""
        return run([self.provider, "ps", "-aq", "--filter", "label=" + LABEL + "=" + self.state["owner"]], capture=True).stdout.split()

    def nodes(self):
        return run([self.kind, "get", "nodes", "--name", self.state["cluster"]], env=self.env, capture=True).stdout.split()

    def verify_nodes(self, nodes):
        for name in nodes:
            item = self.inspect(name)
            if not item or self.state["nodes"].get(name) != item["Id"]:
                raise RuntimeError("node ownership mismatch: " + name)
            if item["Config"].get("Labels", {}).get("io.x-k8s.kind.cluster") != self.state["cluster"]:
                raise RuntimeError("kind cluster label mismatch")

    # -- fixtures ------------------------------------------------------------

    def fixtures(self):
        """Write fixture files. Secrets are generated once and then stay stable.

        The scope directory is mode 0700 on the host; this subdirectory is
        mounted read-only into containers that run as other UIDs.
        """
        directory = self.directory / "fixtures"
        directory.mkdir(exist_ok=True)
        stable = {}
        for name in ("postgres-password", "migration-password", "runtime-password", "nats-token", "git-read-token", "git-write-token", "s3-secret-key"):
            if not (directory / name).exists():
                (directory / name).write_text(secrets.token_hex(24))
            stable[name] = (directory / name).read_text()
        if not (directory / "master-key").exists():
            (directory / "master-key").write_bytes(secrets.token_bytes(32))
        if not (directory / "tls.crt").exists():
            run(["go", "run", "./dev/certs", directory, self.workload_host("git")])
        database = self.database()
        for name, user in (("database-migration-url", "kodama_migration"), ("database-runtime-url", "kodama_runtime")):
            password = stable[name.split("-")[1] + "-password"]
            (directory / name).write_text("postgres://{}:{}@{}:5432/{}?sslmode=disable".format(user, password, self.workload_host("postgres"), database))
        (directory / "s3-access-key").write_text("test")
        (directory / "init.sql").write_text("""CREATE ROLE kodama_migration LOGIN PASSWORD '{migration}';
CREATE ROLE kodama_runtime LOGIN PASSWORD '{runtime}' NOSUPERUSER NOBYPASSRLS;
CREATE DATABASE {database} OWNER kodama_migration;
\\connect {database}
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO kodama_runtime;
""".format(database=database, migration=stable["migration-password"], runtime=stable["runtime-password"]))
        (directory / "nats.conf").write_text('port: 4222\njetstream { store_dir: /data/jetstream }\nauthorization { token: "%s" }\n' % stable["nats-token"])
        environment = probe_env(self.scope, self.workload_host)
        (directory / "oauthsonas.yaml").write_text(json.dumps({
            "issuer": environment["KODAMA_OIDC_ISSUER"], "api_audience": environment["KODAMA_OIDC_AUDIENCE"],
            "token_ttl": "15m", "authorization_code_ttl": "5m", "refresh_token_ttl": "8h",
            "clients": [{"id": "kodama-cli", "name": "Kodama development CLI", "public": True,
                         "redirect_uris": ["http://127.0.0.1:18999/callback"],
                         "post_logout_redirect_uris": ["http://127.0.0.1:18999/"], "allowed_origins": ["http://127.0.0.1:18999"]}],
            "personas": [{"id": "engineer", "subject": "oauthsonas|engineer", "email": "engineer@example.test",
                          "name": "Fixture Engineer", "roles": ["engineer"]}]}))
        directory.chmod(0o755)
        for path in directory.iterdir():
            path.chmod(0o644)
        return directory

    # -- image ----------------------------------------------------------------

    def build(self):
        if run(["go", "env", "GOVERSION"], capture=True).stdout.strip() != "go" + LOCK["go"]:
            raise RuntimeError("expected Go " + LOCK["go"])
        machine = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64"}.get(platform.machine())
        if not machine:
            raise RuntimeError("unsupported host architecture " + platform.machine())
        context = self.directory / "image"
        context.mkdir(exist_ok=True)
        package = LOCK["git_daemon"]
        apk = context / "git-daemon.apk"
        if not apk.exists() or hashlib.sha256(apk.read_bytes()).hexdigest() != package["sha256"][machine]:
            url = "https://dl-cdn.alpinelinux.org/alpine/{}/main/{}/git-daemon-{}.apk".format(
                package["alpine"], {"arm64": "aarch64", "amd64": "x86_64"}[machine], package["version"])
            with urllib.request.urlopen(url, timeout=60) as response:
                data = response.read()
            if hashlib.sha256(data).hexdigest() != package["sha256"][machine]:
                raise RuntimeError("git-daemon package checksum mismatch")
            apk.write_bytes(data)
        for name, source in (("kodama", "./cmd/kodama"), ("probe", "./dev/probe"), ("gitserver", "./dev/gitserver")):
            run(["go", "build", "-mod=readonly", "-trimpath", "-o", context / name, source],
                env=dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=machine), timeout=600)
        repository = "localhost:{}/{}/scaffold".format(self.state["port"], self.scope)
        run([self.provider, "build", "-f", ROOT / "dev/images/Containerfile", "-t", repository + ":dev", context], timeout=600)
        run([self.provider, "push"] + (["--tls-verify=false"] if self.provider == "podman" else []) + [repository + ":dev"], timeout=600)
        request = urllib.request.Request("http://localhost:{}/v2/{}/scaffold/manifests/dev".format(self.state["port"], self.scope),
                                         headers={"Accept": MANIFEST_TYPES})
        with urllib.request.urlopen(request, timeout=30) as response:
            digest = response.headers["Docker-Content-Digest"]
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
            raise RuntimeError("registry returned an invalid image digest")
        self.state.update(local_image=repository + ":dev", image=repository + "@" + digest)
        self.save()

    # -- lifecycle --------------------------------------------------------------

    def ensure_cluster(self):
        nodes = self.nodes()
        if nodes:
            self.verify_nodes(nodes)
            run([self.kind, "export", "kubeconfig", "--name", self.state["cluster"], "--kubeconfig", self.directory / "kubeconfig"], env=self.env)
            return nodes
        config = self.directory / "kind.json"
        write_json(config, {"kind": "Cluster", "apiVersion": "kind.x-k8s.io/v1alpha4",
                            "nodes": [{"role": "control-plane", "labels": {LABEL: self.state["owner"]}}]})
        try:
            run([self.kind, "create", "cluster", "--name", self.state["cluster"], "--config", config, "--image", LOCK["images"]["node"],
                 "--kubeconfig", self.directory / "kubeconfig", "--wait", "180s", "--retain"], env=self.env, timeout=600)
        finally:  # journal nodes even after a partial failure, so teardown can find them
            nodes = self.nodes()
            for name in nodes:
                item = self.inspect(name)
                if item:
                    self.state["nodes"][name] = item["Id"]
            self.save()
        return nodes

    def network_item(self):
        """The scoped network as inspected, or None; refuses a network we do not own."""
        result = run([self.provider, "network", "inspect", self.state["network"]], capture=True, check=False)
        if result.returncode:
            if "no such" in result.stderr.lower() or "not found" in result.stderr.lower():
                return None
            raise RuntimeError("network inspection failed: " + result.stderr)
        item = json.loads(result.stdout)[0]
        labels = item.get("Labels") or item.get("labels") or {}
        if labels.get(LABEL) != self.state["owner"]:
            raise RuntimeError("network ownership mismatch: " + self.state["network"])
        return item

    def ensure_network(self):
        """Create the scoped workload network. Its ownership label gates removal."""
        if not self.network_item():
            run([self.provider, "network", "create", "--label", LABEL + "=" + self.state["owner"], self.state["network"]])
            self.network_item()

    def up(self):
        self.select()
        self.prepare()
        if not self.state:
            self.directory.mkdir(parents=True, mode=0o700, exist_ok=True)
            owner = secrets.token_hex(16)
            cluster = "kodama-" + self.scope + "-" + owner[:8]
            self.state = {"root": str(ROOT), "scope": self.scope, "provider": self.provider, "owner": owner,
                          "stack": self.stack, "cluster": cluster, "nodes": {}}
            if self.stack == "services":
                self.state["network"] = cluster + "-net"
            self.save()  # Persist intent before creating any external resource.
        fixtures = self.fixtures()
        nodes = self.ensure_cluster() if self.provides("kind-cluster") else []  # also creates kind's container network
        if self.stack == "services":
            self.ensure_network()
        self.compose("up", "-d", "registry")
        self.state["port"] = int(self.inspect(self.container("registry"))["NetworkSettings"]["Ports"]["5000/tcp"][0]["HostPort"])
        self.save()
        wait_for("http://127.0.0.1:{}/v2/".format(self.state["port"]))
        for node in nodes:  # kubelet pulls localhost:<port>/... through the registry container
            directory = "/etc/containerd/certs.d/localhost:" + str(self.state["port"])
            run([self.provider, "exec", node, "mkdir", "-p", directory])
            run([self.provider, "exec", "-i", node, "cp", "/dev/stdin", directory + "/hosts.toml"], data=
                '[host."http://{}:5000"]\n  capabilities = ["pull", "resolve"]\n'.format(self.container("registry")))
        self.build()
        self.compose("up", "-d")
        self.write_host_environment(fixtures)
        if self.provides("server-deployment"):
            self.apply(self.manifests(fixtures, self.addresses()))
            self.kube("-n", DEPS, "rollout", "status", "deployment/kodama", "--timeout=300s", timeout=330)
        write_json(self.directory / "endpoints.json", self.workload_env())
        if self.stack == "services":
            print("Ready: {} ({}), network: {}".format(self.scope, self.state["cluster"], self.state["network"]))
        else:
            print("Ready: {} ({}), kubeconfig: {}".format(self.scope, self.state["cluster"], self.directory / "kubeconfig"))

    def write_host_environment(self, fixtures):
        def port(service):
            number = SERVICES[service]
            bindings = self.inspect(self.container(service))["NetworkSettings"]["Ports"][str(number) + "/tcp"]
            return int(bindings[0]["HostPort"])

        password = quote((fixtures / "postgres-password").read_text().strip(), safe="")
        token = quote((fixtures / "nats-token").read_text().strip(), safe="")
        values = {
            "DEV_PROVIDER": self.provider,
            "DEV_SCOPE": self.scope,
            "DEV_STACK": self.stack,
            "KODAMA_TEST_POSTGRES_ADMIN_URL": "postgres://postgres:{}@127.0.0.1:{}/postgres?sslmode=disable".format(password, port("postgres")),
            "KODAMA_NATS_URL": "nats://{}@127.0.0.1:{}".format(token, port("nats")),
            "KODAMA_S3_ENDPOINT": "http://127.0.0.1:{}".format(port("localstack")),
            "KODAMA_S3_BUCKET": "kodama-" + self.scope,
            "KODAMA_S3_PREFIX": self.scope + "/",
            "KODAMA_SECRETS_DIR": str(fixtures),
            "KODAMA_REGISTRY_URL": "http://127.0.0.1:{}".format(port("registry")),
        }
        if self.provides("kubeconfig"):
            values.update(KODAMA_TEST_KUBECONFIG=str(self.directory / "kubeconfig"), KUBECTL=self.kubectl)
        else:  # what the Podman runtime needs to start workloads that reach the services
            values.update(KODAMA_TEST_PODMAN_NETWORK=self.state["network"], KODAMA_TEST_SERVER_CALLBACK_HOST=CALLBACK_HOSTS[self.provider])
        write_json(self.directory / "host-env.json", values)

    def preflight(self):
        """Read-only, bounded check: never provision, download, or print credentials."""
        self.select()
        remedy = "make dev-up DEV_SCOPE={} DEV_PROVIDER={} DEV_STACK={}".format(self.scope, self.provider, self.stack)
        if (not self.state or not (self.directory / "host-env.json").is_file()
                or (self.provides("kubeconfig") and not (self.directory / "kubeconfig").is_file())):
            raise RuntimeError("missing dev scope {}: run {}".format(self.directory, remedy))
        values = json.loads((self.directory / "host-env.json").read_text())
        kubectl = values.get("KUBECTL")
        if self.provides("kubeconfig") and (not kubectl or not Path(kubectl).is_file() or not os.access(kubectl, os.X_OK)):
            raise RuntimeError("fixture unavailable: kubectl for {}; run {}".format(self.directory, remedy))
        for key in ("KODAMA_TEST_POSTGRES_ADMIN_URL", "KODAMA_NATS_URL", "KODAMA_S3_ENDPOINT", "KODAMA_REGISTRY_URL"):
            url = urlparse(values.get(key, ""))
            if not url.hostname or not url.port:
                raise RuntimeError("fixture unavailable: {} in {}; run {}".format(key, self.directory, remedy))
            try:
                with socket.create_connection((url.hostname, url.port), timeout=2):
                    pass
            except OSError:
                raise RuntimeError("fixture unavailable: {} in {}; run {}".format(key, self.directory, remedy)) from None
        if self.provides("kubeconfig"):
            run([kubectl, "--kubeconfig", self.directory / "kubeconfig", "--context", "kind-" + self.state["cluster"], "get", "--raw=/readyz", "--request-timeout=5s"], capture=True, timeout=10)
        else:
            try:
                found = self.network_item()
            except RuntimeError:
                found = None
            if not found:
                raise RuntimeError("fixture unavailable: network {} for {}; run {}".format(self.state["network"], self.directory, remedy))
        print("PASS development scope {} (stack {})".format(self.directory, self.stack))

    def addresses(self):
        result = {}
        for service in SERVICES:
            item = self.inspect(self.container(service))
            if not item or not item["State"]["Running"]:
                raise RuntimeError(service + " is not running; see make dev-diagnostics")
            result[service] = item["NetworkSettings"]["Networks"]["kind"]["IPAddress"]
        return result

    def manifests(self, fixtures, addresses):
        data = {p.name: base64.b64encode(p.read_bytes()).decode() for p in fixtures.iterdir()}
        items = [{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": DEPS}},
                 {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": TESTS, "labels": {"pod-security.kubernetes.io/enforce": "restricted"}}},
                 # The probe receives only the development credentials it needs.
                 {"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "fixtures", "namespace": TESTS},
                  "data": {k: v for k, v in data.items() if k in PROBE_SECRETS}}]
        for name, port in SERVICES.items():  # selector-less Services backed by the compose containers
            items += [{"apiVersion": "v1", "kind": "Service", "metadata": {"name": name, "namespace": DEPS},
                       "spec": {"ports": [{"name": "tcp", "port": port, "targetPort": port}]}},
                      {"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
                       "metadata": {"name": name, "namespace": DEPS, "labels": {"kubernetes.io/service-name": name}},
                       "addressType": "IPv4", "ports": [{"name": "tcp", "protocol": "TCP", "port": port}],
                       "endpoints": [{"addresses": [addresses[name]], "conditions": {"ready": True}}]}]
        items += [{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "kodama", "namespace": DEPS},
                   "spec": {"replicas": 1, "selector": {"matchLabels": {"app": "kodama"}},
                            "template": {"metadata": {"labels": {"app": "kodama"}},
                                         "spec": {"automountServiceAccountToken": False,
                                                  "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
                                                  "containers": [{"name": "kodama", "image": self.state["image"], "ports": [{"containerPort": 8080}],
                                                                  "readinessProbe": {"httpGet": {"path": "/readyz", "port": 8080}, "periodSeconds": 2},
                                                                  "resources": {"requests": {"cpu": "50m", "memory": "64Mi"}, "limits": {"memory": "256Mi"}},
                                                                  "securityContext": HARDENED}]}}}},
                  {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "kodama", "namespace": DEPS},
                   "spec": {"selector": {"app": "kodama"}, "ports": [{"port": 8080, "targetPort": 8080}]}}]
        for item in items:
            item["metadata"].setdefault("labels", {})[LABEL] = self.state["owner"]
        return items

    def apply(self, items):
        self.kube("apply", "-f", "-", data=json.dumps({"apiVersion": "v1", "kind": "List", "items": items}))

    def job(self, name, image, command, *, probe=False):
        container = {"name": "check", "image": image, "imagePullPolicy": "Always", "command": command,
                     "resources": {"requests": {"cpu": "50m", "memory": "64Mi"}, "limits": {"memory": "512Mi"}},
                     "securityContext": HARDENED}
        spec = {"restartPolicy": "Never", "automountServiceAccountToken": False,
                "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
                "containers": [container]}
        if probe:
            container["env"] = [{"name": k, "value": v} for k, v in probe_env(self.scope).items()]
            container["volumeMounts"] = [{"name": "secrets", "mountPath": "/secrets"}]
            spec["volumes"] = [{"name": "secrets", "secret": {"secretName": "fixtures", "defaultMode": 0o444}}]
        self.apply([{"apiVersion": "batch/v1", "kind": "Job", "metadata": {"name": name, "namespace": TESTS, "labels": {LABEL: self.state["owner"]}},
                     "spec": {"backoffLimit": 0, "activeDeadlineSeconds": 420, "template": {"spec": spec}}}])
        deadline = time.monotonic() + 440
        while time.monotonic() < deadline:
            status = json.loads(self.kube("-n", TESTS, "get", "job", name, "-o", "json", capture=True).stdout).get("status", {})
            if status.get("succeeded") or status.get("failed"):
                logs = self.kube("-n", TESTS, "logs", "job/" + name, capture=True, check=False).stdout
                print(logs, end="", flush=True)
                if not status.get("succeeded"):
                    raise RuntimeError("probe Job failed: " + name)
                return logs
            time.sleep(2)
        raise RuntimeError("probe Job timed out: " + name)

    def services_probe(self):
        """Run the probe as a Podman workload on the scoped network.

        It reaches every service by its workload URL; there is no server
        Deployment or kubelet pull to check in this stack.
        """
        fixtures = self.directory / "fixtures"
        environment = self.workload_env()
        environment["KODAMA_PROBE_CHECKS"] = SERVICES_PROBE_CHECKS
        args = [self.provider, "run", "--rm", "--network", self.state["network"], "--user", "65532:65532", "--cap-drop", "ALL",
                "--security-opt", "no-new-privileges:true", "--label", LABEL + "=" + self.state["owner"],
                "--name", self.state["cluster"] + "-probe-" + secrets.token_hex(4)]
        for key, value in environment.items():
            args += ["-e", key + "=" + value]
        for name in PROBE_SECRETS:
            args += ["-v", "{}:/secrets/{}:ro".format(fixtures / name, name)]
        logs = run(args + ["--entrypoint", "/usr/local/bin/probe", self.state["local_image"]], capture=True, timeout=420)
        print(logs.stdout, end="", flush=True)
        print("PASS services_smoke (Podman workload reached PostgreSQL, NATS, S3, Git, OIDC and the registry by workload URL)")

    def runtime_acceptance(self):
        """Prove the endpoint files configure the real Podman runtime adapter.

        Runs dev/runtimeacceptance, which starts workloads through
        internal/runtime/podman: one reaches PostgreSQL by its workload URL,
        one reaches the host callback; both are removed by the runtime. Recorded
        separately from the dependency probe above.
        """
        self.select()
        if self.stack != "services":
            raise RuntimeError("runtime acceptance needs DEV_STACK=services; scope {} is {}".format(self.scope, self.stack))
        if not self.state or "image" not in self.state:
            raise RuntimeError("run make dev-up DEV_STACK=services for this scope first")
        if self.provider != "podman":
            raise RuntimeError("the Podman runtime acceptance needs DEV_PROVIDER=podman")
        run(["go", "test", "-count=1", "-v", "./dev/runtimeacceptance"], env=dict(os.environ, KODAMA_DEV_SCOPE_DIR=str(self.directory),
                                                                                    KODAMA_REQUIRE_PODMAN="1"), timeout=600)
        print("PASS runtime_acceptance (Podman runtime workloads reached PostgreSQL by workload URL and the host callback)")

    def integration(self):
        self.select()
        if not self.state or "image" not in self.state:
            raise RuntimeError("run make dev-up for this scope first")
        self.prepare()
        if self.stack == "services":
            return self.services_probe()
        suffix = secrets.token_hex(4)
        logs = self.job("scaffold-" + suffix, self.state["image"], ["/usr/local/bin/probe"], probe=True)
        match = re.search(r"^PULL_IMAGE ([a-z0-9/-]+@sha256:[0-9a-f]{64})$", logs, re.MULTILINE)
        if not match or not match[1].startswith(self.scope + "/"):
            raise RuntimeError("probe did not report a scoped image digest")
        self.job("registry-pull-" + suffix, "localhost:{}/{}".format(self.state["port"], match[1]), ["/usr/local/bin/kodama", "--help"])
        print("PASS scaffold_smoke (including Pod push and kubelet digest pull)")

    def diagnostics(self):
        if not self.state:
            return
        directory = STATE / "reports" / self.scope
        directory.mkdir(parents=True, exist_ok=True)
        # An allow-list avoids exporting Kubernetes Secrets, private keys, or
        # PostgreSQL logs (startup errors can print SQL).
        for service in ("git", "oauthsonas", "nats", "localstack"):
            result = run([self.provider, "logs", self.container(service)], capture=True, check=False, timeout=30)
            (directory / (service + ".log")).write_text(result.stdout + result.stderr)
        if not self.provides("kubeconfig") or not (self.directory / "kubeconfig").exists():
            return
        self.kubectl = install("kubectl")
        for name, args in (("pods.txt", ["get", "pods", "-A", "-o", "wide"]), ("events.txt", ["get", "events", "-A", "--sort-by=.metadata.creationTimestamp"])):
            result = self.kube(*args, capture=True, check=False, timeout=30)
            (directory / name).write_text(result.stdout + result.stderr)
        for namespace in (DEPS, TESTS):
            pods = self.kube("-n", namespace, "get", "pods", "-o", "json", capture=True, check=False, timeout=30)
            for pod in json.loads(pods.stdout or '{"items": []}')["items"]:
                name = pod["metadata"]["name"]
                result = self.kube("-n", namespace, "logs", name, capture=True, check=False, timeout=30)
                (directory / (name + ".log")).write_text(result.stdout + result.stderr)

    def down(self):
        if not self.state:
            print("Scope already absent: " + self.scope)
            return
        self.prepare()
        if self.provides("kind-cluster"):
            self.verify_nodes(self.nodes())  # refuse before deleting anything
        else:
            self.network_item()  # refuses a network that does not carry our ownership label
        self.compose("down", "-v", check=False)
        for container in self.owned_containers():  # anything compose missed, selected by our random label
            run([self.provider, "rm", "-f", container])
        if self.provides("kind-cluster"):
            run([self.kind, "delete", "cluster", "--name", self.state["cluster"], "--kubeconfig", self.directory / "kubeconfig"], env=self.env)
        elif self.network_item():
            run([self.provider, "network", "rm", self.state["network"]])
        shutil.rmtree(self.directory)
        self.state = None
        print("Removed scope: " + self.scope)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["up", "down", "integration", "runtime", "smoke", "diagnostics", "preflight"])
    parser.add_argument("--scope", default=os.environ.get("DEV_SCOPE", "t-001-local"))
    parser.add_argument("--provider", default=os.environ.get("DEV_PROVIDER", "podman"))
    parser.add_argument("--stack", default=None, help="services or cluster; defaults to $DEV_STACK, then cluster (an existing scope keeps its stack)")
    args = parser.parse_args()
    validate_scope(args.scope)
    stack = requested_stack(args.stack)
    os.umask(0o077)
    (STATE / "locks").mkdir(parents=True, exist_ok=True)
    with (STATE / "locks" / (args.scope + ".lock")).open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        environment = Environment(args.scope, args.provider, stack)
        try:
            if args.command == "smoke":
                if environment.state:
                    raise RuntimeError("scaffold-smoke requires a fresh DEV_SCOPE")
                environment.up()
                environment.integration()
            elif args.command == "runtime":
                environment.runtime_acceptance()
            else:
                getattr(environment, args.command)()
        except (Exception, KeyboardInterrupt):
            if args.command not in ("diagnostics", "preflight"):
                try:
                    environment.diagnostics()
                except Exception as error:
                    print("Diagnostic collection failed: " + str(error), file=sys.stderr)
            raise


if __name__ == "__main__":
    try:
        main()
    except (Exception, KeyboardInterrupt) as error:
        print("Development environment failed: " + (str(error) or type(error).__name__), file=sys.stderr)
        sys.exit(1)
