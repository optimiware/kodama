"""Formatting, configuration, and repository documentation checks."""
import ast
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import unquote

from tools import LOCK, ROOT


def source_files(directory, suffix):
    """Walk repository sources without descending into dependencies or local state."""
    for parent, directories, files in os.walk(directory):
        directories[:] = sorted(name for name in directories if name not in {
            "node_modules", ".git", ".work", ".state", ".harness", "__pycache__",
        })
        for name in sorted(files):
            if name.endswith(suffix):
                yield Path(parent) / name


def fmt(packages=()):
    if packages:
        files = []
        for package in packages:
            recursive = package.endswith("/...")
            clean = package[:-4] if recursive else package
            directory = (ROOT / clean).resolve()
            directory.relative_to(ROOT.resolve())
            if not directory.exists():
                continue
            if recursive:
                files.extend(str(p) for p in source_files(directory, ".go"))
            else:
                files.extend(str(p) for p in sorted(directory.glob("*.go")))
    else:
        files = [str(p) for base in ("cmd", "internal", "dev", "contracts", "test") for p in source_files(ROOT / base, ".go")]
    if not files:
        return
    result = subprocess.run(["gofmt", "-l", *files], capture_output=True, text=True, check=True)
    if result.stdout:
        raise RuntimeError("Run make fmt:\n" + result.stdout)


def containerfile_images(text):
    """Image references on FROM lines, skipping BuildKit platform flags."""
    images = []
    for line in text.splitlines():
        parts = line.split()
        if not parts or parts[0] != "FROM":
            continue
        index = 1
        while index < len(parts) and parts[index].startswith("--"):
            index += 1
        if index < len(parts):
            images.append(parts[index])
    return images


def config():
    for path in (ROOT / "scripts/dev").glob("*.py"):
        ast.parse(path.read_text(), filename=str(path))
    json.loads((ROOT / "openforge.json").read_text())
    for name in ("package.json", "tsconfig.json", "biome.json"):
        json.loads((ROOT / "scripts/openforge" / name).read_text())
    for path in [*source_files(ROOT / ".opencode", ".json"), *ROOT.glob("opencode.json")]:
        json.loads(path.read_text())
    for path in [*(ROOT / "dev/contracts").glob("*.json"), *source_files(ROOT / "contracts", ".json")]:
        json.loads(path.read_text())
    # Every image (compose services, Containerfile base, kind node) needs an immutable digest.
    images = re.findall(r"^\s*image:\s*(\S+)", (ROOT / "dev/compose.yaml").read_text(), re.MULTILINE)
    images += containerfile_images((ROOT / "dev/images/Containerfile").read_text())
    images += containerfile_images((ROOT / "Containerfile").read_text())
    images += LOCK["images"].values()
    for image in images:
        if not image.startswith("${") and not re.fullmatch(r".+:[^@]+@sha256:[a-f0-9]{64}", image):
            raise RuntimeError("Unpinned image " + image)
    if not re.search(r"^go " + re.escape(LOCK["go"]) + "$", (ROOT / "go.mod").read_text(), re.MULTILINE):
        raise RuntimeError("Go toolchain/lock mismatch")
    if "golang:" + LOCK["go"] not in (ROOT / "Containerfile").read_text():
        raise RuntimeError("Containerfile Go image does not match the Go lock")
    print("PASS configuration and lock validation ({} images)".format(len(images)))


def anchors(text):
    result = set()
    counts = {}
    for heading in re.findall(r"^#{1,6} (.+)$", text, re.MULTILINE):
        slug = re.sub(r"[^\w\- ]", "", heading.lower()).replace(" ", "-")
        count = counts.get(slug, 0)
        counts[slug] = count + 1
        result.add(slug if count == 0 else slug + "-" + str(count))
    return result


def docs():
    files = list(ROOT.glob("*.md")) + list(source_files(ROOT / "docs", ".md")) + list((ROOT / "dev").glob("*.md")) + list(source_files(ROOT / "contracts", ".md"))
    files += list(source_files(ROOT / ".opencode/agents", ".md")) + [ROOT / "scripts/openforge/README.md"]
    for path in files:
        text = path.read_text()
        if len(re.findall(r"^```", text, re.MULTILINE)) % 2:
            raise RuntimeError(str(path) + ": unclosed fence")
        for language, body in re.findall(r"^```(\w*)\n(.*?)^```", text, re.MULTILINE | re.DOTALL):
            if language == "json":
                json.loads(body)
            elif language in ("bash", "sh", "shell"):
                subprocess.run(["bash", "-n"], input=body, text=True, check=True)
        without_code = re.sub(r"^```.*?^```", "", text, flags=re.MULTILINE | re.DOTALL)
        for link in re.findall(r"\[[^\]]*\]\(([^ )]+)(?: [^)]*)?\)", without_code):
            if re.match(r"[a-zA-Z]+:", link):
                continue
            destination, _, fragment = unquote(link).partition("#")
            target = (path.parent / destination).resolve() if destination else path
            if not target.exists():
                raise RuntimeError("{}: broken link {}".format(path.relative_to(ROOT), link))
            if fragment and target.suffix == ".md" and fragment not in anchors(target.read_text()):
                raise RuntimeError("{}: broken anchor {}".format(path.relative_to(ROOT), link))
    print("PASS documentation links, anchors, JSON and shell fences ({} files)".format(len(files)))


if __name__ == "__main__":
    if sys.argv[1] == "fmt":
        fmt(sys.argv[2:])
    else:
        {"config": config, "docs": docs}[sys.argv[1]]()
