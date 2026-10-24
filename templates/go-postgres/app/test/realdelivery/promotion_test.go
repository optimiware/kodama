package realdelivery_test

import (
	"strings"
	"testing"
)

// TestRealPromotion deploys one built image digest to a preview and a stage
// environment through the ordinary scripts: separate namespaces, databases,
// credentials and explicit configuration, one image, per-environment evidence.
// It then proves a live deployment or configuration that is not the one under
// test is refused before the end-to-end workflow writes anything, and that a
// retained pinned input is not replaced by a later edit.
func TestRealPromotion(t *testing.T) {
	dev := requireDev(t)
	src := requireSource(t)
	tools := dev.tools(t, src)
	img := dev.build(t, src, tools)

	pEnv, sEnv := dev.newEnvironment(t, "preview"), dev.newEnvironment(t, "stage")
	pd, sd := dev.newDeployment(t, pEnv, src, tools, img, "1"), dev.newDeployment(t, sEnv, src, tools, img, "2")

	_, pdep := pd.deployAndSeed()
	sres, sdep := sd.deployAndSeed()

	t.Run("one_image_digest_with_distinct_explicit_configuration", func(t *testing.T) {
		if pdep.Image.Digest != img.digest || sdep.Image.Digest != img.digest || pdep.Image.Repository != sdep.Image.Repository {
			t.Fatalf("preview and stage must run the one built image: %+v / %+v", pdep.Image, sdep.Image)
		}
		if pdep.Source.Commit != src.commit || sdep.Source.Commit != src.commit || pdep.ToolImage != tools.ref || sdep.ToolImage != tools.ref {
			t.Fatalf("source and tool identity differ: %+v %+v", pdep, sdep)
		}
		if pdep.ConfigurationDigest == sdep.ConfigurationDigest || pdep.DeploymentID == sdep.DeploymentID || pdep.Namespace == sdep.Namespace {
			t.Fatalf("the environments share identity: %+v / %+v", pdep, sdep)
		}
		if pdep.ConfigurationDigest != mustDigest(t, pd.snap) || sdep.ConfigurationDigest != mustDigest(t, sd.snap) {
			t.Fatal("the evidence does not carry the admitted configuration digest")
		}
		if pdep.ExpiresAt == "" || sdep.ExpiresAt != "" {
			t.Errorf("only the preview expires: %q / %q", pdep.ExpiresAt, sdep.ExpiresAt)
		}
		for _, d := range []*deployment{pd, sd} {
			for _, name := range []string{"inventory-api", "inventory-worker"} {
				if got := d.liveImage(name); got != img.ref() {
					t.Errorf("%s/%s runs %s, want %s", d.env.ns, name, got, img.ref())
				}
			}
		}
		// The explicit replica setting is environment configuration: the same image runs 1 and 2 replicas.
		if p, s := pd.readyReplicas("inventory-api"), sd.readyReplicas("inventory-api"); p != 1 || s != 2 {
			t.Errorf("ready API replicas preview %d stage %d, want 1 and 2", p, s)
		}
		// Credentials are separate objects with separate values; each names its own database.
		pURL, sURL := pd.secretValue("inventory-runtime", "database-url"), sd.secretValue("inventory-runtime", "database-url")
		if pURL == sURL || !strings.Contains(pURL, pEnv.db.name) || !strings.Contains(sURL, sEnv.db.name) || strings.Contains(sURL, pEnv.db.name) || strings.Contains(pURL, sEnv.db.name) {
			t.Error("the environments must use separate databases and credentials")
		}
		if pd.secretValue("inventory-runtime", "erp-token") == sd.secretValue("inventory-runtime", "erp-token") ||
			pd.secretValue("inventory-runtime", "cursor-key") == sd.secretValue("inventory-runtime", "cursor-key") {
			t.Error("the environments must not share the ERP token or the cursor key")
		}
		if strings.Contains(pd.secretValue("inventory-migrate", "database-url"), pEnv.db.runtime) {
			t.Error("the migration credential must be the owner identity, not the runtime role")
		}
		// Every object the stage holds carries the stage identity, none the preview's.
		for _, ref := range sd.objectUIDs() {
			if ref == "" {
				t.Error("a stage object has no uid")
			}
		}
		out, err := dev.kube("get", "deployment,job,configmap,secret", "--namespace", sEnv.ns, "-o", "jsonpath={..annotations}")
		if err != nil || strings.Contains(out, pdep.DeploymentID) || !strings.Contains(out, sdep.DeploymentID) {
			t.Errorf("the stage's objects must carry only the stage's deployment identity: %v", err)
		}
	})

	// The deployed APIs are reached through the scoped credential's port-forward for the rest of the test.
	pURL, stopP := pd.portForward()
	sURL, stopS := sd.portForward()
	defer stopP()
	defer stopS()
	t.Run("e2e_in_each_environment", func(t *testing.T) {
		rev, out := pd.tokens()
		for _, c := range []struct {
			d   *deployment
			dep deployed
			url string
		}{{pd, pdep, pURL}, {sd, sdep, sURL}} {
			res := c.d.e2e(c.url, c.dep.DeploymentID, c.dep.ConfigurationDigest, rev, out)
			res.must(t, "e2e.sh in "+c.d.env.kind)
			if passed, state, failed := e2eReport(t, res); !passed || state != "resolved" {
				t.Fatalf("%s: passed %v state %q failed %v", c.d.env.kind, passed, state, failed)
			}
			// Each environment's workflow reached its own ERP credential and its own database.
			if n := c.d.env.erp.TotalEffects(); n != 1 {
				t.Errorf("%s: its ERP peer applied %d effects, want 1", c.d.env.kind, n)
			}
			if n := c.d.exceptionCount(); n != 1 {
				t.Errorf("%s: its database holds %d exceptions, want 1", c.d.env.kind, n)
			}
		}
	})

	t.Run("a_deployment_or_configuration_that_is_not_the_one_under_test_is_refused_before_workflow_writes", func(t *testing.T) {
		before := sd.exceptionCount()
		rev, out := sd.tokens()
		for name, c := range map[string]struct{ id, cfg string }{
			"another environment's deployment": {pdep.DeploymentID, sdep.ConfigurationDigest},
			"another environment's config":     {sdep.DeploymentID, pdep.ConfigurationDigest},
			"an unknown deployment":            {"dep_00000000000000000000000000000000", sdep.ConfigurationDigest},
		} {
			res := sd.e2e(sURL, c.id, c.cfg, rev, out)
			if res.exit == 0 || res.outcome() != "failed" || res.reason() != "deployment_mismatch" {
				t.Errorf("%s: exit %d results %v\n%s", name, res.exit, res.manifest, tail(res.out, 2000))
			}
			if got := sd.exceptionCount(); got != before {
				t.Errorf("%s: the refused run wrote to the database (%d -> %d)", name, before, got)
			}
		}
		// The preview's own address, asked for the stage deployment, is also refused.
		res := sd.e2e(pURL, sdep.DeploymentID, sdep.ConfigurationDigest, rev, out)
		if res.reason() != "deployment_mismatch" || sd.exceptionCount() != before || pd.exceptionCount() != 1 {
			t.Errorf("a live target that is another environment must be refused untouched: %v", res.manifest)
		}
	})

	t.Run("a_retained_pinned_input_is_not_replaced_by_a_later_edit", func(t *testing.T) {
		uids := sd.objectUIDs()
		registers := sd.fac.Calls["register"]
		sd.snap.Config["REPLICAS"] = "3" // an edit of the working copy after the digest was pinned
		res := sd.deploy(nil, nil, "")
		if res.exit == 0 || res.reason() != "snapshot_mismatch" {
			t.Fatalf("an edited snapshot must be refused: exit %d %v\n%s", res.exit, res.manifest, tail(res.out, 2000))
		}
		if sd.fac.Calls["register"] != registers {
			t.Error("a refused snapshot must not reach the factory's registration")
		}
		sd.snap.Config["REPLICAS"] = "2" // the identity of the live objects is that of the admitted snapshot
		after := sd.objectUIDs()
		for k, v := range uids {
			if after[k] != v {
				t.Errorf("%s changed from %s to %s", k, v, after[k])
			}
		}
		if got := sd.readyReplicas("inventory-api"); got != 2 {
			t.Errorf("the live stage must keep its admitted replicas, has %d", got)
		}
	})

	_ = sres
}
