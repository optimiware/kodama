//go:build integration

package services

// Push trigger: a config, pipeline and binding edit after the first selection
// is durable cannot change the replay selection.
//
// The first admission attempt is made to fail transiently at the real image
// resolver boundary (a labelled fault), which leaves the first selection and its
// common receipt durable in PostgreSQL while the event is deferred. The test
// then edits the automatic service and on.push config, the pipeline's action
// bytes, and the repository binding, and releases the retry.
//
// The replay is observed end to end: it must start exactly one execution, the
// execution must admit the first selection's exact YAML bytes, the queued event
// must still name the first-selected automatic service, and no second execution
// or start refusal may appear. Without the first-selection contract the edited
// action bytes would run (and fail) and the admitted snapshot would differ.

import (
	"maps"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestPushEditReplayKeepsFirstSelection proves config, pipeline and binding
// edits after a durable first selection cannot rewrite the replay.
func TestPushEditReplayKeepsFirstSelection(t *testing.T) {
	resolver := newAttemptResolver(factory.DigestResolver{})
	fixture := startPushHarness(t, factory.WithImageResolver(resolver))

	const (
		refA       = "refs/heads/work/edit-first"
		refB       = "refs/heads/work/edit-second"
		firstSvc   = "edit-first-service"
		editedSvc  = "edit-second-service"
		pipelineID = "edit-replay"
	)
	fixture.createPushService(t, firstSvc)
	fixture.createPushService(t, editedSvc)

	directory := fixture.clone(t, fixture.engineerToken, "edit")
	commit := fixture.commit(t, directory, "edit replay")

	firstDocument := pushPipelineDocument(fixture.git.ProjectResourceID, firstSvc,
		fixture.Image.Reference, refA, commit)
	applied := fixture.applyPushPipeline(t, pipelineID, firstDocument)

	// The first admission attempt fails; the retry blocks at the resolver so
	// the edits happen strictly after the durable first selection and strictly
	// before the replay admission.
	resolver.armFailFirst()
	if stderr, code := fixture.push(directory, fixture.engineerToken, commit+":"+refA); code != 0 {
		t.Fatalf("edit replay push %s: exit %d: %s", refA, code, stderr)
	}
	resolver.waitFirst(t)
	resolver.waitBlocked(t)

	entry := fixture.waitPushJournal(t, refA, commit)
	report := fixture.waitPushReportEvent(t, entry.VerificationID)
	if executions := fixture.pushPipelineExecutions(t, applied.PipelineID); len(executions) != 0 {
		t.Fatalf("pipeline %s started %d executions before the deferred admission", applied.PipelineID, len(executions))
	}

	// Edit 1: the automatic-start service and on.push config. The pipeline still
	// subscribes to refA, so the stored selection remains a candidate.
	configDocument := pushPipelineDocumentRefs(fixture.git.ProjectResourceID, editedSvc,
		fixture.Image.Reference, []string{refA, refB}, commit)
	version := fixture.updatePushPipeline(t, applied.PipelineID, pipelineID, configDocument,
		applied.Version, "edit-config")

	// Edit 2: the action bytes now assert a commit that will never run. If the
	// replay re-read the edited definition, the execution fails.
	editedDocument := pushWrongCommitDocument(fixture.git.ProjectResourceID, editedSvc,
		fixture.Image.Reference, refA, refB)
	version = fixture.updatePushPipeline(t, applied.PipelineID, pipelineID, editedDocument,
		version, "edit-pipeline")
	if version == 0 {
		t.Fatal("pipeline.update returned version 0")
	}

	// Edit 3: the registered binding gains metadata at a new version.
	binding := fixture.readPushBinding(t, fixture.git.ProjectBindingID)
	metadata := make(map[string]string, len(binding.Metadata)+1)
	maps.Copy(metadata, binding.Metadata)
	metadata["edit-replay"] = "binding-reviewed"
	updated := fixture.MustREST(fixture.ProjectToken, "resource.binding.update", api.ResourceBindingUpdateRequest{
		BindingID:        binding.BindingID,
		ResourceID:       binding.ResourceID,
		EnforcingService: binding.EnforcingService,
		TrustedOrigins:   binding.TrustedOrigins,
		Operations:       binding.Operations,
		ReportableEvents: binding.ReportableEvents,
		Metadata:         metadata,
	}, &binding.Version, "edit-binding")
	var bindingResult api.ResourceBindingUpdateResult
	fixture.Decode(updated, &bindingResult)
	if bindingResult.Version <= binding.Version {
		t.Fatalf("binding version = %d; want a new version above %d", bindingResult.Version, binding.Version)
	}
	stored := readPipeline(t, fixture.Harness, applied.PipelineID)
	if stored.Version != version {
		t.Fatalf("stored pipeline version = %d; want the edited version %d", stored.Version, version)
	}

	// Release the deferred admission. The replay must use the first selection,
	// not this edited definition.
	resolver.releaseAll()
	executions := fixture.waitPushExecutionCount(t, applied.PipelineID, 1)
	t.Cleanup(func() { fixture.stopPushExecution(t, executions[0].ExecutionID) })
	fixture.waitPushExecutionSucceeded(t, executions[0].ExecutionID)

	if admitted := fixture.executionSnapshotYAML(t, executions[0].ExecutionID); string(admitted) != string(firstDocument) {
		t.Fatalf("replay admitted YAML = %q; want the first selection %q", admitted, firstDocument)
	}
	queued := fixture.queuedEvents(t, executions[0].ExecutionID)
	if len(queued) != 1 {
		t.Fatalf("replay queued events = %d; want exactly one", len(queued))
	}
	if queued[0].Source.PrincipalID != firstSvc {
		t.Fatalf("replay queued source = %q; want the first-selected service %q",
			queued[0].Source.PrincipalID, firstSvc)
	}
	if queued[0].CorrelationID != report.ID {
		t.Fatalf("replay queued correlation = %q; want the original report %q",
			queued[0].CorrelationID, report.ID)
	}

	refusals := readProjectEvents(t, fixture.Harness, "pipeline.start_refused")
	for _, event := range refusals.Events {
		if event.SubjectID == applied.PipelineID {
			t.Fatalf("edit replay committed a start refusal for pipeline %s: %+v", applied.PipelineID, event)
		}
	}
	if remaining := fixture.pushPipelineExecutions(t, applied.PipelineID); len(remaining) != 1 {
		t.Fatalf("pipeline %s has %d executions; want exactly one", applied.PipelineID, len(remaining))
	}
}

// pushWrongCommitDocument renders an edited pipeline whose action asserts a
// commit that cannot match the pushed one.
func pushWrongCommitDocument(repository, service, image, firstRef, secondRef string) []byte {
	return pushMarshal(pushPipelineDoc{
		Repository: repository,
		Service:    service,
		On:         &pushTriggerDoc{Push: []string{firstRef, secondRef}},
		Inputs:     []string{"repository", "ref", "commit", "previous_commit"},
		Actions: []pushActionDoc{{
			Name:    "edited",
			Image:   image,
			Timeout: "10m",
			Commands: []string{
				`test "$(cat /kodama/inputs/commit)" = edited-never-runs`,
				`exit 1`,
			},
		}},
	})
}
