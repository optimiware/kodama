package gitservice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// AssertionHeader carries the forwarding assertion token from the control
// plane's Git route to the worker. The caller's own credential is never
// forwarded.
const AssertionHeader = "Kodama-Forwarding-Assertion"

// ControlPlane is the worker's only port to the control plane, implemented over
// HTTPS to gitv1.ControlBasePath with the worker identity as bearer credential
// (see NewControlPlaneClient). A worker cannot call any other control-plane
// route.
type ControlPlane interface {
	// Authorize is called by pre-receive. Any error (unavailable, timeout,
	// refusal) fails the receive; nothing changes.
	Authorize(ctx context.Context, req gitv1.PushAuthorizeRequest) (gitv1.PushAuthorizeResponse, error)
	// CommitIntent is called by the prepared reference-transaction hook after
	// the prepared journal entry is durable (fsynced). A non-200 answer, a
	// timeout or any error makes the hook exit non-zero so Git aborts the
	// transaction.
	CommitIntent(ctx context.Context, pushID string, req gitv1.CommitIntentRequest) (gitv1.CommitIntentResponse, error)
	// Outcome is called by post-receive. A failure is retried in the
	// background with the same body; if it never arrives, reconciliation
	// settles the entries from the journal.
	Outcome(ctx context.Context, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error)
	// Initialized confirms an initialization; repeated until acknowledged.
	Initialized(ctx context.Context, req gitv1.InitializedRequest) (gitv1.InitializedResponse, error)
	// Assignments lists repositories to initialize, archive, maintain or
	// reconcile; an assignment repeats until its completion is confirmed.
	Assignments(ctx context.Context) (gitv1.AssignmentsResponse, error)
}

// AssertionVerifier verifies the signature and returns the claims of a
// forwarding assertion (gitauth.Assertions.VerifyForwarding is the production
// implementation, T-136). The worker still applies gitv1.CheckForwarding to the
// returned claims, the service permission and the repository match, so a
// verifier that only checks the signature is sufficient.
type AssertionVerifier interface {
	VerifyForwarding(ctx context.Context, token string, workerTenant domain.TenantID, now time.Time) (gitv1.ForwardingAssertion, error)
}

// ControlCallerVerifier authenticates a control-plane call to the worker RPC
// (T-136 provides the identity verification). A nil error admits the call.
// The request is passed whole so the verifier chooses its credential; it must
// not read the body.
type ControlCallerVerifier interface {
	VerifyControlCall(ctx context.Context, r *http.Request) error
}

// contractError builds the contract error every refusal uses.
func contractError(code domain.ErrorCode, format string, args ...any) *gitv1.ContractError {
	return &gitv1.ContractError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// asContract returns the contract error in err's chain, if any.
func asContract(err error) (*gitv1.ContractError, bool) {
	var ce *gitv1.ContractError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}
