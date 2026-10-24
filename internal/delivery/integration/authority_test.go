package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"

	"github.com/jackc/pgx/v5"
)

// hooks lets a test run code at controlled points of a guarded mutation.
type hooks struct {
	mu sync.Mutex
	// afterAuth runs after a successful runtime authentication, before the
	// guarded transaction begins.
	afterAuth func()
	// gate pauses the next appended event: every lock of the transaction (the
	// execution run lock, the grant lock and the deployment row) is still held.
	entered chan struct{}
	release chan struct{}
}

func (h *hooks) wrapAuthority(a delivery.Authority) delivery.Authority {
	return &hookedAuthority{Authority: a, h: h}
}

type hookedAuthority struct {
	delivery.Authority
	h *hooks
}

func (a *hookedAuthority) Authenticate(ctx context.Context, attempt domain.AttemptID, bearer string) (delivery.Principal, error) {
	p, err := a.Authority.Authenticate(ctx, attempt, bearer)
	a.h.mu.Lock()
	fn := a.h.afterAuth
	a.h.mu.Unlock()
	if err == nil && fn != nil {
		fn()
	}
	return p, err
}

func (h *hooks) wrapAppender(next deliverypg.EventAppender) deliverypg.EventAppender {
	return func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		h.mu.Lock()
		entered, release := h.entered, h.release
		h.entered, h.release = nil, nil
		h.mu.Unlock()
		if entered != nil {
			close(entered)
			<-release
		}
		return next(ctx, tx, env)
	}
}

// arm makes the next event append pause; the returned functions wait for the
// pause and let the transaction continue.
func (h *hooks) arm() (waitPaused, release func()) {
	entered, rel := make(chan struct{}), make(chan struct{})
	h.mu.Lock()
	h.entered, h.release = entered, rel
	h.mu.Unlock()
	var once sync.Once
	return func() {
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			panic("the guarded mutation never reached the gate")
		}
	}, func() { once.Do(func() { close(rel) }) }
}

func raceStack(t *testing.T, h *hooks) *stack {
	return newStack(t, deliverytest.Options{WrapAuthority: h.wrapAuthority, WrapAppender: h.wrapAppender}, deliverytest.FlowOptions{})
}

func (s *stack) cancel(t *testing.T) {
	t.Helper()
	if _, err := s.X.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: deliverytest.Engineer, RequestID: "req_t035"}, scopeA, s.D.Run.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

// Test_deployment_authority_races proves that cancellation and revocation racing
// a deployment mutation are ordered by the execution and grant locks, that
// wrong audience, action, generation and route are denied, and that an
// unauthorized replay is never answered from a stored success.
func Test_deployment_authority_races(t *testing.T) {
	ns := deliverytest.TargetNamespace
	ctx := context.Background()

	t.Run("a cancellation that commits between authentication and the mutation wins", func(t *testing.T) {
		h := &hooks{}
		s := raceStack(t, h)
		h.afterAuth = func() { s.cancel(t) }
		_, err := s.register(s.D.Deploy, s.Req)
		is(t, err, delivery.ErrAttemptRevoked)
		h.afterAuth = nil
		if s.deployments() != 0 || s.events() != 0 || s.pins() != 0 {
			t.Fatalf("a cancelled attempt left state: %d deployments, %d events, %d retention pins", s.deployments(), s.events(), s.pins())
		}
	})

	t.Run("a revocation that commits between authentication and the mutation wins", func(t *testing.T) {
		h := &hooks{}
		s := raceStack(t, h)
		id := s.mustRegister(t).Deployment.ID()
		h.afterAuth = func() {
			if err := s.Auth.Revoke(ctx, scopeA, s.D.Deploy.ID()); err != nil {
				t.Error(err)
			}
		}
		_, err := s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: objects(ns, "api")})
		is(t, err, delivery.ErrAttemptRevoked)
		h.afterAuth = nil
		rec, err := s.Store.GetDeployment(ctx, scopeA, id)
		if err != nil || rec.Status() != delivery.StatusPending || s.history(id) != 1 {
			t.Fatalf("a revoked callback changed the deployment: %v %s history=%d", err, rec.Status(), s.history(id))
		}
	})

	t.Run("a mutation holding the locks makes cancellation and revocation wait", func(t *testing.T) {
		h := &hooks{}
		s := raceStack(t, h)
		id := s.mustRegister(t).Deployment.ID()
		waitPaused, release := h.arm()
		var rc delivery.Receipt
		var reportErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, reportErr = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: objects(ns, "api")})
		}()
		waitPaused() // the mutation ran; its transaction holds the run share lock and the grant share lock

		cancelled := make(chan struct{})
		go func() { s.cancel(t); close(cancelled) }()
		select {
		case <-cancelled:
			t.Fatal("the cancellation did not wait for the mutation's execution lock")
		case <-time.After(500 * time.Millisecond):
		}
		revoked := make(chan error, 1)
		go func() { revoked <- s.Auth.Revoke(ctx, scopeA, s.D.Deploy.ID()) }()
		select {
		case <-revoked:
			t.Fatal("the revocation did not wait for the mutation's grant lock")
		case <-time.After(300 * time.Millisecond):
		}
		release()
		wg.Wait()
		if reportErr != nil || rc.Deployment.Status() != delivery.StatusRunning {
			t.Fatalf("the mutation holding the locks must complete: %v", reportErr)
		}
		<-cancelled
		if err := <-revoked; err != nil {
			t.Fatal(err)
		}
		// The recorded intention survives the cancellation; the cancelled attempt
		// can do nothing further.
		got, err := s.Store.GetDeployment(ctx, scopeA, id)
		if err != nil || got.Status() != delivery.StatusRunning || len(got.Record().Objects) != 1 {
			t.Fatalf("cancellation rewrote the committed record: %v %+v", err, got.Record())
		}
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeUnknown})
		is(t, err, delivery.ErrAttemptRevoked)
	})

	t.Run("a revoked attempt is denied every operation, including replays", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		s.Revoke(s.D.Deploy)
		_, err := s.register(s.D.Deploy, s.Req) // an identical replay of a stored success
		is(t, err, delivery.ErrAttemptRevoked)
		_, err = s.Svc.LookupByAction(ctx, s.D.Deploy.ID(), s.D.Deploy.Token, s.D.Deploy.Context.ActionKey)
		is(t, err, delivery.ErrAttemptRevoked)
		_, err = s.Svc.GetRuntime(ctx, s.D.Deploy.ID(), s.D.Deploy.Token, id)
		is(t, err, delivery.ErrAttemptRevoked)
		_, err = s.report(s.D.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeFailed, Reason: "late"})
		is(t, err, delivery.ErrAttemptRevoked)
		// Revocation denies even though the deployment and its receipt exist.
		if s.deployments() != 1 || s.history(id) != 1 {
			t.Fatal("a denied call changed state")
		}
	})

	t.Run("an attempt without deployment.call, the wrong route, audience or generation are denied", func(t *testing.T) {
		e := deliverytest.New(t, deliverytest.Options{WithoutDeploymentCall: true})
		e.Kubeconfig(scopeA, "kubeconfig", "KUBE-SENTINEL")
		env := e.Environment(scopeA, "preview", "kubeconfig")
		d := e.NewDeploy(scopeA, env, deliverytest.FlowOptions{})
		req := delivery.RegistrationRequest{Source: d.Source, Image: delivery.Image{ArtifactID: d.Image, Digest: d.ImageDigest}}
		_, err := e.Svc.RegisterRuntime(ctx, d.Deploy.ID(), d.Deploy.Token, req)
		is(t, err, delivery.ErrDenied)

		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		// Another attempt's token for this route.
		_, err = s.Svc.RegisterRuntime(ctx, s.D.Deploy.ID(), s.D.Build.Token, s.Req)
		is(t, err, delivery.ErrUnauthenticated)
		// A malformed, empty or absent bearer.
		for _, bearer := range []string{"", "garbage", s.D.Deploy.Token + "x"} {
			_, err = s.Svc.RegisterRuntime(ctx, s.D.Deploy.ID(), bearer, s.Req)
			is(t, err, delivery.ErrUnauthenticated)
		}
		// A correctly signed token with another audience or an unissued generation.
		snap := s.Ring.Current()
		for name, c := range map[string]rtest.MintClaims{
			"audience":   {Audience: "kodama-web", Subject: string(s.D.Deploy.Session), Tenant: string(scopeA.TenantID)},
			"generation": {Generation: snap.Generation + 1, Subject: string(s.D.Deploy.Session), Tenant: string(scopeA.TenantID)},
			"tenant":     {Subject: string(s.D.Deploy.Session), Tenant: string(deliverytest.ScopeB.TenantID)},
		} {
			c.IssuedAt, c.ExpiresAt = s.X.Clock.Now(), s.X.Clock.Now().Add(time.Minute)
			tok := rtest.MintWith(t, jwt.Snapshot(snap), c)
			if _, err := s.Svc.RegisterRuntime(ctx, s.D.Deploy.ID(), tok, s.Req); err == nil {
				t.Fatalf("a token with a wrong %s was accepted", name)
			}
		}
		if s.deployments() != 0 {
			t.Fatal("a denied call created a deployment")
		}
		// A signing rotation retires every outstanding token.
		key, err := jwt.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Ring.Rotate(key); err != nil {
			t.Fatal(err)
		}
		_, err = s.register(s.D.Deploy, s.Req)
		is(t, err, delivery.ErrUnauthenticated)
	})

	t.Run("an unknown outcome of the commit is never success and a retry observes it", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		id := s.mustRegister(t).Deployment.ID()
		// A different attempt of another tenant's run cannot touch this deployment.
		s.Kubeconfig(deliverytest.ScopeB, "kubeconfig", "FOREIGN")
		envB := s.Environment(deliverytest.ScopeB, "preview", "kubeconfig")
		foreign := s.NewDeploy(deliverytest.ScopeB, envB, deliverytest.FlowOptions{})
		_, err := s.report(foreign.Deploy, id, delivery.OutcomeReport{State: delivery.OutcomeFailed, Reason: "forged"})
		is(t, err, delivery.ErrNotFound)
		// An attempt of this project bound to a different logical action cannot either.
		other := s.NewPlain(scopeA)
		_, err = s.report(other, id, delivery.OutcomeReport{State: delivery.OutcomeFailed, Reason: "forged"})
		is(t, err, delivery.ErrPinRequired)
		if rc, _ := s.Svc.GetRuntime(ctx, s.D.Deploy.ID(), s.D.Deploy.Token, id); rc.Deployment.Status() != delivery.StatusPending {
			t.Fatal("a forged callback changed the deployment")
		}
	})
}
