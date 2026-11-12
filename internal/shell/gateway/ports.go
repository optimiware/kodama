package gateway

// This file declares the orchestration ports: the optional gateway calls the
// shared authorizer, reads current authority facts, resolves one configured
// secret per call, forwards through an injected transport and commits one
// attributable usage row through the neutral store seam.
//
// Every dependency is required for an enabled gateway. A disabled gateway is
// constructed without any dependency and performs no initialization.

import (
	"context"
	"net/http"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
)

// Clock is the injected authoritative time source for usage and content rows.
type Clock interface {
	Now() time.Time
}

// ContentRecorder persists one authorized raw-content capture. It is called
// only when the configured model opts in. Implementations write the
// content_records row (scope, principal, nullable execution id, model,
// request/response bytes, recorded time and retention expiry); the writer
// enforces the seven-day default retention window and never stores a provider
// credential.
type ContentRecorder interface {
	Record(context.Context, ContentRecord) error
}

// PreWriteFailure is the optional transport capability that keeps a
// dispatch-safe failure definitive. The injected transport is a plain
// http.RoundTripper; when it also implements this interface, the gateway asks it
// to prove that a specific RoundTrip error happened before any request bytes
// were written (for example a connection that could not be dialled). Only that
// proof keeps an error a definitive dependency_unavailable.
//
// The transport owns the dial and the socket, so it alone can distinguish a
// pre-write failure from a lost response. The gateway never guesses from an
// error's text or type name: every unproven transport error may have followed a
// written request, so it is an uncertain external effect that is never replayed.
type PreWriteFailure interface {
	// PrecedesWrite reports whether err was returned before the request was
	// sent to the provider.
	PrecedesWrite(err error) bool
}

// Dependencies is the complete composition of one enabled gateway. Every field
// is required; a missing port is an internal failure rather than a partially
// wired capability.
type Dependencies struct {
	Principals authorize.CurrentPrincipal
	Authorizer authorize.Authorizer
	Authority  authorize.AuthorityReader
	Secrets    secret.Resolver
	Store      store.Executor
	Content    ContentRecorder
	Transport  http.RoundTripper
	Clock      Clock
	NewID      func() (UsageID, error)
}

// Invocation is one model invocation request. The execution permission is
// never caller-supplied: the gateway reads it from the admitted execution of
// an execution caller, and the shared authorizer decides every human or
// service caller's grants.
type Invocation struct {
	Credential string
	ProjectID  id.ProjectID
	Request    api.ChatCompletionRequest
}
