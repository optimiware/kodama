package app

import (
	"errors"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// pinnedAttempt is the part of an attempt's immutable pinned context the
// composition's adapters read, whichever runtime wire the attempt belongs to:
// the v1 context of an attempt_private attempt or the v2 context of a run_shared
// one. Nothing here is taken from a request.
type pinnedAttempt struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	// SourceURL and SourceCommit name the repository and the commit the attempt's
	// workspace was at (v2: the run's head commit, the latest one published in the
	// run); both are empty for a source-free attempt.
	SourceURL    string
	SourceCommit string
	// Secrets are the pinned bindings; HasEnvironment is whether a deployment
	// environment is pinned.
	Secrets        []runtimev1.SecretBinding
	HasEnvironment bool
	Shared         bool
}

var errPinnedContext = errors.New("the attempt's pinned context is not readable")

// decodePinned reads a pinned context of either wire. A v1 context is tried
// first (the unchanged strict decoder); the v2 decoder is used only when the
// document is not a v1 one, so a document of neither wire stays unreadable.
func decodePinned(raw []byte) (pinnedAttempt, error) {
	if c, err := runtimev1.DecodeRequest[runtimev1.Context](raw); err == nil {
		p := pinnedAttempt{
			Scope: c.Scope(), RunID: c.RunID, AttemptID: c.AttemptID, Secrets: c.Secrets, HasEnvironment: c.Environment != nil,
		}
		if src := c.Workspace.Source; src != nil {
			p.SourceURL, p.SourceCommit = src.RepositoryURL, string(src.Commit)
		}
		return p, nil
	}
	c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](raw)
	if err != nil {
		return pinnedAttempt{}, errPinnedContext
	}
	p := pinnedAttempt{
		Scope: domain.Scope{TenantID: c.TenantID, ProjectID: c.ProjectID}, RunID: c.RunID, AttemptID: c.AttemptID,
		Secrets: c.Secrets, HasEnvironment: c.Environment != nil, Shared: true,
	}
	if src := c.Workspace.Source; src != nil {
		p.SourceURL, p.SourceCommit = src.RepositoryURL, string(src.HeadCommit)
	}
	return p, nil
}
