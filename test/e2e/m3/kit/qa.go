package kit

import (
	"context"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// The deterministic exploratory-QA attempt: a container step in the tools image
// that holds an environment (so the admission pins its configuration), reads
// the deployment it inspects from its pinned inputs, registers a browser trace as a
// declared file output through the runtime API with its own token, and returns
// the structured report (internal/qa/schema.json) as its declared output. It is
// not a browser and not an AI agent; it stands where the browser profile will
// (T-069) and exercises the same control-plane path: pinned context, declared
// output, usable evidence artifacts and the mapping that follows.
//
// The evidence is a trace, not a screenshot: the entrypoint types an output file
// by its extension (".png" is application/octet-stream) and the mapping demands
// an image media type of a screenshot, so a container step cannot produce
// screenshot evidence; that belongs to the browser profile's wrapper (T-069).
// The trace is a declared file output. The entrypoint uploads declared
// outputs only after the command ends, so a report could not cite their
// identifiers; the program therefore reserves the same declared output through
// the runtime API first (an identical repeat of a reservation is a replay, see
// internal/artifacts/reserve.go), cites the identifier it was given, and the
// entrypoint's own upload and completion converge on that artifact. The
// accepted result then names it, which is what the mapping requires of
// evidence.
//
// QA_MODE selects the report: "clean" (no observations) or "defect" (one finding
// with the trace as its evidence). The program travels as one step
// argument, so it contains no tab.
const qaScript = `set -eu
RT=${KODAMA_SERVER_URL%/}
TOKEN=$(cat /kodama/identity/token)
CA=/kodama/bootstrap/ca.crt
ATT=$KODAMA_ATTEMPT_ID
mkdir -p /kodama/outputs /tmp/qa
jq -cn --arg d "$DEPLOYMENT_ID" '{format:"t044/browser-trace",deployment:$d,steps:[{goto:"/inventory/gaps"},{select:"warehouse=W2"},{read:"adjustment-total"}]}' > /kodama/outputs/trace.json
DG=sha256:$(sha256sum /kodama/outputs/trace.json | cut -d' ' -f1)
SZ=$(wc -c < /kodama/outputs/trace.json | tr -d ' ')
echo "qa: inspecting deployment $DEPLOYMENT_ID in environment $ENVIRONMENT_ID"
rc=$(curl -sS --cacert "$CA" -o /tmp/qa/reserve.json -w '%{http_code}' -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' --data '{"name":"trace","kind":"file","role":"output","digest":"'$DG'","mediaType":"application/json","sizeBytes":'$SZ',"registry":null}' "$RT/runtime/v1/attempts/$ATT/artifacts")
if [ "$rc" != 200 ] && [ "$rc" != 201 ]; then echo "qa: reserve answered $rc"; cat /tmp/qa/reserve.json; exit 1; fi
EID=$(jq -r .artifactId /tmp/qa/reserve.json)
echo "qa: trace artifact $EID"
OBS='[]'
if [ "${QA_MODE:-clean}" = defect ]; then
  OBS=$(jq -cn --arg e "$EID" --arg d "$DG" '[{title:"Adjustment totals ignore the warehouse filter",severity:"major",criterionId:"AC-1",location:"/inventory/gaps",steps:["open the inventory gap list","filter by warehouse W2","read the adjustment total"],expected:"the total counts only warehouse W2",actual:"the total counts every warehouse",evidence:[{artifactId:$e,digest:$d,kind:"trace",note:null}]}]')
fi
jq -cn --arg w "$WORK_ITEM_ID" --argjson r "$CONTENT_REVISION" --arg d "$DEPLOYMENT_ID" --arg env "$ENVIRONMENT_ID" --arg c "$CONFIG_DIGEST" --arg k "$CANDIDATE_COMMIT" --arg i "$IMAGE_DIGEST" --argjson obs "$OBS" '{format:"kodama/qa-report/v1",subject:{workItemId:$w,contentRevision:$r},deployment:{id:$d,environmentId:$env,configDigest:$c,candidateCommit:$k,imageDigest:$i},summary:"exploratory pass over the staged inventory gap resolution",observations:$obs}' > /kodama/outputs/qa.json
echo "qa: report written"
`

// e2eScript is the candidate's own end-to-end check (ci/e2e.sh): it identifies
// the deployment and the image digest it inspected and writes its report.
const e2eScript = `#!/bin/sh
set -eu
test -n "$DEPLOYMENT_ID"
case "$IMAGE_DIGEST" in sha256:*) ;; *) echo "the image is not pinned by digest" >&2; exit 1;; esac
mkdir -p /kodama/outputs
jq -cn --arg d "$DEPLOYMENT_ID" --arg i "$IMAGE_DIGEST" '{passed:true,deployment:$d,image:$i,checks:["stock-adjustment-resolves","audit-record-written","erp-write-is-idempotent"]}' > /kodama/outputs/e2e.json
`

// RecordQA is the control plane mapping the QA attempt's report to defects: the
// call the QA trigger (T-069) will issue after the attempt's accepted result.
// Its caller is the trusted control plane; the human reviewer, the QA
// attempt's own identity and the mapping principal itself are refused by the
// service.
func (e *Env) RecordQA(attempt string) (qa.Result, error) {
	return e.RecordQAAs(attempt, qa.Actor{Type: qa.ActorSystem, ID: "control-plane"})
}

// RecordQAAs is RecordQA with another caller.
func (e *Env) RecordQAAs(attempt string, actor qa.Actor) (qa.Result, error) {
	e.T.Helper()
	svc := e.Loop().QA
	if svc == nil {
		e.T.Fatal("the QA mapping is not configured")
	}
	return svc.Record(context.Background(), qa.RecordCommand{
		Scope:   domain.Scope{TenantID: domain.TenantID(m2.TenantA), ProjectID: domain.ProjectID(e.World.Project)},
		Attempt: domain.AttemptID(attempt), Caller: qa.Caller{Actor: actor, RequestID: "t044-record"},
	})
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
