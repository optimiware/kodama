"""Scoped Go check runner and FORGE_RESULTS emitter for Kodama."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
DEFAULT_PACKAGES = [
    "./cmd/...",
    "./internal/...",
    "./api/...",
    "./contracts/...",
    "./pkg/...",
    "./test/support/...",
]
SERVICE_ENV = (
    "KODAMA_TEST_POSTGRES_ADMIN_URL",
    "KODAMA_NATS_URL",
    "KODAMA_REGISTRY_URL",
    "KODAMA_S3_ENDPOINT",
    "KODAMA_S3_BUCKET",
    "KODAMA_S3_PREFIX",
    "KODAMA_GIT_URL",
    "KODAMA_TEST_KUBECONFIG",
    "KODAMA_KUBECONFIG",
    "KODAMA_SECRETS_DIR",
    "KODAMA_REQUIRE_POSTGRES",
    "KODAMA_REQUIRE_S3",
    "KODAMA_REQUIRE_REGISTRY",
    "KODAMA_REQUIRE_KUBERNETES",
)
EX_TEMPFAIL = 75


def normalize(name: str) -> str:
    stripped = re.sub(r"[^a-z0-9]", "", name.lower())
    return stripped[4:] if stripped.startswith("test") else stripped


def parse_go_test_json(stream):
    """Parse `go test -json` lines, echoing Output and returning (passed, failed, skipped, pkg_failed)."""
    by_pkg = {}
    for raw in stream:
        line = raw.strip()
        if not line.startswith("{"):
            sys.stdout.write(raw)
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            sys.stdout.write(raw)
            continue
        if not isinstance(event, dict):
            continue
        output = event.get("Output")
        if event.get("Action") == "output" and isinstance(output, str):
            sys.stdout.write(output)
        pkg = event.get("Package")
        action = event.get("Action")
        test = event.get("Test")
        if not isinstance(pkg, str) or not isinstance(action, str):
            continue
        state = by_pkg.setdefault(
            pkg,
            {"action": None, "no_test_files": False, "passed": set(), "failed": set(), "skipped": set()},
        )
        if action == "output" and isinstance(output, str) and "[no test files]" in output:
            state["no_test_files"] = True
        if isinstance(test, str) and test:
            if action == "pass":
                state["passed"].add(test)
            elif action == "fail":
                state["failed"].add(test)
            elif action == "skip":
                state["skipped"].add(test)
        elif action in ("pass", "fail", "skip"):
            if state["action"] not in ("fail", "skip") or action == "fail":
                state["action"] = action

    passed, failed, skipped = set(), set(), set()
    pkg_failed = False
    for state in by_pkg.values():
        if state["action"] == "fail" or (state["action"] in ("skip", None) and not state["no_test_files"]):
            pkg_failed = True
        invalid = state["failed"] | state["skipped"]
        for test in state["failed"]:
            for part in test.split("/"):
                failed.add(part)
                failed.add(normalize(part))
        for test in state["skipped"]:
            for part in test.split("/"):
                skipped.add(part)
                skipped.add(normalize(part))
        for test in state["passed"]:
            parts = test.split("/")
            for idx, part in enumerate(parts):
                prefix = "/".join(parts[: idx + 1])
                if any(
                    bad == prefix or bad.startswith(prefix + "/") or prefix.startswith(bad + "/")
                    for bad in invalid
                ):
                    continue
                passed.add(part)
                passed.add(normalize(part))
    passed -= failed | skipped
    return sorted(passed), sorted(failed), sorted(skipped), pkg_failed


def write_infra_pause(env: dict[str, str], reason: str) -> None:
    results_path = env.get("FORGE_RESULTS")
    if results_path:
        Path(results_path).parent.mkdir(parents=True, exist_ok=True)
        Path(results_path).write_text(
            json.dumps({"infra_pause": reason.strip(), "passed": [], "failed": [], "skipped": []}, indent=2) + "\n"
        )


def provision_missing_scope(env: dict[str, str], scope: str, provider: str) -> None:
    """Bring the scope up when it does not exist yet; a failure is left to preflight to report.

    Set KODAMA_AUTO_PROVISION=0 to require an operator-provisioned scope.
    """
    if env.get("KODAMA_AUTO_PROVISION", "1") == "0":
        return
    if (ROOT / "dev/.state/environments" / scope / "host-env.json").is_file():
        return
    stack = env.get("DEV_STACK") or "cluster"
    sys.stderr.write(f"development scope {scope} is missing; provisioning stack {stack}\n")
    subprocess.run(
        [sys.executable, str(ROOT / "scripts/dev/environment.py"), "up", "--scope", scope, "--provider", provider, "--stack", stack],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
        timeout=1200,
    )


def prepare_env(integration: bool) -> dict[str, str]:
    env = dict(os.environ)
    for key in SERVICE_ENV:
        env.pop(key, None)
    if not integration:
        return env
    scope = env.get("DEV_SCOPE") or env.get("KODAMA_RESOURCE_PREFIX") or "t-001-local"
    provider = env.get("DEV_PROVIDER") or "podman"
    provision_missing_scope(env, scope, provider)
    preflight = subprocess.run(
        [sys.executable, str(ROOT / "scripts/dev/environment.py"), "preflight", "--scope", scope, "--provider", provider],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
    )
    if preflight.returncode != 0:
        msg = (preflight.stdout + preflight.stderr) or f"preflight failed for scope {scope}\n"
        sys.stderr.write(msg)
        write_infra_pause(env, msg)
        sys.exit(EX_TEMPFAIL)
    host_env_path = ROOT / "dev/.state/environments" / scope / "host-env.json"
    if not host_env_path.is_file():
        msg = f"fixture unavailable: missing {host_env_path}; run make dev-up DEV_SCOPE={scope}\n"
        sys.stderr.write(msg)
        write_infra_pause(env, msg)
        sys.exit(EX_TEMPFAIL)
    try:
        host_env = json.loads(host_env_path.read_text())
    except Exception as exc:
        msg = f"fixture unavailable: unreadable {host_env_path}: {exc}\n"
        sys.stderr.write(msg)
        write_infra_pause(env, msg)
        sys.exit(EX_TEMPFAIL)
    for key, value in host_env.items():
        if isinstance(value, str) and not key.startswith("KODAMA_REQUIRE_"):
            env[key] = value
    return env


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="Run scoped Go checks and emit FORGE_RESULTS.")
    parser.add_argument("--integration", action="store_true", help="Run integration checks against DEV_SCOPE")
    parser.add_argument("packages", nargs="*", help="Go package patterns")
    args = parser.parse_args(argv)

    packages = [pkg for item in args.packages for pkg in item.split() if pkg] or list(DEFAULT_PACKAGES)
    env = prepare_env(args.integration)
    go_bin = env.get("GO", "go")

    fmt_run = subprocess.run([sys.executable, str(ROOT / "scripts/dev/check.py"), "fmt", *packages], cwd=ROOT, env=env)
    if fmt_run.returncode != 0:
        return fmt_run.returncode

    vet_cmd = [go_bin, "vet", *([] if args.integration else ["-tags", "unit"]), *packages]
    vet_run = subprocess.run(vet_cmd, cwd=ROOT, env=env)
    if vet_run.returncode != 0:
        return vet_run.returncode

    test_cmd = [
        go_bin,
        "test",
        "-json",
        "-race",
        "-count=1",
        # Integration suites legitimately run for many minutes on a shared host; Go's 10m default kills them.
        *(["-timeout", env.get("KODAMA_GO_TEST_TIMEOUT", "30m")] if args.integration else ["-tags", "unit"]),
        *packages,
    ]
    proc = subprocess.Popen(test_cmd, cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    assert proc.stdout is not None
    passed, failed, skipped, pkg_failed = parse_go_test_json(proc.stdout)
    returncode = proc.wait()

    results_path = env.get("FORGE_RESULTS")
    if results_path:
        Path(results_path).parent.mkdir(parents=True, exist_ok=True)
        Path(results_path).write_text(
            json.dumps({"passed": passed, "failed": failed, "skipped": skipped}, indent=2) + "\n"
        )

    if returncode != 0:
        return returncode
    return 1 if pkg_failed else 0


if __name__ == "__main__":
    sys.exit(main())
