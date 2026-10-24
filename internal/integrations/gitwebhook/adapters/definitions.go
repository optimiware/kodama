package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/pipeline"
)

// DefinitionReader reads registered pipeline definitions. The pipeline module's
// repository (*pipeline/postgres.Store) satisfies it.
type DefinitionReader interface {
	Get(ctx context.Context, scope domain.Scope, id domain.DefinitionID) (pipeline.Definition, error)
}

// Definitions implements gitwebhook.Definitions: a definition is registered
// when the pipeline module returns it for the scope. A missing, hidden or
// foreign definition is ErrNotFound; anything else is an outage.
type Definitions struct{ r DefinitionReader }

var _ gw.Definitions = (*Definitions)(nil)

// NewDefinitions requires the reader.
func NewDefinitions(r DefinitionReader) (*Definitions, error) {
	if r == nil {
		return nil, errors.New("gitwebhook/adapters: a definition reader is required")
	}
	return &Definitions{r: r}, nil
}

// Registered implements gitwebhook.Definitions.
func (d *Definitions) Registered(ctx context.Context, scope domain.Scope, id domain.DefinitionID) error {
	if err := scope.Validate(); err != nil {
		return gw.ErrNotFound
	}
	if err := id.Validate(); err != nil {
		return gw.ErrNotFound
	}
	_, err := d.r.Get(ctx, scope, id)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pipeline.ErrNotFound), errors.Is(err, pipeline.ErrHidden):
		return gw.ErrNotFound
	}
	return fmt.Errorf("%w: definition lookup: %w", gw.ErrUnavailable, err)
}
