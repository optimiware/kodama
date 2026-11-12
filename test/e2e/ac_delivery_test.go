//go:build e2e

package e2e

// AC08 delivery evidence: a digest-pinned image reference is recorded as a
// declared output, the next action runs the exact configured digest, a
// deploy-repository candidate records that same digest, and only the reviewed
// candidate moves the protected ref. The record is attributed through the
// execution's action transitions and the service's verified report. The output
// retry is submitted twice and must converge without a second effect.

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// TestE2E_AC08DeliveryEvidence proves digest publication, exact-digest testing,
// approved deploy-repository promotion without a rebuild, execution attribution
// and identical output-retry convergence on the documented installation.
func TestE2E_AC08DeliveryEvidence(t *testing.T) {
	e2eParallel(t)
	instance, repository := requireProtectedRepository(t, "AC08 delivery evidence", gitSlotDelivery)
	const targetRef = protectedTargetRef
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	reviewer := instance.smokeToken(t, harness.PersonaReviewer)
	reviewerPrincipal := smokePersonaPrincipal(t, instance, harness.PersonaReviewer)
	serviceID := instance.config.GitServiceID()
	ca := instance.materializeCA(t)

	digestReference := instance.image
	digest := instance.config.ImageDigest()
	if digest == "" {
		t.Fatalf("the installation tools image %q is not digest-pinned; AC08 needs an exact digest", digestReference)
	}
	names := smoke.NewNames(instance.runID, smoke.Delivery)
	seedWorkRef := names.WorkRef("seed")
	workRef := names.WorkRef("delivery")

	engineerGit := instance.newHostGit(t, harness.PersonaEngineer, "ac08")
	base, _, seedFinal, seeded := instance.smokeEnsureProtectedRepository(ctx, t, names,
		"TestE2E_AC08DeliveryEvidence-Seed", repository, engineer, reviewer, engineerGit, ca, targetRef, seedWorkRef)

	buildMarker := names.Marker("built")
	commitMarker := names.Marker("committed")
	testMarker := names.Marker("tested")
	integrateMarker := names.Marker("integrated")

	buildSubmission, err := harness.OutputSubmissionScript(map[string]string{"image": digestReference}, ca.path)
	if err != nil {
		t.Fatalf("build the AC08 digest submission: %v", err)
	}
	document, err := (harness.Document{
		Inputs: []string{"repository", "repository_path", "git_url", "target_ref", "expected_base", "work_ref"},
		Actions: []harness.Action{
			{
				Name:     "prepare",
				Image:    instance.image,
				Timeout:  "20m",
				Env:      smokeMergeEnv(ca.environment(), map[string]string{"SMOKE_RUN_MARKER": names.Marker("run"), "SMOKE_PREPARE_MARKER": names.Marker("prepared")}),
				Commands: ca.commands(smokeGitPrelude + "\n" + protectedPrepareScript),
			},
			{
				Name:    "build",
				Image:   digestReference,
				Timeout: "20m",
				Outputs: []string{"image"},
				Env:     smokeMergeEnv(ca.environment(), map[string]string{"SMOKE_BUILD_MARKER": buildMarker}),
				Commands: ca.commands(
					`printf '%s\n' "$SMOKE_BUILD_MARKER"`,
					buildSubmission,
				),
			},
			{
				Name:     "commit",
				Image:    instance.image,
				Timeout:  "20m",
				Env:      smokeMergeEnv(ca.environment(), map[string]string{"SMOKE_CA_FILE": ca.path, "SMOKE_IMAGE": digestReference, "SMOKE_COMMIT_MARKER": commitMarker}),
				Commands: ca.commands(smokeGitPrelude + "\n" + deliveryCommitScript),
			},
			{
				Name:     "test",
				Image:    digestReference,
				Timeout:  "20m",
				Outputs:  []string{"candidate", "test_result"},
				Env:      smokeMergeEnv(ca.environment(), map[string]string{"SMOKE_CA_FILE": ca.path, "SMOKE_IMAGE": digestReference, "SMOKE_TEST_MARKER": testMarker}),
				Commands: ca.commands(smokeGitPrelude + "\n" + deliveryTestScript),
			},
			{
				Name:    "review",
				Request: "role/reviewer",
				Timeout: "10m",
				Subject: map[string]any{
					"operation":     "git.ref.update",
					"resource":      map[string]any{"from": "inputs.repository"},
					"ref":           map[string]any{"from": "inputs.target_ref"},
					"expected_base": map[string]any{"from": "inputs.expected_base"},
					"candidate":     map[string]any{"from": "test.outputs.candidate"},
					"artifact":      map[string]any{"from": "build.outputs.image"},
				},
			},
			{
				Name:     "integrate",
				Image:    instance.image,
				Timeout:  "20m",
				Env:      smokeMergeEnv(ca.environment(), map[string]string{"SMOKE_CA_FILE": ca.path, "SMOKE_RUN_MARKER": names.Marker("run"), "SMOKE_INTEGRATE_MARKER": integrateMarker}),
				Commands: ca.commands(smokeGitPrelude + "\n" + protectedIntegrateScript),
			},
		},
	}).Build()
	if err != nil {
		t.Fatalf("build the AC08 delivery pipeline: %v", err)
	}
	pipelineName := names.PipelineName("delivery")
	applied := instance.smokeApplyPipeline(ctx, t, engineer, pipelineName, document, names.Key("apply"))
	created := instance.smokeCall(ctx, t, engineer, api.MethodPost, "task.create", api.TaskCreateRequest{
		ProjectID:  instance.projectID,
		Title:      "W43a AC08 delivery " + names.Run,
		PipelineID: applied.PipelineID,
		Inputs: map[string]string{
			"repository":      repository.ResourceID,
			"repository_path": repository.Name,
			"git_url":         instance.config.GitWorkloadURL(),
			"target_ref":      targetRef,
			"expected_base":   base,
			"work_ref":        workRef,
		},
	}, names.Key("create"), nil)
	task := decodeResult[api.TaskCreateResult](t, created)
	executionID := ""
	t.Cleanup(func() {
		instance.smokeCleanupTask(t, task.TaskID, executionID)
	})
	open := instance.smokeTaskRead(ctx, t, engineer, task.TaskID)
	startedTask := instance.smokeTaskStart(ctx, t, engineer, api.TaskStartRequest{TaskID: task.TaskID}, &open.LifecycleVersion, names.Key("start"))
	executionID = startedTask.ExecutionID

	waiting := instance.smokeWaitActionRunning(ctx, t, engineer, executionID, "review", 12*time.Minute)
	prepare := actionByName(t, waiting, "prepare")
	build := actionByName(t, waiting, "build")
	commit := actionByName(t, waiting, "commit")
	tested := actionByName(t, waiting, "test")
	review := actionByName(t, waiting, "review")
	integrate := actionByName(t, waiting, "integrate")
	for _, action := range []api.ExecutionAction{prepare, build, commit, tested} {
		if action.State != "succeeded" {
			t.Fatalf("action %q state = %q before review; want succeeded", action.Name, action.State)
		}
	}
	if build.Outputs["image"] != digestReference {
		t.Fatalf("build recorded image = %q; want the exact digest reference %q", build.Outputs["image"], digestReference)
	}
	candidate := tested.Outputs["candidate"]
	if err := smoke.CheckDistinctCommits(base, candidate, false); err != nil {
		t.Fatalf("test candidate output: %v", err)
	}
	if got, want := tested.Outputs["test_result"], "pass:"+candidate; got != want {
		t.Fatalf("test_result output = %q; want %q", got, want)
	}
	if review.Kind != "request" || review.State != "running" {
		t.Fatalf("review action = kind %q state %q; want request/running", review.Kind, review.State)
	}
	if integrate.State != "pending" {
		t.Fatalf("integrate action state = %q while review waits; want pending", integrate.State)
	}
	subject := map[string]string{
		"operation":     "git.ref.update",
		"resource":      repository.ResourceID,
		"ref":           targetRef,
		"expected_base": base,
		"candidate":     candidate,
		"artifact":      digestReference,
	}
	if err := smoke.CheckSubjectExact(review.Subject, subject); err != nil {
		t.Fatalf("review subject: %v", err)
	}

	// The digest is externally owned: the served repository candidate names the
	// same immutable digest the build recorded, and no action rebuilt it.
	manifestDirectory := engineerGit.dir + "/manifest"
	engineerGit.init(ctx, t, manifestDirectory)
	engineerGit.fetch(ctx, t, manifestDirectory, smokeRepositoryURL(instance.config.GitURL(), repository.Name), candidate)
	manifest, err := engineerGit.run(ctx, manifestDirectory, "show", "FETCH_HEAD:deploy/image.txt")
	if err != nil {
		t.Fatalf("read the deploy candidate manifest: %v", err)
	}
	if got := string(manifest); got != digestReference+"\n" {
		t.Fatalf("deploy candidate manifest = %q; want %q", got, digestReference+"\n")
	}

	reviewerInbox := instance.smokeTaskInbox(ctx, t, reviewer, "request")
	item, ok := smokeFindRequestInbox(reviewerInbox, executionID, review.ActionID)
	if !ok {
		t.Fatalf("reviewer inbox has no AC08 request %s/%s: %+v", executionID, review.ActionID, reviewerInbox.Items)
	}
	if err := smoke.CheckSubjectExact(item.Subject, subject); err != nil {
		t.Fatalf("reviewer inbox subject: %v", err)
	}
	if err := smoke.CheckMutationVersion(item.Version, review.Version); err != nil {
		t.Fatalf("reviewer inbox action version: %v", err)
	}
	resolveRequest := api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              review.ActionID,
		ExpectedActionVersion: item.Version,
		Decision:              "pass",
	}
	resolved := instance.smokeResolve(ctx, t, reviewer, resolveRequest, names.Key("resolve"))
	replayed := instance.smokeResolve(ctx, t, reviewer, resolveRequest, names.Key("resolve"))
	if err := smoke.CheckReplayConverged(resolved, replayed); err != nil {
		t.Fatalf("identical AC08 resolve replay: %v", err)
	}
	if resolved.ActionVersion <= item.Version {
		t.Fatalf("AC08 resolve reported action version %d; want > the %d observed while waiting", resolved.ActionVersion, item.Version)
	}

	final := instance.smokeWaitComplete(ctx, t, engineer, executionID, 10*time.Minute)
	if final.State != "succeeded" {
		smokeDumpLogs(t, instance, engineer, final)
	}
	requireSucceeded(t, final)
	finalTest := actionByName(t, final, "test")
	if finalTest.Outputs["candidate"] != candidate {
		t.Fatalf("final test candidate = %q; want the reviewed %q", finalTest.Outputs["candidate"], candidate)
	}
	finalReview := actionByName(t, final, "review")
	if err := smoke.CheckDecisionActor(finalReview, "pass", reviewerPrincipal); err != nil {
		t.Fatalf("AC08 review decision: %v", err)
	}
	if finalReview.Version < resolved.ActionVersion {
		t.Fatalf("AC08 final review action version %d is below the resolve version %d", finalReview.Version, resolved.ActionVersion)
	}
	finalBuild := actionByName(t, final, "build")
	if finalBuild.Outputs["image"] != digestReference {
		t.Fatalf("final build image = %q; want %q", finalBuild.Outputs["image"], digestReference)
	}
	for _, marker := range []struct {
		action string
		value  string
	}{
		{"build", buildMarker},
		{"commit", commitMarker},
		{"test", testMarker},
		{"integrate", integrateMarker},
	} {
		action := actionByName(t, final, marker.action)
		log := instance.smokeReadLogs(ctx, t, engineer, executionID, action.ActionID, "stdout")
		if err := smoke.CheckMarker(log, marker.value, 1); err != nil {
			t.Fatalf("%s marker: %v", marker.action, err)
		}
	}

	// The protected ref moved exactly from the reviewed base to the candidate,
	// with no rebuild and no extra commit.
	url := smokeRepositoryURL(instance.config.GitURL(), repository.Name)
	workDirectory := engineerGit.dir + "/readback"
	engineerGit.init(ctx, t, workDirectory)
	engineerGit.fetch(ctx, t, workDirectory, url, targetRef)
	if got := engineerGit.revParse(ctx, t, workDirectory, "FETCH_HEAD"); got != candidate {
		t.Fatalf("protected ref after promotion = %q; want the reviewed candidate %q", got, candidate)
	}
	if got := engineerGit.parent(ctx, t, workDirectory, "FETCH_HEAD"); got != base {
		t.Fatalf("promoted candidate parent = %q; want the reviewed base %q", got, base)
	}

	// Attribution: the build action owns a committed transition event, and the
	// service's verified report names the execution and the exact digest subject.
	transitions := instance.smokeEventReadAll(ctx, t, engineer, "action.transition")
	buildTransitions := 0
	for _, event := range transitions {
		if event.SubjectID == finalBuild.ActionID {
			buildTransitions++
		}
	}
	if buildTransitions == 0 {
		t.Fatalf("build action %s has no committed action.transition attribution", finalBuild.ActionID)
	}
	report := instance.smokeWaitReport(ctx, t, engineer, smoke.ReportWant{
		Resource:      repository.ResourceID,
		Ref:           targetRef,
		ExpectedBase:  base,
		Candidate:     candidate,
		ServiceID:     serviceID,
		CorrelationID: executionID,
	}, 3*time.Minute)

	finished := time.Now().UTC()
	instance.record(t, "AC08", "8.2", "TestE2E_AC08DeliveryEvidence", &final, started, finished, map[string]any{
		"candidate": candidate,
	})
	instance.record(t, "AC08", "8.3", "TestE2E_AC08DeliveryEvidence", &final, started, finished, map[string]any{
		"target_ref":      targetRef,
		"candidate":       candidate,
		"expected_base":   base,
		"verification_id": report.CausationID,
		"report_id":       report.ID,
	})
	instance.record(t, "AC08", "8.4", "TestE2E_AC08DeliveryEvidence", &final, started, finished, map[string]any{
		"action_id":    finalBuild.ActionID,
		"workspace_id": final.Cleanup.WorkspaceID,
	})
	instance.record(t, "AC08", "8.5", "TestE2E_AC08DeliveryEvidence", &final, started, finished, map[string]any{
		"candidate": candidate,
	})
	if seeded {
		instance.recordScenario(t, "DeliverySeed", "TestE2E_AC08DeliveryEvidence-Seed", &seedFinal, started, finished, map[string]any{
			"resource_id": repository.ResourceID,
			"target_ref":  targetRef,
			"candidate":   base,
		})
	}
}
