"""Measure test-database provisioning on a native (embedded-style) PostgreSQL binary.

T-091 asks whether an embedded PostgreSQL binary beats the template clone. An
embedded library (for example zonky/fergusstrange embedded-postgres) downloads
the same native binaries this script uses, runs initdb into a temporary
directory, starts postmaster on a loopback port and stops it afterwards. This
script does those steps by hand, with no new Go dependency, against the same
BenchmarkProvision that measures the scope's container, then removes the
instance and its data directory. Nothing is adopted or installed system-wide.

    python3 scripts/dev/embedded_benchmark.py [--benchtime 40x]

Python 3.9+, standard library only. Linux amd64; needs network once to fetch
the pinned archive into dev/.state/embedded/ (ignored by Git).
"""
import argparse
import hashlib
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tarfile
import tempfile
import time
import urllib.request
import zipfile

from tools import ROOT

VERSION = "17.6.0"  # the same PostgreSQL release as the scope's container image
URL = ("https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64/{0}/"
       "embedded-postgres-binaries-linux-amd64-{0}.jar").format(VERSION)
SHA256 = "23da5a044b4fb7a5a081a45008c95749c873305328d73f86aefd56922ce1d29d"
CACHE = ROOT / "dev/.state/embedded" / VERSION
# Durable is PostgreSQL's default (what the scope's container runs); relaxed is
# what embedded libraries and throwaway test databases usually choose.
CONFIGURATIONS = {
    "durable (defaults)": [],
    "relaxed (fsync=off, synchronous_commit=off, full_page_writes=off)":
        ["-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"],
}


def fetch():
    """Download and verify the archive once; return the extracted directory."""
    if (CACHE / "bin/initdb").is_file():
        return CACHE
    CACHE.mkdir(parents=True, exist_ok=True)
    archive = CACHE / "binaries.jar"
    with urllib.request.urlopen(URL, timeout=120) as response:
        data = response.read()
    if hashlib.sha256(data).hexdigest() != SHA256:
        raise RuntimeError("embedded PostgreSQL archive checksum mismatch")
    archive.write_bytes(data)
    with zipfile.ZipFile(archive) as jar:
        jar.extract("postgres-linux-x86_64.txz", CACHE)
    with tarfile.open(CACHE / "postgres-linux-x86_64.txz") as tar:
        tar.extractall(CACHE, filter="data") if hasattr(tarfile, "data_filter") else tar.extractall(CACHE)
    return CACHE


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def timed(args, **kwargs):
    start = time.monotonic()
    subprocess.run([str(a) for a in args], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, **kwargs)
    return time.monotonic() - start


def measure(root, name, settings, benchtime):
    print("== native PostgreSQL {} — {}".format(VERSION, name), flush=True)
    data = Path(tempfile.mkdtemp(prefix="kodama-embedded-", dir=ROOT / "dev/.state/embedded"))
    port = free_port()
    pg_ctl = root / "bin/pg_ctl"
    try:
        initdb = timed([root / "bin/initdb", "-D", data / "data", "-U", "postgres", "-A", "trust", "-E", "UTF8", "--locale=C"])
        options = ["-c", "listen_addresses=127.0.0.1", "-c", "port=" + str(port), "-c", "unix_socket_directories=" + str(data)] + settings
        start = timed([pg_ctl, "-D", data / "data", "-o", " ".join(options), "-l", data / "log", "-w", "start"])
        print("instance cost: initdb {:.0f} ms, start {:.0f} ms (paid once per instance)".format(initdb * 1000, start * 1000), flush=True)
        env = dict(os.environ, KODAMA_TEST_POSTGRES_ADMIN_URL="postgres://postgres@127.0.0.1:{}/postgres?sslmode=disable".format(port))
        result = subprocess.run(["go", "test", "-count=1", "-run", "^$", "-bench", "Provision", "-benchtime", benchtime, "-cpu", "1",
                                 "./test/support/postgres"], cwd=ROOT, env=env, text=True, capture_output=True)
        print("\n".join(line for line in result.stdout.splitlines() if line.startswith(("Benchmark", "FAIL", "---", "cpu:"))), flush=True)
        if result.returncode:
            raise RuntimeError("benchmark failed:\n" + result.stdout[-2000:] + result.stderr[-2000:])
    finally:
        subprocess.run([str(pg_ctl), "-D", str(data / "data"), "-m", "immediate", "stop"], check=False, capture_output=True)
        shutil.rmtree(data, ignore_errors=True)  # the instance owns only this temporary directory


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--benchtime", default="40x")
    args = parser.parse_args()
    root = fetch()
    for name, settings in CONFIGURATIONS.items():
        measure(root, name, settings, args.benchtime)


if __name__ == "__main__":
    main()
