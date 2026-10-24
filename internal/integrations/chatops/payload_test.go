package chatops_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func links(t testing.TB) chatops.LinkBuilder {
	t.Helper()
	lb, err := chatops.NewLinkBuilder("https://api.t041.example")
	if err != nil {
		t.Fatal(err)
	}
	return lb
}

// TestPayloadProjection: the documented generic payload for the published event
// fixtures is byte-stable, forwards no arbitrary event data and carries a
// current-record link.
func TestPayloadProjection(t *testing.T) {
	for _, name := range []string{"gate_requested", "deployment_state_changed"} {
		t.Run(name, func(t *testing.T) {
			env, err := eventsv1.Decode(fixture(t, name+".event.json"))
			if err != nil {
				t.Fatal(err)
			}
			p, err := chatops.Project(env, "cdl_fixture", links(t))
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Encode()
			if err != nil {
				t.Fatal(err)
			}
			want := bytes.TrimSpace(fixture(t, name+".payload.json"))
			if !bytes.Equal(got, want) {
				t.Fatalf("payload drifted from the published fixture:\n got: %s\nwant: %s", got, want)
			}
			if strings.Contains(string(got), "internalNote") || strings.Contains(string(got), "must never be forwarded") {
				t.Fatal("free-form event data was forwarded")
			}
			// Receivers decode with the documented members only.
			dec := json.NewDecoder(bytes.NewReader(got))
			dec.DisallowUnknownFields()
			var back chatops.Payload
			if err := dec.Decode(&back); err != nil || back.SchemaVersion != chatops.PayloadSchemaVersion {
				t.Fatalf("round trip: %v %+v", err, back)
			}
		})
	}
}

// TestProjectionOfEveryAllowListedType: every supported type projects with its
// own resource link, and nothing outside the allow-list is projected.
func TestProjectionOfEveryAllowListedType(t *testing.T) {
	sc := domain.Scope{TenantID: "t041_acme", ProjectID: "p041_inventory"}
	cases := map[string]struct {
		env  eventsv1.Envelope
		kind string
	}{}
	data := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	mk := func(typ, resType, id string, d any) eventsv1.Envelope {
		return eventsv1.Envelope{ID: "evt_" + typ, SchemaVersion: 1, Type: typ, OccurredAt: t0, TenantID: sc.TenantID, ProjectID: sc.ProjectID,
			Resource: eventsv1.Resource{Type: resType, ID: id, Version: 2}, Actor: eventsv1.Actor{Type: eventsv1.ActorUser, ID: "usr_1"}, CorrelationID: "req_1", Data: data(d)}
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	rev := int64(9)
	cases[eventsv1.TypeWorkItemTransitioned] = struct {
		env  eventsv1.Envelope
		kind string
	}{mk(eventsv1.TypeWorkItemTransitioned, "work_item", "wi_1", eventsv1.WorkItemTransitioned{From: "ready", To: "active", TransitionID: "tr_1"}), "work-items"}
	cases[eventsv1.TypeGateRequested] = struct {
		env  eventsv1.Envelope
		kind string
	}{mk(eventsv1.TypeGateRequested, "gate", "gate_1", eventsv1.GateRequested{SubjectDigest: digest, ContentRevision: 1, ReviewerScope: "project", Deadline: "2027-01-01T00:00:00Z"}), "gates"}
	cases[eventsv1.TypeGateDecided] = struct {
		env  eventsv1.Envelope
		kind string
	}{mk(eventsv1.TypeGateDecided, "gate", "gate_1", eventsv1.GateDecided{DecidedBy: eventsv1.Actor{Type: eventsv1.ActorUser, ID: "usr_reviewer"}, Decision: "approved", State: "approved", SubjectDigest: digest}), "gates"}
	cases[eventsv1.TypeGateInvalidated] = struct {
		env  eventsv1.Envelope
		kind string
	}{mk(eventsv1.TypeGateInvalidated, "gate", "gate_1", eventsv1.GateInvalidated{SubjectDigest: digest, Reason: "content_changed", SupersedingRevision: &rev}), "gates"}
	cases[eventsv1.TypeRunStateChanged] = struct {
		env  eventsv1.Envelope
		kind string
	}{mk(eventsv1.TypeRunStateChanged, "run", "run_1", eventsv1.RunStateChanged{From: "running", To: "failed", Reason: "step_failed"}), "runs"}
	cases[eventsv1.TypeDeploymentStateChanged] = struct {
		env  eventsv1.Envelope
		kind string
	}{mk(eventsv1.TypeDeploymentStateChanged, "deployment", "dep_1", eventsv1.DeploymentStateChanged{EnvironmentID: "env_1", From: "running", To: "failed", ArtifactIDs: []string{"art_1"}}), "deployments"}

	if len(cases) != len(chatops.SupportedEvents) {
		t.Fatalf("the allow-list has %d types but %d are covered", len(chatops.SupportedEvents), len(cases))
	}
	for typ, c := range cases {
		p, err := chatops.Project(c.env, "cdl_1", links(t))
		if err != nil {
			t.Errorf("%s: %v", typ, err)
			continue
		}
		wantURL := "https://api.t041.example/api/v1/tenants/t041_acme/projects/p041_inventory/" + c.kind + "/" + c.env.Resource.ID
		if p.URL != wantURL || p.Resource.Version != 2 || p.Summary == "" || len(p.Summary) > chatops.MaxSummaryBytes || p.NotificationID != "cdl_1" {
			t.Errorf("%s: %+v", typ, p)
		}
		if _, err := p.Encode(); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
	for _, typ := range []string{eventsv1.TypeUsageRecorded, eventsv1.TypeMembershipChanged, eventsv1.TypeConfigurationChanged, eventsv1.TypeWorkItemUpdated, eventsv1.TypeWorkspaceCheckpoint} {
		env := eventsv1.Envelope{Type: typ, TenantID: sc.TenantID, ProjectID: sc.ProjectID, Resource: eventsv1.Resource{ID: "x1"}}
		if _, err := chatops.Project(env, "cdl_1", links(t)); err == nil {
			t.Errorf("%s was projected", typ)
		}
	}
	// A malformed payload for a supported type is an error, not a partial send.
	bad := cases[eventsv1.TypeGateRequested].env
	bad.Data = json.RawMessage(`[1,2]`)
	if _, err := chatops.Project(bad, "cdl_1", links(t)); err == nil {
		t.Error("a malformed gate payload was projected")
	}
}

// TestPlannerSelection: pure selection by project, enablement and event type.
func TestPlannerSelection(t *testing.T) {
	pl, err := chatops.NewPlanner(links(t), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	mkDest := func(id string, sc domain.Scope, enabled bool, events ...string) chatops.Destination {
		d, err := chatops.NewDestination(sc, chatops.DestinationID(id), chatops.DestinationInput{Name: "n-" + strings.ReplaceAll(id, "_", "-"), Endpoint: "https://hooks.t041.example/" + id, Events: events, Enabled: enabled}, t0)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	env, err := eventsv1.Decode(fixture(t, "gate_requested.event.json"))
	if err != nil {
		t.Fatal(err)
	}
	sc := domain.Scope{TenantID: env.TenantID, ProjectID: env.ProjectID}
	other := domain.Scope{TenantID: env.TenantID, ProjectID: "p_other"}
	got, err := pl.Plan(env, []chatops.Destination{
		mkDest("cdst_1", sc, true, "gate.requested"),
		mkDest("cdst_2", sc, true, "gate.decided"),
		mkDest("cdst_3", sc, false, "gate.requested"),
		mkDest("cdst_4", other, true, "gate.requested"),
		mkDest("cdst_5", sc, true, "gate.requested", "run.state_changed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].DestinationID != "cdst_1" || got[1].DestinationID != "cdst_5" {
		t.Fatalf("selected %v", got)
	}
	if got[0].ID == got[1].ID || got[0].BindingRevision != 1 || !got[0].NextAttemptAt.Equal(t0) {
		t.Fatalf("intents = %+v", got)
	}
	// Each intent's payload names its own delivery as the notification ID.
	for _, d := range got {
		var p chatops.Payload
		if err := json.Unmarshal(d.Payload, &p); err != nil || p.NotificationID != string(d.ID) {
			t.Fatalf("payload %s: %v", d.Payload, err)
		}
	}
	// Tenant-wide events never fan out.
	wide := env
	wide.ProjectID = ""
	if got, err := pl.Plan(wide, []chatops.Destination{mkDest("cdst_1", sc, true, "gate.requested")}); err != nil || len(got) != 0 {
		t.Fatalf("tenant-wide plan = %v %v", got, err)
	}
	if _, err := chatops.NewPlanner(chatops.LinkBuilder{}, nil); err == nil {
		t.Fatal("a planner without an origin was accepted")
	}
}
