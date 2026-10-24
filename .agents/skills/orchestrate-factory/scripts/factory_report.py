#!/usr/bin/env python3
"""Read-only reports for an OpenForge factory: status, stuck, metrics, progress.

Usage (from anywhere inside the repository):
  factory_report.py status  [--log PATH]
  factory_report.py stuck   [--log PATH]
  factory_report.py metrics [--hours N]
  factory_report.py progress
It never writes to the repository or the harness state.
"""
import argparse
import collections
import datetime
import glob
import json
import os
import re
import subprocess
import sys


def root():
    common = subprocess.check_output(["git", "rev-parse", "--path-format=absolute", "--git-common-dir"], text=True).strip()
    return os.path.dirname(common)


ROOT = root()
HOME = os.path.join(ROOT, ".work", ".harness")


def read_json(path, default):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return default


def tasks():
    out = {}
    for path in glob.glob(os.path.join(ROOT, "docs/tasks/T-*.md")):
        parts = open(path).read().split("---")
        meta = {}
        for line in parts[1].strip().splitlines():
            if ":" in line:
                k, v = line.split(":", 1)
                meta[k.strip()] = v.strip()
        meta["path"] = path
        out[meta["id"]] = meta
    return out


def ids(value):
    return [x.strip() for x in value.strip("[]").split(",") if x.strip()]


def events():
    out = []
    try:
        for line in open(os.path.join(HOME, "events.jsonl")):
            try:
                e = json.loads(line)
                e["t"] = datetime.datetime.fromisoformat(e["ts"].replace("Z", "+00:00"))
                out.append(e)
            except ValueError:
                pass
    except OSError:
        pass
    return out


def cli_status():
    p = subprocess.run(["bun", "scripts/openforge/src/cli.ts", "status", "--json"], cwd=ROOT, capture_output=True, text=True)
    text = p.stdout
    start = text.find("{")
    try:
        return json.loads(text[start:])
    except ValueError:
        return {"ready": [], "active": [], "parked": {}, "main": {}}


def alive():
    p = subprocess.run(["ps", "-eo", "pid,etime,args"], capture_output=True, text=True)
    for line in p.stdout.splitlines():
        if "cli.ts run" in line and "grep" not in line and "factory_report" not in line:
            return line.split(None, 2)[:2]
    return None


def cmd_status(args):
    a = alive()
    print("HARNESS:", f"running (pid {a[0]}, up {a[1]})" if a else "NOT RUNNING (drained, halted or crashed)")
    if os.path.exists(os.path.join(HOME, "HALT")):
        print("HALT file present: the harness stops gracefully")
    s = cli_status()
    print("ready:", s.get("ready"))
    for t in s.get("active", []):
        print(f"active {t['id']:7} {t['phase']:12} | {t['activity'][:60]:60} | repairs {t['repairs']}")
    for k, v in s.get("parked", {}).items():
        print(f"PARKED {k}: {str(v)[:360]}")
    m = s.get("main", {})
    last = (m.get("lastResult") or {})
    print(f"main: mergesSinceCheck={m.get('mergesSinceCheck')} blocked={m.get('blocked')} last={last.get('message')}")
    if args.log and os.path.exists(args.log):
        paused = collections.OrderedDict()
        for line in open(args.log, errors="replace"):
            mt = re.search(r"\] (T-\d+): PAUSED: (.*)", line)
            if mt:
                paused[mt.group(1)] = mt.group(2)[:200]
        for k, v in paused.items():
            print(f"PAUSED {k}: {v}")
    print("recent commits on main:")
    print(subprocess.run(["git", "log", "--oneline", "-5"], cwd=ROOT, capture_output=True, text=True).stdout.rstrip())


def cmd_stuck(args):
    """Last shell activity per active task, from the harness log."""
    s = cli_status()
    if not args.log or not os.path.exists(args.log):
        print("pass --log PATH of the harness output")
        return
    lines = open(args.log, errors="replace").read().splitlines()
    now = datetime.datetime.now(datetime.timezone.utc)
    for t in s.get("active", []):
        slug, last, n = t["slug"], None, 0
        stamp = None
        for line in lines:
            if line.startswith("[") and "] INFO" in line:
                stamp = line[1:13]
            if f"cwd: \"{ROOT}/.work/{slug}" in line:
                n += 1
                last = stamp
        print(f"{t['id']:7} {t['activity'][:34]:34} last shell {last or 'never'} (UTC clock {now:%H:%M:%S}) shells {n}")


def cmd_metrics(args):
    ev = events()
    cut = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=args.hours)
    r = [e for e in ev if e["t"] >= cut]
    print(f"window: last {args.hours}h, events {len(r)}, all-time merges {sum(e['type'] == 'merged' for e in ev)}")
    print("counts:", dict(collections.Counter(e["type"] for e in r)))
    print("merged:", [(e["task"], e["ts"][11:16]) for e in r if e["type"] == "merged"])
    print("parked:", dict(collections.Counter(e["task"] for e in r if e["type"] == "parked")))
    print("unparked:", dict(collections.Counter(e["task"] for e in r if e["type"] == "unparked")))
    by = collections.defaultdict(list)
    for e in r:
        if e["type"] == "agent_finished":
            by[e["role"]].append(e["durationSeconds"])
    for role, v in sorted(by.items()):
        print(f"agent {role:12} runs={len(v):3} total={sum(v) / 60:6.0f}m avg={sum(v) / len(v) / 60:5.1f}m max={max(v) / 60:5.1f}m")


def cmd_progress(args):
    t = tasks()
    order = ["foundation", "M1", "M2", "M3", "M4", "production"]
    by = collections.defaultdict(collections.Counter)
    for d in t.values():
        by[d["milestone"]][d["status"]] += 1
    tot = collections.Counter()
    for m in sorted(by, key=lambda x: order.index(x) if x in order else 99):
        c = by[m]
        n = sum(c.values())
        tot.update(c)
        print(f"{m:11} {c['done']:>3}/{n:<3} {100 * c['done'] // n:>3}%  open {c['planned'] + c['blocked']}  deferred {c['deferred']}")
    n = sum(tot.values())
    sched = n - tot["deferred"]
    print(f"ALL         {tot['done']}/{n} {100 * tot['done'] // n}%   excluding deferred {tot['done']}/{sched} = {100 * tot['done'] // sched}%")
    print("open tasks and what they wait on:")
    for i, d in sorted(t.items()):
        if d["status"] in ("planned", "blocked"):
            wait = [(x, t[x]["status"]) for x in ids(d["depends_on"]) if x in t and t[x]["status"] != "done"]
            print(f"  {i} {d['status']:8} waits {wait} decisions {d.get('decision_blockers')}")


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("status", "stuck"):
        p = sub.add_parser(name)
        p.add_argument("--log", default="")
    p = sub.add_parser("metrics")
    p.add_argument("--hours", type=float, default=4)
    sub.add_parser("progress")
    args = ap.parse_args()
    {"status": cmd_status, "stuck": cmd_stuck, "metrics": cmd_metrics, "progress": cmd_progress}[args.cmd](args)
