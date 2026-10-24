package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
)

// Candidates implements gates.Candidates over execution.Sources, whose
// workspace adapter checks that a commit is the work item's accepted commit or
// lies in its retained branch history. The Git observation happens outside any
// SQL transaction.
type Candidates struct{ sources execution.Sources }

var _ gates.Candidates = (*Candidates)(nil)

// NewCandidates requires the sources.
func NewCandidates(s execution.Sources) (*Candidates, error) {
	if s == nil {
		return nil, errors.New("gates/adapters: workspace sources are required")
	}
	return &Candidates{sources: s}, nil
}

// Verify implements gates.Candidates.
func (c *Candidates) Verify(ctx context.Context, scope domain.Scope, item domain.WorkItemID, commit string) (gates.VerifiedSource, error) {
	info, err := c.sources.Verify(ctx, scope, item, commit)
	switch {
	case err == nil:
		// The repository is the verified information the workspace adapter
		// produced; it is carried, not discarded.
		return gates.VerifiedSource{Repository: info.RepositoryURL}, nil
	case errors.Is(err, execution.ErrAdmissionRefused), errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		return gates.VerifiedSource{}, fmt.Errorf("%w: the candidate commit is not in the work item's workspace history", gates.ErrEvidence)
	}
	return gates.VerifiedSource{}, gates.Unavailablef(err, "the candidate history is unavailable")
}
