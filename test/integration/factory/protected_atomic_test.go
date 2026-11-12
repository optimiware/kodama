//go:build integration

package factorytest

// Composed atomic ref updates.
//
// One real execution is reviewed for an exact protected effect (main base B ->
// candidate C), then the same live credential pushes an --atomic batch that
// updates the approval-guarded main ref and a granted work ref together. The
// native receive-pack transaction applies both refs or neither; the managed
// pre-receive hook verifies and journals every ref in the batch before the
// transaction.
//
// A second reviewed execution pushes an atomic batch whose main update is
// approved but whose sibling ref is outside the committed policy. The whole
// batch must be refused with main still at the approved base. Neither the
// approved sibling nor the denied ref may leave an accepted/reported journal
// entry or a resource.verified event.
//
// Nothing here injects approval, a verification or a ref: the request action is
// resolved by one OIDC human through the public operation and the real hooks
// produce every fact.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	atomicWorkRef   = "refs/heads/work/atomic-batch"
	atomicDeniedRef = "refs/heads/zz-atomic-denied"
	atomicRequest   = "review"
	atomicHold      = "hold"
)

// TestProtectedFactoryAtomicRefUpdates proves an all-or-nothing multi-ref
// batch on the real factory: an approved protected update and a granted work
// ref apply together, while a refused sibling applies nothing.
func TestProtectedFactoryAtomicRefUpdates(t *testing.T) {
	fixture := startGitHarness(t)
	reviewerToken, _ := grantProtectedReviewer(t, fixture, protectedGoldenReviewerSubject)
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "atomic")
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded protected main ref is missing")
	}

	candidate := fixture.commit(t, directory, "atomic candidate")
	executionID, credential := approveAtomicBatch(t, fixture, reviewerToken, "atomic-accepted", base, candidate)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stderr, code := fixture.git.PushAtomic(ctx, directory, credential,
		"HEAD:"+gitMainRef, "HEAD:"+atomicWorkRef)
	if code != 0 {
		t.Fatalf("approved atomic batch: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); remote != candidate {
		t.Fatalf("protected main after the atomic batch = %q; want %q", remote, candidate)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, atomicWorkRef); remote != candidate {
		t.Fatalf("work ref after the atomic batch = %q; want the same candidate %q", remote, candidate)
	}

	mainEntry := fixture.waitJournal(t, gitMainRef, candidate)
	workEntry := fixture.waitJournal(t, atomicWorkRef, candidate)
	if mainEntry.SourceEventID == workEntry.SourceEventID ||
		mainEntry.VerificationID == workEntry.VerificationID {
		t.Fatalf("batch entries share identity: main %+v work %+v", mainEntry, workEntry)
	}
	mainVerification := fixture.verificationByID(t, mainEntry.VerificationID)
	workVerification := fixture.verificationByID(t, workEntry.VerificationID)
	for label, verification := range map[string]factory.VerificationRow{
		"main": mainVerification,
		"work": workVerification,
	} {
		if verification.Operation != "git.ref.update" || verification.Result != "approved" {
			t.Fatalf("%s verification = %+v; want an approved git.ref.update", label, verification)
		}
		if label == "main" && !verification.Approved {
			t.Fatalf("main verification = %+v; want the required approval fact", verification)
		}
		if label == "work" && verification.Approved {
			t.Fatalf("work verification = %+v; want no approval fact on an unguarded ref", verification)
		}
		if verification.ExecutionID != executionID {
			t.Fatalf("%s verification execution = %q; want %q", label, verification.ExecutionID, executionID)
		}
	}
	requireAtomicSubject(t, "main verification", mainVerification.Subject, gitMainRef, base, candidate)
	requireAtomicSubject(t, "work verification", workVerification.Subject, atomicWorkRef, "", candidate)

	mainEvent := fixture.waitReportEvent(t, mainVerification.ID)
	workEvent := fixture.waitReportEvent(t, workVerification.ID)
	if mainEvent.ID == workEvent.ID {
		t.Fatalf("both refs share report event %q; want one event per verified effect", mainEvent.ID)
	}
	for label, event := range map[string]factory.EventRow{"main": mainEvent, "work": workEvent} {
		if event.CausationID != mainVerification.ID && event.CausationID != workVerification.ID {
			t.Fatalf("%s event causation = %q; want one of the batch verifications", label, event.CausationID)
		}
		if event.CorrelationID != executionID {
			t.Fatalf("%s event correlation = %q; want the execution %q", label, event.CorrelationID, executionID)
		}
		if event.SubjectID != fixture.git.ProjectResourceID {
			t.Fatalf("%s event subject = %q; want the repository resource %q",
				label, event.SubjectID, fixture.git.ProjectResourceID)
		}
	}
	if mainEvent.CausationID != mainVerification.ID || workEvent.CausationID != workVerification.ID {
		t.Fatalf("batch events are cross-linked: main %q/%q work %q/%q",
			mainEvent.CausationID, mainVerification.ID, workEvent.CausationID, workVerification.ID)
	}

	// A second reviewed execution: main is approved for base C -> D, but the
	// sibling ref is not granted by the current policy. The whole batch must be
	// refused, with main still at C and neither ref journaled as accepted.
	next := fixture.commit(t, directory, "atomic refused candidate")
	_, refusedCredential := approveAtomicBatch(t, fixture, reviewerToken, "atomic-refused", candidate, next)
	if observed := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); observed != candidate {
		t.Fatalf("protected main before the refused batch = %q; want the approved base %q", observed, candidate)
	}
	refusedCtx, refusedCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer refusedCancel()
	stderr, code = fixture.git.PushAtomic(refusedCtx, directory, refusedCredential,
		"HEAD:"+gitMainRef, "HEAD:"+atomicDeniedRef)
	if code == 0 {
		t.Fatalf("atomic batch with a disallowed sibling was accepted: %s", stderr)
	}
	if !strings.Contains(stderr, "update rejected") {
		t.Fatalf("refused atomic batch did not reach the managed hook: %s", stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); remote != candidate {
		t.Fatalf("refused atomic batch moved main to %q; want the unchanged %q", remote, candidate)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, atomicDeniedRef); remote != "" {
		t.Fatalf("refused atomic batch created the denied ref at %q", remote)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, atomicWorkRef); remote != candidate {
		t.Fatalf("refused atomic batch moved the sibling work ref to %q; want %q", remote, candidate)
	}
	requireNoAcceptedJournal(t, fixture, gitMainRef, next)
	requireNoAcceptedJournal(t, fixture, atomicDeniedRef, next)
	rows, err := fixture.git.Verifications(t.Context())
	if err != nil {
		t.Fatalf("read the refused batch verifications: %v", err)
	}
	refusedRows := 0
	for _, row := range rows {
		if row.Operation != "git.ref.update" || !strings.Contains(row.Subject, next) {
			continue
		}
		refusedRows++
		if event, ok, err := fixture.git.ReportEvent(t.Context(), row.ID); err != nil {
			t.Fatalf("read a refused-batch report event: %v", err)
		} else if ok {
			t.Fatalf("refused atomic batch published event %+v for verification %s", event, row.ID)
		}
	}
	if refusedRows == 0 {
		t.Fatalf("the refused atomic batch verified no ref, so the approved sibling was never exercised")
	}
}

// approveAtomicBatch starts one execution with a reviewer request for the exact
// protected effect and a live hold action, resolves the human pass through the
// public operation, and returns the execution identity and its mounted
// credential. The caller pushes while the hold action keeps the credential
// current.
func approveAtomicBatch(t *testing.T, fixture *gitHarness, reviewerToken, name, base, candidate string) (string, string) {
	t.Helper()
	applied := fixture.applyPipeline(name, marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:    atomicRequest,
				Request: "role/reviewer",
				Timeout: protectedGoldenRequestTimeout,
				Subject: map[string]any{
					"operation":     "git.ref.update",
					"resource":      fixture.git.ProjectResourceID,
					"ref":           gitMainRef,
					"expected_base": base,
					"candidate":     candidate,
				},
			},
			{
				Name:    atomicHold,
				Image:   fixture.Image.Reference,
				Timeout: approvalContainerTimeout,
				Commands: []string{
					`test ! -e /kodama/task.json`,
					`while [ ! -e /workspace/release ]; do sleep 1; done`,
				},
			},
		},
	}))
	started := fixture.startPipeline(applied.PipelineID, "start-"+name)
	t.Cleanup(func() { fixture.stopExecution(t, started.ExecutionID) })

	waitProtectedActionRunning(t, fixture, started.ExecutionID, atomicRequest)
	pending := inspectProtectedGoldenExecution(t, fixture, fixture.engineerToken, started.ExecutionID)
	resolveAtomicRequest(t, fixture, reviewerToken, started.ExecutionID, actionByName(t, pending, atomicRequest), name)

	waitProtectedActionRunning(t, fixture, started.ExecutionID, atomicHold)
	return started.ExecutionID, waitProtectedGoldenCredential(t, fixture, started.ExecutionID, atomicHold)
}

// resolveAtomicRequest answers one running request with the exact human pass
// under a request key unique to this execution. The shared golden helper uses
// one fixed key, which would converge a second execution on the first receipt.
func resolveAtomicRequest(t *testing.T, fixture *gitHarness, reviewerToken, executionID string, initial api.ExecutionAction, name string) {
	t.Helper()
	for range 30 {
		current := inspectProtectedGoldenExecution(t, fixture, reviewerToken, executionID)
		if current.State != "running" {
			t.Fatalf("execution %s state = %q before resolve; want running", executionID, current.State)
		}
		action := actionByName(t, current, atomicRequest)
		result, publicError := fixture.REST(reviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
			ExecutionID:           executionID,
			ActionID:              action.ActionID,
			ExpectedActionVersion: action.Version,
			Decision:              "pass",
		}, &current.Version, "resolve-"+name)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("execution.request.resolve refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var resolved api.ExecutionRequestResolveResult
		fixture.Decode(result, &resolved)
		if resolved.ExecutionVersion <= current.Version || resolved.ActionVersion <= action.Version {
			t.Fatalf("resolve versions = %+v; want both advanced past execution=%d action=%d",
				resolved, current.Version, action.Version)
		}
		if initial.ActionID != action.ActionID {
			t.Fatalf("review action identity changed from %q to %q", initial.ActionID, action.ActionID)
		}
		return
	}
	t.Fatalf("execution.request.resolve did not converge on a current version")
}

// requireAtomicSubject decodes one canonical verified subject and checks the
// exact ref/base/candidate fields. An empty expected base is a create.
func requireAtomicSubject(t *testing.T, label, encoded, ref, base, candidate string) {
	t.Helper()
	var subject api.Subject
	if err := json.Unmarshal([]byte(encoded), &subject); err != nil {
		t.Fatalf("decode %s subject %s: %v", label, encoded, err)
	}
	fields := make(map[string]string, len(subject.Fields))
	for _, field := range subject.Fields {
		fields[field.Name] = field.Value
	}
	if fields["operation"] != "git.ref.update" || fields["ref"] != ref {
		t.Fatalf("%s subject = %v; want operation git.ref.update and ref %s", label, fields, ref)
	}
	if fields["candidate"] != candidate {
		t.Fatalf("%s candidate = %q; want %q", label, fields["candidate"], candidate)
	}
	if fields["expected_base"] != base {
		t.Fatalf("%s expected_base = %q; want %q", label, fields["expected_base"], base)
	}
}
