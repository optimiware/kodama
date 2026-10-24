"""Real dev_teardown acceptance: the scope's own resources are removed, independent ones survive.

The cluster stack proves an independent kind cluster survives; the services
stack proves an independent Podman network and container survive.
"""
import argparse
import fcntl
import json
import os
from pathlib import Path
import secrets
import shutil

from environment import Environment, LABEL, LOCK, STATE, run, validate_scope


def check_services(env, provider):
    foreign = "kodama-sentinel-" + secrets.token_hex(6)
    run([provider, "network", "create", foreign])
    try:
        run([provider, "run", "-d", "--name", foreign, "--network", foreign, "--entrypoint", "sleep", env.state["local_image"], "600"])
        identity = env.inspect(foreign)["Id"]
        owned, owner = env.state["network"], env.state["owner"]
        # A network without our ownership label is never removed, and nothing else is deleted first.
        env.state["network"] = foreign
        try:
            env.down()
            raise AssertionError("foreign network was accepted")
        except RuntimeError as error:
            if "ownership mismatch" not in str(error):
                raise
        finally:
            env.state["network"] = owned
        if not env.owned_containers():
            raise AssertionError("refused teardown removed containers")
        # A missing resource models interrupted/partial cleanup.
        run([provider, "rm", "-f", env.owned_containers()[0]])
        env.down()
        env.down()
        remaining = run([provider, "ps", "-aq", "--filter", "label=" + LABEL + "=" + owner], capture=True).stdout.split()
        if env.directory.exists() or remaining or run([provider, "network", "exists", owned], check=False).returncode == 0:
            raise AssertionError("scoped resources remain")
        if env.inspect(foreign)["Id"] != identity or run([provider, "network", "exists", foreign], check=False).returncode:
            raise AssertionError("foreign resources changed")
        print("PASS dev_teardown (services): ownership guard, partial cleanup, repeated cleanup, foreign network/container")
    finally:
        run([provider, "rm", "-f", foreign], check=False)
        run([provider, "network", "rm", foreign], check=False)


def check(scope, provider):
    env = Environment(scope, provider)
    if not env.state:
        raise RuntimeError("dev_teardown needs an existing scope from scaffold-smoke/dev-up")
    env.prepare()
    if env.stack == "services":
        return check_services(env, provider)
    foreign = "kodama-sentinel-" + secrets.token_hex(6)
    sentinel_dir = STATE / "sentinels" / foreign
    sentinel_dir.mkdir(parents=True)
    config = sentinel_dir / "kubeconfig"
    user_config = Path.home() / ".kube/config"
    original = user_config.read_bytes() if user_config.exists() else None
    runtime_env = dict(os.environ, KIND_EXPERIMENTAL_PROVIDER=provider)
    try:
        run([env.kind, "create", "cluster", "--name", foreign, "--image", LOCK["images"]["node"], "--kubeconfig", config, "--wait", "180s"], env=runtime_env, timeout=600)
        kube = [env.kubectl, "--kubeconfig", config, "--context", "kind-" + foreign]
        run(kube + ["create", "configmap", "sentinel", "--from-literal=value=preserved"])
        nodes = run([env.kind, "get", "nodes", "--name", foreign], env=runtime_env, capture=True).stdout.split()
        identities = {name: env.inspect(name)["Id"] for name in nodes}
        # First test cleanup refusal while the original scope remains healthy.
        saved = dict(env.state["nodes"])
        env.state["nodes"] = {name: "wrong-id" for name in saved}
        try:
            env.down()
            raise AssertionError("foreign node identity was accepted")
        except RuntimeError as error:
            if "ownership mismatch" not in str(error):
                raise
        finally:
            env.state["nodes"] = saved
        # A missing resource models interrupted/partial cleanup.
        run([provider, "rm", "-f", env.owned_containers()[0]])
        owned_nodes = list(env.state["nodes"])
        env.down()
        env.down()
        if env.directory.exists() or any(env.inspect(name) for name in owned_nodes):
            raise AssertionError("scoped resources remain")
        for name, identity in identities.items():
            if env.inspect(name)["Id"] != identity:
                raise AssertionError("foreign node changed")
        result = json.loads(run(kube + ["get", "configmap", "sentinel", "-o", "json"], capture=True).stdout)
        if result["data"]["value"] != "preserved":
            raise AssertionError("foreign resource changed")
        after = user_config.read_bytes() if user_config.exists() else None
        if after != original:
            raise AssertionError("user kubeconfig changed")
        print("PASS dev_teardown: identity guard, partial cleanup, repeated cleanup, foreign cluster/resource, user kubeconfig")
    finally:
        # This cluster is owned by the acceptance harness, not by dev-down.
        run([env.kind, "delete", "cluster", "--name", foreign, "--kubeconfig", config], env=runtime_env)
        shutil.rmtree(sentinel_dir)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scope", required=True)
    parser.add_argument("--provider", choices=["podman", "docker"], required=True)
    args = parser.parse_args()
    validate_scope(args.scope)
    locks = STATE / "locks"
    locks.mkdir(parents=True, exist_ok=True)
    with (locks / (args.scope + ".lock")).open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        check(args.scope, args.provider)
