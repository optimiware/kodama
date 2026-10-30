// Package domain holds the primitive values shared by every Kodama module:
// opaque identifiers, tenant/project scope, resource versions and stable error
// codes. It contains no entities and no service interfaces.
package domain

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidValue is wrapped by every validation failure in this package.
var ErrInvalidValue = errors.New("invalid value")

// IDPattern is the syntax of every opaque identifier. Callers must not infer
// ownership, kind or ordering from an identifier's spelling.
const IDPattern = `^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`

var idRegexp = regexp.MustCompile(IDPattern)

func validateID(kind, value string) error {
	if !idRegexp.MatchString(value) {
		return fmt.Errorf("%w: %s must match %s", ErrInvalidValue, kind, IDPattern)
	}
	return nil
}

type (
	TenantID      string
	ProjectID     string
	WorkItemID    string
	DefinitionID  string
	RunID         string
	AttemptID     string
	ArtifactID    string
	ReleaseID     string
	PublicationID string
	EnvironmentID string
)

func (id TenantID) Validate() error      { return validateID("tenant id", string(id)) }
func (id ProjectID) Validate() error     { return validateID("project id", string(id)) }
func (id WorkItemID) Validate() error    { return validateID("work item id", string(id)) }
func (id DefinitionID) Validate() error  { return validateID("definition id", string(id)) }
func (id RunID) Validate() error         { return validateID("run id", string(id)) }
func (id AttemptID) Validate() error     { return validateID("attempt id", string(id)) }
func (id ArtifactID) Validate() error    { return validateID("artifact id", string(id)) }
func (id ReleaseID) Validate() error     { return validateID("release id", string(id)) }
func (id PublicationID) Validate() error { return validateID("publication id", string(id)) }
func (id EnvironmentID) Validate() error { return validateID("environment id", string(id)) }
