// Package promotion_test is the root driver of the blueprint's promotion
// acceptance against the provisioned development scope.
package promotion_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/templates/go-postgres/acceptance/driver"
)

// Test_runtime_config_promotion deploys one built image digest to a preview and
// a stage environment through the ordinary scripts: separate namespaces,
// databases, credentials and explicit configuration; per-environment evidence;
// a live deployment or configuration that is not the one under test is refused
// before the workflow writes anything; and a retained pinned input is not
// replaced by a later edit. Real: Kubernetes, registry, PostgreSQL, OIDC, the
// tools image; fixtures: the factory callbacks and the ERP peer.
// (DQ-08, DQ-10, DQ-15.)
func Test_runtime_config_promotion(t *testing.T) {
	app := driver.Generate(t)
	results := driver.GoTest(t, app, driver.RealEnv(t, app), "^TestRealPromotion$", 25*time.Minute, "./test/realdelivery/")
	driver.RequirePassed(t, results,
		"test/realdelivery.TestRealPromotion",
		"test/realdelivery.TestRealPromotion/one_image_digest_with_distinct_explicit_configuration",
		"test/realdelivery.TestRealPromotion/e2e_in_each_environment",
		"test/realdelivery.TestRealPromotion/a_deployment_or_configuration_that_is_not_the_one_under_test_is_refused_before_workflow_writes",
		"test/realdelivery.TestRealPromotion/a_retained_pinned_input_is_not_replaced_by_a_later_edit",
	)
}
