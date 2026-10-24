package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
)

// Test_gateway_happy_path is the baseline the other integration tests vary: a
// configured project, an admitted attempt, one streamed request, one usage
// record and one usage.recorded event.
func Test_gateway_happy_path(t *testing.T) {
	e := gatewaytest.New(t)
	scope := executiontest.ScopeA
	e.ModelSecret(scope)
	e.Configure(e.Replica, scope, e.Edit())
	a := e.NewAttempt(scope)

	sink := &gatewaytest.Sink{}
	res, err := e.Invoke(e.Replica, a, gatewaytest.Body("gpt-a", "hello", 64), sink)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	frames, finished, failed := sink.Snapshot()
	if !finished || failed != "" || len(frames) == 0 {
		t.Fatalf("stream: finished=%v failed=%q frames=%d", finished, failed, len(frames))
	}
	if res.State != "succeeded" || res.Usage == nil || res.Usage.Charged != 13 || res.Usage.ActualModel != "gpt-a-2026" {
		t.Fatalf("result %+v usage %+v", res, res.Usage)
	}
	adm, charged := e.Budget(a.RunID)
	if adm != 1 || charged != 13 {
		t.Fatalf("budget %d/%d", adm, charged)
	}
	calls := e.Provider.Calls()
	if len(calls) != 1 || calls[0].Authorization != "Bearer "+gatewaytest.Bearer || calls[0].Path != "/v1/chat/completions" {
		t.Fatalf("provider calls %+v", calls)
	}
	if n := e.Events(scope.TenantID, "usage.recorded"); n != 1 {
		t.Fatalf("usage events %d", n)
	}
	// Operational accounting only: one audit row, and an event carrying
	// attributable token counts with no pricing, payment or entitlement member.
	if n := e.Count(`SELECT count(*) FROM audit WHERE action LIKE 'model.request.%'`); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	var payload string
	err = e.X.RuntimeTx(scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT convert_from(payload, 'UTF8') FROM outbox WHERE tenant_id = $1 AND event_type = 'usage.recorded'`, string(scope.TenantID)).Scan(&payload)
	})
	if err != nil {
		t.Fatalf("usage event payload: %v", err)
	}
	for _, want := range []string{string(a.RunID), string(a.ID), "gpt-a-2026"} {
		if !strings.Contains(payload, want) {
			t.Fatalf("the usage event is not attributable to %q: %s", want, payload)
		}
	}
	for _, banned := range []string{"price", "cost", "currency", "amount", "invoice", "plan", "subscription", "entitlement", "hello"} {
		if strings.Contains(strings.ToLower(payload), banned) {
			t.Fatalf("the usage event carries commercial or content member %q: %s", banned, payload)
		}
	}
	if d := e.Dump(scope.TenantID); strings.Contains(d, gatewaytest.Bearer) || strings.Contains(d, "hello") {
		t.Fatal("a credential or prompt leaked into stored state")
	}
}
