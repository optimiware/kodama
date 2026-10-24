package delivery_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
)

// ---- doubles: no service is used; every peer is a recording double ----------

type fakeStore struct {
	delivery.Store // any method not overridden panics: it must not be reached
	snapshot       delivery.ConfigurationSnapshot
	deployment     *delivery.Deployment
	envReads       int
	writes         int
}

func (s *fakeStore) GetEnvironment(context.Context, domain.Scope, domain.EnvironmentID) (delivery.Environment, error) {
	s.envReads++
	return delivery.Environment{}, errors.New("the environment's current settings were consulted")
}

func (s *fakeStore) GetSnapshot(_ context.Context, sc domain.Scope, env domain.EnvironmentID, digest runtimev1.Digest) (delivery.ConfigurationSnapshot, error) {
	if sc == s.snapshot.Scope && env == s.snapshot.Environment && digest == s.snapshot.Digest {
		return s.snapshot, nil
	}
	return delivery.ConfigurationSnapshot{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}

func (s *fakeStore) GetDeployment(context.Context, domain.Scope, delivery.DeploymentID) (delivery.Deployment, error) {
	if s.deployment == nil {
		return delivery.Deployment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return *s.deployment, nil
}

func (s *fakeStore) FindByAction(ctx context.Context, sc domain.Scope, _ string) (delivery.Deployment, error) {
	return s.GetDeployment(ctx, sc, "")
}

func (s *fakeStore) Register(context.Context, delivery.RegisterCommand) (delivery.RegisterResult, error) {
	s.writes++
	return delivery.RegisterResult{}, errors.New("a protected callback reached the store")
}

func (s *fakeStore) Update(context.Context, domain.Scope, delivery.DeploymentID, delivery.Mutation, func(*delivery.Deployment) (delivery.Transition, error)) (delivery.Deployment, delivery.Transition, error) {
	s.writes++
	return delivery.Deployment{}, delivery.Transition{}, errors.New("a protected callback reached the store")
}

type fakeAuthority struct{ p delivery.Principal }

func (a fakeAuthority) Authenticate(_ context.Context, attempt domain.AttemptID, _ string) (delivery.Principal, error) {
	if attempt != a.p.Attempt {
		return delivery.Principal{}, delivery.Fail(delivery.ErrUnauthenticated, nil, "no")
	}
	return a.p, nil
}
func (a fakeAuthority) Bind(ctx context.Context, _ delivery.Principal) (context.Context, error) {
	return ctx, nil
}

type fakeContexts struct {
	c   runtimev1.Context
	err error
}

func (f fakeContexts) Pinned(context.Context, domain.Scope, domain.AttemptID) (runtimev1.Context, error) {
	return f.c, f.err
}

type countingPeers struct{ calls int }

func (p *countingPeers) Evidence(context.Context, domain.Scope, domain.ArtifactID) (delivery.ImageEvidence, error) {
	p.calls++
	return delivery.ImageEvidence{}, errors.New("provenance was consulted")
}
func (p *countingPeers) Protect(context.Context, domain.Scope, domain.ArtifactID, delivery.DeploymentID) error {
	p.calls++
	return errors.New("protect was reached")
}
func (p *countingPeers) Release(context.Context, domain.Scope, domain.ArtifactID, delivery.DeploymentID) error {
	return nil
}
func (p *countingPeers) Produced(context.Context, domain.Scope, domain.AttemptID, domain.ArtifactID) (delivery.ProducedOutput, error) {
	p.calls++
	return delivery.ProducedOutput{}, errors.New("provenance was consulted")
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, delivery.Actor, domain.Scope, delivery.Permission) error {
	return nil
}

type noCredentials struct{ t *testing.T }

func (c noCredentials) Current(context.Context, domain.Scope, string) (delivery.CredentialInfo, error) {
	c.t.Error("current credential versions were read by a callback")
	return delivery.CredentialInfo{}, errors.New("unexpected")
}

type fixture struct {
	t         *testing.T
	store     *fakeStore
	peers     *countingPeers
	snapshot  delivery.ConfigurationSnapshot
	pinned    runtimev1.Context
	principal delivery.Principal
	dep       delivery.Deployment
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	snap, err := delivery.NewSnapshot(newEnv(t), map[string]int64{"secret://project/kubeconfig": 3, "secret://project/token": 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.ConfigDigest, reg.Environment, reg.Namespace = snap.Digest, snap.Environment, snap.Namespace
	dep, err := delivery.NewDeployment(reg, now)
	if err != nil {
		t.Fatal(err)
	}
	pin := snap.Pin()
	c := runtimev1.Context{
		TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: reg.Run, StepID: reg.Step, AttemptID: reg.Attempt, ActionKey: reg.ActionKey,
		Inputs: map[string]runtimev1.Value{"image": runtimev1.StringValue(string(reg.Image.ArtifactID))}, Environment: &pin,
		Secrets: append([]runtimev1.SecretBinding(nil), snap.Secrets...),
	}
	return &fixture{t: t, store: &fakeStore{snapshot: snap, deployment: &dep}, peers: &countingPeers{}, snapshot: snap, pinned: c, dep: dep,
		principal: delivery.Principal{Scope: scope, Run: reg.Run, Attempt: reg.Attempt}}
}

func (f *fixture) service(contexts delivery.PinnedContexts) *delivery.Service {
	svc, err := delivery.NewService(delivery.Config{
		Store: f.store, Access: allowAll{}, Credentials: noCredentials{f.t}, Contexts: contexts, Producers: f.peers, Artifacts: f.peers,
		Authority: fakeAuthority{f.principal}, Clock: func() time.Time { return now },
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return svc
}

// callbacks invokes every protected callback and requires each to fail with want
// without reaching any write, provenance read or current-settings read.
func (f *fixture) denyAll(svc *delivery.Service, want error, skip ...string) {
	f.t.Helper()
	ctx := context.Background()
	a := f.principal.Attempt
	req := delivery.RegistrationRequest{Source: registration().Source, Image: registration().Image}
	id := f.dep.ID()
	calls := map[string]func() error{
		"register": func() error { _, err := svc.RegisterRuntime(ctx, a, "tok", req); return err },
		"lookup":   func() error { _, err := svc.LookupByAction(ctx, a, "tok", f.pinned.ActionKey); return err },
		"get":      func() error { _, err := svc.GetRuntime(ctx, a, "tok", id); return err },
		"outcome": func() error {
			_, err := svc.ReportOutcome(ctx, a, "tok", id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: objs("api")})
			return err
		},
		"cleanup": func() error {
			_, err := svc.ReportCleanup(ctx, a, "tok", id, delivery.CleanupReport{State: delivery.CleanupFailed, Reason: "x_y"})
			return err
		},
		"human register": func() error {
			_, err := svc.RegisterForAttempt(ctx, delivery.Caller{Actor: delivery.Actor{Type: delivery.ActorUser, ID: "u_1"}}, scope, a, req)
			return err
		},
	}
	for name, call := range calls {
		if len(skip) > 0 && name == skip[0] {
			continue // the human route derives the run from the pinned context itself
		}
		err := call()
		if name == "human register" && errors.Is(want, delivery.ErrPinRequired) && errors.Is(err, delivery.ErrNotFound) {
			continue // an absent attempt is simply not found to a human
		}
		if !errors.Is(err, want) {
			f.t.Errorf("%s: got %v, want %v", name, err, want)
		}
	}
	if f.store.writes != 0 || f.store.envReads != 0 || f.peers.calls != 0 {
		f.t.Fatalf("a denied callback wrote %d times, read the current environment %d times and consulted provenance %d times", f.store.writes, f.store.envReads, f.peers.calls)
	}
}

// Test_pinned_context_required proves DQ-15's callback half without any
// service: a nil reader, an absent pin, a foreign scope, a wrong action key and a
// credential-binding mismatch deny every protected callback, and never fall back
// to the environment's current settings.
func Test_pinned_context_required(t *testing.T) {
	t.Run("a nil pinned-context reader", func(t *testing.T) {
		f := newFixture(t)
		f.denyAll(f.service(nil), delivery.ErrPinRequired)
	})

	t.Run("an absent pin", func(t *testing.T) {
		f := newFixture(t)
		f.denyAll(f.service(fakeContexts{err: delivery.Fail(delivery.ErrNotFound, nil, "not found")}), delivery.ErrPinRequired)
	})

	t.Run("a context without a declared environment", func(t *testing.T) {
		f := newFixture(t)
		f.pinned.Environment = nil
		f.denyAll(f.service(fakeContexts{c: f.pinned}), delivery.ErrPinRequired)
	})

	t.Run("a foreign scope, run or attempt", func(t *testing.T) {
		for name, mutate := range map[string]func(*runtimev1.Context){
			"tenant":  func(c *runtimev1.Context) { c.TenantID = "t035_globex" },
			"project": func(c *runtimev1.Context) { c.ProjectID = "p035_other" },
			"run":     func(c *runtimev1.Context) { c.RunID = "run_other" },
			"attempt": func(c *runtimev1.Context) { c.AttemptID = "att_other" },
		} {
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				mutate(&f.pinned)
				if name == "run" {
					f.denyAll(f.service(fakeContexts{c: f.pinned}), delivery.ErrPinRequired, "human register")
					return
				}
				f.denyAll(f.service(fakeContexts{c: f.pinned}), delivery.ErrPinRequired)
			})
		}
	})

	t.Run("a context without a usable action key", func(t *testing.T) {
		f := newFixture(t)
		f.pinned.ActionKey = ""
		f.denyAll(f.service(fakeContexts{c: f.pinned}), delivery.ErrPinRequired)
	})

	t.Run("a credential-binding mismatch", func(t *testing.T) {
		for name, mutate := range map[string]func(*runtimev1.Context){
			"version":  func(c *runtimev1.Context) { c.Secrets[0].Version = 2 },
			"delivery": func(c *runtimev1.Context) { c.Secrets[0].Delivery = runtimev1.DeliveryEnv },
			"name":     func(c *runtimev1.Context) { c.Secrets[0].Name = "OTHER" },
			"ref":      func(c *runtimev1.Context) { c.Secrets[0].Ref = "secret://project/other" },
			"missing":  func(c *runtimev1.Context) { c.Secrets = c.Secrets[:1] },
			"none":     func(c *runtimev1.Context) { c.Secrets = nil },
		} {
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				mutate(&f.pinned)
				svc := f.service(fakeContexts{c: f.pinned})
				_, err := svc.RegisterRuntime(context.Background(), f.principal.Attempt, "tok", delivery.RegistrationRequest{Source: registration().Source, Image: registration().Image})
				if !errors.Is(err, delivery.ErrPinRequired) {
					t.Fatalf("got %v", err)
				}
				if f.store.writes != 0 || f.store.envReads != 0 || f.peers.calls != 0 {
					t.Fatalf("writes=%d envReads=%d peers=%d", f.store.writes, f.store.envReads, f.peers.calls)
				}
			})
		}
	})

	t.Run("a pin that names no retained snapshot is never replaced by current settings", func(t *testing.T) {
		f := newFixture(t)
		f.pinned.Environment = &runtimev1.EnvironmentPin{ID: f.snapshot.Environment, ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("0", 64))}
		svc := f.service(fakeContexts{c: f.pinned})
		_, err := svc.RegisterRuntime(context.Background(), f.principal.Attempt, "tok", delivery.RegistrationRequest{Source: registration().Source, Image: registration().Image})
		if !errors.Is(err, delivery.ErrPinRequired) || f.store.envReads != 0 || f.store.writes != 0 {
			t.Fatalf("got %v, envReads=%d", err, f.store.envReads)
		}
	})

	t.Run("a wrong action key", func(t *testing.T) {
		f := newFixture(t)
		svc := f.service(fakeContexts{c: f.pinned})
		_, err := svc.LookupByAction(context.Background(), f.principal.Attempt, "tok", "act:someone-elses")
		if !errors.Is(err, delivery.ErrPinRequired) {
			t.Fatalf("lookup of another action: %v", err)
		}
		// A deployment of another logical action or configuration is not this
		// attempt's: reads and outcome reports are refused.
		other := registration()
		other.ActionKey, other.ConfigDigest, other.Environment, other.Namespace = "act:other", f.snapshot.Digest, f.snapshot.Environment, f.snapshot.Namespace
		d, _ := delivery.NewDeployment(other, now)
		f.store.deployment = &d
		if _, err := svc.GetRuntime(context.Background(), f.principal.Attempt, "tok", d.ID()); !errors.Is(err, delivery.ErrDenied) {
			t.Fatalf("read of another action's deployment: %v", err)
		}
		if _, err := svc.ReportOutcome(context.Background(), f.principal.Attempt, "tok", d.ID(), delivery.OutcomeReport{State: delivery.OutcomeFailed, Reason: "x_y"}); !errors.Is(err, delivery.ErrPinRequired) {
			t.Fatalf("outcome for another action's deployment: %v", err)
		}
		if f.store.writes != 0 {
			t.Fatal("a mismatched callback wrote")
		}
	})

	t.Run("a token for another attempt is unauthenticated, before any pin is read", func(t *testing.T) {
		f := newFixture(t)
		svc := f.service(fakeContexts{c: f.pinned})
		_, err := svc.RegisterRuntime(context.Background(), "att_someone_else", "tok", delivery.RegistrationRequest{Source: registration().Source, Image: registration().Image})
		if !errors.Is(err, delivery.ErrUnauthenticated) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("without an authority no callback is served", func(t *testing.T) {
		f := newFixture(t)
		svc, err := delivery.NewService(delivery.Config{Store: f.store, Access: allowAll{}, Credentials: noCredentials{t}, Contexts: fakeContexts{c: f.pinned}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.LookupByAction(context.Background(), f.principal.Attempt, "tok", f.pinned.ActionKey)
		if !errors.Is(err, delivery.ErrNotConfigured) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestServiceRequiresItsCollaborators(t *testing.T) {
	f := newFixture(t)
	for name, cfg := range map[string]delivery.Config{
		"no store":       {Access: allowAll{}, Credentials: noCredentials{t}},
		"no access":      {Store: f.store, Credentials: noCredentials{t}},
		"no credentials": {Store: f.store, Access: allowAll{}},
		"bad ttl":        {Store: f.store, Access: allowAll{}, Credentials: noCredentials{t}, DefaultPreviewTTL: 1},
		"bad annotation": {Store: f.store, Access: allowAll{}, Credentials: noCredentials{t}, Annotations: delivery.AnnotationNames{Deployment: "x", Action: "x", Configuration: "y"}},
	} {
		if _, err := delivery.NewService(cfg); err == nil {
			t.Errorf("%s: constructed", name)
		}
	}
}
