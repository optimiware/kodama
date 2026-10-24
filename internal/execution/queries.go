package execution

import (
	"context"

	"github.com/optimiware/kodama/internal/domain"
)

// Get returns a run with its steps and attempts under run:read. A run of
// another scope is ErrNotFound.
func (s *Service) Get(ctx context.Context, caller Caller, scope domain.Scope, id domain.RunID) (*Aggregate, error) {
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	if _, err := s.authorize(ctx, caller, scope, PermRunRead); err != nil {
		return nil, err
	}
	return s.d.Store.Get(ctx, scope, id)
}

// List pages a project's runs under run:read.
func (s *Service) List(ctx context.Context, caller Caller, scope domain.Scope, q ListQuery) ([]*Run, error) {
	if _, err := s.authorize(ctx, caller, scope, PermRunRead); err != nil {
		return nil, err
	}
	if q.Limit < 1 || q.Limit > 201 {
		return nil, invalidf("limit is out of range")
	}
	return s.d.Store.List(ctx, scope, q)
}
