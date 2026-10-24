"""Install checksum-locked development tools into this worktree only."""
import hashlib
import json
import os
from pathlib import Path
import platform
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
LOCK = json.loads((ROOT / "dev/versions.lock.json").read_text())


def command_path(name, target):
    """Stable, worktree-local executable name for host fixtures and PATH."""
    directory = target.parent / "bin"
    directory.mkdir(parents=True, exist_ok=True)
    alias = directory / name
    if alias.is_symlink():
        if alias.resolve() == target.resolve():
            return str(alias)
        alias.unlink()
    elif alias.exists():
        raise RuntimeError("refusing to replace unmanaged tool " + str(alias))
    alias.symlink_to(target.resolve())
    return str(alias)


def install(name):
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64"}[platform.machine()]
    system = platform.system().lower()
    spec = LOCK[name]
    expected = spec["sha256"][system + "-" + arch]
    target = ROOT / "dev/.tools" / (name + "-" + spec["version"])
    if target.exists() and hashlib.sha256(target.read_bytes()).hexdigest() == expected:
        return command_path(name, target)
    if name == "kind":
        url = "https://github.com/kubernetes-sigs/kind/releases/download/v{}/kind-{}-{}".format(spec["version"], system, arch)
    else:
        url = "https://dl.k8s.io/release/v{}/bin/{}/{}/kubectl".format(spec["version"], system, arch)
    print("Downloading " + url, flush=True)
    with urllib.request.urlopen(url, timeout=120) as response:
        data = response.read()
    if hashlib.sha256(data).hexdigest() != expected:
        raise RuntimeError(name + " checksum mismatch")
    target.parent.mkdir(parents=True, exist_ok=True)
    temporary = target.with_suffix(".tmp-" + str(os.getpid()))
    temporary.write_bytes(data)
    temporary.chmod(0o755)
    temporary.replace(target)
    return command_path(name, target)


if __name__ == "__main__":
    for tool in ("kind", "kubectl"):
        print(install(tool))
