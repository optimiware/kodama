package bootstrap

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Checkout material: the scoped, read-only Git credential and the Git CA a
// source-consuming attempt's entrypoint needs to clone its pinned commit, as in
// the Kubernetes runtime's bootstrap Secret.
//
// A runtime owns only the delivery. A CheckoutProvider, supplied by
// composition per tenant/project/workspace, owns the credential's issue and
// scope. The material is delivered as read-only files scoped to one attempt's
// workload, never through the environment, the command line or the image.

// Names of the checkout files inside an attempt's bootstrap directory, and the
// entrypoint flags that point at them.
const (
	CheckoutCredentialFile = "checkout-credential"
	GitCAFile              = "git-ca.crt"
)

const (
	maxCheckoutCredential = 4096
	maxPEMBundle          = 64 << 10
)

// ErrCheckoutDenied: the provider definitely refused to issue a checkout
// credential for the attempt (or answered for another scope, workspace or
// repository). Nothing is started.
var ErrCheckoutDenied = fmt.Errorf("%w: the checkout credential was not issued", execution.ErrDenied)

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
	// Access is read or write; a writer's publisher credential stays
	// server-side and is never requested here.
	Access        execution.Access
	RepositoryURL string
	Commit        runtimev1.GitCommit
}

// CheckoutMaterial is a provider's answer. Scope, WorkspaceID and RepositoryURL
// echo what the credential was issued for; a runtime refuses an answer whose
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

// String and GoString keep the credential out of formatted output.
func (CheckoutMaterial) String() string   { return "bootstrap.CheckoutMaterial{redacted}" }
func (CheckoutMaterial) GoString() string { return "bootstrap.CheckoutMaterial{redacted}" }

// CheckoutProvider issues checkout material for one attempt's source. It must
// answer only for the repository the attempt's pinned context names. An error
// wrapping ErrCheckoutDenied is a definite refusal; any other error is an
// outage and the dispatch is redriven. Implementations must be safe for
// concurrent use.
type CheckoutProvider interface {
	Checkout(ctx context.Context, req CheckoutRequest) (CheckoutMaterial, error)
}

// ConsumesSource reports whether the pinned context's attempt reads or writes
// the workspace.
func ConsumesSource(c runtimev1.Context) bool {
	return c.Workspace.Access != runtimev1.AccessNone
}

// IssueCheckout resolves the checkout material of a source-consuming attempt.
// It returns nil for an attempt that consumes no source, and ErrNoCheckout for
// one that does when no provider is configured. The answer is validated: it
// must echo the request, carry one username:password line and a certificate
// bundle. Errors never carry the provider's text or any credential.
func IssueCheckout(ctx context.Context, p CheckoutProvider, spec execution.AttemptSpec, c runtimev1.Context) (*CheckoutMaterial, error) {
	if spec.Shared != nil {
		// A run_shared attempt receives no checkout credential: only the one-shot
		// preparation workload holds read-only repository access, and it is gone
		// before the first attempt starts.
		return nil, nil
	}
	if !ConsumesSource(c) {
		return nil, nil
	}
	access := execution.Access(c.Workspace.Access)
	if access != execution.AccessRead && access != execution.AccessWrite {
		return nil, fmt.Errorf("%w: the pinned workspace access is unknown", execution.ErrInvalid)
	}
	if spec.Access != "" && spec.Access != access {
		return nil, fmt.Errorf("%w: the attempt's workspace access differs from its pinned context", execution.ErrInvalid)
	}
	src := c.Workspace.Source
	if src == nil || src.RepositoryURL == "" {
		return nil, fmt.Errorf("%w: the pinned context names no source", execution.ErrInvalid)
	}
	if spec.WorkspaceID == "" {
		return nil, fmt.Errorf("%w: the committed attempt has no workspace binding", execution.ErrInvalid)
	}
	if p == nil {
		return nil, ErrNoCheckout
	}
	req := CheckoutRequest{
		Scope: spec.Scope, RunID: spec.RunID, StepID: spec.StepID, AttemptID: spec.AttemptID,
		WorkspaceID: spec.WorkspaceID, Access: access, RepositoryURL: src.RepositoryURL, Commit: src.Commit,
	}
	m, err := p.Checkout(ctx, req)
	switch {
	case err == nil:
	case errors.Is(err, ErrCheckoutDenied):
		return nil, fmt.Errorf("%w: the provider refused the request", ErrCheckoutDenied)
	default:
		// A provider's error text is not trusted to keep credentials out of logs.
		return nil, &opaqueError{kind: execution.ErrUnavailable, cause: err, text: "the checkout credential provider failed"}
	}
	if m.Scope != req.Scope || m.WorkspaceID != req.WorkspaceID || m.RepositoryURL != req.RepositoryURL {
		return nil, fmt.Errorf("%w: the provider answered for another scope, workspace or repository", ErrCheckoutDenied)
	}
	if err := validCheckoutCredential(m.Credential); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckoutDenied, err)
	}
	if err := validatePEMCerts(m.GitCA); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckoutDenied, err)
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

// validatePEMCerts requires a bounded PEM bundle of at least one certificate and
// nothing else (no private keys).
func validatePEMCerts(b []byte) error {
	if len(b) == 0 || len(b) > maxPEMBundle {
		return errors.New("the Git CA is required")
	}
	if !x509.NewCertPool().AppendCertsFromPEM(b) {
		return errors.New("the Git CA holds no certificate")
	}
	for rest := b; ; {
		var blk *pem.Block
		if blk, rest = pem.Decode(rest); blk == nil {
			return nil
		}
		if blk.Type != "CERTIFICATE" {
			return errors.New("the Git CA may hold certificates only")
		}
	}
}

// opaqueError classifies like kind and matches its cause with errors.Is, but
// prints only the fixed text.
type opaqueError struct {
	kind, cause error
	text        string
}

func (e *opaqueError) Error() string   { return e.kind.Error() + ": " + e.text }
func (e *opaqueError) Unwrap() []error { return []error{e.kind, e.cause} }

// WriteCheckout stores the material under dir as CheckoutCredentialFile and
// GitCAFile, mode 0600. Each file is replaced atomically, so a reader never sees
// a partial credential; a redrive before the workload exists writes the
// provider's current answer. dir must be the attempt's own directory.
func WriteCheckout(dir string, m CheckoutMaterial) error {
	if err := writeAtomic(filepath.Join(dir, CheckoutCredentialFile), m.Credential); err != nil {
		return fmt.Errorf("storing the checkout credential: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, GitCAFile), m.GitCA); err != nil {
		return fmt.Errorf("storing the Git CA: %w", err)
	}
	return nil
}

func writeAtomic(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return err
	}
	return syncDir(filepath.Dir(file))
}
