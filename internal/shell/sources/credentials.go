package sources

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	secret "github.com/optimiware/kodama/internal/shell/secrets"
)

// CredentialSource is the secret custody's source for the references
// [Preparer.Inputs] names: the run's own Git credential for one repository,
// issued from the run's pinned sources while the run is live. Custody puts the
// value in private material outside /workspace and removes it at revocation; the
// issuer refuses a run that ended, was cancelled or has no authority over the
// repository, so a rotation after the run's end finds nothing to issue.
// Any other reference goes to Next, when set.
type CredentialSource struct {
	Issuer RunCredentialIssuer
	Next   secret.Source
}

var _ secret.Source = CredentialSource{}

// Fetch issues the credential named by ref for the run that owns the attempt.
func (c CredentialSource) Fetch(ctx context.Context, owner runtime.MaterializationRef, ref runtime.SecretRef) (secret.Secret, error) {
	repositoryID, ok := repositoryOf(ref.Name)
	if !ok {
		if c.Next == nil {
			return secret.Secret{}, fmt.Errorf("%w: %s", secret.ErrUnavailable, ref.Name)
		}
		return c.Next.Fetch(ctx, owner, ref)
	}
	// A Git credential belongs to the run's own project, never to the tenant at large or another project.
	if ref.Owner != (domain.Owner{TenantID: owner.Scope.TenantID, ProjectID: owner.Scope.ProjectID}) {
		return secret.Secret{}, fmt.Errorf("%w: %s is not the run's project credential", secret.ErrUnavailable, ref.Name)
	}
	if c.Issuer == nil {
		return secret.Secret{}, errors.New("sources: no run credential issuer")
	}
	cred, err := c.Issuer.IssueRunCredential(ctx, owner.Scope.TenantID, owner.Scope.ProjectID, string(owner.RunID), repositoryID)
	if err != nil {
		return secret.Secret{}, err
	}
	if cred.Secret == "" || strings.ContainsAny(cred.Secret, "\r\n \t") {
		return secret.Secret{}, errors.New("sources: the issued credential is not an opaque secret")
	}
	return secret.Secret{Value: []byte(cred.Secret), Version: cred.GrantID}, nil
}
