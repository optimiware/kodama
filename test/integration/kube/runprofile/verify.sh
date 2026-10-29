#!/usr/bin/env bash
# Explicit-target wrapper for the O-30 run-resource profile qualification.
#
# It runs the REAL Test_run_resource_profile_qualification (never with
# -tags unit, so the test can neither skip nor be replaced by the offline
# layer) against the target named by the KODAMA_RUNPROFILE_* variables and
# captures go test's JSON outcomes. The test takes the shared integration
# lease itself; this wrapper takes none. It never prints credentials (no
# kubeconfig contents, tokens or other secrets).
#
#   KODAMA_RUNPROFILE_KUBECONFIG     explicit kubeconfig of the target (ambient KUBECONFIG is ignored)
#   KODAMA_RUNPROFILE_TARGET         name recorded in the report
#   KODAMA_RUNPROFILE_STORAGE_CLASS  CSI class for the RWOP run claim
#   KODAMA_RUNPROFILE_REPORT_DIR     where receipts and report.json/report.md are retained
#   FORGE_RESULTS                    optional: file that receives the JSON outcomes
set -euo pipefail

required=(KODAMA_RUNPROFILE_KUBECONFIG KODAMA_RUNPROFILE_TARGET KODAMA_RUNPROFILE_STORAGE_CLASS KODAMA_RUNPROFILE_REPORT_DIR)
missing=()
for name in "${required[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    missing+=("$name")
  fi
done
if ((${#missing[@]} > 0)); then
  echo "verify.sh: refusing to run, missing required input(s): ${missing[*]}" >&2
  echo "verify.sh: the run-resource profile qualification needs an explicit target; the legacy kind scope does not qualify it." >&2
  exit 2
fi
if [[ ! -r "$KODAMA_RUNPROFILE_KUBECONFIG" ]]; then
  echo "verify.sh: refusing to run, the kubeconfig named by KODAMA_RUNPROFILE_KUBECONFIG is not readable" >&2
  exit 2
fi
case "${GOFLAGS:-}" in
  *tags*)
    echo "verify.sh: refusing to run, GOFLAGS sets build tags; the real qualification must not be built with the unit tag" >&2
    exit 2
    ;;
esac

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "$root"

if [[ -n "${FORGE_RESULTS:-}" ]]; then
  results="$FORGE_RESULTS"
  mkdir -p "$(dirname "$results")"
else
  results="$(mktemp "${TMPDIR:-/tmp}/kodama-runprofile-results.XXXXXX")"
fi
echo "verify.sh: target ${KODAMA_RUNPROFILE_TARGET}, JSON outcomes -> ${results}"

# Not -tags unit; the test acquires the integration lease itself.
set +e
go test -count=1 -timeout 90m -json -run Test_run_resource_profile_qualification ./test/integration/kube/runprofile/ >"$results"
status=$?
set -e

name='"Test":"Test_run_resource_profile_qualification"'
if ((status == 0)); then
  # A zero exit is only a pass when the real test reported pass and never skipped.
  if grep -E '"Action":"skip".*'"$name" "$results" >/dev/null 2>&1 || ! grep -E '"Action":"pass".*'"$name" "$results" >/dev/null 2>&1; then
    echo "verify.sh: go test exited 0 but Test_run_resource_profile_qualification did not report a pass (skipped or absent); treating it as a failure" >&2
    status=1
  fi
fi
if ((status == 0)); then
  echo "verify.sh: Test_run_resource_profile_qualification passed"
else
  echo "verify.sh: Test_run_resource_profile_qualification did not pass (exit ${status}); see ${results} and the retained report" >&2
fi
exit "$status"
