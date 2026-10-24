package acceptance_test

import "testing"

// TestDeliveryRecovery: lost registration/apply/outcome responses, replay
// conflicts, foreign annotations, readiness timeout, wrong namespace, revoked
// callbacks, and unknown-outcome refusal without blind reapply.
//
// Callback behavior runs against the factory-callback double and the cluster
// is a double or a kubectl-compatible stand-in: the recovery semantics are
// proven, real-cluster behavior is not (see docs/handoff.md). The e2e script
// scenario of the scripts test needs PostgreSQL, so this driver runs only the
// service-free groups; the scripts-level recovery runs in Test_blueprint_go_delivery.
func TestDeliveryRecovery(t *testing.T) {
	app := generate(t)
	results := goTest(t, app, nil, "^(TestDeployHappyPathRecordsBeforeApplyAndObservesReadiness|TestDeployRefusesMismatchedInputsBeforeAnyEffect|TestDeployRefusesIncompleteInputs|TestDeployRecovery|TestDeployForeignAndMismatchedObjects|TestDeployReadinessAndRevocation|TestKubectlAdapter.*)$", "./internal/delivery/")
	requirePassed(t, results,
		"internal/delivery.TestDeployRecovery/a_lost_registration_response_observes_the_same_record",
		"internal/delivery.TestDeployRecovery/a_lost_apply-intent_response_is_repeated,_not_skipped",
		"internal/delivery.TestDeployRecovery/a_lost_apply_response_is_settled_by_observation_without_a_second_apply",
		"internal/delivery.TestDeployRecovery/a_lost_success_response_is_reconciled_by_a_retry_attempt_without_disturbing_objects",
		"internal/delivery.TestDeployRecovery/a_recorded_unknown_outcome_is_observed_and_settled,_never_reapplied",
		"internal/delivery.TestDeployRecovery/an_unobservable_cluster_leaves_the_unknown_outcome_unsettled",
		"internal/delivery.TestDeployForeignAndMismatchedObjects/an_object_owned_by_another_deployment_is_never_adopted",
		"internal/delivery.TestDeployForeignAndMismatchedObjects/the_credential_does_not_cover_the_namespace",
		"internal/delivery.TestDeployForeignAndMismatchedObjects/a_receipt_for_another_namespace_stops_everything",
		"internal/delivery.TestDeployForeignAndMismatchedObjects/a_registration_that_contradicts_the_recorded_action_conflicts",
		"internal/delivery.TestDeployReadinessAndRevocation/readiness_is_structured_and_bounded",
		"internal/delivery.TestDeployReadinessAndRevocation/revoked_authority_before_registration",
		"internal/delivery.TestKubectlAdapterNamespaceScoping",
		"internal/delivery.TestKubectlAdapterLostApplyResponseIsObservable",
		"internal/delivery.TestKubectlAdapterEndToEndWithDeployer",
	)
}
