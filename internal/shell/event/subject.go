package event

import (
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// EventSubject builds the one scope-qualified committed-event subject for a
// scope. It follows the declared shape:
//
//	kodama.v1.<tenant>.<project|_>.events
//
// The encoding is injective: a tenant-wide scope uses "_" and a project id is
// never encoded as that placeholder. Although "_" is a syntactically valid
// project id, the builder refuses it so a project subject can never be confused
// with the tenant-wide subject. No wildcard is ever produced.
func EventSubject(scope tenancy.Scope) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	tenant := string(scope.TenantID)
	project := "_"
	if scope.ProjectScoped() {
		if string(scope.ProjectID) == "_" {
			return "", fault.New(fault.InvalidInput, "project id \"_\" is reserved for the tenant-wide event subject")
		}
		project = string(scope.ProjectID)
	}
	if strings.ContainsAny(tenant, "*>") || strings.ContainsAny(project, "*>") {
		return "", fault.New(fault.InvalidInput, "event subject scope contains a wildcard")
	}
	return fmt.Sprintf("kodama.v1.%s.%s.events", tenant, project), nil
}
