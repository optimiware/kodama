package adapters

import (
	"context"
	"errors"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
	wsruntime "github.com/optimiware/kodama/internal/workspace/runtime"
)

// SourceProposer is the workspace module's runtime translation
// (*runtime.Source).
type SourceProposer interface {
	Propose(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, req runtimev1.SourceProposalRequest) (wsruntime.Result, error)
}

// Source implements results.Source over the workspace module.
type Source struct{ p SourceProposer }

var _ results.Source = (*Source)(nil)

// NewSource requires the proposer.
func NewSource(p SourceProposer) (*Source, error) {
	if p == nil {
		return nil, errors.New("results/adapters: a source proposer is required")
	}
	return &Source{p: p}, nil
}

// Propose implements results.Source. It reuses the workspace module's
// translation, so the accepted, pending and conflict responses, the observed tip
// and the credential exclusion are exactly the owner's; this adapter only maps
// the owner's wire error into this module's classes.
func (s *Source) Propose(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, req runtimev1.SourceProposalRequest) (results.SourceOutcome, error) {
	res, err := s.p.Propose(ctx, scope, attempt, req)
	if err != nil {
		return results.SourceOutcome{}, mapSource(err)
	}
	return results.SourceOutcome{HTTPStatus: res.Status, Response: res.Response}, nil
}

func mapSource(err error) error {
	var we *wsruntime.Error
	if !errors.As(err, &we) {
		return results.Fail(results.ErrUnavailable, err, "the publication outcome is not known; repeat the proposal")
	}
	switch we.Code {
	case runtimev1.CodePublicationConflict:
		c := &results.PublicationConflict{}
		c.Reason, _ = we.Details["reason"].(string)
		c.ObservedTip, _ = we.Details["observedTip"].(string)
		return c
	case runtimev1.CodeAttemptRevoked:
		return results.Fail(results.ErrAttemptRevoked, err, "the attempt may not publish source")
	case domain.CodeNotFound:
		return results.Fail(results.ErrNotFound, err, "the resource was not found")
	case runtimev1.CodeArtifactIncomplete:
		return results.Fail(results.ErrArtifactIncomplete, err, "the bundle artifact is not complete")
	case domain.CodeInvalidRequest:
		return results.Fail(results.ErrInvalidRequest, err, "the request is not valid for this operation")
	}
	return results.Fail(results.ErrUnavailable, err, "the publication outcome is not known; repeat the proposal")
}
