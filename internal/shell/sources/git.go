package sources

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/shell/api"
	"github.com/optimiware/kodama/internal/shell/store"
)

// RunInfoReader reads what Git authority needs of a run, at every use. The
// PostgreSQL store, the in-memory Git store and tests implement it.
type RunInfoReader interface {
	RunGitInfo(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) (store.RunGitInfo, error)
}

// RunCredentialIssuer issues a run's Git credential from the run's pinned
// sources while the run is live. *api.GitServer is one.
type RunCredentialIssuer interface {
	IssueRunCredential(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID, repositoryID string) (api.IssuedCredential, error)
}

// credentials adapts the Git server's run credential issuance to the runner's
// port: the credential is derived from the run's pinned sources and its liveness
// at every call, so a rotation after the run ended is refused.
type credentials struct{ issuer RunCredentialIssuer }

// Credentials adapts a run credential issuer to the runner's CredentialIssuer.
func Credentials(i RunCredentialIssuer) runner.CredentialIssuer { return credentials{i} }

func (c credentials) IssueRunCredential(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID, repositoryID string) (runner.IssuedCredential, error) {
	cred, err := c.issuer.IssueRunCredential(ctx, tenant, project, runID, repositoryID)
	if err != nil {
		return runner.IssuedCredential{}, err
	}
	return runner.IssuedCredential{GrantID: cred.GrantID, Secret: cred.Secret, ExpiresAt: cred.ExpiresAt}, nil
}

// liveness reads whether a run still holds Git authority, as the Git server does
// at every use: queued or running and not cancelled. A run that does not exist is
// not live; any other failure is unknown.
type liveness struct{ runs RunInfoReader }

// Liveness reads a run's Git authority from the run record.
func Liveness(r RunInfoReader) runner.RunLiveness { return liveness{r} }

func (l liveness) RunActive(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) (bool, error) {
	info, err := l.runs.RunGitInfo(ctx, tenant, project, runID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return info.Active(), nil
}

// GitConfig wires the runs' source checkout.
type GitConfig struct {
	// Backend is the executor; it must be a runner.Checkout to check sources out.
	Backend      runner.Backend
	Repositories runner.Repositories
	Issuer       RunCredentialIssuer
	Runs         RunInfoReader
	// PublicURL is the Git service's base URL.
	PublicURL string
	// StateDir holds the runs' credential directories (git-credentials/), outside
	// every workspace.
	StateDir string
	Logger   *slog.Logger
}

// NewGit wires the runs' source checkout: the backend's own Checkout, the
// repository records and the Git server's run credentials. It returns nil, so a
// run that pins sources fails with a stable reason instead of starting on an empty
// workspace, when the backend cannot check out or no state directory exists for
// the credential files.
func NewGit(cfg GitConfig) *runner.GitSources {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	checkout, ok := cfg.Backend.(runner.Checkout)
	if !ok || cfg.StateDir == "" {
		log.Warn("runs cannot check their Git sources out", "executor", cfg.Backend.Name())
		return nil
	}
	dir, err := filepath.Abs(filepath.Join(cfg.StateDir, "git-credentials"))
	if err != nil {
		log.Warn("runs cannot check their Git sources out", "error", err)
		return nil
	}
	return &runner.GitSources{Checkout: checkout, Repositories: cfg.Repositories, Credentials: Credentials(cfg.Issuer),
		Liveness: Liveness(cfg.Runs), BaseURL: cfg.PublicURL, Dir: dir, Logger: log}
}
