package postgres_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
	"github.com/optimiware/kodama/internal/work/dependencies/depstest"
)

// Test_dependency_history_events: link deltas, owner versions, actor and timing
// in history, audit and a schema-valid work_item.updated envelope agree; the
// content revision stays unchanged; a service actor is audited as system. Real
// PostgreSQL. WM-02, WM-08, IT-11, SD-A06.
func Test_dependency_history_events(t *testing.T) {
	st := depstest.NewStack(t)
	st.Principal(depstest.TenantA, "svc_planner", work.ActorService, work.PermRead, work.PermEditSpec)
	dep, pre1, pre2 := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre1"), st.Create(scopeA, work.KindStory, "pre2")
	ctx := context.Background()

	time.Sleep(5 * time.Millisecond)
	added, err := st.Deps.Add(ctx, dependencies.AddCommand{Scope: scopeA, Caller: work.Caller{PrincipalID: depstest.Engineer, RequestID: "req_add"}, Dependent: dep.ID, Prerequisite: pre1.ID, Expected: dep.Version})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	// The second change is made by a service principal and carries no request id.
	_, err = st.Deps.Add(ctx, dependencies.AddCommand{Scope: scopeA, Caller: work.Caller{PrincipalID: "svc_planner"}, Dependent: dep.ID, Prerequisite: pre2.ID, Expected: added.Owner.Version})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	removed, err := st.Deps.Remove(ctx, dependencies.RemoveCommand{Scope: scopeA, Caller: work.Caller{PrincipalID: depstest.Engineer, RequestID: "req_remove"}, Dependent: dep.ID, Prerequisite: pre1.ID, Expected: added.Owner.Version + 1})
	if err != nil {
		t.Fatal(err)
	}

	final := st.Get(dep)
	if final.Version != dep.Version+3 || final.ContentRevision != dep.ContentRevision || final.State != work.StateBacklog || final.Version != removed.Owner.Version {
		t.Fatalf("owner after three changes: %+v (was %+v)", final, dep)
	}

	hist, err := st.Work.History(ctx, scopeA, depstest.Caller(depstest.Reader), dep.ID, work.HistoryQuery{Limit: 10})
	if err != nil || len(hist) != 4 {
		t.Fatalf("history: %v %d", err, len(hist))
	}
	addE, svcE, rmE := hist[1], hist[2], hist[3]
	delta := func(m map[string]any) any {
		inner, ok := m[dependencies.FieldDependencies].(map[string]any)
		if !ok {
			t.Fatalf("no dependencies delta in %+v", m)
		}
		return inner["prerequisiteId"]
	}
	if addE.Kind != work.ChangeUpdated || addE.Version != dep.Version+1 || addE.ContentRevision != dep.ContentRevision || addE.ContentChanged() ||
		addE.Actor.ID != depstest.Engineer || addE.Actor.Type != work.ActorUser || addE.RequestID != "req_add" ||
		len(addE.ChangedFields) != 1 || addE.ChangedFields[0] != dependencies.FieldDependencies ||
		delta(addE.Before) != nil || delta(addE.After) != string(pre1.ID) || addE.From != work.StateBacklog || addE.To != work.StateBacklog {
		t.Fatalf("add entry: %+v", addE)
	}
	if svcE.Actor.ID != "svc_planner" || svcE.Actor.Type != work.ActorService || svcE.RequestID != "" || delta(svcE.After) != string(pre2.ID) {
		t.Fatalf("service entry: %+v", svcE)
	}
	if rmE.Version != final.Version || rmE.RequestID != "req_remove" || delta(rmE.Before) != string(pre1.ID) || delta(rmE.After) != nil || rmE.ContentChanged() {
		t.Fatalf("remove entry: %+v", rmE)
	}
	if !addE.OccurredAt.After(hist[0].OccurredAt) || !svcE.OccurredAt.After(addE.OccurredAt) || !rmE.OccurredAt.After(svcE.OccurredAt) {
		t.Fatalf("timestamps do not advance: %v %v %v %v", hist[0].OccurredAt, addE.OccurredAt, svcE.OccurredAt, rmE.OccurredAt)
	}
	if !final.UpdatedAt.Equal(rmE.OccurredAt) {
		t.Fatalf("updated_at %v != last history entry %v", final.UpdatedAt, rmE.OccurredAt)
	}

	tenant := string(depstest.TenantA)
	// Audit agrees with history on actor, action, version and timing, and holds no content.
	for _, e := range []work.HistoryEntry{addE, svcE, rmE} {
		var action, actorType, actorID, meta string
		var version int64
		var at time.Time
		err := st.Owner.QueryRow(ctx, `SELECT action, actor_type, actor_id, resource_version, occurred_at, metadata::text FROM audit
			WHERE tenant_id = $1 AND resource_id = $2 AND resource_version = $3`, tenant, string(dep.ID), int64(e.Version)).Scan(&action, &actorType, &actorID, &version, &at, &meta)
		if err != nil {
			t.Fatalf("audit of version %d: %v", e.Version, err)
		}
		wantActor := string(e.Actor.Type)
		if e.Actor.Type == work.ActorService {
			wantActor = "system"
		}
		if action != "work_item.updated" || actorType != wantActor || actorID != e.Actor.ID || !at.Equal(e.OccurredAt) ||
			!strings.Contains(meta, dependencies.FieldDependencies) || strings.Contains(meta, "Do it") {
			t.Fatalf("audit of version %d: %s %s %s %v %s", e.Version, action, actorType, actorID, at, meta)
		}
	}

	// Events: schema-valid work_item.updated envelopes naming the field only.
	for _, e := range []work.HistoryEntry{addE, svcE, rmE} {
		var raw []byte
		if err := scanTenant(t, st, tenant, `SELECT payload FROM outbox WHERE tenant_id = $1 AND event_id = $2`, []any{tenant, "evt_" + e.ID}, &raw); err != nil {
			t.Fatalf("outbox event of version %d: %v", e.Version, err)
		}
		var env eventsv1.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("envelope: %v", err)
		}
		if err := env.Validate(); err != nil {
			t.Fatalf("the envelope is not schema-valid: %v\n%s", err, raw)
		}
		if env.Type != eventsv1.TypeWorkItemUpdated || env.Resource.Type != "work_item" || env.Resource.ID != string(dep.ID) ||
			env.Resource.Version != int64(e.Version) || env.Actor.ID != e.Actor.ID || string(env.Actor.Type) != string(e.Actor.Type) ||
			string(env.TenantID) != tenant || string(env.ProjectID) != string(scopeA.ProjectID) || !env.OccurredAt.Equal(e.OccurredAt) {
			t.Fatalf("envelope: %+v", env)
		}
		wantCorrelation := e.RequestID
		if wantCorrelation == "" {
			wantCorrelation = e.ID
		}
		if env.CorrelationID != wantCorrelation {
			t.Fatalf("correlation %q, want %q", env.CorrelationID, wantCorrelation)
		}
		var data eventsv1.WorkItemUpdated
		if err := json.Unmarshal(env.Data, &data); err != nil || len(data.ChangedFields) != 1 || data.ChangedFields[0] != "dependencies" || data.ContentRevision != nil {
			t.Fatalf("event data %s: %v", env.Data, err)
		}
		if strings.Contains(string(env.Data), string(pre1.ID)) || strings.Contains(string(env.Data), string(pre2.ID)) {
			t.Fatalf("the event copies graph content: %s", env.Data)
		}
	}

	// One history row, one audit row and one event per accepted change; the
	// rejected and no-op operations added none.
	h, a, o := st.Records(dep)
	if h != 4 || a != 4 || o != 4 {
		t.Fatalf("records history=%d audit=%d outbox=%d, want 4 each", h, a, o)
	}

	// The prerequisites are untouched by links pointing at them.
	for _, p := range []work.Item{pre1, pre2} {
		if got := st.Get(p); got.Version != p.Version || got.UpdatedAt != p.UpdatedAt {
			t.Fatalf("prerequisite %s changed: %+v", p.ID, got)
		}
	}

	// A link change is an edit: the first content edit still starts the usual revision count.
	edited, err := st.Work.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: depstest.Caller(depstest.Engineer), ID: dep.ID, Expected: final.Version, Patch: work.Patch{Spec: strPtr("# Plan\nChanged.")}})
	if err != nil || edited.ContentRevision != dep.ContentRevision+1 || edited.Version != final.Version+1 {
		t.Fatalf("content edit after link changes: %+v %v", edited, err)
	}
}

func strPtr(s string) *string { return &s }
