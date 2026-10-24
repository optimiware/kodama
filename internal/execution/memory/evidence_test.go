package memory_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
)

// TestUnitEvidenceIsSerialized proves append-only evidence and audit payloads
// cross the store by serialization, as they do into PostgreSQL jsonb: nested
// maps and slices are never shared with the caller's payload or with a read
// result, and an unserializable payload is refused with nothing committed.
func TestUnitEvidenceIsSerialized(t *testing.T) {
	ctx := context.Background()
	m, p := fixture(t)
	run := m.MustAdmit(p, define(m, job("job", readRun)), nil)
	in := m.MustClaim(scopeA.TenantID)

	nested := map[string]any{"verdict": "unknown"}
	list := []any{"a", map[string]any{"k": "v"}}
	payload := map[string]any{"details": nested, "items": list, "count": 2}
	record := func(detail map[string]any) error {
		_, _, err := m.Mem.Update(ctx, scopeA, run.ID, execution.UpdateOptions{}, func(_ context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
			agg.RecordEvidence(in.AttemptID, execution.EvidenceLateObservation, detail, env.Now())
			return execution.Effects{}, nil
		})
		return err
	}
	if err := record(payload); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"details": map[string]any{"verdict": "unknown"},
		"items":   []any{"a", map[string]any{"k": "v"}},
		"count":   float64(2),
	}
	read := func() map[string]any {
		t.Helper()
		rows, err := m.Mem.EvidenceOf(ctx, scopeA, in.AttemptID)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%d evidence rows (%v)", len(rows), err)
		}
		return rows[0].Detail
	}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored %v, want %v", got, want)
	}

	// Changing the original payload after the commit changes nothing stored.
	nested["verdict"], list[0], list[1].(map[string]any)["k"], payload["count"] = "changed", "changed", "changed", 3
	payload["details"].(map[string]any)["extra"] = true
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("changing the original payload rewrote evidence: %v", got)
	}
	// Neither does changing what a read returned.
	got := read()
	got["details"].(map[string]any)["verdict"] = "changed"
	got["items"].([]any)[1].(map[string]any)["k"] = "changed"
	got["items"].([]any)[0] = "changed"
	got["count"] = 0
	if again := read(); !reflect.DeepEqual(again, want) {
		t.Fatalf("changing a read result rewrote evidence: %v", again)
	}

	// An unserializable payload is refused and commits nothing.
	if err := record(map[string]any{"bad": make(chan int)}); !errors.Is(err, execution.ErrInvalid) {
		t.Fatalf("an unserializable payload: %v", err)
	}
	if rows, _ := m.Mem.EvidenceOf(ctx, scopeA, in.AttemptID); len(rows) != 1 {
		t.Fatalf("%d evidence rows after a refused payload", len(rows))
	}

	// Audit metadata is a copy too.
	rows, err := m.Mem.AuditRows(ctx, scopeA)
	if err != nil || len(rows) == 0 {
		t.Fatalf("audit %d %v", len(rows), err)
	}
	rows[0].Metadata["to"] = "changed"
	again, _ := m.Mem.AuditRows(ctx, scopeA)
	if again[0].Metadata["to"] == "changed" {
		t.Fatal("changing a read audit row rewrote the stored metadata")
	}
}
