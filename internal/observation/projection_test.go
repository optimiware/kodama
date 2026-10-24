package observation

import (
	"encoding/json"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func domainRun(s string) domain.RunID { return domain.RunID(s) }

func env(typ, resType, resID, tenant, project string, data any) eventsv1.Envelope {
	raw, _ := json.Marshal(data)
	return eventsv1.Envelope{
		ID: "evt_1", SchemaVersion: 1, Type: typ, TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project),
		Resource: eventsv1.Resource{Type: resType, ID: resID, Version: 1}, Data: raw,
	}
}

func TestProjectionMatching(t *testing.T) {
	project := mustScope(t, "")
	run := mustScope(t, "run_1")
	work := env(eventsv1.TypeWorkItemCreated, "work_item", "wi_1", "t_acme", "p_inventory", eventsv1.WorkItemCreated{Kind: "story", State: "backlog"})
	runEv := func(id string) eventsv1.Envelope {
		return env(eventsv1.TypeRunStateChanged, "run", id, "t_acme", "p_inventory", eventsv1.RunStateChanged{})
	}
	step := func(runID string) eventsv1.Envelope {
		return env(eventsv1.TypeStepAttemptStateChange, "step_attempt", "att_1", "t_acme", "p_inventory",
			eventsv1.StepAttemptStateChanged{RunID: runID, StepID: "build"})
	}
	gate := env(eventsv1.TypeGateRequested, "gate", "g_1", "t_acme", "p_inventory", eventsv1.GateRequested{})

	cases := []struct {
		name  string
		scope Scope
		ev    eventsv1.Envelope
		want  Family
		ok    bool
	}{
		{"project carries work", project, work, FamilyWork, true},
		{"project carries runs", project, runEv("run_2"), FamilyRun, true},
		{"project carries attempts", project, step("run_2"), FamilyStepAttempt, true},
		{"unsupported types are never forwarded", project, gate, "", false},
		{"run stream excludes work", run, work, "", false},
		{"run stream carries its run", run, runEv("run_1"), FamilyRun, true},
		{"run stream excludes other runs", run, runEv("run_2"), "", false},
		{"run stream matches attempts by typed runId", run, step("run_1"), FamilyStepAttempt, true},
		{"run stream excludes other runs' attempts", run, step("run_2"), "", false},
		{"foreign tenant is excluded", project, env(eventsv1.TypeWorkItemCreated, "work_item", "wi_1", "t_other", "p_inventory", nil), "", false},
		{"foreign project is excluded", project, env(eventsv1.TypeWorkItemCreated, "work_item", "wi_1", "t_acme", "p_other", nil), "", false},
		{"type filter", mustScope(t, "", "run.state_changed"), work, "", false},
		{"type filter keeps a match", mustScope(t, "", "run.state_changed"), runEv("r"), FamilyRun, true},
	}
	for _, c := range cases {
		got, ok := c.scope.Match(c.ev)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}

	t.Run("a step attempt with an undecodable payload is excluded", func(t *testing.T) {
		bad := step("run_1")
		bad.Data = json.RawMessage(`{"runId": 7}`)
		if _, ok := run.Match(bad); ok {
			t.Fatal("matched an attempt without a typed runId")
		}
	})
	t.Run("a correlation id never selects a run", func(t *testing.T) {
		ev := step("run_2")
		ev.CorrelationID = "run_1"
		ev.CausationID = new(string)
		*ev.CausationID = "run_1"
		if _, ok := run.Match(ev); ok {
			t.Fatal("matched through the correlation or causation id")
		}
	})
	t.Run("families", func(t *testing.T) {
		if got := project.Families(); len(got) != 3 {
			t.Fatalf("project families = %v", got)
		}
		if got := run.Families(); len(got) != 2 || got[0] != FamilyRun {
			t.Fatalf("run families = %v", got)
		}
		if got := mustScope(t, "", "work_item.created").Families(); len(got) != 1 || got[0] != FamilyWork {
			t.Fatalf("filtered families = %v", got)
		}
	})
}
