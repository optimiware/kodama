package delivery_test

import (
	"context"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/test/integration/delivery/stack"
)

// garbageKubeconfig is a credential that works against no cluster: a deployment
// that used it (a current version substituted for the pinned one) would fail.
const garbageKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: nowhere, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: nobody, user: {token: GARBAGE-SENTINEL}}]
contexts: [{name: nowhere, context: {cluster: nowhere, user: nobody}}]
current-context: nowhere
`

// Test_deployment_provenance_join — deployment_provenance_join (DQ-07–DQ-10,
// DQ-15/DQ-A11, PX-06/PX-07): admission -> persisted snapshot -> runtime context
// -> delivery -> real apply, with the credential materialized by the real
// runtime secret route. An environment or credential edit after admission leaves
// the admitted configuration unchanged everywhere it is visible; a missing or
// mismatched pin, and a revoked pinned credential, fail closed before anything is
// recorded or applied; the verified image, source and configuration the script
// reports are the ones the record and the live objects carry; and cleanup stays
// bound to the recorded deployment configuration and credential.
func Test_deployment_provenance_join(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("an_edit_after_admission_cannot_change_what_is_deployed", func(t *testing.T) {
		w := newWorld(t, "prov", worldOptions{})
		f := w.flow(stack.FlowOptions{})
		pin := f.Run.Snapshot.Environments[0].Pin
		admitted := pin.ConfigDigest

		// After admission: another namespace and configuration, and the credential
		// rotated to a value that opens no cluster.
		other := "t067-edited-" + randHex(2)
		cfg := map[string]string{"REPLICAS": "9"}
		env, err := w.S.Svc.UpdateEnvironment(ctx, stack.Caller(), w.scope, w.env.ID(), w.env.Version(), delivery.EnvironmentEdit{Namespace: &other, Config: &cfg})
		if err != nil {
			t.Fatal(err)
		}
		w.S.X.RotateSecret(w.scope, w.kubeSecret.ID(), garbageKubeconfig)
		current, err := w.S.Svc.ResolveSnapshot(ctx, w.scope, delivery.Actor{Type: delivery.ActorUser, ID: "u020_engineer"}, env.ID())
		if err != nil {
			t.Fatal(err)
		}
		if current.Digest == admitted || current.Namespace != other || current.Secrets[0].Version != 2 {
			t.Fatalf("the edits did not produce a different current configuration: %+v", current)
		}
		w.S.Restart()

		o := w.run(f)
		outs := w.requireCompleted(o)
		rec := w.mustDeployment(f)

		// The credential the script used is the pinned version 1: the rotated
		// version 2 would have failed against the cluster. The runtime route audited
		// exactly that resolution.
		if w.secretResolutions(o.Attempt.ID, 1) != 1 || w.secretResolutions(o.Attempt.ID, 2) != 0 {
			t.Fatalf("the attempt resolved v1=%d v2=%d", w.secretResolutions(o.Attempt.ID, 1), w.secretResolutions(o.Attempt.ID, 2))
		}
		// The record, the receipt, the live objects and the accepted result all
		// carry the admitted configuration and the verified inputs.
		if rec.ConfigDigest != admitted || rec.Namespace != w.ns.name || rec.Environment != w.env.ID() {
			t.Fatalf("the deployment was recorded against the edited environment: ns=%s digest=%s", rec.Namespace, rec.ConfigDigest)
		}
		want := map[string]string{
			"deployment_id": string(rec.ID), "deployment_status": "succeeded", "configuration_digest": string(admitted),
			"image_digest": string(f.Img.Digest), "source_commit": f.Source.Commit,
		}
		for k, v := range want {
			if outs[k] != v {
				t.Errorf("output %s = %v, want %s", k, outs[k], v)
			}
		}
		if outs["ready"] != true {
			t.Errorf("ready = %v", outs["ready"])
		}
		obj := w.requireAnnotated(rec, "deployment", "inventory")
		if got := annotationsOf(obj)[delivery.DefaultAnnotations.Configuration]; got != string(admitted) {
			t.Fatalf("the live Deployment carries configuration %q, want the admitted %q", got, admitted)
		}
		if w.countObjects(string(rec.ID)) != 2 {
			t.Fatal("the objects do not carry the deployment id")
		}
		if got := w.ns.admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.spec.template.spec.containers[0].image}"); !strings.HasSuffix(got, "@"+string(f.Img.Digest)) {
			t.Fatalf("the live workload runs %q, not the verified digest %s", got, f.Img.Digest)
		}
		pods := w.ns.admin(t, "get", "pods", "-l", "app=inventory", "-o", "jsonpath={.items[*].status.containerStatuses[*].imageID}")
		if !strings.Contains(pods, string(f.Img.Digest)) {
			t.Fatalf("no running container resolved the verified digest: %q", pods)
		}

		// The persisted row joins the producer and the source evidence.
		var artifact, producerAttempt, toolImage, commit, repo, imgDigest string
		for col, dst := range map[string]*string{"image_artifact_id": &artifact, "producer_attempt_id": &producerAttempt, "tool_image": &toolImage,
			"source_commit": &commit, "source_repository_url": &repo, "image_digest": &imgDigest} {
			w.S.Scalar(dst, `SELECT `+col+` FROM deployments WHERE tenant_id = $1 AND id = $2`, string(w.scope.TenantID), string(rec.ID))
		}
		if artifact != string(f.Image) || producerAttempt != string(f.Build.ID()) || toolImage != f.Build.Context.Command.Image ||
			commit != f.Source.Commit || repo != f.Source.RepositoryURL || imgDigest != string(f.Img.Digest) {
			t.Fatalf("the persisted provenance is not the verified one: artifact=%s producer=%s tool=%s commit=%s repo=%s digest=%s", artifact, producerAttempt, toolImage, commit, repo, imgDigest)
		}
		// The accepted result of the step names the same identities, and the run
		// is the one execution accepted it for.
		if o.Attempt.Result == nil || o.Output("deployment_id").String() != string(rec.ID) || o.Output("configuration_digest").String() != string(admitted) {
			t.Fatalf("the accepted result differs from the record: %+v", o.Attempt.Result)
		}
		if o.Agg.Run.Status != execution.RunSucceeded {
			t.Fatalf("the run is %s", o.Agg.Run.Status)
		}
	})

	t.Run("missing_or_mismatched_pins_fail_closed_before_anything_is_recorded_or_applied", func(t *testing.T) {
		w := newWorld(t, "pins", worldOptions{})
		wrongDigest := runtimev1.Digest("sha256:" + strings.Repeat("c", 64))
		cases := []struct {
			name string
			edit func(runtimev1.Context) (runtimev1.Context, error)
		}{
			{"an absent pin", func(runtimev1.Context) (runtimev1.Context, error) {
				return runtimev1.Context{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
			}},
			{"an environment pin naming an unretained configuration", func(c runtimev1.Context) (runtimev1.Context, error) {
				e := *c.Environment
				e.ConfigDigest = wrongDigest
				c.Environment = &e
				return c, nil
			}},
			{"a context without an environment", func(c runtimev1.Context) (runtimev1.Context, error) {
				c.Environment = nil
				return c, nil
			}},
			{"a credential binding at another version", func(c runtimev1.Context) (runtimev1.Context, error) {
				c.Secrets = append([]runtimev1.SecretBinding(nil), c.Secrets...)
				c.Secrets[0].Version = 99
				return c, nil
			}},
			{"a credential binding without the retained one", func(c runtimev1.Context) (runtimev1.Context, error) {
				c.Secrets = nil
				return c, nil
			}},
			{"a context of another run", func(c runtimev1.Context) (runtimev1.Context, error) {
				c.RunID = domain.RunID("run_t067_other")
				return c, nil
			}},
		}
		for _, tc := range cases {
			w.S.EditPins(tc.edit)
			f := w.flow(stack.FlowOptions{})
			o := w.run(f)
			if o.Attempt.Status != execution.AttemptFailed {
				t.Fatalf("%s: the attempt is %s\n%s", tc.name, o.Attempt.Status, tail(o.Logs, 1500))
			}
			t.Logf("%s: %s", tc.name, o.Reason())
			if n := w.deploymentRows(f); n != 0 || w.S.Deployments(w.scope) != 0 {
				t.Fatalf("%s recorded a deployment", tc.name)
			}
			if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
				t.Fatalf("%s applied objects", tc.name)
			}
		}

		// The control: the persisted pin, untouched, deploys once.
		w.S.EditPins(nil)
		f := w.flow(stack.FlowOptions{})
		w.requireCompleted(w.run(f))
		if n := w.deploymentRows(f); n != 1 {
			t.Fatalf("%d deployments after the control", n)
		}
	})

	t.Run("a_revoked_pinned_credential_fails_closed_without_cluster_effects", func(t *testing.T) {
		w := newWorld(t, "revoked", worldOptions{})
		f := w.flow(stack.FlowOptions{})
		// The credential's pinned version is revoked after admission and before the
		// step runs: the route must refuse it, and the script must never see the
		// cluster.
		if err := w.S.X.RevokeSecret(w.scope, w.kubeSecret.ID(), 1); err != nil {
			t.Fatal(err)
		}
		o := w.run(f)
		if o.Attempt.Status != execution.AttemptFailed {
			t.Fatalf("a step whose credential was revoked ended %s", o.Attempt.Status)
		}
		t.Logf("revoked credential: %s", o.Reason())
		if w.secretResolutions(o.Attempt.ID, 1) != 0 {
			t.Fatal("a revoked version was audited as delivered")
		}
		if w.deploymentRows(f) != 0 || w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
			t.Fatal("a step without its credential had effects")
		}
		if n := w.srv.Since(0); stack.CountCalls(n, "POST", "/deployments") != 0 {
			t.Fatal("the script reached the record interface")
		}
	})

	t.Run("cleanup_stays_bound_to_the_recorded_deployment_configuration_and_credential", func(t *testing.T) {
		w := newWorld(t, "provclean", worldOptions{})
		f, rec := w.settledFlowReplicas("inventory", 0)
		w.cleanupDefinition(rec)
		admitted := f.Run.Snapshot.Environments[0].Pin.ConfigDigest

		// The environment moves on: another namespace and another credential
		// reference, and the current version of the old credential is replaced by
		// one that opens no cluster.
		other := "t067-moved-" + randHex(2)
		w.S.Kubeconfig(w.scope, "kubeconfig-moved", garbageKubeconfig)
		cred := delivery.Credential{Name: delivery.DefaultKubeconfig, Ref: "secret://project/kubeconfig-moved", Delivery: runtimev1.DeliveryFile}
		if _, err := w.S.Svc.UpdateEnvironment(ctx, stack.Caller(), w.scope, w.env.ID(), w.env.Version(), delivery.EnvironmentEdit{Namespace: &other, Kubeconfig: &cred}); err != nil {
			t.Fatal(err)
		}
		w.S.X.RotateSecret(w.scope, w.kubeSecret.ID(), garbageKubeconfig)
		w.S.Restart()
		w.expire()
		it := w.requireAdmitted(w.scope, rec.ID)
		agg, err := w.S.ExecStore.Get(ctx, w.scope, it.RunID)
		if err != nil {
			t.Fatal(err)
		}
		run := agg.Run
		if run.Purpose != execution.PurposeCleanup || run.DeploymentID != string(rec.ID) || len(run.Snapshot.Environments) != 1 {
			t.Fatalf("the cleanup run: %+v", run)
		}
		if got := run.Snapshot.Environments[0].Pin; got.ConfigDigest != admitted || got.ID != w.env.ID() {
			t.Fatalf("cleanup pinned %+v, want the recorded configuration %s", got, admitted)
		}
		if len(run.Snapshot.Secrets) != 1 || run.Snapshot.Secrets[0].Binding.Ref != "secret://project/kubeconfig" || run.Snapshot.Secrets[0].Binding.Version != 1 {
			t.Fatalf("cleanup pinned credentials %+v, want the recorded kubeconfig credential", run.Snapshot.Secrets)
		}
		o := w.runCleanup(w.scope, it.RunID)
		if w.secretResolutions(o.Attempt.ID, 1) != 1 || w.secretResolutions(o.Attempt.ID, 2) != 0 {
			t.Fatalf("cleanup resolved v1=%d v2=%d", w.secretResolutions(o.Attempt.ID, 1), w.secretResolutions(o.Attempt.ID, 2))
		}
		if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
			t.Fatal("cleanup did not delete the recorded objects in the recorded namespace")
		}
		after := w.mustDeployment(f)
		if after.DeletedAt == nil || after.CleanupState != delivery.CleanupDone || after.Status != delivery.StatusSucceeded {
			t.Fatalf("the record after cleanup: %s", w.S.Describe(w.scope, rec.ID))
		}
	})

	t.Run("a_revoked_retained_credential_refuses_cleanup_admission_without_cluster_effects", func(t *testing.T) {
		w := newWorld(t, "clrevoked", worldOptions{})
		f, rec := w.settledFlowReplicas("inventory", 0)
		w.cleanupDefinition(rec)
		if err := w.S.X.RevokeSecret(w.scope, w.kubeSecret.ID(), 1); err != nil {
			t.Fatal(err)
		}
		w.expire()
		rep := w.sweep(w.scope)
		it, ok := item(rep, rec.ID)
		if !ok || it.Outcome != deliverycleanup.OutcomeRefused {
			t.Fatalf("the cleanup of a deployment whose retained credential is revoked: %+v", rep.Items)
		}
		if w.cleanupRuns(w.scope, rec.ID) != 0 {
			t.Fatal("a cleanup run was admitted over a revoked retained credential")
		}
		if w.ns.object(t, "deployment", "inventory") == nil || w.mustDeployment(f).DeletedAt != nil {
			t.Fatal("a refused cleanup changed the cluster or the record")
		}
	})
}
