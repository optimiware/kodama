// Package delivery_test is the root driver of the blueprint's delivery
// acceptance. It generates a fresh application outside the checkout and runs its
// real-delivery tests against the provisioned development scope.
package delivery_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/templates/go-postgres/acceptance/driver"
)

// Test_blueprint_go_delivery runs the generated quality, build, preview and e2e
// scripts in the tools image the registry holds, under the restricted container
// baseline, against real PostgreSQL, a real OCI registry, a real Kubernetes
// cluster and the development OIDC provider: the real checks, a reproducible
// image with source/tool/digest evidence, real migration Jobs, API and worker
// Deployments, readiness from the live objects, and end-to-end evidence that
// reaches the ERP's acknowledgment through the deployed worker. The factory's
// callbacks and the ERP peer are the only fixtures. (DQ-07, DQ-10, DQ-A04.)
//
// Assembled factory admission, gates and human acceptance remain T-046/T-044.
func Test_blueprint_go_delivery(t *testing.T) {
	app := driver.Generate(t)
	results := driver.GoTest(t, app, driver.RealEnv(t, app), "^TestRealDelivery$", 25*time.Minute, "./test/realdelivery/")
	driver.RequirePassed(t, results,
		"test/realdelivery.TestRealDelivery",
		"test/realdelivery.TestRealDelivery/tools_image",
		"test/realdelivery.TestRealDelivery/quality",
		"test/realdelivery.TestRealDelivery/build_is_reproducible_and_pullable",
		"test/realdelivery.TestRealDelivery/preview_deploy_and_e2e",
	)
}
