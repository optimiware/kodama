package acceptance_test

import "testing"

// Test_blueprint_go_tenant proves, on a freshly generated application with real
// PostgreSQL and an OIDC fixture: verified identity plus current application
// membership, authorized tenant selection, cross-tenant read/write denial,
// worker isolation, and operation with every factory endpoint unreachable.
// (DQ-07, DQ-10; blueprint checklist: independence and tenant isolation.)
func Test_blueprint_go_tenant(t *testing.T) {
	env := postgresEnv(t)
	app := generate(t)
	results := goTest(t, app, env, "^(TestBlueprintGoTenant|TestWorkerAwaitsProvisioning|TestTenantTransactions|TestOIDCVerifier|TestOIDCVerifierOutageFailsClosedAndRecovers|TestOIDCVerifierReadiness|TestHandlerAuthorityComesFromVerifiedIdentityAndMembership|TestHandlerPaginationAndCursorBinding|TestHandlerHealthIndependentOfFactoryAndReadinessReportsDependencies)$",
		"./test/acceptance/", "./internal/exceptions/postgres/", "./internal/identity/", "./internal/exceptions/http/")
	requirePassed(t, results,
		"test/acceptance.TestBlueprintGoTenant",
		"test/acceptance.TestWorkerAwaitsProvisioning",
		"test/acceptance.TestBlueprintGoTenant/identity_is_verified_and_membership_decides_access",
		"test/acceptance.TestBlueprintGoTenant/cross-tenant_reads_and_writes_are_denied",
		"test/acceptance.TestBlueprintGoTenant/revoking_membership_takes_effect_on_the_next_request",
		"test/acceptance.TestBlueprintGoTenant/workers_are_scoped_to_one_tenant_and_one_service_identity",
		"test/acceptance.TestBlueprintGoTenant/the_application_works_with_every_factory_endpoint_unreachable",
		"test/acceptance.TestBlueprintGoTenant/readiness_detects_identity_provider_outages_after_initialization_and_recovers",
		"internal/exceptions/postgres.TestTenantTransactions",
		"internal/identity.TestOIDCVerifier",
		"internal/identity.TestOIDCVerifierOutageFailsClosedAndRecovers",
		"internal/identity.TestOIDCVerifierReadiness",
		"internal/exceptions/http.TestHandlerAuthorityComesFromVerifiedIdentityAndMembership",
	)
}
