package observationhttp

import (
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
)

// Authorization actions of this module's routes.
const (
	// ActionProjectEvents opens the project stream and snapshot.
	ActionProjectEvents httpapi.Action = "observation.project.events"
	// ActionRunEvents opens one run's stream and snapshot.
	ActionRunEvents httpapi.Action = "observation.run.events"
)

// Rules is the authorization-rule contribution for the composition root, to be
// merged into the map given to identityhttp.NewAuthorizer (identityhttp's
// DefaultRules does not list this module's actions, and a missing rule denies).
//
// The route rules are coarse project-level gates that keep unrelated callers
// out: the project stream opens for work_item:read or run:read, the run stream
// for run:read. The stream itself then asks the authority again, for the exact
// permission of each family, before the snapshot is released, before each
// event, and while idle.
func Rules() map[httpapi.Action]identityhttp.Rule {
	return map[httpapi.Action]identityhttp.Rule{
		ActionProjectEvents: {Level: identityhttp.LevelProject, Any: []identity.Permission{identity.PermWorkRead, identity.PermRunRead}},
		ActionRunEvents:     {Level: identityhttp.LevelProject, Any: []identity.Permission{identity.PermRunRead}},
	}
}
