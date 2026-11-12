//go:build component

package postgrescomponent

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestDeclaredControllerInsertPoliciesPresent is the policy-catalog proof for
// the controller INSERT policies. Migration 0014_controller_event_policies.sql
// renders the two declared kodama_controller INSERT policies on events and
// audit. The live catalog must contain both with the declared role and the
// transaction-scope WITH CHECK; 0011 excludes them and 0013 only grants the
// INSERT privileges.
func TestDeclaredControllerInsertPoliciesPresent(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	if _, err := pgtest.NewMigrationRunner(shard).Apply(ctx, 0); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	for _, table := range []string{"events", "audit"} {
		name := "kodama_controller_" + table + "_insert"
		cmd, roles, _, check, ok := policyDefinition(ctx, t, shard, name)
		if !ok {
			t.Fatalf("declared controller INSERT policy %q is missing from the live catalog", name)
		}
		if !strings.EqualFold(cmd, "INSERT") {
			t.Fatalf("policy %q command = %q; want INSERT", name, cmd)
		}
		if !sameSet(roles, []string{"kodama_controller"}) {
			t.Fatalf("policy %q roles = %v; want kodama_controller", name, roles)
		}
		lower := strings.ToLower(check)
		if !strings.Contains(lower, "tenant_id") || !strings.Contains(lower, "project_id") {
			t.Fatalf("policy %q WITH CHECK has no transaction-scope predicate: %q", name, check)
		}
	}
}
