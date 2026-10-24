package realdelivery_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"example.invalid/inventory/internal/delivery"
	"example.invalid/inventory/test/support/factoryfake"
)

// chaos wraps the real kubectl adapter. Every call still reaches the real
// cluster: a lost apply response is real kubectl performing the effect and the
// caller then being told it failed; nothing here synthesizes an object, a UID or
// readiness.
type chaos struct {
	delivery.Cluster
	mu        sync.Mutex
	loseApply int  // applies that take effect, then report an error
	blind     bool // observation fails: the cluster cannot be asked
	blindOnce bool // the cluster becomes unobservable once a response was lost
	applies   int  // real applies performed
}

func (c *chaos) Apply(ctx context.Context, namespace string, list []byte) error {
	c.mu.Lock()
	c.applies++
	lose := c.loseApply > 0
	if lose {
		c.loseApply--
	}
	c.mu.Unlock()
	err := c.Cluster.Apply(ctx, namespace, list)
	if err == nil && lose {
		c.mu.Lock()
		c.blind = c.blind || c.blindOnce
		c.mu.Unlock()
		return errors.New("kubectl apply failed: connection reset by peer (the response was lost)")
	}
	return err
}

func (c *chaos) Get(ctx context.Context, ref delivery.ObjectRef) (delivery.Observed, bool, error) {
	c.mu.Lock()
	blind := c.blind
	c.mu.Unlock()
	if blind {
		return delivery.Observed{}, false, errors.New("kubectl get failed: Unable to connect to the server")
	}
	return c.Cluster.Get(ctx, ref)
}

func (c *chaos) setBlind(v bool) { c.mu.Lock(); c.blind = v; c.mu.Unlock() }
func (c *chaos) applyCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.applies }

// inprocess runs the deployer, the code the deploy scripts run, in the test
// process against the real cluster through the environment's namespace-scoped
// credential, the real registry and the factory fixture. The script wiring is
// covered by TestRealDelivery; this form makes the readiness bound short and
// lets a fault be injected between the helper and the real kubectl.
func (d *deployment) inprocess(cl *chaos, readiness time.Duration) delivery.Result {
	d.t.Helper()
	dir := d.t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		d.t.Fatal(err)
	}
	for name, v := range d.secrets {
		if err := os.WriteFile(filepath.Join(secrets, name), []byte(v+"\n"), 0o600); err != nil {
			d.t.Fatal(err)
		}
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("runtime-token-"+d.attempt+"\n"), 0o600); err != nil {
		d.t.Fatal(err)
	}
	out := filepath.Join(dir, "outputs")
	if err := os.MkdirAll(out, 0o700); err != nil {
		d.t.Fatal(err)
	}
	rt, err := delivery.NewRuntime(d.fac.URL(), token, d.attempt)
	if err != nil {
		d.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(d.ca)
	rt.HTTP = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	rt.Backoff = 20 * time.Millisecond
	dep := &delivery.Deployer{Factory: rt, In: delivery.Input{
		Kind: d.env.kind, SnapshotJSON: d.snapshotJSON(), SecretsDir: secrets, Getenv: os.Getenv, OutputsDir: out,
		ClusterFor: func(kubeconfig string) delivery.Cluster {
			cl.Cluster = &delivery.Kubectl{Binary: d.dev.kubectl, Kubeconfig: kubeconfig}
			return cl
		},
		Images: &delivery.Registry{Insecure: true, HTTP: &http.Client{Timeout: 20 * time.Second}}, ReadinessTimeout: readiness, PollInterval: time.Second,
	}}
	res, err := dep.Run(context.Background())
	if err != nil {
		d.t.Fatalf("an internal error, not a reportable outcome: %v", err)
	}
	return res
}

// liveNames lists the deployment-owned objects that exist in a namespace
// (administrator view; the namespace's own defaults are excluded).
func (e *devEnv) liveNames(t *testing.T, ns string) []string {
	t.Helper()
	out, err := e.kube("get", "configmap,secret,job,deployment,service", "--namespace", ns, "-o", "name")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range strings.Fields(out) {
		if n != "configmap/kube-root-ca.crt" && n != "service/erp" { // the namespace's own default and its ERP peer address
			names = append(names, n)
		}
	}
	return names
}

func (d *deployment) record() delivery.Record {
	d.t.Helper()
	rec, ok := d.fac.Record(d.action)
	if !ok {
		d.t.Fatal("the factory holds no record for the logical action")
	}
	return rec
}

func sameUIDs(t *testing.T, label string, before, after map[string]string) {
	t.Helper()
	if len(before) == 0 {
		t.Fatalf("%s: no objects were observed", label)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: %s changed from uid %q to %q", label, k, v, after[k])
		}
	}
	if len(after) != len(before) {
		t.Errorf("%s: %d objects before, %d after", label, len(before), len(after))
	}
}

func (d *deployment) evidenceMatchesCluster(res delivery.Result) {
	d.t.Helper()
	live := d.objectUIDs()
	if len(live) != 7 {
		d.t.Fatalf("the cluster holds %d of the 7 objects: %v", len(live), live)
	}
	for _, o := range d.record().Objects {
		if o.UID == "" || o.Ready == nil || !*o.Ready || live[o.Kind+"/"+o.Name] != o.UID {
			d.t.Errorf("the recorded %s/%s (uid %q) is not what the cluster holds (%q)", o.Kind, o.Name, o.UID, live[o.Kind+"/"+o.Name])
		}
	}
}

// TestRealDeliveryRecovery exercises lost callback responses, retries, restarts,
// authority and pin refusals, foreign objects, RBAC denial, migration failure and
// bounded readiness against the real cluster. The factory's record is the
// authority; the helper reconciles by observing the cluster, never by assumption.
func TestRealDeliveryRecovery(t *testing.T) {
	dev := requireDev(t)
	src := requireSource(t)
	tools := dev.tools(t, src)
	img := dev.build(t, src, tools)
	setup := func(t *testing.T, kind string) (*deployment, *chaos) {
		t.Helper()
		env := dev.newEnvironment(t, kind)
		return dev.newDeployment(t, env, src, tools, img, "1"), &chaos{}
	}
	const generous = 4 * time.Minute

	t.Run("lost_registration_and_apply_intent_responses_are_retried_to_one_record", func(t *testing.T) {
		d, cl := setup(t, "preview")
		d.fac.LoseResponse("register", 2)
		d.fac.LoseResponse("applying", 1)
		res := d.inprocess(cl, generous)
		if !res.Completed {
			d.diagnose()
			t.Fatalf("the deployment did not complete: %s", res.Reason)
		}
		if want := factoryfake.DeploymentID(d.snap.Tenant, d.snap.Project, d.action); res.DeploymentID != want || d.record().ID != want {
			t.Fatalf("the deployment identity is not the logical action's: %s / %s", res.DeploymentID, want)
		}
		if d.fac.Calls["register"] < 3 || d.fac.Calls["applying"] < 2 {
			t.Fatalf("the lost responses must be retried: %v", d.fac.Calls)
		}
		if cl.applyCount() != 1 {
			t.Fatalf("one logical deployment applies once, applied %d times", cl.applyCount())
		}
		d.evidenceMatchesCluster(res)
	})

	t.Run("a_lost_apply_response_is_settled_by_observation_without_a_second_apply", func(t *testing.T) {
		d, cl := setup(t, "preview")
		cl.loseApply = 1
		res := d.inprocess(cl, generous)
		if !res.Completed {
			d.diagnose()
			t.Fatalf("the deployment did not complete: %s", res.Reason)
		}
		if cl.applyCount() != 1 {
			t.Fatalf("a lost response must be settled by observation, not a second apply (applied %d times)", cl.applyCount())
		}
		requireOrder(t, d.log.Events(), "factory:register:201", "factory:applying:200", "factory:unknown:200", "factory:succeeded:200")
		if rec := d.record(); rec.Status != "succeeded" || rec.OutcomeUnknown {
			t.Fatalf("the record must be settled from the live objects: %+v", rec)
		}
		d.evidenceMatchesCluster(res)
	})

	t.Run("an_unreported_success_is_reconciled_by_a_retry_attempt_without_disturbing_objects", func(t *testing.T) {
		d, cl := setup(t, "preview")
		d.fac.Unavailable("succeeded", 5) // every bounded retry is refused: the live system is ready but unrecorded
		first := d.inprocess(cl, generous)
		if first.Completed || first.Reason != delivery.ReasonUnreported {
			d.diagnose()
			t.Fatalf("the unacknowledged success must not be reported complete: %+v", first)
		}
		if rec := d.record(); rec.Status == "succeeded" {
			t.Fatalf("the factory cannot have settled a report it never received: %+v", rec)
		}
		before := d.objectUIDs()
		gen := d.generation("inventory-api")
		d.attempt = "att_retry_" + randHex(3) // a new attempt of the same logical action
		d.install(nil)
		second := d.inprocess(cl, generous)
		if !second.Completed {
			d.diagnose()
			t.Fatalf("the retry attempt must reconcile the deployment: %s", second.Reason)
		}
		if second.DeploymentID != first.DeploymentID {
			t.Fatalf("a retry must not create a second deployment: %s vs %s", second.DeploymentID, first.DeploymentID)
		}
		sameUIDs(t, "objects across the retry", before, d.objectUIDs())
		if g := d.generation("inventory-api"); g != gen {
			t.Errorf("the retry must not change the live Deployment (generation %s -> %s)", gen, g)
		}
		d.evidenceMatchesCluster(second)
	})

	t.Run("an_unknown_outcome_is_observed_and_settled_never_blindly_reapplied", func(t *testing.T) {
		d, cl := setup(t, "preview")
		cl.loseApply, cl.blindOnce = 1, true // the effect happens, the response is lost, and the cluster can no longer be asked
		first := d.inprocess(cl, generous)
		if first.Completed || first.Reason != delivery.ReasonObservation {
			t.Fatalf("an unobservable apply must stay unsettled: %+v", first)
		}
		if rec := d.record(); !rec.OutcomeUnknown || rec.Status == "succeeded" || rec.Status == "failed" {
			t.Fatalf("the uncertainty must be recorded, not hidden: %+v", rec)
		}
		cl.setBlind(false)
		before := d.objectUIDs() // real objects exist: the first apply did take effect
		d.attempt = "att_restart_" + randHex(3)
		d.install(nil)
		second := d.inprocess(cl, generous)
		if !second.Completed {
			d.diagnose()
			t.Fatalf("the restarted attempt must observe and settle: %s", second.Reason)
		}
		if cl.applyCount() != 1 {
			t.Fatalf("an unknown outcome must never be blindly reapplied (applies: %d)", cl.applyCount())
		}
		sameUIDs(t, "objects across the restart", before, d.objectUIDs())
		if rec := d.record(); rec.Status != "succeeded" || rec.OutcomeUnknown {
			t.Fatalf("the observation must settle the record: %+v", rec)
		}
		d.evidenceMatchesCluster(second)
	})

	t.Run("revoked_authority_stops_before_any_effect", func(t *testing.T) {
		d, cl := setup(t, "preview")
		d.fac.RevokeAt("applying", d.attempt)
		res := d.inprocess(cl, generous)
		if res.Completed || res.Reason != delivery.ReasonAuthority {
			t.Fatalf("revoked authority must stop the deployment: %+v", res)
		}
		if names := dev.liveNames(t, d.env.ns); len(names) != 0 || cl.applyCount() != 0 {
			t.Fatalf("no effect may follow revoked authority: %v (applies %d)", names, cl.applyCount())
		}
	})

	t.Run("pins_that_do_not_match_are_refused_before_any_effect", func(t *testing.T) {
		d, cl := setup(t, "preview")
		refuse := func(label, reason string) {
			t.Helper()
			res := d.inprocess(cl, generous)
			if res.Completed || res.Reason != reason {
				t.Errorf("%s: want %s, got %+v", label, reason, res)
			}
			if names := dev.liveNames(t, d.env.ns); len(names) != 0 || cl.applyCount() != 0 {
				t.Errorf("%s: nothing may be applied: %v", label, names)
			}
			if n := d.fac.Calls["register"]; n != 0 {
				t.Errorf("%s: nothing may be registered before the pins verify (%d registrations)", label, n)
			}
		}
		// A binding the snapshot relies on is missing from the pinned context.
		d.install(func(c map[string]any) {
			var kept []map[string]any
			for _, b := range c["secrets"].([]map[string]any) {
				if b["name"] != "ERP_TOKEN" {
					kept = append(kept, b)
				}
			}
			c["secrets"] = kept
		})
		refuse("a missing credential binding", delivery.ReasonSnapshot)
		// The snapshot differs from the admitted configuration.
		d.install(nil)
		d.snap.Config["ERP_BASE_URL"] = "http://erp.elsewhere.invalid:9"
		refuse("an edited snapshot", delivery.ReasonSnapshot)
		// The pinned image digest is not in the registry.
		d.snap.Config["ERP_BASE_URL"] = d.env.erpURL
		d.install(func(c map[string]any) {
			c["inputs"].(map[string]any)["image_digest"] = "sha256:" + strings.Repeat("0", 64)
		})
		refuse("an image digest the registry does not hold", delivery.ReasonImage)
	})

	t.Run("a_credential_that_does_not_cover_the_namespace_is_denied_and_nothing_is_created_there", func(t *testing.T) {
		d, cl := setup(t, "preview")
		other := dev.newEnvironment(t, "stage") // an unrelated namespace the deployer's role does not cover
		d.snap.Namespace = other.ns
		d.install(nil)
		res := d.inprocess(cl, generous)
		if res.Completed {
			t.Fatalf("a deployment into a namespace the credential does not cover must not complete: %+v", res)
		}
		if res.Reason != delivery.ReasonObservation && res.Reason != delivery.ReasonApply {
			t.Fatalf("unexpected reason %s", res.Reason)
		}
		for _, ns := range []string{other.ns, d.env.ns} {
			if names := dev.liveNames(t, ns); len(names) != 0 {
				t.Fatalf("RBAC denial must leave no object in %s: %v", ns, names)
			}
		}
		if rec := d.record(); rec.Status == "succeeded" {
			t.Fatalf("the record must not claim success: %+v", rec)
		}
	})

	t.Run("a_foreign_object_is_never_adopted_or_modified", func(t *testing.T) {
		d, cl := setup(t, "preview")
		dev.apply(t, obj("ConfigMap", "v1", d.env.ns, "inventory-config", map[string]any{"data": map[string]any{"owner": "someone-else"}}))
		uid := d.objectUIDs()["ConfigMap/inventory-config"]
		res := d.inprocess(cl, generous)
		if res.Completed || res.Reason != delivery.ReasonForeign {
			t.Fatalf("a foreign object must stop the deployment: %+v", res)
		}
		if cl.applyCount() != 0 {
			t.Fatal("nothing may be applied over a foreign object")
		}
		o, err := dev.getJSON(d.env.kubeconfig, "configmap/inventory-config")
		if err != nil {
			t.Fatal(err)
		}
		md, _ := o["metadata"].(map[string]any)
		data, _ := o["data"].(map[string]any)
		if md["uid"] != uid || data["owner"] != "someone-else" || len(data) != 1 {
			t.Fatalf("the foreign object was modified: %v", o)
		}
		if names := dev.liveNames(t, d.env.ns); len(names) != 1 {
			t.Fatalf("only the foreign object may exist: %v", names)
		}
		if rec := d.record(); rec.Status != "failed" || rec.Reason != delivery.ReasonForeign {
			t.Fatalf("the refusal is recorded: %+v", rec)
		}
	})

	t.Run("a_failed_migration_is_reported_and_recorded_not_declared_ready", func(t *testing.T) {
		d, cl := setup(t, "preview")
		d.secrets["DATABASE_MIGRATION_URL"] = strings.Replace(d.env.db.ownerURL, d.env.db.ownerPW, "wrong-"+randHex(4), 1)
		res := d.inprocess(cl, generous)
		if res.Completed || res.Reason != delivery.ReasonMigration {
			d.diagnose()
			t.Fatalf("a failed migration Job must fail the deployment: %+v", res)
		}
		if rec := d.record(); rec.Status != "failed" || rec.Reason != delivery.ReasonMigration {
			t.Fatalf("the failure is recorded: %+v", rec)
		}
		// The application never served: its schema was never created.
		if out, err := dev.queryOne(d.env.db.name, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'"); err != nil || out != "0" {
			t.Fatalf("a failed migration left schema objects: %q %v", out, err)
		}
	})

	t.Run("readiness_is_bounded_and_a_failed_deployment_is_never_reapplied", func(t *testing.T) {
		d, cl := setup(t, "preview")
		d.secrets["DATABASE_RUNTIME_URL"] = strings.Replace(d.env.db.runtimeURL, d.env.db.runtimePW, "wrong-"+randHex(4), 1) // the API can never become ready
		begin := time.Now()
		res := d.inprocess(cl, 45*time.Second)
		elapsed := time.Since(begin)
		if res.Completed || res.Reason != delivery.ReasonRollout {
			d.diagnose()
			t.Fatalf("a deployment that never becomes ready must time out: %+v", res)
		}
		if elapsed > 3*time.Minute {
			t.Fatalf("the wait is bounded by the readiness timeout, took %s", elapsed)
		}
		if rec := d.record(); rec.Status != "failed" || rec.Reason != delivery.ReasonRollout {
			t.Fatalf("the failure is recorded: %+v", rec)
		}
		before := cl.applyCount()
		d.attempt = "att_after_failure_" + randHex(3)
		d.install(nil)
		again := d.inprocess(cl, 45*time.Second)
		if again.Completed || again.Reason != delivery.ReasonEarlierFailure || cl.applyCount() != before {
			t.Fatalf("a settled failure is not retried by repeating the action: %+v (applies %d -> %d)", again, before, cl.applyCount())
		}
	})
}

// generation returns a live Deployment's metadata.generation (any spec change bumps it).
func (d *deployment) generation(name string) string {
	d.t.Helper()
	out, err := d.dev.kube("get", "deployment", name, "--namespace", d.env.ns, "-o", "jsonpath={.metadata.generation}")
	if err != nil {
		d.t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// TestRealDeliveryScriptRefusal runs the deploy script itself, in the tools image,
// after the factory revoked the attempt: the script reports a structured failure
// and nothing exists in the namespace.
func TestRealDeliveryScriptRefusal(t *testing.T) {
	dev := requireDev(t)
	src := requireSource(t)
	tools := dev.tools(t, src)
	img := dev.build(t, src, tools)
	env := dev.newEnvironment(t, "preview")
	d := dev.newDeployment(t, env, src, tools, img, "1")
	d.fac.Revoke(d.attempt)
	res := d.deploy(nil, nil, "")
	if res.exit == 0 || res.outcome() != "failed" || res.reason() != "authority_lost" {
		t.Fatalf("the script must fail closed on revoked authority: exit %d %v\n%s", res.exit, res.manifest, tail(res.out, 2000))
	}
	if names := dev.liveNames(t, env.ns); len(names) != 0 {
		t.Fatalf("a revoked script left objects: %v", names)
	}
}
