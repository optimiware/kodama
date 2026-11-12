//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// smokeBindingID returns the configured smoke repository binding.
func (instance *live) smokeBindingID() string {
	if repository, ok := instance.config.Repository(); ok {
		return repository.BindingID
	}
	return ""
}

// smokeRequireNoPipelineExecutions polls one pipeline's execution list for a
// fixed settle window and requires it to stay empty throughout. The trigger
// loop runs about every second, so one read is not evidence of the exclusion;
// a late admission must fail before the human push.
func (instance *live) smokeRequireNoPipelineExecutions(ctx context.Context, t *testing.T, token harness.Secret, pipelineID string, window time.Duration) {
	t.Helper()
	if window < 5*time.Second {
		window = 5 * time.Second
	}
	deadline := time.Now().Add(window)
	reads := 0
	for {
		executions, err := instance.smokePipelineExecutionsResult(ctx, token, pipelineID)
		if err != nil {
			t.Fatalf("pre-human execution.list for pipeline %s: %v", pipelineID, err)
		}
		if err := smoke.CheckNoPipelineExecution(executions, pipelineID); err != nil {
			t.Fatalf("the execution-originated push admitted an execution before the human push: %v", err)
		}
		reads++
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if reads < 10 {
		t.Fatalf("the pre-human settlement window produced only %d execution-list reads; want at least 10", reads)
	}
}

// logTriggerDiagnostics renders the bounded push-trigger facts when the human
// event did not start the pipeline: any permanent refusal event, the full
// project execution list, the pipeline identity and the report attribution.
// It scans every rendered value for credentials first.
func (instance *live) logTriggerDiagnostics(ctx context.Context, t *testing.T, token, admin harness.Secret, pipelineID string, report api.Event) {
	t.Helper()
	reportData := string(report.Data)
	if scanErr := harness.ScanCredentials(reportData, instance.config.PersonaSecrets()...); scanErr != nil {
		reportData = "withheld"
	}
	if len(reportData) > 400 {
		reportData = reportData[:400]
	}
	t.Logf("trigger diagnostics: pipeline=%s report_event=%s type=%s report_type=%s source=%s/%s correlation=%q causation=%q data=%s",
		pipelineID, report.ID, report.Type, report.ReportType, report.Source.Kind, report.Source.PrincipalID,
		report.CorrelationID, report.CausationID, reportData)
	if result, err := instance.invoke(ctx, token, api.MethodGet, "pipeline.read", api.PipelineReadRequest{
		ProjectID: instance.projectID, PipelineID: pipelineID,
	}, "", nil); err != nil {
		t.Logf("trigger diagnostics: pipeline.read: %v", err)
	} else {
		read := decodeResult[api.PipelineReadResult](t, result)
		yaml := string(read.YAML)
		if len(yaml) > 1200 {
			yaml = yaml[:1200]
		}
		t.Logf("trigger diagnostics: pipeline name=%s version=%d yaml=%s", read.Name, read.Version, yaml)
	}
	if bindingID := instance.smokeBindingID(); bindingID != "" {
		if result, err := instance.invoke(ctx, admin, api.MethodGet, "resource.binding.read", api.ResourceBindingReadRequest{
			BindingID: bindingID,
		}, "", nil); err != nil {
			t.Logf("trigger diagnostics: binding.read %s: %v", bindingID, err)
		} else {
			read := decodeResult[api.ResourceBindingReadResult](t, result)
			t.Logf("trigger diagnostics: binding=%s resource=%s service=%s operations=%v reportable=%v origins=%v",
				read.BindingID, read.ResourceID, read.EnforcingService, read.Operations, read.ReportableEvents, read.TrustedOrigins)
		}
	}
	refusals, err := instance.smokeEventReadResult(ctx, token, "pipeline.start_refused")
	if err != nil {
		t.Logf("trigger diagnostics: read pipeline.start_refused: %v", err)
	} else {
		for _, event := range refusals {
			data := string(event.Data)
			if scanErr := harness.ScanCredentials(data, instance.config.PersonaSecrets()...); scanErr != nil {
				data = "withheld"
			}
			if len(data) > 400 {
				data = data[:400]
			}
			t.Logf("trigger diagnostics: refusal event=%s subject=%s correlation=%q causation=%q data=%s",
				event.ID, event.SubjectID, event.CorrelationID, event.CausationID, data)
		}
	}
	all, err := instance.smokeExecutionListResult(ctx, token)
	if err != nil {
		t.Logf("trigger diagnostics: list executions: %v", err)
		return
	}
	for _, execution := range all {
		t.Logf("trigger diagnostics: execution=%s pipeline=%q state=%s", execution.ExecutionID, execution.PipelineID, execution.State)
	}
}

// smokeExecutionListResult lists every execution in the project.
func (instance *live) smokeExecutionListResult(ctx context.Context, token harness.Secret) ([]api.ExecutionSummary, error) {
	summaries := []api.ExecutionSummary{}
	cursor := ""
	for range 64 {
		request := api.ExecutionListRequest{ProjectID: instance.projectID, Page: &api.Page{Limit: api.MaxPageLimit}}
		if cursor != "" {
			request.Page.Cursor = cursor
		}
		result, err := instance.invoke(ctx, token, api.MethodGet, "execution.list", request, "", nil)
		if err != nil {
			classified, _ := smoke.Classify("execution.list", err)
			return nil, classified
		}
		var decoded api.ExecutionListResult
		if err := decodeResultBody(result, &decoded); err != nil {
			return nil, err
		}
		summaries = append(summaries, decoded.Executions...)
		if decoded.NextCursor == "" {
			return summaries, nil
		}
		cursor = decoded.NextCursor
	}
	return nil, fmt.Errorf("execution.list did not finish within 64 pages")
}

// testReleaseSmokeHumanPush is S5. An admin creates an engineer-role starter
// service and binds an on.push pipeline to two run-unique work refs. A separate
// inline execution pushes to one subscribed ref with its mounted token: the
// accepted report carries that execution as its correlation and starts nothing
// (the exclusion control). The host engineer's human push to the other
// subscribed ref is reported with a verification causation and no correlation
// and starts exactly one execution, whose action proves the human event's
// repository, ref, commit and previous commit inputs. pipeline.start is never
// called for the subscribed pipeline.
func testReleaseSmokeHumanPush(t *testing.T, instance *live, names smoke.Names, probe string, recordScenario bool) api.ExecutionInspectResult {
	t.Helper()
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	admin := instance.smokeToken(t, harness.PersonaAdmin)
	repository, _ := instance.smokeGitInputs(t, gitSlotHumanPush)
	serviceID := instance.config.GitServiceID()
	ca := instance.materializeCA(t)

	executionRef := names.WorkRef("execution-push")
	humanRef := names.WorkRef("human-push")
	starter := names.Prefix() + "-starter"
	createResult := instance.smokeCall(ctx, t, admin, api.MethodPost, "service.create", api.ServiceCreateRequest{
		ProjectID: instance.projectID,
		ServiceID: starter,
		Roles:     []string{"engineer"},
	}, names.Key("service-create"), nil)
	service := decodeResult[api.ServiceCreateResult](t, createResult)
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
		version := service.Version
		if _, err := instance.invoke(cleanupCtx, admin, api.MethodPost, "service.disable",
			api.ServiceDisableRequest{ServiceID: service.ServiceID}, names.Key("service-disable"), &version); err != nil {
			t.Errorf("cleanup: disable the run-owned starter service %s: %v", service.ServiceID, err)
		}
	})

	url := smokeRepositoryURL(instance.config.GitURL(), repository.Name)
	engineerGit := instance.newHostGit(t, harness.PersonaEngineer, "human")
	humanDirectory := filepath.Join(engineerGit.dir, "human")
	engineerGit.clone(ctx, t, url, humanDirectory)
	if _, err := engineerGit.run(ctx, humanDirectory, "rev-parse", "--verify", "HEAD"); err != nil {
		// An empty repository clones with exit zero and no commit.
		engineerGit.mustRun(ctx, t, humanDirectory, "checkout", "-B", "w49-human")
	}
	humanCommit := engineerGit.commit(ctx, t, humanDirectory, "w49-human.txt",
		names.Marker("human")+"\n", "W49 human push")
	if err := smoke.CheckCommitIdentity("human commit", humanCommit); err != nil {
		t.Fatalf("%v", err)
	}

	actionMarker := names.Marker("push-inputs-ok")
	document, err := (harness.Document{
		Repository: repository.ResourceID,
		Service:    service.ServiceID,
		On:         &harness.On{Push: []string{executionRef, humanRef}},
		Inputs:     []string{"repository", "ref", "commit", "previous_commit"},
		Actions: []harness.Action{{
			Name:    "verify",
			Image:   instance.image,
			Timeout: "20m",
			Env: map[string]string{
				"SMOKE_EXPECT_REPOSITORY":  repository.ResourceID,
				"SMOKE_EXPECT_REF":         humanRef,
				"SMOKE_EXPECT_COMMIT":      humanCommit,
				"SMOKE_EXPECT_PREVIOUS":    "",
				"SMOKE_PUSH_ACTION_MARKER": actionMarker,
			},
			Commands: []string{smokeGitPrelude + "\n" + humanPushActionScript},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build the S5 push-trigger pipeline: %v", err)
	}
	applied := instance.smokeApplyPipeline(ctx, t, admin, names.PipelineName("human-push"), document, names.Key("apply"))

	pusherMarker := names.Marker("execution-pushed")
	pusherDocument, err := (harness.Document{
		Actions: []harness.Action{{
			Name:    "push",
			Image:   instance.image,
			Timeout: "20m",
			Env: smokeMergeEnv(ca.environment(), map[string]string{
				"SMOKE_CA_FILE":       ca.path,
				"SMOKE_GIT_URL":       instance.config.GitWorkloadURL(),
				"SMOKE_REPOSITORY":    repository.Name,
				"SMOKE_EXECUTION_REF": executionRef,
				"SMOKE_RUN_MARKER":    names.Marker("execution-run"),
				"SMOKE_PUSHER_MARKER": pusherMarker,
			}),
			Commands: ca.commands(smokeGitPrelude + "\n" + humanPushExecutionScript),
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build the S5 execution-originated push: %v", err)
	}
	pusher := instance.smokeStartInline(ctx, t, engineer, pusherDocument, names.Key("execution-push"))
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, pusher.ExecutionID)
	})
	pusherFinal := instance.smokeWaitComplete(ctx, t, engineer, pusher.ExecutionID, 6*time.Minute)
	requireSucceeded(t, pusherFinal)
	pusherAction := actionByName(t, pusherFinal, "push")
	pusherLog := instance.smokeReadLogs(ctx, t, engineer, pusher.ExecutionID, pusherAction.ActionID, "stdout")
	if err := smoke.CheckMarker(pusherLog, pusherMarker, 1); err != nil {
		t.Fatalf("execution push marker: %v", err)
	}
	pushedCommit, ok := smokeLogValue(pusherLog, "w49-pushed-commit")
	if !ok {
		t.Fatalf("the execution push did not report its commit")
	}
	if err := smoke.CheckCommitIdentity("execution push candidate", pushedCommit); err != nil {
		t.Fatalf("%v", err)
	}

	// Exclusion control: the execution-originated report cites this
	// execution as its correlation. It must start nothing.
	pusherReport := instance.smokeWaitReport(ctx, t, engineer, smoke.ReportWant{
		Resource:      repository.ResourceID,
		Ref:           executionRef,
		ExpectedBase:  "",
		Candidate:     pushedCommit,
		ServiceID:     serviceID,
		CorrelationID: pusher.ExecutionID,
	}, 3*time.Minute)
	if recordScenario {
		instance.recordScenario(t, "HumanPushExecutionControl", probe+"-ExecutionControl", &pusherFinal, started, time.Now().UTC(), map[string]any{
			"resource_id":     repository.ResourceID,
			"target_ref":      executionRef,
			"candidate":       pushedCommit,
			"expected_base":   "none",
			"verification_id": pusherReport.CausationID,
			"report_id":       pusherReport.ID,
		})
	}

	// Ordered exclusion control: let the trigger processor settle the
	// execution-originated report, then require the subscribed pipeline still
	// has no execution. Poll a fixed window; a single empty read or a later
	// non-empty list could miss a late admission.
	instance.smokeRequireNoPipelineExecutions(ctx, t, engineer, applied.PipelineID, 8*time.Second)

	// The host engineer's human push: causation is the verification, no
	// execution correlation, and the ref is created by this push.
	if _, err := engineerGit.push(ctx, humanDirectory, url, "HEAD:"+humanRef); err != nil {
		t.Fatalf("the host engineer's human push to %s: %v", humanRef, err)
	}
	humanReport := instance.smokeWaitReport(ctx, t, engineer, smoke.ReportWant{
		Resource:      repository.ResourceID,
		Ref:           humanRef,
		ExpectedBase:  "",
		Candidate:     humanCommit,
		ServiceID:     serviceID,
		CorrelationID: "",
	}, 3*time.Minute)

	var executions []api.ExecutionSummary
	executions, _, err = smoke.Wait(ctx, 500*time.Millisecond, 5*time.Minute, "execution.list",
		func(ctx context.Context) ([]api.ExecutionSummary, error) {
			return instance.smokePipelineExecutionsResult(ctx, engineer, applied.PipelineID)
		}, func(list []api.ExecutionSummary) bool { return len(list) > 0 })
	if err != nil {
		instance.logTriggerDiagnostics(ctx, t, engineer, admin, applied.PipelineID, humanReport)
		t.Fatalf("the human push started no execution of pipeline %s: %v", applied.PipelineID, err)
	}
	triggeredID, err := smoke.CheckPipelineExecutions(executions, applied.PipelineID)
	if err != nil {
		t.Fatalf("the human push started: %v", err)
	}
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, triggeredID)
	})
	final := instance.smokeWaitComplete(ctx, t, engineer, triggeredID, 6*time.Minute)
	if final.State != "succeeded" {
		smokeDumpLogs(t, instance, engineer, final)
	}
	requireSucceeded(t, final)
	triggeredAction := actionByName(t, final, "verify")
	log := instance.smokeReadLogs(ctx, t, engineer, triggeredID, triggeredAction.ActionID, "stdout")
	if err := smoke.CheckMarker(log, actionMarker, 1); err != nil {
		t.Fatalf("subscribed pipeline marker: %v", err)
	}
	if err := smoke.CheckInputValues(log, map[string]string{
		"repository":      repository.ResourceID,
		"ref":             humanRef,
		"commit":          humanCommit,
		"previous_commit": "",
	}); err != nil {
		t.Fatalf("the subscribed pipeline's admitted inputs: %v", err)
	}

	// Re-list after completion: still exactly the one human-push execution.
	after := instance.smokePipelineExecutions(ctx, t, engineer, applied.PipelineID)
	if _, err := smoke.CheckPipelineExecutions(after, applied.PipelineID); err != nil {
		t.Fatalf("execution set after completion: %v", err)
	}

	finished := time.Now().UTC()
	if recordScenario {
		instance.recordScenario(t, "HumanPush", probe, &final, started, finished, map[string]any{
			"resource_id":             repository.ResourceID,
			"target_ref":              humanRef,
			"candidate":               humanCommit,
			"expected_base":           "none",
			"action_id":               triggeredAction.ActionID,
			"workspace_id":            final.Cleanup.WorkspaceID,
			"verification_id":         humanReport.CausationID,
			"report_id":               humanReport.ID,
			"external_harness_digest": instance.config.ExternalHarnessDigest(),
		})
	}
	return final
}
