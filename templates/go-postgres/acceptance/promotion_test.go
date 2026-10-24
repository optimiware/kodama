package acceptance_test

import "testing"

// TestRuntimeConfigPromotionContracts is the fixture-level companion of the
// real promotion acceptance (acceptance/promotion, Test_runtime_config_promotion).
// Preview and stage use the same image digest with distinct explicit
// configuration, retained inputs survive later edits, changed credentials or
// configuration alter identity, and the e2e checks reject a mismatched live
// deployment or configuration. (DQ-08, DQ-10, DQ-15.)
//
// Fixtures here: the factory callbacks and the cluster; real: PostgreSQL (the
// e2e target is the real application) and the generated scripts. The real
// cluster promotion is acceptance/promotion.
func TestRuntimeConfigPromotionContracts(t *testing.T) {
	env := postgresEnv(t)
	app := generate(t)
	results := goTest(t, app, env, "^(TestRuntimeConfigPromotion|TestSnapshotDigestIsDeterministicAndCoversEverything|TestDeliveryScripts|TestE2EAgainstLiveDeployment)$",
		"./internal/delivery/", "./test/acceptance/")
	requirePassed(t, results,
		"internal/delivery.TestRuntimeConfigPromotion",
		"internal/delivery.TestRuntimeConfigPromotion/a_retained_input_survives_a_later_edit;_the_edited_copy_is_not_the_pinned_one",
		"internal/delivery.TestRuntimeConfigPromotion/changed_credentials_or_configuration_alter_the_environment_identity",
		"internal/delivery.TestSnapshotDigestIsDeterministicAndCoversEverything",
		"test/acceptance.TestDeliveryScripts/preview_and_stage_promote_one_image_digest_with_distinct_explicit_configuration",
		"test/acceptance.TestDeliveryScripts/e2e_refuses_a_live_target_that_is_not_the_intended_deployment",
		"test/acceptance.TestE2EAgainstLiveDeployment",
		"test/acceptance.TestE2EAgainstLiveDeployment/a_live_target_that_is_not_the_intended_deployment_is_never_tested",
	)
}
