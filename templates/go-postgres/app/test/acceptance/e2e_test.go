package acceptance

import (
	"context"
	"strings"
	"testing"
	"time"

	"example.invalid/inventory/internal/delivery"
)

// TestE2EAgainstLiveDeployment runs the delivery helper's end-to-end checks
// against the real application, and proves they refuse a target that is not the
// deployment under test.
func TestE2EAgainstLiveDeployment(t *testing.T) {
	const (
		depID  = "dep_0123456789abcdef0123456789abcdef"
		cfgDig = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		imgDig = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		commit = "3333333333333333333333333333333333333333"
	)
	t.Setenv("INVENTORY_DEPLOYMENT_ID", depID)
	t.Setenv("INVENTORY_CONFIGURATION_DIGEST", cfgDig)
	t.Setenv("INVENTORY_IMAGE_DIGEST", imgDig)
	t.Setenv("INVENTORY_SOURCE_COMMIT", commit)
	s := newSystem(t)
	if _, err := s.worker(tenantA, "svc-worker-a"); err != nil {
		t.Fatal(err)
	}
	in := delivery.E2EInput{AppURL: s.base, DeploymentID: depID, ConfigurationDigest: cfgDig, ImageDigest: imgDig, SourceCommit: commit,
		Tenant: tenantA, OtherTenant: tenantB, ReviewerToken: s.token("rita"), OutsiderToken: s.token("bea"), Settle: 15 * time.Second}

	rep, err := delivery.RunE2E(context.Background(), in)
	if err != nil || !rep.Passed {
		t.Fatalf("%v %+v", err, rep)
	}
	if rep.ResolutionState != "resolved" {
		t.Fatalf("with a worker and a cooperative ERP the exception resolves: %s", rep.ResolutionState)
	}
	ids := map[string]bool{}
	for _, c := range rep.Checks {
		ids[c.ID] = c.Passed
	}
	for _, want := range []string{"live-target-matches-deployment", "outsider-cannot-read", "exception-invisible-through-another-tenant", "outsider-cannot-resolve",
		"resolution-replay-is-idempotent", "changed-payload-conflicts", "resolved-only-with-erp-acknowledgment"} {
		if !ids[want] {
			t.Errorf("check %s did not pass: %v", want, ids)
		}
	}

	t.Run("a live target that is not the intended deployment is never tested", func(t *testing.T) {
		for name, mutate := range map[string]func(*delivery.E2EInput){
			"deployment id":        func(i *delivery.E2EInput) { i.DeploymentID = "dep_ffffffffffffffffffffffffffffffff" },
			"configuration digest": func(i *delivery.E2EInput) { i.ConfigurationDigest = strings.Replace(cfgDig, "1", "9", -1) },
			"image digest":         func(i *delivery.E2EInput) { i.ImageDigest = strings.Replace(imgDig, "2", "9", -1) },
			"source commit":        func(i *delivery.E2EInput) { i.SourceCommit = strings.Repeat("9", 40) },
		} {
			bad := in
			mutate(&bad)
			before := listCount(t, s)
			rep, err := delivery.RunE2E(context.Background(), bad)
			if err != nil || rep.Passed || len(rep.Checks) != 1 || rep.Checks[0].ID != "live-target-matches-deployment" {
				t.Errorf("%s: %v %+v", name, err, rep)
			}
			if listCount(t, s) != before {
				t.Errorf("%s: the mismatched target was modified", name)
			}
		}
	})

	t.Run("an unreachable target is an error, not a pass", func(t *testing.T) {
		bad := in
		bad.AppURL = "http://127.0.0.1:1"
		if _, err := delivery.RunE2E(context.Background(), bad); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func listCount(t *testing.T, s *system) int {
	t.Helper()
	_, body, _ := s.do("GET", "/api/v1/tenants/"+tenantA+"/exceptions?limit=100", s.token("rita"), nil, nil)
	return len(body["items"].([]any))
}
