package serve

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/api"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/kernel/store"
)

// runCredentials adapts the Git server's run credential issuance to the runner's
// port: the credential is derived from the run's pinned sources and its liveness
// at every call, so a rotation after the run ended is refused.
type runCredentials struct{ git *api.GitServer }

func (c runCredentials) IssueRunCredential(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID, repositoryID string) (runner.IssuedCredential, error) {
	cred, err := c.git.IssueRunCredential(ctx, tenant, project, runID, repositoryID)
	if err != nil {
		return runner.IssuedCredential{}, err
	}
	return runner.IssuedCredential{GrantID: cred.GrantID, Secret: cred.Secret, ExpiresAt: cred.ExpiresAt}, nil
}

// runLiveness reads whether a run still holds Git authority, as the Git server
// does at every use: queued or running and not cancelled. A run that does not exist
// is not live; any other failure is unknown.
type runLiveness struct{ st *store.Store }

func (l runLiveness) RunActive(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) (bool, error) {
	info, err := l.st.RunGitInfo(ctx, tenant, project, runID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return info.Active(), nil
}

// runGitSources wires the runs' source checkout: the backend's own Checkout, the
// repository records and the Git server's run credentials. It returns nil, so a
// run that pins sources fails with a stable reason instead of starting on an
// empty workspace, when the backend cannot check out or no state directory exists
// for the credential files.
func runGitSources(backend runner.Backend, st *store.Store, git *api.GitServer, publicURL, stateDir string, log *slog.Logger) *runner.GitSources {
	checkout, ok := backend.(runner.Checkout)
	if !ok || stateDir == "" {
		log.Warn("runs cannot check their Git sources out", "executor", backend.Name())
		return nil
	}
	dir, err := filepath.Abs(filepath.Join(stateDir, "git-credentials"))
	if err != nil {
		log.Warn("runs cannot check their Git sources out", "error", err)
		return nil
	}
	return &runner.GitSources{Checkout: checkout, Repositories: st, Credentials: runCredentials{git: git},
		Liveness: runLiveness{st: st}, BaseURL: publicURL, Dir: dir, Logger: log}
}
