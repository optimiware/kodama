package gitwebhook_test

import (
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
)

// Test_unit_trigger_native_store_contract runs the persistence contract the
// PostgreSQL adapter also runs, over the memory store.
func Test_unit_trigger_native_store_contract(t *testing.T) {
	gitwebhooktest.NativeStoreContract(t, func(t testing.TB) gitwebhooktest.NativeSubject {
		s := gitwebhooktest.NewNativeStore()
		a2 := domain.Scope{TenantID: scopeA.TenantID, ProjectID: "project-a2"}
		return gitwebhooktest.NativeSubject{
			Store: s, Restart: func() gw.NativeStore { return s },
			ScopeA: scopeA, ScopeB: scopeB, ScopeA2: a2,
			NewTrigger: func(t testing.TB, scope domain.Scope, id string) gw.PushTrigger {
				v := true
				return gw.PushTrigger{
					Scope: scope, ConfiguredBy: "usr_1", Version: 1, EnabledAt: t0, CreatedAt: t0, UpdatedAt: t0,
					Config: gitv1.TriggerConfig{TriggerID: id, RepositoryID: "repo_1", RefPatterns: []string{"refs/heads/main"}, ExcludeOriginRuns: &v, DefinitionID: "pd_ci", Enabled: true},
				}
			},
			NewRoute: func(t testing.TB, scope domain.Scope) gw.TaskRoute {
				return gw.TaskRoute{Scope: scope, DefinitionID: "pd_task", Enabled: true, ConfiguredBy: "usr_1", Version: 1, EnabledAt: t0, CreatedAt: t0, UpdatedAt: t0}
			},
			Audits: func(t testing.TB, scope domain.Scope, action, resource string) int {
				n := 0
				for _, a := range s.Audits() {
					if a.Action == action && a.Scope == scope && string(a.TriggerID) == resource {
						n++
					}
				}
				return n
			},
		}
	})
}
