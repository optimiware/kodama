package kit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/optimiware/kodama/api/client"
)

// Gates lists the run's gates as the persona.
func (e *Env) Gates(persona, run string) []client.Gate {
	t := e.T
	t.Helper()
	code, b := e.Do(persona, "GET", e.ProjectPath()+"/gates?runId="+run, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("list gates: HTTP %d: %s", code, b)
	}
	var page client.GatePage
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// WaitGate waits for the run's gate to exist (the gate reconciler requests it
// once the step is ready) and returns it.
func (e *Env) WaitGate(run string, within time.Duration) client.Gate {
	t := e.T
	t.Helper()
	var g client.Gate
	WaitFor(t, "the gate of "+run, within, func() bool {
		gs := e.Gates("reviewer", run)
		if len(gs) == 0 {
			return false
		}
		g = gs[0]
		return true
	})
	return g
}

// GetGate reads one gate.
func (e *Env) GetGate(persona, id string) client.Gate {
	t := e.T
	t.Helper()
	code, b := e.Do(persona, "GET", e.ProjectPath()+"/gates/"+id, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("get gate: HTTP %d: %s", code, b)
	}
	var g client.Gate
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// Decide posts a decision on the gate as the persona with its current version
// and subject digest, and reports the raw answer.
func (e *Env) Decide(persona string, g client.Gate, decision, reason, key string) (int, []byte) {
	return e.DecideAt(persona, g, int64(g.Version), string(g.SubjectDigest), decision, reason, key)
}

// DecideAt is Decide with an explicit version and subject digest.
func (e *Env) DecideAt(persona string, g client.Gate, version int64, digest, decision, reason, key string) (int, []byte) {
	return e.Do(persona, "POST", e.ProjectPath()+"/gates/"+g.Id+"/decisions",
		map[string]any{"decision": decision, "subjectDigest": digest, "reason": reason},
		map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version), "Idempotency-Key": key})
}

// GateEvents lists the gate's committed events (event types) in creation order.
func (e *Env) GateEvents(gate string) []string {
	e.T.Helper()
	return e.TenantStrings(TenantA, `SELECT event_type FROM outbox WHERE event_type LIKE 'gate.%' AND position($1 in convert_from(payload, 'UTF8')) > 0 ORDER BY created_at, event_id`, gate)
}
