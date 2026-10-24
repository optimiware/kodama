package delivery_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/test/integration/delivery/stack"
)

func (w *world) request(f *stack.Flow) delivery.RegistrationRequest {
	return delivery.RegistrationRequest{Source: f.Source, Image: delivery.Image{ArtifactID: f.Image, Digest: runtimev1.Digest(string(f.Img.Digest))}}
}

// Test_duplicate_deployment_key — duplicate_deployment_key (DQ-08, PX-06): one
// logical action owns exactly one deployment however it is claimed — concurrent
// registrations, a retry attempt, a restart, the human route — and the real
// scripts never apply a settled action twice (that replay is asserted through the
// recovery path in Test_apply_ack_loss). The API-level cases claim the step
// without launching a workload, so no command ran for them and a retry after
// them is truthfully a not-started one.
func Test_duplicate_deployment_key(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("concurrent_registrations_converge_on_one_record", func(t *testing.T) {
		w := newWorld(t, "dupconc", worldOptions{})
		f := w.flow(stack.FlowOptions{})
		a := w.S.DirectAttempt(f)
		req := w.request(f)

		const claimants = 8
		type outcome struct {
			rc  delivery.Receipt
			err error
		}
		results := make([]outcome, claimants)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rc, err := w.S.Svc.RegisterRuntime(ctx, a.ID(), a.Token, req)
				results[i] = outcome{rc, err}
			}()
		}
		close(start)
		wg.Wait()
		created := 0
		var id delivery.DeploymentID
		for i, r := range results {
			if r.err != nil {
				t.Fatalf("claimant %d: %v", i, r.err)
			}
			if r.rc.Created {
				created++
			}
			if id == "" {
				id = r.rc.Deployment.ID()
			}
			if r.rc.Deployment.ID() != id {
				t.Fatalf("claimants named different deployments: %s and %s", id, r.rc.Deployment.ID())
			}
		}
		if created != 1 {
			t.Fatalf("%d claimants created the deployment, want exactly one", created)
		}
		if n := w.deploymentRows(f); n != 1 {
			t.Fatalf("%d rows for one logical action", n)
		}
		if got := w.history(id); countOf(got, "registered") != 1 {
			t.Fatalf("the registration was recorded %d times: %v", countOf(got, "registered"), got)
		}
		if n := w.S.X.CountTenant(w.scope.TenantID, `SELECT count(*) FROM outbox WHERE event_type = 'deployment.state_changed'`); n != 1 {
			t.Fatalf("%d state-change events for one registration", n)
		}

		// A restart and the originating step's retry attempt observe the same record.
		w.S.Restart()
		retry := w.S.RetryDirect(f, a)
		rc, err := w.S.Svc.RegisterRuntime(ctx, retry.ID(), retry.Token, req)
		if err != nil || rc.Created || rc.Deployment.ID() != id {
			t.Fatalf("a retry attempt: created=%v id=%s err=%v", rc.Created, rc.Deployment.ID(), err)
		}
		// The human route for the same logical action observes it as well.
		rc, err = w.S.Svc.RegisterForAttempt(ctx, stack.Caller(), w.scope, retry.ID(), req)
		if err != nil || rc.Created || rc.Deployment.ID() != id {
			t.Fatalf("the human route: created=%v id=%s err=%v", rc.Created, rc.Deployment.ID(), err)
		}
		if n := w.deploymentRows(f); n != 1 {
			t.Fatalf("%d rows after the retries", n)
		}
	})

	t.Run("a_changed_request_is_refused_never_a_second_deployment", func(t *testing.T) {
		w := newWorld(t, "dupchg", worldOptions{})
		f := w.flow(stack.FlowOptions{})
		a := w.S.DirectAttempt(f)
		req := w.request(f)
		if _, err := w.S.Svc.RegisterRuntime(ctx, a.ID(), a.Token, req); err != nil {
			t.Fatal(err)
		}
		other := req
		other.Image.Digest = runtimev1.Digest("sha256:" + strings.Repeat("d", 64))
		if _, err := w.S.Svc.RegisterRuntime(ctx, a.ID(), a.Token, other); err == nil {
			t.Fatal("a registration with another image digest was accepted for the same action")
		}
		other = req
		other.Source.Commit = strings.Repeat("9", 40)
		if _, err := w.S.Svc.RegisterRuntime(ctx, a.ID(), a.Token, other); err == nil {
			t.Fatal("a registration with another source commit was accepted for the same action")
		}
		if n := w.deploymentRows(f); n != 1 {
			t.Fatalf("%d rows", n)
		}
	})

	t.Run("the_unique_action_key_is_enforced_by_the_schema_for_the_application_role", func(t *testing.T) {
		w := newWorld(t, "dupsql", worldOptions{})
		f := w.flow(stack.FlowOptions{})
		a := w.S.DirectAttempt(f)
		rc, err := w.S.Svc.RegisterRuntime(ctx, a.ID(), a.Token, w.request(f))
		if err != nil {
			t.Fatal(err)
		}
		err = w.S.X.RuntimeTx(w.scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO deployments (tenant_id, project_id, id, environment_id, config_digest, namespace, source_repository_url, source_commit,
image_artifact_id, image_digest, producer_attempt_id, producer_run_id, producer_step_id, tool_image, run_id, step_id, action_key, attempt_id, request_fingerprint,
status, reason, objects, expires_at, version, created_at, updated_at)
SELECT tenant_id, project_id, 'dep_t067_second', environment_id, config_digest, namespace, source_repository_url, source_commit,
image_artifact_id, image_digest, producer_attempt_id, producer_run_id, producer_step_id, tool_image, run_id, step_id, action_key, attempt_id, request_fingerprint,
status, reason, objects, expires_at, version, created_at, updated_at FROM deployments WHERE id = $1`, string(rc.Deployment.ID()))
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23505" {
			t.Fatalf("a second record for one action key was not refused as a unique violation: %v", err)
		}
	})

	t.Run("another_logical_action_for_the_same_image_is_another_deployment", func(t *testing.T) {
		w := newWorld(t, "duppromo", worldOptions{})
		f := w.flow(stack.FlowOptions{})
		a := w.S.DirectAttempt(f)
		first, err := w.S.Svc.RegisterRuntime(ctx, a.ID(), a.Token, w.request(f))
		if err != nil {
			t.Fatal(err)
		}
		promoted := w.S.Promote(f, w.env)
		pa := w.S.DirectAttempt(promoted)
		second, err := w.S.Svc.RegisterRuntime(ctx, pa.ID(), pa.Token, w.request(promoted))
		if err != nil || !second.Created || second.Deployment.ID() == first.Deployment.ID() {
			t.Fatalf("a promotion is its own deployment: %v created=%v", err, second.Created)
		}
		if n := w.S.Deployments(w.scope); n != 2 {
			t.Fatalf("%d deployments for two actions", n)
		}
		if first.Deployment.Record().Fingerprint == second.Deployment.Record().Fingerprint {
			t.Fatal("two actions share one request fingerprint")
		}
	})
}

// Test_wrong_image_source — wrong_image_source (DQ-01, DQ-07): through the real
// path a deployment is registered and applied only for an image that is a usable
// OCI output of a succeeded producer, of exactly the source and digest the
// admitted step was given. Anything else is refused by the admitted step's real
// script before the intent is recorded or any object is applied, and leaves no
// record.
func Test_wrong_image_source(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, w *world, what string, o stack.FlowOptions) {
		t.Helper()
		before := w.S.Deployments(w.scope)
		f := w.flow(o)
		out := w.run(f)
		if out.Attempt.Status != execution.AttemptFailed {
			t.Fatalf("%s: the attempt is %s, want a refusal\n%s", what, out.Attempt.Status, tail(out.Logs, 1500))
		}
		t.Logf("%s: %s", what, out.Reason())
		if after := w.S.Deployments(w.scope); after != before {
			t.Fatalf("%s left a deployment record", what)
		}
		if w.ns.object(t, "deployment", "inventory") != nil || w.ns.object(t, "configmap", "inventory-config") != nil {
			t.Fatalf("%s applied objects", what)
		}
	}

	t.Run("an_image_not_built_from_this_source_is_refused", func(t *testing.T) {
		w := newWorld(t, "wrongsrc", worldOptions{})
		other := artifacts.Provenance{RepositoryURL: "https://git.t067.invalid/other/repo.git", Commit: strings.Repeat("7", 40)}
		refused(t, w, "an image of another repository and commit", stack.FlowOptions{Provenance: &other})
	})

	t.Run("a_claimed_source_that_is_not_the_attempts_candidate_is_refused", func(t *testing.T) {
		w := newWorld(t, "claimsrc", worldOptions{})
		refused(t, w, "another commit", stack.FlowOptions{Env: map[string]string{"DEPLOY_COMMIT": strings.Repeat("8", 40)}})
		refused(t, w, "another repository", stack.FlowOptions{Env: map[string]string{"DEPLOY_REPOSITORY_URL": "https://git.t067.invalid/other/repo.git"}})
	})

	t.Run("an_image_that_is_not_an_accepted_output_of_a_succeeded_producer_is_refused", func(t *testing.T) {
		w := newWorld(t, "wrongout", worldOptions{})
		// The registered image is not what the build's accepted result names.
		refused(t, w, "a registered but unaccepted image", stack.FlowOptions{OutputOther: true, Other: w.S.FixtureImage("other")})
		refused(t, w, "an image of a failed build", stack.FlowOptions{BuildFails: true})
	})

	t.Run("an_image_with_another_digest_than_the_registered_one_is_refused", func(t *testing.T) {
		w := newWorld(t, "wrongdig", worldOptions{})
		wrong := strings.Replace(w.img.NodeRef, "@"+string(w.img.Digest), "@sha256:"+strings.Repeat("e", 64), 1)
		refused(t, w, "another digest than the registered one", stack.FlowOptions{Env: map[string]string{"DEPLOY_IMAGE": wrong}})
	})

	t.Run("manifests_running_another_image_never_reach_the_record", func(t *testing.T) {
		w := newWorld(t, "foreignimg", worldOptions{})
		bad := w.appManifests(1, "inventory")
		bad["app.yaml"] = strings.Replace(bad["app.yaml"], "@@IMAGE@@", "registry.example.invalid/other/app:latest", 1)
		refused(t, w, "a manifest with another image", stack.FlowOptions{Manifests: bad})

		// The control: the verified inputs deploy.
		f := w.flow(stack.FlowOptions{})
		w.requireCompleted(w.run(f))
		if n := w.deploymentRows(f); n != 1 {
			t.Fatalf("%d deployments", n)
		}
	})
}
