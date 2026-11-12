package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
)

func TestCheckProviderFailureIsDependencyUnavailable(t *testing.T) {
	validValues := hostedClaimValues([]string{"tenant-a"}, []string{"operator"})

	t.Run("source_error", func(t *testing.T) {
		source := newFakeSource([]testApplication{defaultTestApplication()}, validValues)
		source.setError(errors.New("platform transport failed: provider-body-marker"))
		authority := newAuthority(t, source)
		response, err := authority.Check(context.Background(), testHostedRequest())
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if response.Decision.Allowed {
			t.Fatal("provider failure returned an allowed decision")
		}
		requireNoMarker(t, err.Error(), "provider-body-marker")
	})

	t.Run("empty_applications", func(t *testing.T) {
		source := newFakeSource(nil, validValues)
		authority := newAuthority(t, source)
		response, err := authority.Check(context.Background(), testHostedRequest())
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if response.Decision.Allowed {
			t.Fatal("empty application snapshot was allowed")
		}
	})

	t.Run("nil_claim_values", func(t *testing.T) {
		source := newFakeSource([]testApplication{defaultTestApplication()}, nil)
		authority := newAuthority(t, source)
		response, err := authority.Check(context.Background(), testHostedRequest())
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if response.Decision.Allowed {
			t.Fatal("nil claim values were allowed")
		}
	})

	t.Run("invalid_rbac", func(t *testing.T) {
		application := defaultTestApplication()
		application.rbac = authorize.RBACTable{}
		source := newFakeSource([]testApplication{application}, validValues)
		authority := newAuthority(t, source)
		response, err := authority.Check(context.Background(), testHostedRequest())
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if response.Decision.Allowed {
			t.Fatal("invalid RBAC was allowed")
		}
	})

	t.Run("invalid_claims", func(t *testing.T) {
		application := defaultTestApplication()
		application.claims = identity.OAuthClaims{}
		source := newFakeSource([]testApplication{application}, validValues)
		authority := newAuthority(t, source)
		response, err := authority.Check(context.Background(), testHostedRequest())
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if response.Decision.Allowed {
			t.Fatal("invalid application claims were allowed")
		}
	})

	t.Run("invalid_client", func(t *testing.T) {
		application := defaultTestApplication()
		application.client = identity.OAuthClientConfig{}
		source := newFakeSource([]testApplication{application}, validValues)
		authority := newAuthority(t, source)
		response, err := authority.Check(context.Background(), testHostedRequest())
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if response.Decision.Allowed {
			t.Fatal("invalid OAuth client configuration was allowed")
		}
	})
}

func TestCheckCancellationAndTimeoutAreDependencyUnavailable(t *testing.T) {
	source := newFakeSource(
		[]testApplication{defaultTestApplication()},
		hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
	)
	authority := newAuthority(t, source)

	t.Run("canceled_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		response, err := authority.Check(ctx, testHostedRequest())
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation cause is not reachable: %v", err)
		}
		if response.Decision.Allowed {
			t.Fatal("canceled call was allowed")
		}
	})

	t.Run("expired_deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		response, err := authority.Check(ctx, testHostedRequest())
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline cause is not reachable: %v", err)
		}
		if response.Decision.Allowed {
			t.Fatal("expired deadline call was allowed")
		}
	})
}

type standaloneGrants struct {
	allow bool
	calls int
}

func (grants *standaloneGrants) Allows() bool {
	grants.calls++
	return grants.allow
}

// decideWithHostedOnly models the caller contract: a hosted check error is a
// refusal, never a reason to consult standalone grants. A hosted deny is also
// final. The standalone fake is deliberately never consulted.
func decideWithHostedOnly(ctx context.Context, hosted authority, request authorize.HostedAuthorityRequest, standalone *standaloneGrants) bool {
	response, err := hosted.Check(ctx, request)
	if err != nil {
		return false
	}
	if !response.Decision.Allowed {
		return false
	}
	return true
}

func TestCheckNeverFallsBackToStandaloneGrants(t *testing.T) {
	standalone := &standaloneGrants{allow: true}

	t.Run("hosted_error", func(t *testing.T) {
		source := newFakeSource(
			[]testApplication{defaultTestApplication()},
			hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
		)
		source.setError(fault.New(fault.DependencyUnavailable, "hosted authority unavailable"))
		authority := newAuthority(t, source)
		if decideWithHostedOnly(context.Background(), authority, testHostedRequest(), standalone) {
			t.Fatal("hosted failure fell back to a standalone grant")
		}
		if standalone.calls != 0 {
			t.Fatalf("standalone grants consulted %d times after hosted failure; want 0", standalone.calls)
		}
	})

	t.Run("hosted_deny", func(t *testing.T) {
		application := defaultTestApplication()
		application.rbac = authorize.RBACTable{"operator": {"platform.read"}}
		source := newFakeSource(
			[]testApplication{application},
			hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
		)
		authority := newAuthority(t, source)
		if decideWithHostedOnly(context.Background(), authority, requestForTenant("tenant-a", "platform.invoke"), standalone) {
			t.Fatal("hosted deny fell back to a standalone grant")
		}
		if standalone.calls != 0 {
			t.Fatalf("standalone grants consulted %d times after hosted deny; want 0", standalone.calls)
		}
	})
}

func TestCheckRedactsProviderBodiesAndClaimValues(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	joined := func() string { return strings.Join(logs, "\n") }
	const marker = "provider-secret-marker"

	t.Run("provider_error_cause", func(t *testing.T) {
		source := newFakeSource(
			[]testApplication{defaultTestApplication()},
			hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
		)
		source.setError(fmt.Errorf("platform call failed: %s", marker))
		authority := newAuthorityWithLog(t, source, logf)
		_, err := authority.Check(context.Background(), testHostedRequest())
		requireFaultClass(t, err, fault.DependencyUnavailable)
		requireNoMarker(t, err.Error(), marker)
		requireNoMarker(t, joined(), marker)
	})

	t.Run("claim_values", func(t *testing.T) {
		values := hostedClaimValues([]string{"tenant-a"}, []string{"operator"})
		values["sub"] = []string{"auth0|" + marker}
		source := newFakeSource([]testApplication{defaultTestApplication()}, values)
		authority := newAuthorityWithLog(t, source, logf)
		if _, err := authority.Check(context.Background(), testHostedRequest()); err != nil {
			t.Fatalf("Check: %v", err)
		}
		requireNoMarker(t, joined(), marker)
	})

	t.Run("policy_deny", func(t *testing.T) {
		values := hostedClaimValues([]string{"tenant-a"}, []string{"operator"})
		values["sub"] = []string{"auth0|" + marker}
		application := defaultTestApplication()
		application.rbac = authorize.RBACTable{"operator": {"platform.read"}}
		source := newFakeSource([]testApplication{application}, values)
		authority := newAuthorityWithLog(t, source, logf)
		response, err := authority.Check(context.Background(), requestForTenant("tenant-a", "platform.invoke"))
		requireDenied(t, response, err)
		requireNoMarker(t, joined(), marker)
	})
}

func TestAuthorityConfigValidation(t *testing.T) {
	instance, err := buildAuthority(testAuthorityConfig{})
	if err == nil || instance != nil {
		t.Fatalf("nil Source built an authority: %v", err)
	}
	requireFaultClass(t, err, fault.InvalidInput)

	source := newFakeSource(
		[]testApplication{defaultTestApplication()},
		hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
	)
	instance, err = buildAuthority(testAuthorityConfig{source: source})
	if err != nil || instance == nil {
		t.Fatalf("valid config did not build an authority: %v", err)
	}
}
