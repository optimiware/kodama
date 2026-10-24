package identityhttp

import (
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
)

// Authorization actions of this module's routes.
const (
	ActionMe               httpapi.Action = "identity.me"
	ActionTenantList       httpapi.Action = "identity.tenant.list"
	ActionMembershipList   httpapi.Action = "membership.list"
	ActionMembershipCreate httpapi.Action = "membership.create"
	ActionMembershipUpdate httpapi.Action = "membership.update"
	ActionMembershipDelete httpapi.Action = "membership.delete"
	ActionProjectList      httpapi.Action = "project.list"
	ActionProjectCreate    httpapi.Action = "project.create"
	ActionProjectRead      httpapi.Action = "project.read"
	ActionProjectUpdate    httpapi.Action = "project.update"
)

// DefaultRules maps this module's actions and the actions registered by the work
// and configuration modules (named as their packages document them) to the fixed
// role permissions. Route rules are coarse gates; each module's use cases repeat
// their precise permission checks through identity.Authority. The composition
// root may extend the map with actions of later modules; a missing rule denies.
func DefaultRules() map[httpapi.Action]Rule {
	p := func(perms ...identity.Permission) []identity.Permission { return perms }
	return map[httpapi.Action]Rule{
		ActionMe:         {Level: LevelPrincipal},
		ActionTenantList: {Level: LevelPrincipal},

		ActionMembershipList:   {Level: LevelAnyScope, Any: p(identity.PermMembershipManage)},
		ActionMembershipCreate: {Level: LevelAnyScope, Any: p(identity.PermMembershipManage)},
		ActionMembershipUpdate: {Level: LevelAnyScope, Any: p(identity.PermMembershipManage)},
		ActionMembershipDelete: {Level: LevelAnyScope, Any: p(identity.PermMembershipManage)},
		ActionProjectList:      {Level: LevelAnyScope, Any: p(identity.PermProjectRead)},
		ActionProjectCreate:    {Level: LevelTenant, Any: p(identity.PermProjectCreate)},
		ActionProjectRead:      {Level: LevelProject, Any: p(identity.PermProjectRead)},
		ActionProjectUpdate:    {Level: LevelProject, Any: p(identity.PermProjectSettings)},

		// Work management (internal/work/http). HTTP actions are not application
		// permissions: the work service checks the exact permission of each edit
		// and transition itself.
		"work_item.list":    {Level: LevelProject, Any: p(identity.PermWorkRead)},
		"work_item.read":    {Level: LevelProject, Any: p(identity.PermWorkRead)},
		"work_item.history": {Level: LevelProject, Any: p(identity.PermWorkRead)},
		"work_item.create":  {Level: LevelProject, Any: p(identity.PermWorkCreate)},
		"work_item.update":  {Level: LevelProject, Any: p(identity.PermWorkEditSpec, identity.PermWorkEditTechnical)},
		"work_item.transition": {Level: LevelProject, Any: p(identity.PermWorkRefine, identity.PermWorkWork, identity.PermWorkComplete,
			identity.PermWorkReopen, identity.PermWorkCancel)},

		// Variables and secrets (internal/secrets/http). Administrative authority
		// never implies plaintext read, which no route offers.
		"variable.list": {Level: LevelProject, Any: p(identity.PermConfigRead)}, "variable.read": {Level: LevelProject, Any: p(identity.PermConfigRead)},
		"variable.create": {Level: LevelProject, Any: p(identity.PermConfigWrite)}, "variable.update": {Level: LevelProject, Any: p(identity.PermConfigWrite)},
		"variable.delete": {Level: LevelProject, Any: p(identity.PermConfigWrite)},
		"secret.list":     {Level: LevelProject, Any: p(identity.PermCredentialRead)}, "secret.read": {Level: LevelProject, Any: p(identity.PermCredentialRead)},
		"secret.version.list": {Level: LevelProject, Any: p(identity.PermCredentialRead)},
		"secret.create":       {Level: LevelProject, Any: p(identity.PermCredentialManage)}, "secret.delete": {Level: LevelProject, Any: p(identity.PermCredentialManage)},
		"secret.version.create": {Level: LevelProject, Any: p(identity.PermCredentialManage)},
		"secret.version.revoke": {Level: LevelProject, Any: p(identity.PermCredentialManage)},
	}
}
