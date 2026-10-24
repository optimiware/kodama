package gatewaytest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
)

// Test_memstore_inspection_views_are_copies: the inspection methods of MemStore
// (the outbox and the audit log) hand out copies, so a test cannot edit history
// through them.
func Test_memstore_inspection_views_are_copies(t *testing.T) {
	u := gatewaytest.NewUnit(t, gatewaytest.UnitOptions{})
	u.Configure(gatewaytest.ScopeA, nil)
	a := u.NewAttempt(gatewaytest.ScopeA)
	u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 10, 5, "ok"))
	if _, err := u.Invoke(a, gatewaytest.Body("gpt-a", "hi", 32), &gatewaytest.Sink{}); err != nil {
		t.Fatal(err)
	}
	tenant := gatewaytest.ScopeA.TenantID

	evs := u.Store.Events(tenant)
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	want := string(evs[0].Data)
	for i := range evs[0].Data {
		evs[0].Data[i] = 'x'
	}
	if got := string(u.Store.Events(tenant)[0].Data); got != want {
		t.Fatalf("an event payload changed through a returned view: %s", got)
	}

	audits := u.Store.Audits(tenant)
	var settings gatewaytest.AuditEntry
	for _, e := range audits {
		if e.Action == "model_settings.update" {
			settings = e
		}
	}
	models, ok := settings.Metadata["models"].([]string)
	if !ok || len(models) != 2 {
		t.Fatalf("audit metadata: %+v", settings.Metadata)
	}
	models[0] = "gpt-evil"
	settings.Metadata["credentialRef"] = "tampered"
	for _, e := range u.Store.Audits(tenant) {
		if e.Action != "model_settings.update" {
			continue
		}
		if e.Metadata["models"].([]string)[0] != "gpt-a" || e.Metadata["credentialRef"] != gatewaytest.Secret.Ref {
			t.Fatalf("audit history changed through a returned view: %+v", e.Metadata)
		}
	}
}
