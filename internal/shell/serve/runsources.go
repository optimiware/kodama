package serve

import (
	"log/slog"

	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/shell/api"
	"github.com/optimiware/kodama/internal/shell/sources"
	"github.com/optimiware/kodama/internal/shell/store"
)

// runGitSources composes the runs' source checkout from the shell's source
// preparation package: the backend's own Checkout, the repository records, the
// Git server's run credentials and the run record's liveness. It returns nil, so
// a run that pins sources fails with a stable reason instead of starting on an
// empty workspace, when the backend cannot check out or no state directory exists.
func runGitSources(backend runner.Backend, st *store.Store, git *api.GitServer, publicURL, stateDir string, log *slog.Logger) *runner.GitSources {
	return sources.NewGit(sources.GitConfig{Backend: backend, Repositories: st, Issuer: git, Runs: st,
		PublicURL: publicURL, StateDir: stateDir, Logger: log})
}
