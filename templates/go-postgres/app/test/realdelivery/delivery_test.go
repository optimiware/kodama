package realdelivery_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"example.invalid/inventory/test/support/factoryfake"
)

// TestRealDelivery executes the ordinary delivery scripts in the tools image the
// scope's registry holds, as the step entrypoint would (non-root, read-only root
// filesystem, no added capabilities, no container socket), against real
// PostgreSQL, a real OCI registry, a real Kubernetes cluster reached with a
// namespace-scoped credential and the development OIDC provider. The only
// fixtures are the factory's runtime callbacks and the controlled ERP peer.
func TestRealDelivery(t *testing.T) {
	dev := requireDev(t)
	src := requireSource(t)
	tools := dev.tools(t, src)

	t.Run("tools_image", func(t *testing.T) {
		dev.pullAndVerify(t, strings.TrimSuffix(tools.ref, "@"+tools.digest), tools.digest)
		res := dev.runScriptRaw(t, tools, "id -u; ko version; kubectl version --client; go version; git --version; (touch /etc/should-fail 2>&1 || true); ls /var/run/docker.sock 2>&1 | head -n 1")
		for _, want := range []string{"65532", "0.19.1", "v1.37.1", "go1.27.2", "git version", "Read-only file system", "No such file"} {
			if !strings.Contains(res, want) {
				t.Errorf("the tools image lacks %q:\n%s", want, res)
			}
		}
	})

	t.Run("quality", func(t *testing.T) {
		res := dev.runScript(t, tools, scriptSpec{script: "quality.sh", src: src, env: map[string]string{
			"INVENTORY_TEST_ADMIN_URL": dev.qaAdminURL(t), "INVENTORY_TEST_PREFIX": dev.prefix}})
		res.must(t, "quality.sh")
		var rep struct {
			SourceCommit string
			GoVersion    string
			Passed       bool
			Checks       []struct{ ID, Status string }
		}
		raw, err := os.ReadFile(filepath.Join(res.outputs, "quality.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatal(err)
		}
		if !rep.Passed || rep.SourceCommit != src.commit || !strings.HasPrefix(rep.GoVersion, "go1.27") {
			t.Fatalf("quality evidence: %s", raw)
		}
		want := map[string]bool{"gofmt": false, "vet": false, "modules": false, "scripts": false, "imports": false, "unit": false, "migrations": false, "tenancy": false, "acceptance": false}
		for _, c := range rep.Checks {
			if c.Status != "passed" {
				t.Errorf("check %s: %s", c.ID, c.Status)
			}
			want[c.ID] = true
		}
		for id, seen := range want {
			if !seen {
				t.Errorf("check %s did not run", id)
			}
		}
		// The database checks reached the real server: the log names the packages that need it.
		log, _ := os.ReadFile(filepath.Join(res.outputs, "quality.log"))
		for _, pkg := range []string{"/migrations", "/internal/exceptions/postgres", "/test/acceptance"} {
			if !regexp.MustCompile(`(?m)^ok\s+\S+` + regexp.QuoteMeta(pkg) + `\s`).Match(log) {
				t.Errorf("the quality log shows no passing run of %s:\n%s", pkg, tail(string(log), 3000))
			}
		}
	})

	t.Run("build_is_reproducible_and_pullable", func(t *testing.T) {
		// Two builds of one commit, into two repositories, at the same time.
		var wg sync.WaitGroup
		imgs := make([]appImage, 2)
		for i := range imgs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				imgs[i] = dev.buildOnce(t, src, tools, dev.registry()+"/"+dev.prefix+"/inventory-"+randHex(3))
			}()
		}
		wg.Wait()
		first, second := imgs[0], imgs[1]
		if first.digest == "" || second.digest == "" {
			t.FailNow()
		}
		if first.digest != second.digest {
			t.Fatalf("one commit built twice produced two images: %s and %s", first.digest, second.digest)
		}
		adopt(first) // the deployments below run the image whose build evidence was just verified
		for _, img := range []appImage{first, second} {
			m := img.meta
			if m.SourceCommit != src.commit || m.ToolImage != tools.ref || m.GoVersion != "1.27.2" || m.Image.Repository != img.repo || m.Image.Digest != img.digest ||
				len(m.Platforms) != 2 || m.Platforms[0] != "linux/amd64" || m.Platforms[1] != "linux/arm64" {
				t.Errorf("the build evidence does not identify source, tool image, platforms and digest: %+v", m)
			}
			dev.pullAndVerify(t, img.repo, img.digest)
		}
		out := dev.runImage(t, first.ref(), "version")
		if !strings.Contains(out, src.commit) {
			t.Fatalf("the image the registry holds must report the pinned commit when run restricted: %s", out)
		}
	})

	t.Run("preview_deploy_and_e2e", func(t *testing.T) {
		img := dev.build(t, src, tools)
		env := dev.newEnvironment(t, "preview")
		d := dev.newDeployment(t, env, src, tools, img, "1")
		res := d.deploy(nil, nil, "")
		if res.exit != 0 {
			d.diagnose()
		}
		res.must(t, "deploy-preview.sh")
		dep := readDeployed(t, res)

		wantID := factoryfake.DeploymentID(d.snap.Tenant, d.snap.Project, d.action)
		if dep.DeploymentID != wantID || res.output("deployment_id") != wantID {
			t.Fatalf("the deployment identity is not the logical action's: %s / %s, want %s", dep.DeploymentID, res.output("deployment_id"), wantID)
		}
		if dep.Source.Commit != src.commit || dep.Image.Digest != img.digest || dep.Image.Repository != img.repo || dep.ToolImage != tools.ref ||
			dep.ConfigurationDigest != mustDigest(t, d.snap) || dep.Namespace != env.ns || dep.ExpiresAt == "" || len(dep.Objects) != 7 {
			t.Fatalf("incomplete deployment evidence: %+v", dep)
		}
		live := d.objectUIDs()
		for _, o := range dep.Objects {
			if o.UID == "" || o.Ready == nil || !*o.Ready || live[o.Kind+"/"+o.Name] != o.UID {
				t.Errorf("the evidence for %s/%s is not what the cluster holds (live uid %q): %+v", o.Kind, o.Name, live[o.Kind+"/"+o.Name], o)
			}
		}
		for _, name := range []string{"inventory-api", "inventory-worker"} {
			if got := d.liveImage(name); got != img.ref() {
				t.Errorf("%s runs %s, want %s", name, got, img.ref())
			}
		}
		if rec, ok := d.fac.Record(d.action); !ok || rec.Status != "succeeded" || rec.OutcomeUnknown {
			t.Fatalf("the factory record is not settled as succeeded: %+v", rec)
		}
		requireOrder(t, d.log.Events(), "factory:register:201", "factory:applying:200", "factory:succeeded:200")

		// Seed only the controlled application tenants; the factory grants no application access.
		d.seedAndSettle()
		url, stop := d.portForward()
		defer stop()
		reviewer, outsider := d.tokens()
		e2e := d.e2e(url, dep.DeploymentID, dep.ConfigurationDigest, reviewer, outsider)
		e2e.must(t, "e2e.sh")
		raw, err := os.ReadFile(filepath.Join(e2e.outputs, "e2e.json"))
		if err != nil {
			t.Fatal(err)
		}
		var rep struct {
			Passed          bool
			DeploymentID    string
			ImageDigest     string
			ResolutionState string
			Checks          []struct {
				ID     string
				Passed bool
			}
		}
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatal(err)
		}
		if !rep.Passed || rep.DeploymentID != dep.DeploymentID || rep.ImageDigest != img.digest {
			t.Fatalf("e2e evidence: %s", raw)
		}
		// The worker is operational: the resolution reached the ERP's acknowledgment, exactly once.
		if rep.ResolutionState != "resolved" {
			t.Fatalf("the deployed worker did not resolve the exception through the ERP: state %q\n%s", rep.ResolutionState, raw)
		}
		if n := env.erp.TotalEffects(); n != 1 {
			t.Fatalf("the ERP applied %d effects, want exactly 1", n)
		}
		seen := map[string]bool{}
		for _, c := range rep.Checks {
			seen[c.ID] = c.Passed
		}
		for _, id := range []string{"live-target-matches-deployment", "ready", "reviewer-creates-exception", "outsider-cannot-read", "exception-invisible-through-another-tenant",
			"outsider-cannot-resolve", "resolution-accepted", "resolution-replay-is-idempotent", "changed-payload-conflicts", "resolved-only-with-erp-acknowledgment"} {
			if !seen[id] {
				t.Errorf("e2e check %s did not pass (%v)", id, seen)
			}
		}
	})
}

// requireOrder asserts the events occur in the given relative order.
func requireOrder(t *testing.T, events []string, want ...string) {
	t.Helper()
	i := 0
	for _, e := range events {
		if i < len(want) && e == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("expected events %v in order, got %v", want, events)
	}
}
