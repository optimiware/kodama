package acceptance_test

import "testing"

// Test_erp_duplicate_resolution proves, on a freshly generated application with
// real PostgreSQL and the controlled HTTP ERP peer: concurrent duplicate
// resolutions create one durable intent and effect, changed-payload conflicts,
// acknowledgment loss and worker restart reconcile the same operation, and
// unresolved outcomes remain visible. (DQ-07, DQ-10.)
func Test_erp_duplicate_resolution(t *testing.T) {
	env := postgresEnv(t)
	app := generate(t)
	results := goTest(t, app, env, "^(TestERPDuplicateResolution|TestAdapterProtocol|TestAdapterFailureClassification|TestExceptionTransitions.*)$",
		"./test/acceptance/", "./internal/erp/", "./internal/exceptions/")
	requirePassed(t, results,
		"test/acceptance.TestERPDuplicateResolution",
		"test/acceptance.TestERPDuplicateResolution/concurrent_duplicates_create_one_intent_and_one_effect",
		"test/acceptance.TestERPDuplicateResolution/a_changed_payload_under_a_used_key_conflicts_and_nothing_is_replaced",
		"test/acceptance.TestERPDuplicateResolution/acknowledgment_loss_is_reconciled_by_the_same_operation_key",
		"test/acceptance.TestERPDuplicateResolution/a_worker_killed_after_the_effect_leaves_a_recoverable_operation",
		"test/acceptance.TestERPDuplicateResolution/an_unreachable_ERP_stays_visibly_unknown,_then_reconciles_when_it_returns",
		"test/acceptance.TestERPDuplicateResolution/a_rejection_fails_the_exception_with_the_ERP's_reason",
		"internal/erp.TestAdapterProtocol",
		"internal/erp.TestAdapterFailureClassification",
		"internal/exceptions.TestExceptionTransitions",
		"internal/exceptions.TestExceptionTransitionsConcurrentDuplicates",
		"internal/exceptions.TestExceptionTransitionsERPOutcomes",
	)
}
