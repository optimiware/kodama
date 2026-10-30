package kube

import (
	"context"
	"errors"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Checkout material: the scoped, read-only Git credential and the Git CA a
// source-consuming attempt's entrypoint needs to clone its pinned commit.
//
// The executor owns only the delivery. A CheckoutProvider, supplied by
// composition per tenant/project/workspace, owns the credential's issue and
// scope; the executor binds what it returns into the attempt's immutable
// bootstrap Secret once (see createAdmission) and every restart or redrive
// reads that stored Secret back. The provider is never consulted again for an
// attempt whose Secret exists, so a changed provider answer can neither replace
// the delivered credential nor split a Job's two Pods onto different material.
//
// The Job, its annotations and labels, the Secret's annotations, events and logs
// never contain the credential: only the Secret's data keys do.

// Bootstrap Secret keys of the checkout material. They are present exactly for
// source-consuming attempts (workspace access read or write).
const (
	KeyCheckoutCredential = "checkout-credential"
	KeyGitCA              = "git-ca.crt"
)

const maxCheckoutCredential = 4096

// ErrCheckoutDenied: no checkout credential was issued for the attempt (no
// provider is configured, the provider refused the request, or it answered for a
// different scope, workspace or repository). Dispatch is denied and nothing is
// created.
var ErrCheckoutDenied = errors.New("kube: the checkout credential was not issued")

// CheckoutRequest identifies the source an attempt will check out. Every field
// comes from the committed attempt and its pinned context, never from a
// workload. The credential it asks for is read-only.
type CheckoutRequest struct {
	Scope     domain.Scope
	RunID     domain.RunID
	StepID    string
	AttemptID domain.AttemptID
	// WorkspaceID is the workspace the committed attempt is bound to.
	WorkspaceID string
	// Access is read or write; a writer's publisher credential stays server-side
	// and is never requested here.
	Access        execution.Access
	RepositoryURL string
	Commit        runtimev1.GitCommit
}

// CheckoutMaterial is a provider's answer. Scope, WorkspaceID and RepositoryURL
// echo what the credential was issued for; the executor refuses an answer whose
// echo differs from the request.
type CheckoutMaterial struct {
	Scope         domain.Scope
	WorkspaceID   string
	RepositoryURL string
	// Credential is the single line "username:password" the entrypoint turns
	// into an HTTP Basic header for the clone.
	Credential []byte
	// GitCA is the PEM trust roots for the Git server.
	GitCA []byte
}

// String, GoString and LogValue keep the credential out of formatted output.
func (CheckoutMaterial) String() string   { return "kube.CheckoutMaterial{redacted}" }
func (CheckoutMaterial) GoString() string { return "kube.CheckoutMaterial{redacted}" }

// CheckoutProvider issues checkout material for one attempt's source. An error
// wrapping ErrCheckoutDenied is a definite refusal; any other error is an outage
// and the dispatch is redriven. Implementations must be safe for concurrent use.
type CheckoutProvider interface {
	Checkout(ctx context.Context, req CheckoutRequest) (CheckoutMaterial, error)
}

// sourceAccess reports whether an attempt with the access consumes source.
func sourceAccess(a execution.Access) bool {
	return a == execution.AccessRead || a == execution.AccessWrite
}

// Checkout derives the provider request of the attempt, or false when it
// consumes no source.
func (e Expectation) Checkout() (CheckoutRequest, bool) {
	src := e.Context.Workspace.Source
	if !sourceAccess(e.Recipe.Access) || src == nil || e.Shared != nil {
		return CheckoutRequest{}, false
	}
	return CheckoutRequest{
		Scope: e.Identity.Scope, RunID: e.Identity.RunID, StepID: e.Identity.StepID, AttemptID: e.Identity.AttemptID,
		WorkspaceID: e.WorkspaceID, Access: e.Recipe.Access, RepositoryURL: src.RepositoryURL, Commit: src.Commit,
	}, true
}

// resolveCheckout asks the provider once and validates the answer. It returns
// nil for an attempt that consumes no source.
func (c *core) resolveCheckout(ctx context.Context, exp Expectation) (*CheckoutMaterial, error) {
	req, ok := exp.Checkout()
	if !ok {
		return nil, nil
	}
	if req.WorkspaceID == "" {
		return nil, wrap(ErrNotDispatchable, "the committed attempt has no workspace binding")
	}
	if c.checkout == nil {
		return nil, wrap(ErrCheckoutDenied, "no checkout credential provider is configured")
	}
	m, err := c.checkout.Checkout(ctx, req)
	switch {
	case err == nil:
	case errors.Is(err, ErrCheckoutDenied):
		return nil, wrap(ErrCheckoutDenied, "the provider refused the request")
	default:
		return nil, opaque(ErrUnavailable, err, "the checkout credential provider failed")
	}
	if m.Scope != req.Scope || m.WorkspaceID != req.WorkspaceID || m.RepositoryURL != req.RepositoryURL {
		return nil, wrap(ErrCheckoutDenied, "the provider answered for another scope, workspace or repository")
	}
	if err := validCheckoutCredential(m.Credential); err != nil {
		return nil, wrap(ErrCheckoutDenied, "%v", err)
	}
	if err := validatePEMCerts(m.GitCA, "Git CA"); err != nil {
		return nil, wrap(ErrCheckoutDenied, "%v", err)
	}
	return &CheckoutMaterial{
		Scope: m.Scope, WorkspaceID: m.WorkspaceID, RepositoryURL: m.RepositoryURL,
		Credential: append([]byte(nil), m.Credential...), GitCA: append([]byte(nil), m.GitCA...),
	}, nil
}

// validCheckoutCredential accepts exactly what the entrypoint parses: one line
// "username:password" with both parts non-empty and no control characters.
func validCheckoutCredential(b []byte) error {
	s := string(b)
	user, pass, ok := strings.Cut(s, ":")
	if !ok || user == "" || pass == "" || len(b) > maxCheckoutCredential {
		return errors.New("the checkout credential must be a username:password line")
	}
	if strings.TrimSpace(s) != s {
		return errors.New("the checkout credential has surrounding whitespace")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return errors.New("the checkout credential has control characters")
		}
	}
	return nil
}

// secretLayout is the exact key set an attempt's bootstrap Secret may hold.
type secretLayout uint8

const (
	// layoutAny accepts either exact set: an object claimed through its own
	// annotations, whose committed attempt is not known.
	layoutAny secretLayout = iota
	// layoutBootstrap is {admission, ca.crt}: attempts without a workspace.
	layoutBootstrap
	// layoutCheckout adds {checkout-credential, git-ca.crt}: source consumers.
	layoutCheckout
)

func layoutFor(id Identity) secretLayout {
	a := id.Access
	switch {
	case id.Shared:
		// A run_shared attempt never receives checkout material (seam S2).
		return layoutBootstrap
	case a == "":
		return layoutAny
	case sourceAccess(a):
		return layoutCheckout
	}
	return layoutBootstrap
}

// checkSecretKeys verifies the exact key set and the content of the checkout
// keys. Messages never carry values.
func checkSecretKeys(l secretLayout, data map[string][]byte) error {
	has := func(k string) bool { _, ok := data[k]; return ok }
	if len(data[KeyAdmission]) == 0 || len(data[KeyCA]) == 0 {
		return errors.New("unexpected content")
	}
	withCheckout := has(KeyCheckoutCredential) || has(KeyGitCA)
	switch {
	case withCheckout && l == layoutBootstrap:
		return errors.New("checkout material on an attempt without a workspace")
	case !withCheckout && l == layoutCheckout:
		return errors.New("checkout material is missing")
	}
	want := 2
	if withCheckout {
		want = 4
	}
	if len(data) != want {
		return errors.New("unexpected content")
	}
	if withCheckout {
		if err := validCheckoutCredential(data[KeyCheckoutCredential]); err != nil {
			return errors.New("malformed checkout credential")
		}
		if err := validatePEMCerts(data[KeyGitCA], "Git CA"); err != nil {
			return errors.New("malformed Git CA")
		}
	}
	return nil
}

// opaqueError classifies like kind and matches its cause with errors.Is, but
// prints only the fixed text: a provider's error text is not trusted to keep
// credential material out of logs.
type opaqueError struct {
	kind, cause error
	text        string
}

func opaque(kind, cause error, text string) error {
	return &opaqueError{kind: kind, cause: cause, text: text}
}

func (e *opaqueError) Error() string   { return e.kind.Error() + ": " + e.text }
func (e *opaqueError) Unwrap() []error { return []error{e.kind, e.cause} }
