#!/usr/bin/env python3
"""PostgreSQL store mutant gate: 53 product/helper mutants, three-tier kill check.

usage: gate.py list
       gate.py apply <name> <src-tree> <dst-tree>     (copy + mutate only)
       gate.py run <name> [<name> ...]                 (full gate, writes results.tsv)

Source tree = a clean `git archive` export of the candidate commit (SRC env var). Run
from a directory that can hold the copies (MUT_WORK); on macOS export TMPDIR to a
short path so unix-socket tests stay under the path limit.
Tier 0: pgtest database-free tests.  Tier 1: targeted component run (-run).
Tier 2: the full unfiltered component run (TestMain gates included).
INVALID = pattern did not apply or the mutant does not build; never skipped.
"""
import os, re, shutil, subprocess, sys, tempfile, time, warnings

warnings.filterwarnings("ignore", category=SyntaxWarning)  # definitions.py keeps regex escapes verbatim

HERE = os.path.dirname(os.path.abspath(__file__))
WORK = os.environ.get("MUT_WORK", os.path.join(tempfile.gettempdir(), "kodama-mut"))
SRC = os.environ.get("SRC", os.path.join(tempfile.gettempdir(), "kodama-mut-src"))
RESULTS = os.environ.get("MUT_RESULTS", os.path.join(WORK, "results.tsv"))
ENV = dict(os.environ)

U = "internal/shell/store/units/"
P = "internal/shell/store/postgres/"
S = "test/support/infrastructure/postgres/"
M = {}


def add(name, file, pattern, repl, k=1):
    M.setdefault(name, []).append((file, pattern, repl, k))


# --- the 51 single-edit mutants (verbatim) --------------------------------------
_ns = {}
exec(open(os.path.join(HERE, "definitions.py")).read().split("name=sys.argv[1]")[0], _ns)
for _name, _edit in _ns["M"].items():
    M[_name] = [_edit]

# --- GO_matcherempty lives with the matcher code ---------------------------------
M["GO_matcherempty"] = [(
    S + "matchers.go",
    r'if fmt\.Sprint\(value\) == "" \{\n\t\t\treturn fmt\.Errorf\("value is empty"\)',
    'if false {\n\t\t\treturn fmt.Errorf("value is empty")',
    1,
)]

M["YAML_zero"] = [(
    U + "work/task.go",
    r'"yaml_sha256":      digest\[:\],',
    '"yaml_sha256":      append(make([]byte, 32), digest[:0]...),',
    1,
)]
M["RCH_zero"] = [(
    P + "unit.go",
    r'contentHash, result, completed, execution, snapshot, record\.At',
    'append(make([]byte, 32), contentHash[:0]...), result, completed, execution, snapshot, record.At',
    1,
)]

# --- the two unmasking probes ----------------------------------------------------
DROP_UNIQUE = (
    P + "migrations.go",
    r'(\tfor _, table := range tables \{\n\t\tfor _, unique := range table\.Unique \{\n)(\t\t\tnulls := "")',
    '\\1\t\t\tif table.Name == "memberships" && unique.Name == "memberships_scope_principal" {\n\t\t\t\tcontinue\n\t\t\t}\n\\2',
    1,
)
M["C1"] = [M["ABS_nolock"][0], DROP_UNIQUE]  # no advisory lock AND no unique index
M["C2"] = [DROP_UNIQUE]                          # unique index only

# --- tier 1 targeted regexes -----------------------------------------------------
FAM = {
    "task": r"TestRealTaskLifecycle|TestTaskCreateAndEdits|TestObservedTask|TestObservedPipeline|TestTaskVersionLosers|TestPipelinePushTriggers|TestExecutionPrincipalMembership",
    "exec": r"TestRequestActivation|TestExecutionSettlement|TestEngineAdapter|TestEngineCommit|TestObservedResolve|TestRequestResolve|TestOutputSubmission",
    "identity": r"TestRealCredentialAdmin|TestRealTenancyAdmin|TestResourceRegister|TestCredentialInstall|TestIdentityPrincipalDecode",
    "platform": r"TestRealVerifierAndReporter|TestVerificationRecord|TestRealEventIngress|TestTriggersAdapter|TestTriggerStarted",
    "membership": r"TestMembership",
    "gateway": r"TestGatewayUsageAndContentRows",
    "store": r"TestUnitRollbackAtomicity|TestReceipt|TestGoldenReceiptRows|TestGoldenEventAndAuditRows|TestGoldenCleanupRows",
}
ALL = "|".join(FAM.values())
T1 = {
    "PRG_done": FAM["task"], "RES_task": FAM["task"] + "|" + FAM["identity"], "FAIL_json": FAM["exec"],
    "SNAP_exec": FAM["task"] + "|" + FAM["exec"], "SNAP_task": FAM["task"], "NONCE_zero": FAM["identity"],
    "EV_type": ALL, "EV_subject": ALL, "EV_data": ALL, "EV_src": ALL,
    "RC_caller": ALL, "RC_op": ALL, "RC_created": ALL, "AU_detail": ALL, "AU_actor": ALL, "REPLAY_short": ALL,
    "EXTRA_upd": ALL, "MEM_ver": FAM["identity"] + "|" + FAM["membership"], "PRJ_ver": FAM["identity"],
    "EXE_ver": FAM["task"], "CLN_ws": FAM["exec"] + "|" + FAM["task"], "MSG_author": FAM["task"],
    "GW_model": FAM["gateway"], "GW_retain": FAM["gateway"], "GW_at": FAM["gateway"],
    "BD_audit": FAM["identity"], "TRG_sel": FAM["platform"], "TA3": FAM["task"], "TE4": FAM["task"],
    "TNE1": FAM["identity"], "VRE1": FAM["platform"], "WS_task": FAM["task"],
    "WS_exec": FAM["exec"] + "|" + FAM["task"], "YAML_zero": FAM["task"] + "|" + FAM["exec"],
    "CLI_intent": FAM["exec"] + "|" + FAM["task"], "RCH_zero": ALL,
    "ABS_nolock": FAM["membership"], "ABS_noguard": FAM["membership"], "ABS_rawmap": FAM["membership"],
    "C1": FAM["membership"], "C2": r"TestFrozenSchemaFacts|TestMigration|" + FAM["membership"],
    "OB_aftereqbefore": FAM["task"],
    "OB_skiptable": FAM["task"] + "|" + FAM["exec"],
}
TIER0_ONLY = {n for n in M if n.startswith("OB_") and n not in ("OB_aftereqbefore", "OB_skiptable")} | {"GO_omitcol", "GO_nullcmp", "GO_matcherempty"}
ACCEPTED = {"ABS_rawmap": "equivalent: the I01c driver maps 23505 to version_conflict before the store sees it (internal/shell/app/drivers/postgres/postgres.go:445)"}


def apply_edits(name, tree):
    for file, pattern, repl, k in M[name]:
        path = os.path.join(tree, file)
        text = open(path).read()
        matches = list(re.finditer(pattern, text))
        if len(matches) < k:
            raise SystemExit("INVALID %s: pattern %r matched %d (< %d) in %s" % (name, pattern[:70], len(matches), k, file))
        m = matches[k - 1]
        repl = repl.replace("\\&", "&")
        out = m.expand(repl) if ("\\1" in repl or "\\2" in repl) else repl
        text = text[: m.start()] + out + text[m.end():]
        if name == "OB_globalasserted":
            text += "\nvar sharedAsserted = map[string]bool{}\n"
        open(path, "w").write(text)


def sh(cmd, cwd, log, timeout):
    with open(log, "a") as fh:
        fh.write("\n$ %s\n" % cmd)
        fh.flush()
        try:
            p = subprocess.run(cmd, shell=True, cwd=cwd, env=ENV, stdout=fh, stderr=subprocess.STDOUT, timeout=timeout)
            return p.returncode
        except subprocess.TimeoutExpired:
            fh.write("\nTIMEOUT after %ss\n" % timeout)
            return 124


def hung(tail, rc=0):
    return "test timed out" in tail or rc == 124


def killed(log):
    text = open(log).read()
    return "--- FAIL" in text or "unit×state" in text and "coverage" in text or "golden coverage" in text or "unit×state matrix" in text


def run(name):
    start = time.time()
    tree = os.path.join(WORK, name)
    log = os.path.join(WORK, name + ".log")
    for p in (tree, log):
        shutil.rmtree(p, ignore_errors=True) if os.path.isdir(p) else (os.path.exists(p) and os.remove(p))
    os.makedirs(WORK, exist_ok=True)
    subprocess.check_call(["cp", "-cR", SRC, tree])
    try:
        apply_edits(name, tree)
    except SystemExit as exc:
        return name, "-", "INVALID", str(exc), time.time() - start
    if sh("go build ./... && go vet -tags=component ./test/component/postgres ./test/support/infrastructure/postgres", tree, log, 1200) != 0:
        return name, "-", "INVALID", "mutant does not build/vet", time.time() - start
    sh("go test -count=1 ./test/support/infrastructure/postgres/", tree, log, 900)
    tail = open(log).read().split("$ go test -count=1 ./test/support/infrastructure/postgres/")[-1]
    if "--- FAIL" in tail or "\nFAIL" in tail:
        who = re.findall(r"--- FAIL: (\S+)", tail)
        return name, "0", "KILLED", ",".join(who[:3]) or "pgtest build/test failure", time.time() - start
    if name in TIER0_ONLY:
        return name, "0", "SURVIVED", "pgtest unit tests passed (tier-0 mutant)", time.time() - start
    if name in T1:
        marker = "\n#TIER1\n"
        open(log, "a").write(marker)
        sh("go test -tags=component -count=1 -timeout 12m -run '%s' ./test/component/postgres" % T1[name], tree, log, 840)
        tail = open(log).read().split(marker)[-1]
        if hung(tail):
            return name, "1", "HUNG", "suite timed out instead of failing", time.time() - start
        if "--- FAIL" in tail or "\nFAIL" in tail:
            who = re.findall(r"--- FAIL: (\S+)", tail)
            return name, "1", "KILLED", ",".join(who[:3]) or "component failure", time.time() - start
    marker = "\n#TIER2\n"
    open(log, "a").write(marker)
    rc = sh("go test -tags=component -count=1 -timeout 20m ./test/component/postgres", tree, log, 1260)
    tail = open(log).read().split(marker)[-1]
    if hung(tail, rc):
        return name, "2", "HUNG", "suite timed out instead of failing", time.time() - start
    if rc != 0 or "--- FAIL" in tail or "\nFAIL" in tail:
        who = re.findall(r"--- FAIL: (\S+)", tail)
        return name, "2", "KILLED", ",".join(who[:3]) or "gate/failure (exit %d)" % rc, time.time() - start
    return name, "2", "SURVIVED", "full unfiltered component run passed", time.time() - start


def main():
    cmd = sys.argv[1]
    if cmd == "list":
        print(len(M)); print(" ".join(M)); return
    if cmd == "apply":
        name, src, dst = sys.argv[2:5]
        shutil.rmtree(dst, ignore_errors=True)
        subprocess.check_call(["cp", "-cR", src, dst])
        apply_edits(name, dst)
        print("ok", name); return
    if cmd == "run":
        for name in sys.argv[2:]:
            row = run(name)
            line = "%s\t%s\t%s\t%s\t%.0f" % row
            with open(RESULTS, "a") as fh:
                fh.write(line + "\n")
            print(line, flush=True)
            shutil.rmtree(os.path.join(WORK, name), ignore_errors=True)


if __name__ == "__main__":
    main()
