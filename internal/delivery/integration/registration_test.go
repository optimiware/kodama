package integration_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// registration is the pre-apply record the stack's deploy attempt registers.
func (s *stack) registration() delivery.Registration {
	c := s.D.Deploy.Context
	return delivery.Registration{
		Scope: scopeA, Environment: c.Environment.ID, ConfigDigest: c.Environment.ConfigDigest, Namespace: deliverytest.TargetNamespace,
		Source: s.D.Source, Image: s.Req.Image,
		Producer: delivery.Producer{Attempt: s.D.Build.ID(), Run: s.D.BuildRun.ID, Step: "build", ToolImage: pipelinetest.Image},
		Run:      c.RunID, Step: c.StepID, ActionKey: c.ActionKey, Attempt: c.AttemptID,
	}
}

func (s *stack) storeRegister(t *testing.T, r delivery.Registration) (delivery.RegisterResult, error) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	d, err := delivery.NewDeployment(r, now)
	if err != nil {
		t.Fatal(err)
	}
	return s.Store.Register(context.Background(), delivery.RegisterCommand{
		Mutation: delivery.Mutation{Actor: delivery.Actor{Type: delivery.ActorUser, ID: deliverytest.Engineer}, Now: now}, Deployment: d, Attempt: r.Attempt,
	})
}

// Test_duplicate_deployment_key proves DQ-08 (and DQ-07's identity half): one
// deployment exists per logical action, however many identical or retried
// registrations arrive; changed immutable inputs conflict; absent, foreign or
// mismatched immutable pins fail closed.
func Test_duplicate_deployment_key(t *testing.T) {
	t.Run("concurrent identical registrations create one deployment and one receipt", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		const n = 12
		var wg sync.WaitGroup
		receipts := make([]delivery.Receipt, n)
		errs := make([]error, n)
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				receipts[i], errs[i] = s.register(s.D.Deploy, s.Req)
			}()
		}
		close(start)
		wg.Wait()
		created := 0
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("registration %d: %v", i, errs[i])
			}
			if receipts[i].Created {
				created++
			}
			if receipts[i].Deployment.ID() != receipts[0].Deployment.ID() || receipts[i].Annotations[delivery.DefaultAnnotations.Deployment] != string(receipts[0].Deployment.ID()) {
				t.Fatalf("registration %d returned another identity", i)
			}
		}
		id := receipts[0].Deployment.ID()
		if created != 1 || s.deployments() != 1 || s.history(id) != 1 || s.events() != 1 {
			t.Fatalf("created=%d deployments=%d history=%d events=%d, want one of each", created, s.deployments(), s.history(id), s.events())
		}
		if s.audits("deployment.register") != 1 || s.audits("deployment.observe") != n-1 {
			t.Fatalf("audits: register=%d observe=%d", s.audits("deployment.register"), s.audits("deployment.observe"))
		}
		rec := receipts[0].Deployment.Record()
		if rec.Status != delivery.StatusPending || rec.Reason != delivery.ReasonRegistered || rec.Attempt != s.D.Deploy.ID() || rec.ExpiresAt == nil {
			t.Fatalf("pre-apply record: %+v", rec)
		}
		ann := receipts[0].Annotations
		if ann[delivery.DefaultAnnotations.Action] != rec.ActionKey || ann[delivery.DefaultAnnotations.Configuration] != string(rec.ConfigDigest) {
			t.Fatalf("receipt annotations: %v", ann)
		}
		if rec.Source != s.D.Source || rec.Image != s.Req.Image || rec.Producer.ToolImage != pipelinetest.Image || rec.Producer.Attempt != s.D.Build.ID() {
			t.Fatalf("recorded provenance: %+v", rec)
		}
	})

	t.Run("an authorized retry attempt observes the original deployment", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		first := s.mustRegister(t)

		// The first attempt dies (an infrastructure failure): the step's next
		// attempt has a new attempt ID and token but the same logical action.
		retry := s.Retry(s.D)
		if retry.ID() == s.D.Deploy.ID() || retry.Context.ActionKey != s.D.Deploy.Context.ActionKey {
			t.Fatalf("not a retry of the same step: %s %s", retry.ID(), retry.Context.ActionKey)
		}
		again, err := s.register(retry, s.Req)
		if err != nil {
			t.Fatal(err)
		}
		if again.Created || again.Deployment.ID() != first.Deployment.ID() || again.Deployment.Record().Attempt != s.D.Deploy.ID() {
			t.Fatalf("the retry did not observe the original: created=%v origin=%s", again.Created, again.Deployment.Record().Attempt)
		}
		if s.deployments() != 1 || s.events() != 1 {
			t.Fatalf("a retry attempt created %d deployments / %d events", s.deployments(), s.events())
		}
		// The subsequent caller is audited separately, with its own attempt.
		if n := s.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = 'deployment.observe' AND metadata->>'attemptId' = $2`,
			string(scopeA.TenantID), string(retry.ID())); n != 1 {
			t.Fatalf("the retry attempt was audited %d times", n)
		}
		// The lookup-by-action route returns the same identities before any reapply.
		look, err := s.Svc.LookupByAction(context.Background(), retry.ID(), retry.Token, retry.Context.ActionKey)
		if err != nil || look.Deployment.ID() != first.Deployment.ID() {
			t.Fatalf("lookup: %v", err)
		}
		// The failed attempt's token no longer holds deployment authority.
		if _, err := s.register(s.D.Deploy, s.Req); err == nil {
			t.Fatal("the failed attempt still registers deployments")
		}
	})

	t.Run("changed immutable inputs for the same action conflict", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		first := s.mustRegister(t)
		// A second retained snapshot (the environment was edited after admission)
		// and a second verified image give the variations a valid foreign key.
		cfg := map[string]string{"REPLICAS": "3"}
		if _, err := s.Svc.UpdateEnvironment(context.Background(), deliverytest.Caller(deliverytest.Engineer), scopeA, s.D.Env.ID(), s.D.Env.Version(), delivery.EnvironmentEdit{Config: &cfg}); err != nil {
			t.Fatal(err)
		}
		snap, err := s.Svc.ResolveSnapshot(context.Background(), scopeA, delivery.Actor{Type: delivery.ActorUser, ID: deliverytest.Engineer}, s.D.Env.ID())
		if err != nil {
			t.Fatal(err)
		}
		otherImage, otherDigest := s.RegisterImage(s.D.Build, "t035/other", s.D.Provenance())

		base := s.registration()
		cases := map[string]func(*delivery.Registration){
			"source commit":  func(r *delivery.Registration) { r.Source.Commit = "2222222222222222222222222222222222222222" },
			"source URL":     func(r *delivery.Registration) { r.Source.RepositoryURL = "https://git.example.invalid/other.git" },
			"image artifact": func(r *delivery.Registration) { r.Image.ArtifactID = otherImage },
			"image digest":   func(r *delivery.Registration) { r.Image.Digest = otherDigest },
			"configuration":  func(r *delivery.Registration) { r.ConfigDigest = snap.Digest },
			"run":            func(r *delivery.Registration) { r.Run = s.D.BuildRun.ID },
			"step":           func(r *delivery.Registration) { r.Step = "other" },
			"environment":    func(r *delivery.Registration) { r.Environment = "env_other" },
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				r := base
				mutate(&r)
				_, err := s.storeRegister(t, r)
				is(t, err, delivery.ErrConflict)
			})
		}
		if s.deployments() != 1 || s.history(first.Deployment.ID()) != 1 || s.events() != 1 {
			t.Fatalf("a conflicting registration changed state")
		}
		// The identical request, however, is still the replay.
		res, err := s.storeRegister(t, base)
		if err != nil || res.Created || res.Deployment.ID() != first.Deployment.ID() {
			t.Fatalf("identical replay: %v created=%v", err, res.Created)
		}
	})

	t.Run("a promotion of the same digest is a different logical action", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		first := s.mustRegister(t)
		stage := s.Env.Environment(scopeA, "stage-preview", "kubeconfig")
		promo := s.Promote(s.D, stage)
		rc, err := s.register(promo.Deploy, delivery.RegistrationRequest{Source: promo.Source, Image: delivery.Image{ArtifactID: promo.Image, Digest: promo.ImageDigest}})
		if err != nil {
			t.Fatal(err)
		}
		if !rc.Created || rc.Deployment.ID() == first.Deployment.ID() || s.deployments() != 2 {
			t.Fatalf("created=%v deployments=%d", rc.Created, s.deployments())
		}
		if rc.Deployment.Record().Image != first.Deployment.Record().Image {
			t.Fatal("the promotion did not reuse the verified image")
		}
		if rc.Deployment.Record().Producer.Attempt == promo.Deploy.ID() {
			t.Fatal("the deploying attempt was recorded as the producer")
		}
	})

	t.Run("absent, foreign or mismatched pins fail closed", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})

		plain := s.NewPlain(scopeA)
		_, err := s.register(plain, s.Req)
		is(t, err, delivery.ErrPinRequired)

		// A token for another attempt is refused.
		if _, err := s.Svc.RegisterRuntime(context.Background(), s.D.Build.ID(), s.D.Deploy.Token, s.Req); err == nil {
			t.Fatal("a token was accepted for another attempt")
		}
		// An image that is not a pinned input of this attempt is not eligible.
		other, otherDigest := s.RegisterImage(s.D.Build, "t035/other", s.D.Provenance())
		_, err = s.register(s.D.Deploy, delivery.RegistrationRequest{Source: s.D.Source, Image: delivery.Image{ArtifactID: other, Digest: otherDigest}})
		is(t, err, delivery.ErrProvenance)
		// A source that is not the attempt's source is refused.
		bad := s.Req
		bad.Source.Commit = "3333333333333333333333333333333333333333"
		_, err = s.register(s.D.Deploy, bad)
		is(t, err, delivery.ErrProvenance)
		if s.deployments() != 0 || s.events() != 0 {
			t.Fatalf("a refused registration created state: %d deployments", s.deployments())
		}
		// The environment's current settings play no part: the pinned snapshot is
		// the retained one even after the environment was edited.
		cfg := map[string]string{"REPLICAS": "9"}
		if _, err := s.Svc.UpdateEnvironment(context.Background(), deliverytest.Caller(deliverytest.Engineer), scopeA, s.D.Env.ID(), s.D.Env.Version(), delivery.EnvironmentEdit{Config: &cfg}); err != nil {
			t.Fatal(err)
		}
		rc := s.mustRegister(t)
		if rc.Deployment.Record().ConfigDigest != s.D.Deploy.Context.Environment.ConfigDigest {
			t.Fatal("registration used the environment's current configuration instead of the pinned one")
		}
	})

	t.Run("human creation reads the same immutable pins and refuses a mismatch", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		ctx := context.Background()
		engineer := deliverytest.Caller(deliverytest.Engineer)
		// An attempt of another tenant is not found, never registered.
		s.Kubeconfig(deliverytest.ScopeB, "kubeconfig", "FOREIGN")
		foreign := s.NewDeploy(deliverytest.ScopeB, s.Environment(deliverytest.ScopeB, "preview", "kubeconfig"), deliverytest.FlowOptions{})
		_, err := s.Svc.RegisterForAttempt(ctx, engineer, scopeA, foreign.Deploy.ID(), s.Req)
		is(t, err, delivery.ErrNotFound)
		// A mismatched or absent pin writes nothing and holds no pin.
		for name, edit := range map[string]func(runtimev1.Context) (runtimev1.Context, error){
			"a configuration that was never retained": func(c runtimev1.Context) (runtimev1.Context, error) {
				pin := *c.Environment
				pin.ConfigDigest = runtimev1.Digest("sha256:" + strings.Repeat("d", 64))
				c.Environment = &pin
				return c, nil
			},
			"another credential version": func(c runtimev1.Context) (runtimev1.Context, error) {
				c.Secrets = append([]runtimev1.SecretBinding(nil), c.Secrets...)
				c.Secrets[0].Version++
				return c, nil
			},
			"no pinned environment": func(c runtimev1.Context) (runtimev1.Context, error) { c.Environment = nil; return c, nil },
		} {
			s.Pins.Edit(s.D.Deploy.ID(), edit)
			_, err := s.Svc.RegisterForAttempt(ctx, engineer, scopeA, s.D.Deploy.ID(), s.Req)
			is(t, err, delivery.ErrPinRequired)
			s.Pins.Clear()
			if s.deployments() != 0 || s.events() != 0 || s.pins() != 0 {
				t.Fatalf("%s: a refused creation left state", name)
			}
		}
		// The unedited pin registers, and a second human creation observes it.
		first, err := s.Svc.RegisterForAttempt(ctx, engineer, scopeA, s.D.Deploy.ID(), s.Req)
		if err != nil || !first.Created {
			t.Fatalf("human creation: %v", err)
		}
		again, err := s.Svc.RegisterForAttempt(ctx, engineer, scopeA, s.D.Deploy.ID(), s.Req)
		if err != nil || again.Created || again.Deployment.ID() != first.Deployment.ID() || s.deployments() != 1 || s.pins() != 1 {
			t.Fatalf("a repeated human creation: %v created=%v", err, again.Created)
		}
		// A viewer cannot register.
		_, err = s.Svc.RegisterForAttempt(ctx, deliverytest.Caller(deliverytest.Viewer), scopeA, s.D.Deploy.ID(), s.Req)
		is(t, err, delivery.ErrDenied)
	})
}

var _ = runtimev1.DeliveryFile
