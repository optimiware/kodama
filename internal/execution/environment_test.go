package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

type environmentResolverFunc func(context.Context, EnvironmentRequest) (EnvironmentSnapshot, error)

func (f environmentResolverFunc) ResolveEnvironment(ctx context.Context, req EnvironmentRequest) (EnvironmentSnapshot, error) {
	return f(ctx, req)
}

func environmentDoc(steps ...string) pipeline.Document {
	var doc pipeline.Document
	for _, id := range steps {
		s := container(id, noWorkspace())
		s.Environment = "env_staging"
		doc.Spec.Steps = append(doc.Spec.Steps, s)
	}
	return doc
}

func environmentFixture() EnvironmentSnapshot {
	return EnvironmentSnapshot{
		Scope:   domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"},
		Pin:     runtimev1.EnvironmentPin{ID: "env_staging", ConfigDigest: runtimev1.Digest("sha256:" + hex64("a"))},
		Secrets: []runtimev1.SecretBinding{{Ref: "secret://project/kubeconfig", Version: 1, Name: "KUBECONFIG", Delivery: runtimev1.DeliveryFile}},
	}
}

func Test_environment_snapshot_contract(t *testing.T) {
	fixture := environmentFixture()
	actor := Actor{Type: ActorUser, ID: "u_engineer"}
	calls := 0
	svc := &Service{environments: environmentResolverFunc(func(_ context.Context, req EnvironmentRequest) (EnvironmentSnapshot, error) {
		calls++
		if req.Scope != fixture.Scope || req.Actor != actor || req.EnvironmentID != fixture.Pin.ID {
			t.Fatalf("wrong authority supplied to resolver: %+v", req)
		}
		return fixture, nil
	})}
	snap, err := svc.resolveEnvironments(context.Background(), fixture.Scope, actor, environmentDoc("deploy", "inspect"), Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(snap.Environments) != 2 || len(snap.Secrets) != 2 {
		t.Fatalf("same environment was not resolved once: %d calls, %+v", calls, snap)
	}
	fixture.Secrets[0].Version = 2
	if snap.Secrets[0].Binding.Version != 1 || snap.Secrets[1].Binding.Version != 1 {
		t.Fatal("resolver mutated the admitted credential snapshot")
	}
	for _, step := range []string{"deploy", "inspect"} {
		pin, err := snap.environmentOf(&StepRun{ID: step, Spec: pipeline.Step{Environment: "env_staging"}})
		if err != nil || pin == nil || *pin != fixture.Pin {
			t.Fatalf("missing per-step identity: %+v, %v", pin, err)
		}
	}
}

func Test_environment_snapshot_denials(t *testing.T) {
	fixture := environmentFixture()
	doc := environmentDoc("deploy")
	for name, alter := range map[string]func(*EnvironmentSnapshot){
		"foreign tenant":         func(p *EnvironmentSnapshot) { p.Scope.TenantID = "t_foreign" },
		"foreign project":        func(p *EnvironmentSnapshot) { p.Scope.ProjectID = "p_foreign" },
		"wrong environment":      func(p *EnvironmentSnapshot) { p.Pin.ID = "env_production" },
		"bad digest":             func(p *EnvironmentSnapshot) { p.Pin.ConfigDigest = "latest" },
		"bad credential version": func(p *EnvironmentSnapshot) { p.Secrets[0].Version = 0 },
		"bad credential name":    func(p *EnvironmentSnapshot) { p.Secrets[0].Name = "../credential" },
		"bad delivery":           func(p *EnvironmentSnapshot) { p.Secrets[0].Delivery = "manifest" },
		"duplicate names":        func(p *EnvironmentSnapshot) { p.Secrets = append(p.Secrets, p.Secrets[0]) },
		"aliased credential": func(p *EnvironmentSnapshot) {
			alias := p.Secrets[0]
			alias.Name, alias.Delivery = "TOKEN", runtimev1.DeliveryEnv
			p.Secrets = append(p.Secrets, alias)
		},
	} {
		t.Run(name, func(t *testing.T) {
			pin := environmentFixture()
			alter(&pin)
			svc := &Service{environments: environmentResolverFunc(func(context.Context, EnvironmentRequest) (EnvironmentSnapshot, error) { return pin, nil })}
			if _, err := svc.resolveEnvironments(context.Background(), fixture.Scope, System, doc, Snapshot{}); !errors.Is(err, ErrAdmissionRefused) {
				t.Fatalf("invalid snapshot accepted: %v", err)
			}
		})
	}
	for _, cause := range []error{ErrDenied, ErrHidden, ErrNotFound, errors.New("provider outage")} {
		svc := &Service{environments: environmentResolverFunc(func(context.Context, EnvironmentRequest) (EnvironmentSnapshot, error) {
			return EnvironmentSnapshot{}, cause
		})}
		_, err := svc.resolveEnvironments(context.Background(), fixture.Scope, System, doc, Snapshot{})
		if cause.Error() == "provider outage" {
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("outage not classified: %v", err)
			}
		} else if !errors.Is(err, ErrAdmissionRefused) {
			t.Fatalf("denial not preserved: %v", err)
		}
	}
	if _, err := (&Service{}).resolveEnvironments(context.Background(), fixture.Scope, System, doc, Snapshot{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("missing resolver accepted: %v", err)
	}
	if _, err := NewServiceWithEnvironments(Dependencies{}, Options{}, nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("constructor allowed an absent resolver: %v", err)
	}
}

func Test_environment_credential_conflicts(t *testing.T) {
	fixture := environmentFixture()
	svc := &Service{environments: environmentResolverFunc(func(context.Context, EnvironmentRequest) (EnvironmentSnapshot, error) { return fixture, nil })}
	for _, binding := range []runtimev1.SecretBinding{
		{Ref: "secret://project/kubeconfig", Version: 2, Name: "other", Delivery: runtimev1.DeliveryFile},
		{Ref: "secret://project/another", Version: 1, Name: "KUBECONFIG", Delivery: runtimev1.DeliveryFile},
		{Ref: "secret://project/kubeconfig", Version: 1, Name: "alias", Delivery: runtimev1.DeliveryFile},
		{Ref: "secret://project/kubeconfig", Version: 1, Name: "TOKEN", Delivery: runtimev1.DeliveryEnv},
	} {
		_, err := svc.resolveEnvironments(context.Background(), fixture.Scope, System, environmentDoc("deploy"), Snapshot{Secrets: []StepSecret{{Step: "deploy", Binding: binding}}})
		if !errors.Is(err, ErrAdmissionRefused) {
			t.Fatalf("credential conflict accepted: %v", err)
		}
	}
	snap, err := svc.resolveEnvironments(context.Background(), fixture.Scope, System, environmentDoc("deploy"), Snapshot{Secrets: []StepSecret{{Step: "deploy", Binding: fixture.Secrets[0]}}})
	if err != nil || len(snap.Secrets) != 1 {
		t.Fatalf("an identical explicit binding was not reused: %+v, %v", snap.Secrets, err)
	}
}

func Test_environment_runtime_context(t *testing.T) {
	step := container("deploy", noWorkspace())
	step.Environment = "env_staging"
	agg := newAgg(t, nil, step)
	pin := environmentFixture().Pin
	agg.Run.Snapshot.Environments = []StepEnvironment{{Step: step.ID, Pin: pin}}
	rs, err := agg.Resolve(agg.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	build := func() (runtimev1.Context, error) {
		return BuildContext(agg.Run, agg.Steps[0], "att_env", rs, WorkspaceBinding{Access: AccessNone}, SourceInfo{}, Limit{
			DefaultTimeout: time.Minute, MaxTimeout: time.Hour, MaxOutputBytes: 1024, MaxOutputFiles: 8, MaxDiagnosticBytes: 1024,
		})
	}
	c, err := build()
	if err != nil || c.Environment == nil || *c.Environment != pin {
		t.Fatalf("runtime pin missing: %+v, %v", c.Environment, err)
	}
	if _, err := (DefaultPlanner{}).Plan(c.TenantID, c); err != nil {
		t.Fatalf("pure job planner rejected the pinned context: %v", err)
	}
	c.Environment.ConfigDigest = runtimev1.Digest("sha256:" + hex64("b"))
	if agg.Run.Snapshot.Environments[0].Pin != pin {
		t.Fatal("runtime context aliases the persisted pin")
	}
	agg.Run.Snapshot.Environments = nil
	if _, err := build(); !errors.Is(err, ErrAdmissionRefused) {
		t.Fatalf("legacy environment-declaring run dispatched without a pin: %v", err)
	}
	agg.Run.Snapshot.Environments = []StepEnvironment{{Step: step.ID, Pin: pin}, {Step: step.ID, Pin: pin}}
	if _, err := build(); !errors.Is(err, ErrAdmissionRefused) {
		t.Fatalf("duplicate stored pins accepted: %v", err)
	}
}

func Test_environment_snapshot_identity(t *testing.T) {
	digest := func(s Snapshot) string {
		d, err := Digest("pd_1", "sha256:"+hex64("b"), "wi_1", 1, commitA, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	// This is the pre-environment snapshot document: absence must retain exactly
	// its old identity, not introduce an empty/null environment field into hashes.
	legacy := []byte(`{"definition":"pd_1","definitionDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","workItem":"wi_1","contentRevision":1,"sourceCommit":"1111111111111111111111111111111111111111","inputs":null,"secrets":null}`)
	canonical, err := runtimev1.Canonicalize(legacy)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(canonical)
	if got, want := digest(Snapshot{}), "sha256:"+hex.EncodeToString(hash[:]); got != want {
		t.Fatalf("historical digest changed: %s != %s", got, want)
	}
	snap := Snapshot{Environments: []StepEnvironment{{Step: "one", Pin: environmentFixture().Pin}, {Step: "two", Pin: environmentFixture().Pin}}}
	before := digest(snap)
	snap.Environments[0], snap.Environments[1] = snap.Environments[1], snap.Environments[0]
	if digest(snap) != before {
		t.Fatal("snapshot identity depends on resolver ordering")
	}
	encoded, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Snapshot
	if err := json.Unmarshal(encoded, &loaded); err != nil || digest(loaded) != before {
		t.Fatalf("snapshot JSON round trip changed identity: %v", err)
	}
	loaded.Environments[0].Pin.ConfigDigest = runtimev1.Digest("sha256:" + hex64("c"))
	if digest(loaded) == before {
		t.Fatal("changed environment did not change run identity")
	}
}
