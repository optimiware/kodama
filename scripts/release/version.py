#!/usr/bin/env python3
"""Plan the semver git tag used to publish the Kodama image.

Git tags and image tags are the same semver string (0.0.1, not v0.0.1). A
leading v is accepted when reading existing tags so a manual v0.1.0 still
counts. VERSION is the baseline published only when the repository has no
semver tag yet; this tree commits 0.0.1. After that, an untagged commit gets
the next patch. Minor and major bumps are manual tags.
"""

import argparse
import re
import subprocess
import sys
from pathlib import Path

SEMVER = re.compile(r"^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")
ROOT = Path(__file__).resolve().parents[2]


def parse(tag):
    match = SEMVER.fullmatch(tag.strip())
    if not match:
        return None
    return tuple(int(part) for part in match.groups())


def format_version(version):
    return "{}.{}.{}".format(*version)


def plan(baseline, tags, head_tags):
    """Return (version, create_tag) for this commit.

    baseline is the VERSION file. tags are every tag name. head_tags are the
    tag names that point at the commit being published. create_tag is true
    when that exact version tag is not already on the commit.
    """
    baseline_version = parse(baseline)
    if baseline_version is None:
        raise ValueError("VERSION is not a semver release: {!r}".format(baseline.strip()))
    on_head = [version for version in (parse(tag) for tag in head_tags) if version]
    if on_head:
        chosen = max(on_head)
        return format_version(chosen), False
    existing = [version for version in (parse(tag) for tag in tags) if version]
    if not existing:
        return format_version(baseline_version), True
    major, minor, patch = max(existing)
    return format_version((major, minor, patch + 1)), True


def read_baseline(root):
    return (root / "VERSION").read_text(encoding="utf-8").strip()


def git_tags(root, *args):
    result = subprocess.run(
        ["git", "-C", str(root), "tag", "--list", *args],
        check=True, capture_output=True, text=True,
    )
    return [line for line in result.stdout.splitlines() if line]


def write_output(path, values):
    with open(path, "a", encoding="utf-8") as handle:
        for key, value in values.items():
            handle.write("{}={}\n".format(key, value))


def main(argv):
    parser = argparse.ArgumentParser(description="Plan the Kodama image semver tag")
    parser.add_argument("command", choices=("plan",))
    parser.add_argument("--root", type=Path, default=ROOT)
    parser.add_argument("--github-output", type=Path)
    args = parser.parse_args(argv)
    root = args.root
    version, create = plan(read_baseline(root), git_tags(root), git_tags(root, "--points-at", "HEAD"))
    values = {"version": version, "create_tag": "true" if create else "false"}
    if args.github_output:
        write_output(args.github_output, values)
    print("version={} create_tag={}".format(version, values["create_tag"]))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except (ValueError, OSError, subprocess.CalledProcessError) as exc:
        print("version plan failed: {}".format(exc), file=sys.stderr)
        sys.exit(1)
