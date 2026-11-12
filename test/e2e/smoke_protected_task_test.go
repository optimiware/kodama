//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// testReleaseSmokeProtectedTask is S2. It first prepares the run-unique
// repository through the served Git service only: the host engineer pushes the
// initial commit B to a granted work ref, then a task-free request→integration
// pipeline creates the absent main ref at B after the reviewer's pass. It then
// runs one task, one execution and one workspace through five actions:
// external-harness implement, tools-image commit/test, the reviewer's inbox
// decision and the protected integration of the exact reviewed candidate.
//
// The caller owns the repository's protected ref (requireProtectedRepository).
func testReleaseSmokeProtectedTask(t *testing.T, instance *live, repository harness.GitRepository, names smoke.Names, probe string, recordScenario bool) api.ExecutionInspectResult {
	t.Helper()
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	reviewer := instance.smokeToken(t, harness.PersonaReviewer)
	reviewerPrincipal := smokePersonaPrincipal(t, instance, harness.PersonaReviewer)
	serviceID := instance.config.GitServiceID()
	ca := instance.materializeCA(t)

	const targetRef = protectedTargetRef
	seedWorkRef := names.WorkRef("seed")
	workRef := names.WorkRef("protected")

	instance.smokeWorkloadGitReadControl(ctx, t, names, repository, engineer, ca, targetRef)

	engineerGit := instance.newHostGit(t, harness.PersonaEngineer, "engineer")
	base, seedExecutionID, seedFinal, seeded := instance.smokeEnsureProtectedRepository(
		ctx, t, names, probe+"-Seed", repository, engineer, reviewer, engineerGit, ca, targetRef, seedWorkRef)
	if seeded {
		seedEvent := instance.smokeWaitReport(ctx, t, engineer, smoke.ReportWant{
			Resource:      repository.ResourceID,
			Ref:           targetRef,
			ExpectedBase:  "",
			Candidate:     base,
			ServiceID:     serviceID,
			CorrelationID: seedExecutionID,
		}, 3*time.Minute)
		if recordScenario {
			instance.recordScenario(t, "ProtectedTaskSeed", probe+"-Seed", &seedFinal, started, time.Now().UTC(), map[string]any{
				"candidate":       base,
				"expected_base":   "none",
				"target_ref":      targetRef,
				"resource_id":     repository.ResourceID,
				"verification_id": seedEvent.CausationID,
				"report_id":       seedEvent.ID,
			})
		}
	}

	runMarker := names.Marker("run")
	prepareMarker := names.Marker("prepared")
	implementMarker := names.Marker("implemented")
	testMarker := names.Marker("tested")
	integrateMarker := names.Marker("integrated")
	pipelineName := names.PipelineName("protected-task")
	inputs := map[string]string{
		"repository":      repository.ResourceID,
		"repository_path": repository.Name,
		"git_url":         instance.config.GitWorkloadURL(),
		"target_ref":      targetRef,
		"expected_base":   base,
		"work_ref":        workRef,
	}
	environment := func(markers map[string]string) map[string]string {
		return smokeMergeEnv(ca.environment(), map[string]string{
			"SMOKE_CA_FILE":    ca.path,
			"SMOKE_RUN_MARKER": runMarker,
		}, markers)
	}
	testCommands := append(ca.commands(smokeGitPrelude+"\n"+protectedTestScript),
		smokeOutputSubmissionCommands(t,
			map[string]string{"candidate": "__W49_CANDIDATE__", "test_result": "pass:__W49_CANDIDATE__"},
			ca.path,
			[]smokeSubstitution{{Placeholder: "__W49_CANDIDATE__", Shell: "$candidate"}},
		)...)
	testCommands = append(testCommands, `printf '%s\n' "$SMOKE_TEST_MARKER"`)

	document, err := (harness.Document{
		Inputs: []string{"repository", "repository_path", "git_url", "target_ref", "expected_base", "work_ref"},
		Actions: []harness.Action{
			{
				Name:     "prepare",
				Image:    instance.image,
				Timeout:  "20m",
				Env:      environment(map[string]string{"SMOKE_PREPARE_MARKER": prepareMarker}),
				Commands: ca.commands(smokeGitPrelude + "\n" + protectedPrepareScript),
			},
			{
				Name:     "implement",
				Image:    instance.config.ExternalHarnessReference(),
				Timeout:  "20m",
				Env:      smokeMergeEnv(map[string]string{"SMOKE_RUN_MARKER": runMarker, "SMOKE_IMPLEMENT_MARKER": implementMarker}),
				Commands: []string{smokeGitPrelude + "\n" + protectedImplementScript},
			},
			{
				Name:     "test",
				Image:    instance.image,
				Timeout:  "20m",
				Outputs:  []string{"candidate", "test_result"},
				Env:      environment(map[string]string{"SMOKE_TEST_MARKER": testMarker}),
				Commands: testCommands,
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
				},
			},
			{
				Name:     "integrate",
				Image:    instance.image,
				Timeout:  "20m",
				Env:      environment(map[string]string{"SMOKE_INTEGRATE_MARKER": integrateMarker}),
				Commands: ca.commands(smokeGitPrelude + "\n" + protectedIntegrateScript),
			},
		},
	}).Build()
	if err != nil {
		t.Fatalf("build the S2 protected-task pipeline: %v", err)
	}
	applied := instance.smokeApplyPipeline(ctx, t, engineer, pipelineName, document, names.Key("apply"))
	createdResult := instance.smokeCall(ctx, t, engineer, api.MethodPost, "task.create", api.TaskCreateRequest{
		ProjectID:  instance.projectID,
		Title:      "W49 protected task " + names.Run,
		PipelineID: applied.PipelineID,
		Inputs:     inputs,
	}, names.Key("create"), nil)
	created := decodeResult[api.TaskCreateResult](t, createdResult)
	executionID := ""
	t.Cleanup(func() {
		instance.smokeCleanupTask(t, created.TaskID, executionID)
	})
	open := instance.smokeTaskRead(ctx, t, engineer, created.TaskID)
	if open.State != "open" || open.CurrentExecution != "" || len(open.ExecutionHistory) != 0 {
		t.Fatalf("created task = state %q current %q history %v; want open with no execution", open.State, open.CurrentExecution, open.ExecutionHistory)
	}
	if open.SelectedPipeline != applied.PipelineID {
		t.Fatalf("created task pipeline = %q; want %q", open.SelectedPipeline, applied.PipelineID)
	}
	startedTask := instance.smokeTaskStart(ctx, t, engineer, api.TaskStartRequest{TaskID: created.TaskID}, &open.LifecycleVersion, names.Key("start"))
	executionID = startedTask.ExecutionID

	// Wait for the review action. Reaching it proves prepare, implement and
	// test all succeeded, and that the declared candidate output settled.
	waiting := instance.smokeWaitActionRunning(ctx, t, engineer, executionID, "review", 10*time.Minute)
	prepare := actionByName(t, waiting, "prepare")
	implement := actionByName(t, waiting, "implement")
	tested := actionByName(t, waiting, "test")
	review := actionByName(t, waiting, "review")
	integrate := actionByName(t, waiting, "integrate")
	for _, action := range []api.ExecutionAction{prepare, implement, tested} {
		if action.State != "succeeded" {
			t.Fatalf("action %q state = %q before the review; want succeeded", action.Name, action.State)
		}
	}
	if review.Kind != "request" || review.State != "running" {
		t.Fatalf("review action = kind %q state %q; want request/running", review.Kind, review.State)
	}
	if integrate.State != "pending" {
		t.Fatalf("integrate action state = %q while review is pending; want pending", integrate.State)
	}
	candidate := tested.Outputs["candidate"]
	if err := smoke.CheckDistinctCommits(base, candidate, false); err != nil {
		t.Fatalf("test candidate output: %v", err)
	}
	if got := tested.Outputs["test_result"]; got != "pass:"+candidate {
		t.Fatalf("test_result output = %q; want pass:%s", got, candidate)
	}
	task := instance.smokeTaskRead(ctx, t, engineer, created.TaskID)
	if task.State != "open" || task.CurrentExecution != executionID {
		t.Fatalf("task during review = state %q current %q; want open/%q", task.State, task.CurrentExecution, executionID)
	}
	subject := map[string]string{
		"operation":     "git.ref.update",
		"resource":      repository.ResourceID,
		"ref":           targetRef,
		"expected_base": base,
		"candidate":     candidate,
	}
	if err := smoke.CheckSubjectExact(review.Subject, subject); err != nil {
		t.Fatalf("review subject: %v", err)
	}

	reviewerInbox := instance.smokeTaskInbox(ctx, t, reviewer, "request")
	item, ok := smokeFindRequestInbox(reviewerInbox, executionID, review.ActionID)
	if !ok {
		t.Fatalf("reviewer inbox has no request %s/%s: %+v", executionID, review.ActionID, reviewerInbox.Items)
	}
	if item.Assignment != "role/reviewer" {
		t.Fatalf("reviewer inbox assignment = %q; want role/reviewer", item.Assignment)
	}
	if item.Version != review.Version {
		t.Fatalf("reviewer inbox version = %d; want the running action version %d", item.Version, review.Version)
	}
	if err := smoke.CheckSubjectExact(item.Subject, subject); err != nil {
		t.Fatalf("reviewer inbox subject: %v", err)
	}
	engineerInbox := instance.smokeTaskInbox(ctx, t, engineer, "request")
	if _, found := smokeFindRequestInbox(engineerInbox, executionID, review.ActionID); found {
		t.Fatalf("engineer inbox exposed the reviewer-assigned request: %+v", engineerInbox.Items)
	}

	// The host engineer's direct push of the exact candidate to the protected
	// ref is refused: humans carry no approval evidence. The ref stays at B.
	// The seed step only runs when the protected base is absent, so the fetch
	// directory is created here instead of relying on the seed checkout.
	seedDirectory := filepath.Join(engineerGit.dir, "seed")
	engineerGit.init(ctx, t, seedDirectory)
	if got := engineerGit.fetch(ctx, t, seedDirectory, smokeRepositoryURL(instance.config.GitURL(), repository.Name), workRef); got != candidate {
		t.Fatalf("fetched work ref = %q; want the test candidate %q", got, candidate)
	}
	directOutput, directErr := engineerGit.push(ctx, seedDirectory,
		smokeRepositoryURL(instance.config.GitURL(), repository.Name), "FETCH_HEAD:"+targetRef)
	if directErr == nil {
		t.Fatalf("the host engineer's direct push to %s was accepted", targetRef)
	}
	if err := smoke.CheckHookRejected(directOutput); err != nil {
		t.Fatalf("the host engineer's direct push: %v; output=%s", err, scanSafeHostGitOutput(directOutput, engineerGit.secrets))
	}
	if main := engineerGit.lsRemote(ctx, t, smokeRepositoryURL(instance.config.GitURL(), repository.Name), targetRef); main != base {
		t.Fatalf("protected main after the engineer's refusal = %q; want the reviewed base %q", main, base)
	}

	// The engineer cannot settle the reviewer-assigned request. The refusal is
	// the public forbidden class and commits nothing.
	engineerResolve := api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              review.ActionID,
		ExpectedActionVersion: item.Version,
		Decision:              "pass",
	}
	_, refusalErr := instance.invoke(ctx, engineer, api.MethodPost, "execution.request.resolve", engineerResolve, names.Key("engineer-resolve"), nil)
	var refusal *harness.APIError
	if !errors.As(refusalErr, &refusal) || refusal.Status != 403 || refusal.Code != api.CodeForbidden {
		t.Fatalf("engineer resolve of the reviewer request = %v; want HTTP 403 %q", refusalErr, api.CodeForbidden)
	}
	afterRefusal := instance.smokeInspect(ctx, t, engineer, executionID)
	stillWaiting := actionByName(t, afterRefusal, "review")
	if stillWaiting.State != "running" || stillWaiting.Version != review.Version {
		t.Fatalf("engineer refusal left review = state %q version %d; want running at %d", stillWaiting.State, stillWaiting.Version, review.Version)
	}
	if stillWaiting.Decision != "" || stillWaiting.Actor != "" || stillWaiting.AnsweredAt != nil {
		t.Fatalf("engineer refusal recorded a decision: %+v", stillWaiting)
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
		t.Fatalf("identical resolve replay: %v", err)
	}

	final := instance.smokeWaitComplete(ctx, t, engineer, executionID, 10*time.Minute)
	if final.State != "succeeded" {
		smokeDumpLogs(t, instance, engineer, final)
	}
	requireSucceeded(t, final)
	if final.ExecutionID != executionID {
		t.Fatalf("terminal execution = %q; want the admitted %q", final.ExecutionID, executionID)
	}
	namesInOrder := actionNames(final)
	wantOrder := []string{"prepare", "implement", "test", "review", "integrate"}
	if len(namesInOrder) != len(wantOrder) {
		t.Fatalf("execution actions = %v; want exactly %v", namesInOrder, wantOrder)
	}
	for index := range wantOrder {
		if namesInOrder[index] != wantOrder[index] {
			t.Fatalf("execution actions = %v; want %v", namesInOrder, wantOrder)
		}
	}
	for _, name := range wantOrder {
		if action := actionByName(t, final, name); action.State != "succeeded" {
			t.Fatalf("action %q state = %q; want succeeded", name, action.State)
		}
	}
	finalReview := actionByName(t, final, "review")
	if err := smoke.CheckDecisionActor(finalReview, "pass", reviewerPrincipal); err != nil {
		t.Fatalf("resolved review: %v", err)
	}
	if err := smoke.CheckSubjectEqual(review.Subject, finalReview.Subject); err != nil {
		t.Fatalf("review subject changed after resolution: %v", err)
	}
	finalIntegrate := actionByName(t, final, "integrate")
	finalTest := actionByName(t, final, "test")
	if got := finalTest.Outputs["candidate"]; got != candidate {
		t.Fatalf("final test candidate output = %q; want the reviewed %q", got, candidate)
	}
	if got := finalTest.Outputs["test_result"]; got != "pass:"+candidate {
		t.Fatalf("final test_result output = %q; want pass:%s", got, candidate)
	}
	requireOpaqueIdentity(t, "cleanup workspace", final.Cleanup.WorkspaceID)

	finalTask := instance.smokeWaitTaskState(ctx, t, engineer, created.TaskID, "done", 60*time.Second)
	if err := smoke.CheckExactHistory(finalTask.ExecutionHistory, executionID); err != nil {
		t.Fatalf("task history: %v", err)
	}
	if finalTask.CurrentExecution != "" && finalTask.CurrentExecution != executionID {
		t.Fatalf("task current execution = %q; want %q", finalTask.CurrentExecution, executionID)
	}

	// Every workload action's marker appears exactly once in its own retained
	// stream, proving one execution and one retained workspace.
	for _, marker := range []struct {
		action string
		value  string
	}{
		{"prepare", prepareMarker},
		{"implement", implementMarker},
		{"test", testMarker},
		{"integrate", integrateMarker},
	} {
		action := actionByName(t, final, marker.action)
		log := instance.smokeReadLogs(ctx, t, engineer, executionID, action.ActionID, "stdout")
		if err := smoke.CheckMarker(log, marker.value, 1); err != nil {
			t.Fatalf("%s marker: %v", marker.action, err)
		}
	}

	// The host engineer reads the protected ref at exactly C with parent B.
	url := smokeRepositoryURL(instance.config.GitURL(), repository.Name)
	fetched := engineerGit.fetch(ctx, t, seedDirectory, url, targetRef)
	if fetched != candidate {
		t.Fatalf("fetched protected main = %q; want the candidate %q", fetched, candidate)
	}
	if parent := engineerGit.parent(ctx, t, seedDirectory, "FETCH_HEAD"); parent != base {
		t.Fatalf("candidate %s parent = %q; want the reviewed base %q", candidate, parent, base)
	}

	want := smoke.ReportWant{
		Resource:      repository.ResourceID,
		Ref:           targetRef,
		ExpectedBase:  base,
		Candidate:     candidate,
		ServiceID:     serviceID,
		CorrelationID: executionID,
	}
	report := instance.smokeWaitReport(ctx, t, engineer, want, 3*time.Minute)

	finished := time.Now().UTC()
	if recordScenario {
		instance.recordScenario(t, "ProtectedTask", probe, &final, started, finished, map[string]any{
			"task_id":                 created.TaskID,
			"action_id":               finalIntegrate.ActionID,
			"workspace_id":            final.Cleanup.WorkspaceID,
			"resource_id":             repository.ResourceID,
			"target_ref":              targetRef,
			"candidate":               candidate,
			"expected_base":           base,
			"decision":                "pass",
			"actor":                   reviewerPrincipal,
			"verification_id":         report.CausationID,
			"report_id":               report.ID,
			"external_harness_digest": instance.config.ExternalHarnessDigest(),
		})
	}
	return final
}

// smokeWorkloadGitReadControl proves a fresh execution's mounted credential
// reads the served repository through the workload Git HTTPS URL and the public
// CA. It is the positive control for the Git edge and the credential path.
func (instance *live) smokeWorkloadGitReadControl(
	ctx context.Context,
	t *testing.T,
	names smoke.Names,
	repository harness.GitRepository,
	engineer harness.Secret,
	ca *workloadCA,
	targetRef string,
) {
	t.Helper()
	document, err := (harness.Document{
		Actions: []harness.Action{{
			Name:    "read",
			Image:   instance.image,
			Timeout: "20m",
			Env: smokeMergeEnv(ca.environment(), map[string]string{
				"SMOKE_CA_FILE":    ca.path,
				"SMOKE_GIT_URL":    instance.config.GitWorkloadURL(),
				"SMOKE_REPOSITORY": repository.Name,
				"SMOKE_TARGET_REF": targetRef,
			}),
			Commands: ca.commands(smokeGitPrelude + "\n" + gitReadControlScript),
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build the workload Git read control: %v", err)
	}
	admitted := instance.smokeStartInline(ctx, t, engineer, document, names.Key("git-read-control"))
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, admitted.ExecutionID)
	})
	final := instance.smokeWaitComplete(ctx, t, engineer, admitted.ExecutionID, 6*time.Minute)
	if final.State != "succeeded" {
		smokeDumpLogs(t, instance, engineer, final)
	}
	requireSucceeded(t, final)
	action := actionByName(t, final, "read")
	log := instance.smokeReadLogs(ctx, t, engineer, admitted.ExecutionID, action.ActionID, "stdout")
	if err := smoke.CheckMarker(log, "w49-git-read-control=ok", 1); err != nil {
		t.Fatalf("workload Git read control: %v", err)
	}
}

// smokeEnsureProtectedRepository returns the current protected base, seeding
// the absent ref through the public seed pipeline only when it does not exist.
// Proofs that share a repository hold its protected ref in turn, so each one
// builds on the current main commit instead of racing a second create-only
// seed.
func (instance *live) smokeEnsureProtectedRepository(
	ctx context.Context,
	t *testing.T,
	names smoke.Names,
	probe string,
	repository harness.GitRepository,
	engineer, reviewer harness.Secret,
	engineerGit *hostGit,
	ca *workloadCA,
	targetRef, seedWorkRef string,
) (string, string, api.ExecutionInspectResult, bool) {
	t.Helper()
	url := smokeRepositoryURL(instance.config.GitURL(), repository.Name)
	if current := engineerGit.lsRemote(ctx, t, url, targetRef); current != "" {
		return current, "", api.ExecutionInspectResult{}, false
	}
	base, executionID, final := instance.smokeSeedProtectedRepository(
		ctx, t, names, probe, repository, engineer, reviewer, engineerGit, ca, targetRef, seedWorkRef)
	return base, executionID, final, true
}

// smokeSeedProtectedRepository prepares the run-unique repository through the
// public API and the served Git service: it asserts the committed admin policy,
// seeds commit B on a granted work ref as the host engineer, then creates the
// absent protected target ref at B exactly once through a task-free
// request→integration pipeline after the reviewer's pass. It never writes bare
// state or weakens the policy.
func (instance *live) smokeSeedProtectedRepository(
	ctx context.Context,
	t *testing.T,
	names smoke.Names,
	probe string,
	repository harness.GitRepository,
	engineer, reviewer harness.Secret,
	engineerGit *hostGit,
	ca *workloadCA,
	targetRef, seedWorkRef string,
) (string, string, api.ExecutionInspectResult) {
	t.Helper()
	adminGit := instance.newHostGit(t, harness.PersonaAdmin, "admin")
	adminRepository := instance.smokeAdminRepository(t)
	adminDirectory := filepath.Join(adminGit.dir, "admin")
	adminGit.clone(ctx, t, smokeRepositoryURL(instance.config.GitURL(), adminRepository.Name), adminDirectory)
	policy, err := readHostFile(filepath.Join(adminDirectory, "conf", "git.conf"))
	if err != nil {
		t.Fatalf("read the committed admin policy: %v", err)
	}
	if err := smoke.CheckRepositoryPolicy(policy, repository.Name); err != nil {
		t.Fatalf("the committed admin policy for %q: %v", repository.Name, err)
	}

	seedDirectory := filepath.Join(engineerGit.dir, "seed")
	engineerGit.init(ctx, t, seedDirectory)
	base := engineerGit.commit(ctx, t, seedDirectory, "w49-seed.txt", names.Marker("seed")+"\n", "W49 repository seed")
	url := smokeRepositoryURL(instance.config.GitURL(), repository.Name)
	if _, err := engineerGit.push(ctx, seedDirectory, url, base+":"+seedWorkRef); err != nil {
		t.Fatalf("the host engineer could not seed the granted work ref %s: %v", seedWorkRef, err)
	}
	if got := engineerGit.lsRemote(ctx, t, url, seedWorkRef); got != base {
		t.Fatalf("seed work ref = %q; want the host commit %q", got, base)
	}

	seedMarker := names.Marker("seed-integrated")
	document, err := (harness.Document{
		Inputs: []string{"repository", "repository_path", "git_url", "target_ref", "expected_base", "candidate", "work_ref"},
		Actions: []harness.Action{
			{
				Name:    "review",
				Request: "role/reviewer",
				Timeout: "10m",
				Subject: map[string]any{
					"operation":     "git.ref.update",
					"resource":      map[string]any{"from": "inputs.repository"},
					"ref":           map[string]any{"from": "inputs.target_ref"},
					"expected_base": map[string]any{"from": "inputs.expected_base"},
					"candidate":     map[string]any{"from": "inputs.candidate"},
				},
			},
			{
				Name:    "integrate",
				Image:   instance.image,
				Timeout: "20m",
				Env: smokeMergeEnv(ca.environment(), map[string]string{
					"SMOKE_CA_FILE":     ca.path,
					"SMOKE_SEED_MARKER": seedMarker,
				}),
				Commands: ca.commands(smokeGitPrelude + "\n" + protectedSeedIntegrateScript),
			},
		},
	}).Build()
	if err != nil {
		t.Fatalf("build the repository seed pipeline: %v", err)
	}
	startResult := instance.smokeCall(ctx, t, engineer, api.MethodPost, "pipeline.start", api.PipelineStartRequest{
		ProjectID: instance.projectID,
		YAML:      document,
		Inputs: map[string]string{
			"repository":      repository.ResourceID,
			"repository_path": repository.Name,
			"git_url":         instance.config.GitWorkloadURL(),
			"target_ref":      targetRef,
			"expected_base":   "",
			"candidate":       base,
			"work_ref":        seedWorkRef,
		},
	}, names.Key("seed-start"), nil)
	admitted := decodeResult[api.PipelineStartResult](t, startResult)
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, admitted.ExecutionID)
	})

	waiting := instance.smokeWaitActionRunning(ctx, t, engineer, admitted.ExecutionID, "review", 6*time.Minute)
	reviewAction := actionByName(t, waiting, "review")
	item := instance.smokeWaitRequestInbox(ctx, t, reviewer, admitted.ExecutionID, reviewAction.ActionID, 3*time.Minute)
	if err := smoke.CheckSubjectExact(item.Subject, map[string]string{
		"operation":     "git.ref.update",
		"resource":      repository.ResourceID,
		"ref":           targetRef,
		"expected_base": "",
		"candidate":     base,
	}); err != nil {
		t.Fatalf("repository seed subject: %v", err)
	}
	instance.smokeResolve(ctx, t, reviewer, api.ExecutionRequestResolveRequest{
		ExecutionID:           admitted.ExecutionID,
		ActionID:              reviewAction.ActionID,
		ExpectedActionVersion: item.Version,
		Decision:              "pass",
	}, names.Key("seed-resolve"))
	final := instance.smokeWaitComplete(ctx, t, engineer, admitted.ExecutionID, 6*time.Minute)
	if final.State != "succeeded" {
		smokeDumpLogs(t, instance, engineer, final)
	}
	requireSucceeded(t, final)
	integrate := actionByName(t, final, "integrate")
	log := instance.smokeReadLogs(ctx, t, engineer, admitted.ExecutionID, integrate.ActionID, "stdout")
	if err := smoke.CheckMarker(log, seedMarker, 1); err != nil {
		t.Fatalf("repository seed marker: %v", err)
	}
	if got := engineerGit.lsRemote(ctx, t, url, targetRef); got != base {
		t.Fatalf("protected target ref after the seed = %q; want the seed commit %q", got, base)
	}
	return base, admitted.ExecutionID, final
}

// readHostFile reads one bounded host file.
func readHostFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return data, nil
}
