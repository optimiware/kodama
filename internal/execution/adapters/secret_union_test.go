package adapters_test

import (
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution/adapters"
)

func Test_unit_step_environment_secret_union(t *testing.T) {
	w := newSecretWorld(t)
	unionRoute, err := adapters.NewSecretUnionRoute(w.stack.Verifier, w.stack.Ring, w.stack.Env.Grants, w.snapshots, w.source, w.stack.Clock.Now)
	if err != nil {
		t.Fatalf("NewSecretUnionRoute: %v", err)
	}
	w.route = unionRoute

	// Both the environment's pinned credentials AND the step's explicit secret
	// binding resolve and are audited.
	for _, b := range w.pinned {
		got, err := w.resolve(b.Ref, b.Version)
		if err != nil {
			t.Fatalf("resolve %s (%s): %v", b.Name, b.Ref, err)
		}
		if got.Name != b.Name || got.Ref != b.Ref || got.Version != b.Version {
			t.Fatalf("got %+v, want %+v", got, b)
		}
	}
	if got := w.source.auditCount(); got != len(w.pinned) {
		t.Fatalf("expected %d audit entries, got %d", len(w.pinned), got)
	}

	// Undeclared secret or wrong version still fails closed with forbidden.
	if _, err := w.resolve(otherSecret, 1); code(t, err) != domain.CodeForbidden {
		t.Fatalf("expected forbidden for undeclared secret, got %v", err)
	}
	if _, err := w.resolve(stepSecret, 2); code(t, err) != domain.CodeForbidden {
		t.Fatalf("expected forbidden for wrong version of step secret, got %v", err)
	}

	// A context that omits or alters one of the environment's required bindings
	// fails closed with not_found.
	altered := newSecretWorldWith(t, func(c *runtimev1.Context) {
		c.Secrets = []runtimev1.SecretBinding{
			{Ref: stepSecret, Version: 1, Name: "STEP_ONLY", Delivery: runtimev1.DeliveryEnv},
		}
	})
	uAltered, err := adapters.NewSecretUnionRoute(altered.stack.Verifier, altered.stack.Ring, altered.stack.Env.Grants, altered.snapshots, altered.source, altered.stack.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	altered.route = uAltered
	if _, err := altered.resolve(stepSecret, 1); code(t, err) != domain.CodeNotFound {
		t.Fatalf("expected not_found when environment credentials are missing from context, got %v", err)
	}
}
