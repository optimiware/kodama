package chatopshttp

import (
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
)

// Authorization actions of this module's routes.
const (
	ActionDestinationList   httpapi.Action = "chatops.destination.list"
	ActionDestinationCreate httpapi.Action = "chatops.destination.create"
	ActionDestinationRead   httpapi.Action = "chatops.destination.read"
	ActionDestinationUpdate httpapi.Action = "chatops.destination.update"
	ActionDeliveryList      httpapi.Action = "chatops.delivery.list"
	ActionDeliveryRead      httpapi.Action = "chatops.delivery.read"
	ActionDeliveryReplay    httpapi.Action = "chatops.delivery.replay"
)

// Rules is the authorization-rule contribution for the composition root, to be
// merged into the map given to identityhttp.NewAuthorizer (identityhttp's
// DefaultRules does not list this module's actions, and a missing rule denies).
//
// The mapping uses existing permissions only, no new role: destination
// configuration (including its reads) and manual replay need project:settings,
// sanitized delivery inspection needs project:read. The route rules are coarse
// project-level gates; the store repeats the exact permission inside each
// operation's transaction through the transaction-bound authority, so a
// mis-mapped route cannot widen access.
func Rules() map[httpapi.Action]identityhttp.Rule {
	settings := []identity.Permission{identity.PermProjectSettings}
	read := []identity.Permission{identity.PermProjectRead}
	return map[httpapi.Action]identityhttp.Rule{
		ActionDestinationList:   {Level: identityhttp.LevelProject, Any: settings},
		ActionDestinationCreate: {Level: identityhttp.LevelProject, Any: settings},
		ActionDestinationRead:   {Level: identityhttp.LevelProject, Any: settings},
		ActionDestinationUpdate: {Level: identityhttp.LevelProject, Any: settings},
		ActionDeliveryList:      {Level: identityhttp.LevelProject, Any: read},
		ActionDeliveryRead:      {Level: identityhttp.LevelProject, Any: read},
		ActionDeliveryReplay:    {Level: identityhttp.LevelProject, Any: settings},
	}
}
