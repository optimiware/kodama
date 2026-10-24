package m2_test

import (
	"encoding/json"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
)

// TestM2EvidenceProvenance covers DQ-A01/DQ-01 on the real stack: evidence is
// bound into a gate subject only when the artifact's provenance names the
// repository and commit of the candidate. A report produced by a workspace-free
// step (the only kind the Podman runtime launches) is a usable, verified
// artifact of this run with NO source provenance, so the gate is never created:
// the step waits, the build never starts and nothing is allocated. This is the
// fail-closed property that also makes the ERP pipeline's evidence-bearing
// review unreachable on this runtime until it can check out source (finding 3).
func TestM2EvidenceProvenance(t *testing.T) {
	e := kit.New(t, kit.Options{})
	def := e.Register("evidence", kit.AgentInputs(),
		kit.ContainerStep("test", `echo '{"passed":true}' > /kodama/outputs/report`, map[string]any{"report": map[string]any{"type": "artifact", "path": "/kodama/outputs/report"}}),
		kit.GateStep("review", []string{"${{ steps.test.outputs.report }}"}, "test"),
		kit.ContainerStep("build", "echo build", nil, "review"),
	)
	item := e.Item("Evidence without provenance", "# spec")
	run := e.StartRun("evidence-1", item, def.Id, nil)
	test := e.WaitStep(run.Id, "test", client.StepStatusSucceeded, 2*time.Minute)
	id := kit.Output(test, "report")
	if id == "" {
		t.Fatalf("the report output %+v", test.Outputs)
	}
	code, body := e.Do("engineer", "GET", e.ProjectPath()+"/artifacts/"+id, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("artifact: %d %s", code, body)
	}
	var art client.Artifact
	if err := json.Unmarshal(body, &art); err != nil {
		t.Fatal(err)
	}
	if art.State != "usable" || art.RunId != run.Id || art.StepId != "test" || art.Source != nil {
		t.Fatalf("the evidence artifact %+v: want usable, this run's, without source provenance", art)
	}
	time.Sleep(3 * time.Second) // many reconcile passes
	if gates := e.Gates("reviewer", run.Id); len(gates) != 0 {
		t.Fatalf("a gate was created over evidence without source provenance: %+v", gates)
	}
	if st := e.StepOf(run.Id, "review"); st.Status != client.StepStatusPending || len(st.Attempts) != 0 {
		t.Fatalf("review %+v", st)
	}
	if st := e.StepOf(run.Id, "build"); st.Status != client.StepStatusPending || len(st.Attempts) != 0 {
		t.Fatalf("build %+v", st)
	}
	if n := e.GetRunStatus(run.Id); n != string(client.RunStatusRunning) {
		t.Fatalf("run %s", n)
	}
}
