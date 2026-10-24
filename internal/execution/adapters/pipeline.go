package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Definitions adapts the pipeline module's repository: an immutable registered
// definition resolved inside the scope.
type Definitions struct{ repo pipeline.Repository }

var _ execution.Definitions = Definitions{}

// NewDefinitions requires the repository.
func NewDefinitions(r pipeline.Repository) (Definitions, error) {
	if r == nil {
		return Definitions{}, errors.New("execution/adapters: a definition repository is required")
	}
	return Definitions{repo: r}, nil
}

// Get implements execution.Definitions.
func (d Definitions) Get(ctx context.Context, scope domain.Scope, id domain.DefinitionID) (pipeline.Definition, error) {
	def, err := d.repo.Get(ctx, scope, id)
	switch {
	case err == nil:
		return def, nil
	case errors.Is(err, pipeline.ErrNotFound), errors.Is(err, pipeline.ErrHidden):
		return pipeline.Definition{}, execution.ErrNotFound
	}
	return pipeline.Definition{}, execution.Unavailablef(err, "the definition repository is unavailable")
}
