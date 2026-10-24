package adapters

import (
	"errors"
	"fmt"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/runtimeauth"
	wsruntime "github.com/optimiware/kodama/internal/workspace/runtime"
)

func TestExecutionErrorsMapToResultClasses(t *testing.T) {
	for _, c := range []struct {
		in   error
		want error
	}{
		{execution.ErrNotFound, results.ErrNotFound},
		{execution.ErrHidden, results.ErrNotFound},
		{execution.ErrAttemptRevoked, results.ErrAttemptRevoked},
		{execution.ErrResultConflict, results.ErrResultConflict},
		{fmt.Errorf("%w: x", execution.ErrInvalid), results.ErrInvalidRequest},
		{fmt.Errorf("%w: unsettled", execution.ErrBusy), results.ErrUnavailable},
		{fmt.Errorf("%w: not published", execution.ErrStale), results.ErrUnavailable},
		{execution.Unavailablef(nil, "db"), results.ErrUnavailable},
		{errors.Join(runtimeauth.ErrAttemptRevoked, errors.New("grant")), results.ErrAttemptRevoked},
		{errors.Join(runtimeauth.ErrInvalidCredential, errors.New("grant")), results.ErrUnauthenticated},
		{errors.New("driver: commit status unknown"), results.ErrUnavailable},
	} {
		if got := mapExecution(c.in); !errors.Is(got, c.want) {
			t.Errorf("%v -> %v, want %v", c.in, got, c.want)
		}
	}
	if mapExecution(nil) != nil {
		t.Error("nil must map to nil")
	}
}

func TestAuthorityErrorsMapWithoutDisclosure(t *testing.T) {
	for _, c := range []struct {
		in   error
		want error
	}{
		{runtimeauth.ErrNotFound, results.ErrUnauthenticated},
		{runtimeauth.ErrInvalidCredential, results.ErrUnauthenticated},
		{runtimeauth.ErrInvalidRequest, results.ErrUnauthenticated},
		{runtimeauth.ErrForbidden, results.ErrForbidden},
		{runtimeauth.ErrAttemptRevoked, results.ErrAttemptRevoked},
		{runtimeauth.ErrUnavailable, results.ErrUnavailable},
		{errors.New("pgx: connection refused"), results.ErrUnavailable},
	} {
		if got := MapAuthority(c.in); !errors.Is(got, c.want) {
			t.Errorf("%v -> %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSourceErrorsKeepTheObservedTip(t *testing.T) {
	conflict := &wsruntime.Error{Code: runtimev1.CodePublicationConflict, Details: map[string]any{"reason": "tip_moved", "observedTip": "2222222222222222222222222222222222222222"}}
	var pc *results.PublicationConflict
	if err := mapSource(conflict); !errors.As(err, &pc) || pc.ObservedTip != "2222222222222222222222222222222222222222" || pc.Reason != "tip_moved" {
		t.Fatalf("%v", err)
	}
	for code, want := range map[domain.ErrorCode]error{
		runtimev1.CodeAttemptRevoked:     results.ErrAttemptRevoked,
		domain.CodeNotFound:              results.ErrNotFound,
		runtimev1.CodeArtifactIncomplete: results.ErrArtifactIncomplete,
		domain.CodeInvalidRequest:        results.ErrInvalidRequest,
		domain.CodeUnavailable:           results.ErrUnavailable,
	} {
		if err := mapSource(&wsruntime.Error{Code: code}); !errors.Is(err, want) {
			t.Errorf("%s -> %v, want %v", code, err, want)
		}
	}
	if err := mapSource(errors.New("unexpected")); !errors.Is(err, results.ErrUnavailable) {
		t.Errorf("%v", err)
	}
}

func TestRuntimePrincipalRoundTrip(t *testing.T) {
	const session = runtimeauth.SessionID("ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	tenant, got, ok := parsePrincipal(RuntimePrincipal("t022_acme", session))
	if !ok || tenant != "t022_acme" || got != session {
		t.Fatalf("%v %v %v", tenant, got, ok)
	}
	for _, bad := range []string{"", "u_engineer", "runtime:", "runtime:t022_acme", "runtime:t022_acme:ses_short", "runtime::" + string(session), "runtime:t 022:" + string(session), "attempt:att_1"} {
		if _, _, ok := parsePrincipal(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestWireStatus(t *testing.T) {
	for in, want := range map[execution.AttemptStatus]runtimev1.AttemptStatus{
		execution.AttemptSucceeded: runtimev1.AttemptSucceeded, execution.AttemptFailed: runtimev1.AttemptFailed, execution.AttemptCancelled: runtimev1.AttemptCancelled,
	} {
		if got, ok := wireStatus(in); !ok || got != want {
			t.Errorf("%s -> %s %v", in, got, ok)
		}
	}
	if _, ok := wireStatus(execution.AttemptRunning); ok {
		t.Error("a running attempt has no acknowledgment status")
	}
}

func TestConstructorsFailClosed(t *testing.T) {
	if _, err := NewAuthority(nil, nil, nil, nil); err == nil {
		t.Error("authority without dependencies")
	}
	if _, err := NewExecution(nil, nil); err == nil {
		t.Error("execution without dependencies")
	}
	if _, err := NewArtifacts(nil); err == nil {
		t.Error("artifacts without a service")
	}
	if _, err := NewArtifactAuthority(nil, nil, nil); err == nil {
		t.Error("artifact authority without dependencies")
	}
	if _, err := NewBundles(nil, nil); err == nil {
		t.Error("bundles without dependencies")
	}
	if _, err := NewSource(nil); err == nil {
		t.Error("source without a proposer")
	}
}
