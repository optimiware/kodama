package pipeline

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Definition is an immutable, registered version of a pipeline: one validated
// canonical document owned by a project. There is no transition after
// creation: changed content is registered as a new version, identical content
// resolves to the existing record, and nothing updates or deletes a version.
type Definition struct {
	Scope     domain.Scope
	ID        domain.DefinitionID
	Name      string
	Version   domain.Version
	Digest    Digest
	Content   Validated
	CreatedAt time.Time
	CreatedBy Actor
}

// NewDefinition creates the registered form of a candidate at the given
// version. It is the only constructor that turns validated content into a
// Definition.
func NewDefinition(c Candidate, version domain.Version) (Definition, error) {
	if err := c.Validate(); err != nil {
		return Definition{}, err
	}
	d := Definition{
		Scope: c.Scope, ID: c.ID, Name: c.Content.Name(), Version: version, Digest: c.Content.Digest(),
		Content: c.Content, CreatedAt: c.CreatedAt.UTC().Truncate(time.Microsecond), CreatedBy: c.CreatedBy,
	}
	if err := d.Validate(); err != nil {
		return Definition{}, err
	}
	return d, nil
}

// Validate checks the definition's internal consistency. Persistence adapters
// call it on every row they load.
func (d Definition) Validate() error {
	if err := d.Scope.Validate(); err != nil {
		return err
	}
	if err := d.ID.Validate(); err != nil {
		return err
	}
	if err := d.Version.Validate(); err != nil {
		return err
	}
	if err := d.Digest.Validate(); err != nil {
		return err
	}
	if d.Content.IsZero() || d.Content.Digest() != d.Digest || d.Content.Name() != d.Name {
		return fmt.Errorf("%w: definition content does not match its name or digest", ErrConflict)
	}
	return d.CreatedBy.Validate()
}

// Validate checks that a candidate is complete.
func (c Candidate) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := c.ID.Validate(); err != nil {
		return err
	}
	if c.Content.IsZero() {
		return fmt.Errorf("%w: candidate has no validated content", domain.ErrInvalidValue)
	}
	if c.CreatedAt.IsZero() {
		return fmt.Errorf("%w: candidate has no creation time", domain.ErrInvalidValue)
	}
	return c.CreatedBy.Validate()
}
