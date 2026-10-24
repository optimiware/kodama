package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

// hookArtifacts wraps the artifact owner port with test hooks.
type hookArtifacts struct {
	delivery.Artifacts
	afterEvidence func()
	protectErr    error
}

func (h *hookArtifacts) Evidence(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (delivery.ImageEvidence, error) {
	ev, err := h.Artifacts.Evidence(ctx, scope, id)
	if err == nil && h.afterEvidence != nil {
		h.afterEvidence()
	}
	return ev, err
}

func (h *hookArtifacts) Protect(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner delivery.DeploymentID) error {
	if h.protectErr != nil {
		return h.protectErr
	}
	return h.Artifacts.Protect(ctx, scope, id, owner)
}

// retain tries to remove the artifact as the retention sweep does.
func (s *stack) retain(id domain.ArtifactID) error {
	return s.ArtStore.BeginRetentionDeletion(context.Background(), scopeA, id, time.Now().UTC(), artifacts.Actor{Type: "system", ID: "retention"})
}

// Test_artifact_retention_recovery proves that retention racing a registration,
// and a failed or uncertain deployment commit, can neither expose an
// unprotected accepted image reference nor prematurely release its pin. The
// artifact owner's pin API opens its own transaction, so protection is taken
// first and idempotently, kept on an uncertain commit and released only after
// authoritative reconciliation shows no committed consumer.
func Test_artifact_retention_recovery(t *testing.T) {
	ctx := context.Background()

	t.Run("a committed deployment protects its image from retention", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		s.mustRegister(t)
		if err := s.retain(s.D.Image); !errors.Is(err, artifacts.ErrPinned) {
			t.Fatalf("retention removed a deployed image: %v", err)
		}
	})

	t.Run("the pin exists before the deployment commits", func(t *testing.T) {
		h := &hooks{}
		s := raceStack(t, h)
		waitPaused, release := h.arm()
		done := make(chan error, 1)
		go func() { _, err := s.register(s.D.Deploy, s.Req); done <- err }()
		waitPaused() // the deployment transaction ran but has not committed
		if s.deployments() != 0 {
			t.Fatal("the deployment is visible before its commit")
		}
		// The image's row is key-share locked by the deployment's foreign key, so
		// retention waits for the commit; whichever order, the pin is already there.
		retained := make(chan error, 1)
		go func() { retained <- s.retain(s.D.Image) }()
		var retainErr error
		finished := false
		select {
		case retainErr = <-retained:
			finished = true
		case <-time.After(500 * time.Millisecond):
		}
		if s.pins() != 1 {
			t.Fatalf("the pin was not taken before the commit: %d", s.pins())
		}
		release()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !finished {
			retainErr = <-retained
		}
		if !errors.Is(retainErr, artifacts.ErrPinned) {
			t.Fatalf("an accepted image was unprotected while its reference was being committed: %v", retainErr)
		}
		if s.pins() != 1 {
			t.Fatal("the pin was lost")
		}
	})

	t.Run("retention winning the race refuses the registration", func(t *testing.T) {
		hook := &hookArtifacts{}
		s := newStack(t, deliverytest.Options{WrapArtifacts: func(a delivery.Artifacts) delivery.Artifacts { hook.Artifacts = a; return hook }}, deliverytest.FlowOptions{})
		// Verification saw a usable artifact; retention removes it before the pin.
		hook.afterEvidence = func() {
			if err := s.retain(s.D.Image); err != nil {
				t.Errorf("retention: %v", err)
			}
		}
		_, err := s.register(s.D.Deploy, s.Req)
		is(t, err, delivery.ErrProvenance)
		if s.deployments() != 0 || s.events() != 0 || s.pins() != 0 {
			t.Fatalf("a removed image produced state: %d deployments, %d events, %d pins", s.deployments(), s.events(), s.pins())
		}
	})

	t.Run("an unavailable artifact owner never yields an unprotected deployment", func(t *testing.T) {
		hook := &hookArtifacts{}
		s := newStack(t, deliverytest.Options{WrapArtifacts: func(a delivery.Artifacts) delivery.Artifacts { hook.Artifacts = a; return hook }}, deliverytest.FlowOptions{})
		hook.protectErr = errors.New("artifact store outage")
		_, err := s.register(s.D.Deploy, s.Req)
		is(t, err, delivery.ErrUnavailable)
		if s.deployments() != 0 {
			t.Fatal("a deployment was committed without protection")
		}
		hook.protectErr = nil
		if _, err := s.register(s.D.Deploy, s.Req); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a lost commit acknowledgment keeps the pin and never releases it", func(t *testing.T) {
		var db *deliverytest.LossyDB
		s := newStack(t, deliverytest.Options{WrapDB: func(in storage.TxBeginner) storage.TxBeginner { db = &deliverytest.LossyDB{Inner: in}; return db }}, deliverytest.FlowOptions{})
		db.Lose()
		_, err := s.register(s.D.Deploy, s.Req)
		is(t, err, delivery.ErrUnavailable)
		db.Off()
		if s.deployments() != 1 || s.pins() != 1 {
			t.Fatalf("committed deployment=%d pins=%d", s.deployments(), s.pins())
		}
		id := delivery.DeploymentIDFor(scopeA, s.D.Deploy.Context.ActionKey)
		// Authoritative reconciliation finds the committed consumer and keeps the pin.
		if err := s.Svc.ReconcileRetention(ctx, scopeA, id, s.D.Image); err != nil {
			t.Fatal(err)
		}
		if err := s.retain(s.D.Image); !errors.Is(err, artifacts.ErrPinned) {
			t.Fatalf("reconciliation released the pin of a committed deployment: %v", err)
		}
		// The retry commits nothing new and keeps exactly one pin.
		rc, err := s.register(s.D.Deploy, s.Req)
		if err != nil || rc.Created || s.pins() != 1 {
			t.Fatalf("retry: %v created=%v pins=%d", err, rc.Created, s.pins())
		}
	})

	t.Run("a rolled back uncertain commit keeps the pin until reconciliation shows no consumer", func(t *testing.T) {
		var db *deliverytest.LossyDB
		s := newStack(t, deliverytest.Options{WrapDB: func(in storage.TxBeginner) storage.TxBeginner { db = &deliverytest.LossyDB{Inner: in}; return db }}, deliverytest.FlowOptions{})
		db.Fail()
		_, err := s.register(s.D.Deploy, s.Req)
		is(t, err, delivery.ErrUnavailable)
		db.Off()
		if s.deployments() != 0 {
			t.Fatal("the rolled back commit left a deployment")
		}
		// The outcome was uncertain to the caller: the image stays protected.
		if s.pins() != 1 {
			t.Fatalf("an uncertain commit released the pin: %d", s.pins())
		}
		if err := s.retain(s.D.Image); !errors.Is(err, artifacts.ErrPinned) {
			t.Fatalf("an image of an uncertain deployment was removable: %v", err)
		}
		// Reconciliation: authoritatively no consumer exists, so the pin is released.
		id := delivery.DeploymentIDFor(scopeA, s.D.Deploy.Context.ActionKey)
		if err := s.Svc.ReconcileRetention(ctx, scopeA, id, s.D.Image); err != nil {
			t.Fatal(err)
		}
		if s.pins() != 0 {
			t.Fatalf("reconciliation left %d pins", s.pins())
		}
		if err := s.retain(s.D.Image); err != nil {
			t.Fatalf("an image with no consumer must be removable again: %v", err)
		}
	})

	t.Run("reconciliation releases only the pin of an artifact no deployment uses", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		rc := s.mustRegister(t)
		id := rc.Deployment.ID()
		other, _ := s.RegisterImage(s.D.Build, "t035/other", s.D.Provenance())
		shim, err := adapters.NewArtifacts(shimOf(s))
		if err != nil {
			t.Fatal(err)
		}
		if err := shim.Protect(ctx, scopeA, other, id); err != nil { // a losing request's leftover pin
			t.Fatal(err)
		}
		if s.pins() != 2 {
			t.Fatalf("pins: %d", s.pins())
		}
		if err := s.Svc.ReconcileRetention(ctx, scopeA, id, other); err != nil {
			t.Fatal(err)
		}
		if s.pins() != 1 {
			t.Fatalf("the loser's pin was not released: %d", s.pins())
		}
		if err := s.retain(s.D.Image); !errors.Is(err, artifacts.ErrPinned) {
			t.Fatalf("the winner lost its pin: %v", err)
		}
		// Idempotent, and repeatable.
		if err := s.Svc.ReconcileRetention(ctx, scopeA, id, s.D.Image); err != nil {
			t.Fatal(err)
		}
		if s.pins() != 1 {
			t.Fatalf("pins: %d", s.pins())
		}
	})

	t.Run("a definite refusal releases the pin it took", func(t *testing.T) {
		s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
		s.Revoke(s.D.Deploy)
		_, err := s.register(s.D.Deploy, s.Req)
		if err == nil {
			t.Fatal("a revoked attempt registered")
		}
		if s.pins() != 0 {
			t.Fatalf("a refused registration left %d pins", s.pins())
		}
	})
}
