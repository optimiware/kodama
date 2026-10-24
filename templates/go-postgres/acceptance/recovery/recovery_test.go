// Package recovery_test is the root driver of the blueprint's real-cluster
// delivery recovery evidence.
package recovery_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/templates/go-postgres/acceptance/driver"
)

// TestRealDeliveryRecovery proves lost registration, apply-intent and success
// responses, retries and restarts, unknown outcomes, revoked authority, pin
// refusals, namespace RBAC denial, foreign objects, migration failure and bounded
// readiness against a real cluster, with real kubectl effects. The in-memory
// fault injection of acceptance.TestDeliveryRecovery stays supplementary.
func TestRealDeliveryRecovery(t *testing.T) {
	app := driver.Generate(t)
	results := driver.GoTest(t, app, driver.RealEnv(t, app), "^(TestRealDeliveryRecovery|TestRealDeliveryScriptRefusal)$", 25*time.Minute, "./test/realdelivery/")
	const p = "test/realdelivery.TestRealDeliveryRecovery"
	driver.RequirePassed(t, results,
		p,
		p+"/lost_registration_and_apply_intent_responses_are_retried_to_one_record",
		p+"/a_lost_apply_response_is_settled_by_observation_without_a_second_apply",
		p+"/an_unreported_success_is_reconciled_by_a_retry_attempt_without_disturbing_objects",
		p+"/an_unknown_outcome_is_observed_and_settled_never_blindly_reapplied",
		p+"/revoked_authority_stops_before_any_effect",
		p+"/pins_that_do_not_match_are_refused_before_any_effect",
		p+"/a_credential_that_does_not_cover_the_namespace_is_denied_and_nothing_is_created_there",
		p+"/a_foreign_object_is_never_adopted_or_modified",
		p+"/a_failed_migration_is_reported_and_recorded_not_declared_ready",
		p+"/readiness_is_bounded_and_a_failed_deployment_is_never_reapplied",
		"test/realdelivery.TestRealDeliveryScriptRefusal",
	)
}
