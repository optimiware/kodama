package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
)

// checkoutProvider issues the read-only checkout material of a Kubernetes
// attempt from the same trusted sources the workspace module uses: the
// project's repository (which already bound the credential reference to a URL
// prefix and tenant list) and the installation's credential for that
// reference. It never issues the publisher credential, which stays server
// side, and it answers only for the repository the attempt's pinned context
// names: a mismatch is a definite refusal.
type checkoutProvider struct {
	repos workspace.Repositories
	creds gitad.Credentials
	// gitCA is the PEM trust roots of the Git server; the entrypoint requires
	// them, so an installation without them issues nothing.
	gitCA []byte
}

var _ kube.CheckoutProvider = checkoutProvider{}

func (p checkoutProvider) Checkout(ctx context.Context, req kube.CheckoutRequest) (kube.CheckoutMaterial, error) {
	denied := func(why string) (kube.CheckoutMaterial, error) {
		return kube.CheckoutMaterial{}, fmt.Errorf("%w: %s", kube.ErrCheckoutDenied, why)
	}
	repo, err := p.repos.Repository(ctx, req.Scope)
	switch {
	case err == nil:
	case errors.Is(err, workspace.ErrNotFound):
		return denied("the project has no repository")
	default:
		return kube.CheckoutMaterial{}, err // an outage: the dispatch is redriven
	}
	if repo.URL != req.RepositoryURL {
		return denied("the repository is not the attempt's pinned source")
	}
	cred, err := p.creds.Resolve(ctx, req.Scope, repo.CheckoutSecretRef)
	if err != nil {
		return denied("no checkout credential is configured for the repository")
	}
	if cred.Username == "" || cred.Password == "" || strings.Contains(cred.Username, ":") {
		return denied("the checkout credential cannot be written as a username:password line")
	}
	if len(p.gitCA) == 0 {
		return denied("no Git CA is configured")
	}
	return kube.CheckoutMaterial{
		Scope: req.Scope, WorkspaceID: req.WorkspaceID, RepositoryURL: req.RepositoryURL,
		Credential: []byte(cred.Username + ":" + cred.Password), GitCA: p.gitCA,
	}, nil
}

func readGitCA(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("git CA: %w", err)
	}
	return b, nil
}

// podmanCheckout adapts the one checkout provider of the composition to the
// Podman runtime's bootstrap.CheckoutProvider (T-098). The two request and
// material types are field for field the same: the Kubernetes runtime's and the
// Podman runtime's, which share one source of material, so a source-consuming
// attempt receives the credential of its own pinned repository on either
// backend and nothing else. A nil provider stays nil, so a Podman attempt that
// consumes source is refused (bootstrap.ErrNoCheckout) rather than started
// without a checkout.
func podmanCheckout(p kube.CheckoutProvider) bootstrap.CheckoutProvider {
	if p == nil {
		return nil
	}
	return bootstrapCheckout{inner: p}
}

type bootstrapCheckout struct{ inner kube.CheckoutProvider }

var _ bootstrap.CheckoutProvider = bootstrapCheckout{}

func (b bootstrapCheckout) Checkout(ctx context.Context, req bootstrap.CheckoutRequest) (bootstrap.CheckoutMaterial, error) {
	m, err := b.inner.Checkout(ctx, kube.CheckoutRequest(req))
	switch {
	case err == nil:
	case errors.Is(err, kube.ErrCheckoutDenied):
		return bootstrap.CheckoutMaterial{}, fmt.Errorf("%w: the provider refused the request", bootstrap.ErrCheckoutDenied)
	default:
		return bootstrap.CheckoutMaterial{}, err // an outage: the dispatch is redriven
	}
	return bootstrap.CheckoutMaterial(m), nil
}
