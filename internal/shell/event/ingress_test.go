package event_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestIngressPublishesCustomObservation is the successful event.publish path:
// the authenticated execution credential supplies the source and scope, the
// observation event, receipt, audit and publication intent are committed
// together, and no credential material reaches the unit.
func TestIngressPublishesCustomObservation(t *testing.T) {
	harness := newIngressHarness(t)
	attempt := publishAttempt()

	result, err := harness.ingress.Publish(context.Background(), attempt)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if result.Replayed {
		t.Error("a first publication was reported as a replay")
	}
	if result.EventID != id.EventID("event-1") {
		t.Errorf("event id = %q, want the committed %q", result.EventID, "event-1")
	}
	if result.ReceiptID != id.ReceiptID("receipt-1") {
		t.Errorf("receipt id = %q, want the committed %q", result.ReceiptID, "receipt-1")
	}
	if harness.port.commitCount() != 1 {
		t.Fatalf("commits = %d, want 1", harness.port.commitCount())
	}

	unit := harness.port.unitAt(0)
	if unit.Owner != testScope() {
		t.Errorf("unit owner = %#v, want %#v", unit.Owner, testScope())
	}
	if unit.ExecutionID != testExecution {
		t.Errorf("unit execution = %q, want %q", unit.ExecutionID, testExecution)
	}
	if unit.Event.Type != event.TypeCustomObservation {
		t.Errorf("event type = %q, want %q", unit.Event.Type, event.TypeCustomObservation)
	}
	if unit.Event.ReportType != "" {
		t.Errorf("event report type = %q, want empty for a custom observation", unit.Event.ReportType)
	}
	if unit.Event.Source != testSource() {
		t.Errorf("event source = %#v, want the authenticated %#v", unit.Event.Source, testSource())
	}
	if unit.Event.Scope != testScope() {
		t.Errorf("event scope = %#v, want the authenticated %#v", unit.Event.Scope, testScope())
	}
	if !unit.Event.At.Equal(fixedNow) {
		t.Errorf("event time = %v, want the injected clock %v", unit.Event.At, fixedNow)
	}
	if unit.Event.SubjectID != attempt.SubjectID {
		t.Errorf("event subject = %q, want %q", unit.Event.SubjectID, attempt.SubjectID)
	}

	wantFingerprint, err := receipt.Fingerprint(attempt.Content)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	wantKey := receipt.Key{
		TenantID:    testTenant,
		ProjectID:   testProject,
		PrincipalID: testPrincipal,
		Operation:   testOperation,
		CallerKey:   attempt.CallerKey,
	}
	if unit.Receipt.Key != wantKey {
		t.Errorf("receipt key = %#v, want %#v", unit.Receipt.Key, wantKey)
	}
	if unit.Receipt.ContentHash != wantFingerprint {
		t.Errorf("receipt content hash = %q, want %q", unit.Receipt.ContentHash, wantFingerprint)
	}
	if !unit.Receipt.At.Equal(fixedNow) {
		t.Errorf("receipt time = %v, want the injected clock %v", unit.Receipt.At, fixedNow)
	}
	if string(unit.Receipt.Result) != string(unit.Event.ID) {
		t.Errorf("receipt result = %q, want the accepted event id %q", unit.Receipt.Result, unit.Event.ID)
	}

	if len(unit.Audit) == 0 {
		t.Fatal("unit carries no audit row")
	}
	for _, audit := range unit.Audit {
		if audit.Operation != testOperation {
			t.Errorf("audit operation = %q, want %q", audit.Operation, testOperation)
		}
		if audit.Actor != testPrincipal {
			t.Errorf("audit actor = %q, want the authenticated execution %q", audit.Actor, testPrincipal)
		}
		if audit.Scope != testScope() {
			t.Errorf("audit scope = %#v, want %#v", audit.Scope, testScope())
		}
		if !audit.At.Equal(fixedNow) {
			t.Errorf("audit time = %v, want %v", audit.At, fixedNow)
		}
	}

	if got, want := unit.Authority, expectedIngressReads(executionPrincipal()); !reflect.DeepEqual(got, want) {
		t.Errorf("authority reads = %#v, want %#v", got, want)
	}

	if len(unit.PostCommit) != 1 {
		t.Fatalf("post-commit effects = %d, want exactly the publication intent", len(unit.PostCommit))
	}
	if unit.PostCommit[0].Kind != event.PostPublishEvent {
		t.Errorf("post-commit kind = %q, want %q", unit.PostCommit[0].Kind, event.PostPublishEvent)
	}
	if unit.PostCommit[0].Target != string(unit.Event.ID) {
		t.Errorf("post-commit target = %q, want the committed event id %q", unit.PostCommit[0].Target, unit.Event.ID)
	}

	if err := unit.Validate(); err != nil {
		t.Errorf("committed unit does not validate: %v", err)
	}
	encoded, err := json.Marshal(unit)
	if err != nil {
		t.Fatalf("marshal unit: %v", err)
	}
	if bytes.Contains(encoded, []byte(testCredential)) {
		t.Error("the committed unit serialized the presented credential")
	}
}

// TestIngressAcceptsMatchingBodyScopeAndSource asserts a body that repeats the
// authenticated scope and source is accepted; the committed envelope still
// uses the credential-derived values.
func TestIngressAcceptsMatchingBodyScopeAndSource(t *testing.T) {
	harness := newIngressHarness(t)
	attempt := publishAttempt()
	attempt.Scope = testScope()
	attempt.Source = testSource()

	if _, err := harness.ingress.Publish(context.Background(), attempt); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if harness.port.commitCount() != 1 {
		t.Fatalf("commits = %d, want 1", harness.port.commitCount())
	}
	unit := harness.port.unitAt(0)
	if unit.Event.Scope != testScope() || unit.Event.Source != testSource() {
		t.Errorf("committed scope/source = %#v/%#v, want the authenticated values", unit.Event.Scope, unit.Event.Source)
	}
}

// TestIngressRefusesSpoofedScopeAndSource asserts a body can never choose the
// event's ownership scope or producer identity.
func TestIngressRefusesSpoofedScopeAndSource(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*event.PublishAttempt)
	}{
		{"body tenant scope", func(attempt *event.PublishAttempt) {
			attempt.Scope = otherScope()
		}},
		{"body project scope", func(attempt *event.PublishAttempt) {
			attempt.Scope = tenancy.Scope{TenantID: testTenant, ProjectID: otherProject}
		}},
		{"body shell source", func(attempt *event.PublishAttempt) {
			attempt.Source = event.Source{Kind: event.SourceShell, PrincipalID: testPrincipal}
		}},
		{"body service source", func(attempt *event.PublishAttempt) {
			attempt.Source = event.Source{Kind: event.SourceService, PrincipalID: testPrincipal}
		}},
		{"body other principal", func(attempt *event.PublishAttempt) {
			attempt.Source = event.Source{Kind: event.SourceExecution, PrincipalID: otherPrincipal}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newIngressHarness(t)
			attempt := publishAttempt()
			testCase.mutate(&attempt)

			result, err := harness.ingress.Publish(context.Background(), attempt)
			assertFaultClass(t, err, fault.Forbidden)
			if result.EventID != "" || result.ReceiptID != "" {
				t.Errorf("refused publication returned a result: %#v", result)
			}
			if harness.port.callCount() != 0 {
				t.Errorf("ingress port calls = %d, want 0 for a spoofed body", harness.port.callCount())
			}
		})
	}
}

// TestIngressAllowsEngineerHumanAndService asserts the event.publish registry
// row also admits engineer humans and engineer services: the source kind is
// derived from the authenticated principal, no execution id is carried and the
// unit carries only the applicable current-authority reads.
func TestIngressAllowsEngineerHumanAndService(t *testing.T) {
	cases := []struct {
		name      string
		principal identity.Principal
		wantKind  event.SourceKind
	}{
		{"human engineer", humanPrincipal(), event.SourceShell},
		{"service engineer", servicePrincipal(), event.SourceService},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newIngressHarness(t)
			harness.credentials.principal = testCase.principal

			result, err := harness.ingress.Publish(context.Background(), publishAttempt())
			if err != nil {
				t.Fatalf("Publish: %v", err)
			}
			if result.EventID != id.EventID("event-1") {
				t.Errorf("event id = %q, want the committed %q", result.EventID, "event-1")
			}
			if harness.port.commitCount() != 1 {
				t.Fatalf("commits = %d, want 1", harness.port.commitCount())
			}
			unit := harness.port.unitAt(0)
			if unit.Event.Source.Kind != testCase.wantKind {
				t.Errorf("event source kind = %q, want the authenticated %q", unit.Event.Source.Kind, testCase.wantKind)
			}
			if unit.Event.Source.PrincipalID != testCase.principal.ID {
				t.Errorf("event source principal = %q, want the authenticated %q", unit.Event.Source.PrincipalID, testCase.principal.ID)
			}
			if unit.Event.Scope != testCase.principal.Scope {
				t.Errorf("event scope = %#v, want the authenticated %#v", unit.Event.Scope, testCase.principal.Scope)
			}
			if unit.ExecutionID != "" {
				t.Errorf("unit execution = %q, want empty for a non-execution source", unit.ExecutionID)
			}
			if got, want := unit.Authority, expectedIngressReads(testCase.principal); !reflect.DeepEqual(got, want) {
				t.Errorf("authority reads = %#v, want %#v", got, want)
			}
			if err := unit.Validate(); err != nil {
				t.Errorf("committed unit does not validate: %v", err)
			}
			encoded, err := json.Marshal(unit)
			if err != nil {
				t.Fatalf("marshal unit: %v", err)
			}
			if bytes.Contains(encoded, []byte(testCredential)) {
				t.Error("the committed unit serialized the presented credential")
			}
		})
	}
}

// TestIngressRefusesUnauthorizedPrincipals asserts the event.publish grant is
// the registry row: humans and services must currently hold engineer, a
// disabled principal is refused, and an execution without its execution id is
// refused. Every refusal happens before any authority read or commit.
func TestIngressRefusesUnauthorizedPrincipals(t *testing.T) {
	viewerHuman := humanPrincipal()
	viewerHuman.Roles = []tenancy.Role{tenancy.RoleViewer}
	adminHuman := humanPrincipal()
	adminHuman.Roles = []tenancy.Role{tenancy.RoleAdmin}
	viewerService := servicePrincipal()
	viewerService.Roles = []tenancy.Role{tenancy.RoleViewer}
	disabledEngineer := humanPrincipal()
	disabledEngineer.Enabled = false
	executionWithoutID := executionPrincipal()
	executionWithoutID.ExecutionID = ""

	cases := []struct {
		name      string
		principal identity.Principal
		wantClass fault.Class
	}{
		{"viewer human", viewerHuman, fault.Forbidden},
		{"admin human", adminHuman, fault.Forbidden},
		{"viewer service", viewerService, fault.Forbidden},
		{"disabled engineer", disabledEngineer, fault.Forbidden},
		{"execution without id", executionWithoutID, fault.Unauthenticated},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newIngressHarness(t)
			harness.credentials.principal = testCase.principal

			_, err := harness.ingress.Publish(context.Background(), publishAttempt())
			assertFaultClass(t, err, testCase.wantClass)
			if harness.port.callCount() != 0 {
				t.Errorf("ingress port calls = %d, want 0 for an unauthorized principal", harness.port.callCount())
			}
			if harness.authority.callCount() != 0 {
				t.Errorf("authority reads = %d, want 0 before the credential is accepted", harness.authority.callCount())
			}
		})
	}
}

// TestIngressRefusesHumanSpoofedSource asserts a human or service publisher
// cannot claim the execution source, even with a matching principal id.
func TestIngressRefusesHumanSpoofedSource(t *testing.T) {
	harness := newIngressHarness(t)
	harness.credentials.principal = humanPrincipal()
	attempt := publishAttempt()
	attempt.Source = event.Source{Kind: event.SourceExecution, PrincipalID: humanPrincipal().ID}

	_, err := harness.ingress.Publish(context.Background(), attempt)
	assertFaultClass(t, err, fault.Forbidden)
	if harness.port.callCount() != 0 {
		t.Errorf("ingress port calls = %d, want 0 for a spoofed source", harness.port.callCount())
	}
}

// TestIngressRefusesLifecycleTypeImpersonation asserts a custom observation can
// never claim a lifecycle or decision event type, including the internal
// request.activated alias. An unregistered name is invalid input.
func TestIngressRefusesLifecycleTypeImpersonation(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		wantClass fault.Class
	}{
		{"task lifecycle", "task.created", fault.Forbidden},
		{"request resolution", "request.resolved", fault.Forbidden},
		{"trusted verified report", "resource.verified", fault.Forbidden},
		{"canonical request activation", "action.requested", fault.Forbidden},
		{"internal activation alias", "request.activated", fault.Forbidden},
		{"unknown type", "bogus", fault.InvalidInput},
		{"empty type", "", fault.InvalidInput},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newIngressHarness(t)
			attempt := publishAttempt()
			attempt.Type = testCase.eventType

			_, err := harness.ingress.Publish(context.Background(), attempt)
			assertFaultClass(t, err, testCase.wantClass)
			if harness.port.callCount() != 0 {
				t.Errorf("ingress port calls = %d, want 0 for an impersonated type", harness.port.callCount())
			}
		})
	}
}

// TestIngressRefusesOversizedObservation asserts the 64 KiB envelope bound is
// enforced before any commit.
func TestIngressRefusesOversizedObservation(t *testing.T) {
	t.Run("exact bound", func(t *testing.T) {
		harness := newIngressHarness(t)
		attempt := publishAttempt()
		attempt.Data = make([]byte, event.DefaultBounds.MaxDataBytes)

		if _, err := harness.ingress.Publish(context.Background(), attempt); err != nil {
			t.Fatalf("Publish at the exact bound: %v", err)
		}
		if harness.port.commitCount() != 1 {
			t.Fatalf("commits = %d, want 1", harness.port.commitCount())
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		harness := newIngressHarness(t)
		attempt := publishAttempt()
		attempt.Data = make([]byte, event.DefaultBounds.MaxDataBytes+1)

		_, err := harness.ingress.Publish(context.Background(), attempt)
		assertFaultClass(t, err, fault.InvalidInput)
		if harness.port.callCount() != 0 {
			t.Errorf("ingress port calls = %d, want 0 for oversized data", harness.port.callCount())
		}
	})
}

// TestIngressRefusesWhenCurrentAuthorityDoesNotHold asserts the event.publish
// authority reads are rechecked before the commit: a revoked actor, a
// non-running action and an action past its deadline are all refused.
func TestIngressRefusesWhenCurrentAuthorityDoesNotHold(t *testing.T) {
	cases := []struct {
		name  string
		check authorize.AuthorityCheck
	}{
		{"revoked actor", authorize.CheckMembership},
		{"non-running action", authorize.CheckExecutionRunning},
		{"action past deadline", authorize.CheckExecutionRunning},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newIngressHarness(t)
			harness.authority.deny(testCase.check)

			result, err := harness.ingress.Publish(context.Background(), publishAttempt())
			assertFaultClass(t, err, fault.Forbidden)
			if result.EventID != "" {
				t.Errorf("refused publication returned event %q", result.EventID)
			}
			if harness.port.callCount() != 0 {
				t.Errorf("ingress port calls = %d, want 0 when current authority does not hold", harness.port.callCount())
			}
		})
	}
}

// TestIngressRefusesWhenAuthorityReadFails asserts a dependency failure reading
// current authority is propagated and no observation is committed.
func TestIngressRefusesWhenAuthorityReadFails(t *testing.T) {
	harness := newIngressHarness(t)
	harness.authority.setError(fault.New(fault.DependencyUnavailable, "authority store unavailable"))

	_, err := harness.ingress.Publish(context.Background(), publishAttempt())
	assertFaultClass(t, err, fault.DependencyUnavailable)
	if harness.port.callCount() != 0 {
		t.Errorf("ingress port calls = %d, want 0 when authority cannot be read", harness.port.callCount())
	}
}

// TestIngressRefusesUnauthenticatedCredential asserts an unresolvable
// credential is an authentication refusal that reads no authority and commits
// nothing.
func TestIngressRefusesUnauthenticatedCredential(t *testing.T) {
	harness := newIngressHarness(t)
	harness.credentials.err = fault.New(fault.Unauthenticated, "credential expired")

	_, err := harness.ingress.Publish(context.Background(), publishAttempt())
	assertFaultClass(t, err, fault.Unauthenticated)
	if harness.authority.callCount() != 0 {
		t.Errorf("authority reads = %d, want 0 for an unauthenticated credential", harness.authority.callCount())
	}
	if harness.port.callCount() != 0 {
		t.Errorf("ingress port calls = %d, want 0 for an unauthenticated credential", harness.port.callCount())
	}
}

// TestIngressReplaysOriginalResultForIdenticalRetry asserts an identical retry
// returns the originally committed event and receipt identities even though the
// orchestrator generated fresh candidate identities, and commits no second
// observation.
func TestIngressReplaysOriginalResultForIdenticalRetry(t *testing.T) {
	harness := newIngressHarness(t)
	attempt := publishAttempt()

	first, err := harness.ingress.Publish(context.Background(), attempt)
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	second, err := harness.ingress.Publish(context.Background(), attempt)
	if err != nil {
		t.Fatalf("retry Publish: %v", err)
	}
	if !second.Replayed {
		t.Error("an identical retry was not reported as a replay")
	}
	if second.EventID != first.EventID {
		t.Errorf("retry event id = %q, want the original %q", second.EventID, first.EventID)
	}
	if second.ReceiptID != first.ReceiptID {
		t.Errorf("retry receipt id = %q, want the original %q", second.ReceiptID, first.ReceiptID)
	}
	if harness.port.commitCount() != 1 {
		t.Errorf("commits = %d, want 1: a replay must not repeat the effect", harness.port.commitCount())
	}
	if harness.port.eventCount() != 1 {
		t.Errorf("committed events = %d, want 1", harness.port.eventCount())
	}
}

// TestIngressRefusesChangedContentForSameCallerKey asserts a caller key reused
// with different content is refused with changed_idempotency_content.
func TestIngressRefusesChangedContentForSameCallerKey(t *testing.T) {
	harness := newIngressHarness(t)
	first := publishAttempt()
	if _, err := harness.ingress.Publish(context.Background(), first); err != nil {
		t.Fatalf("first Publish: %v", err)
	}

	changed := first
	changed.Content = receipt.Content{MediaType: "application/json", Bytes: []byte(`{"note":"changed"}`)}
	_, err := harness.ingress.Publish(context.Background(), changed)
	assertFaultClass(t, err, fault.ChangedIdempotencyContent)
	if harness.port.commitCount() != 1 {
		t.Errorf("commits = %d, want 1: changed content must not commit a second effect", harness.port.commitCount())
	}
	if harness.port.eventCount() != 1 {
		t.Errorf("committed events = %d, want 1", harness.port.eventCount())
	}
}

// TestIngressRecoversLostResponseWithoutSecondEffect asserts a response lost
// after commit converges on the original result on retry without a second
// observation.
func TestIngressRecoversLostResponseWithoutSecondEffect(t *testing.T) {
	harness := newIngressHarness(t)
	attempt := publishAttempt()
	harness.port.failAfterCommit = fault.New(fault.DependencyUnavailable, "response lost")

	result, err := harness.ingress.Publish(context.Background(), attempt)
	assertFaultClass(t, err, fault.DependencyUnavailable)
	if result.EventID != "" {
		t.Errorf("a lost response returned event %q", result.EventID)
	}
	if harness.port.commitCount() != 1 {
		t.Fatalf("commits after the lost response = %d, want the first commit", harness.port.commitCount())
	}

	retried, err := harness.ingress.Publish(context.Background(), attempt)
	if err != nil {
		t.Fatalf("retry Publish: %v", err)
	}
	if !retried.Replayed {
		t.Error("recovery retry was not reported as a replay")
	}
	if retried.EventID != id.EventID("event-1") {
		t.Errorf("recovered event id = %q, want the original %q", retried.EventID, "event-1")
	}
	if harness.port.commitCount() != 1 {
		t.Errorf("commits = %d, want 1: recovery must not repeat the effect", harness.port.commitCount())
	}
	if harness.port.eventCount() != 1 {
		t.Errorf("committed events = %d, want 1", harness.port.eventCount())
	}
}

// TestIngressNeverReportsUnknownOutcomeAsSuccess asserts an ambiguous adapter
// outcome is an error, never a successful publication.
func TestIngressNeverReportsUnknownOutcomeAsSuccess(t *testing.T) {
	harness := newIngressHarness(t)
	harness.port.refuseBeforeCommit = fault.New(fault.UncertainExternalEffect, "outcome unknown")

	result, err := harness.ingress.Publish(context.Background(), publishAttempt())
	assertFaultClass(t, err, fault.UncertainExternalEffect)
	if result.EventID != "" || result.ReceiptID != "" || result.Replayed {
		t.Errorf("an unknown outcome returned a success result: %#v", result)
	}
}

// TestIngressRefusesReplayAfterAuthorityRevoked asserts current authority is
// rechecked before an existing receipt result is exposed: a revoked execution
// cannot use an identical retry to read the original result.
func TestIngressRefusesReplayAfterAuthorityRevoked(t *testing.T) {
	cases := []struct {
		name  string
		check authorize.AuthorityCheck
	}{
		{"revoked actor", authorize.CheckMembership},
		{"non-running action", authorize.CheckExecutionRunning},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newIngressHarness(t)
			attempt := publishAttempt()
			if _, err := harness.ingress.Publish(context.Background(), attempt); err != nil {
				t.Fatalf("first Publish: %v", err)
			}
			callsAfterCommit := harness.port.callCount()

			harness.authority.deny(testCase.check)
			result, err := harness.ingress.Publish(context.Background(), attempt)
			assertFaultClass(t, err, fault.Forbidden)
			if result.EventID != "" || result.Replayed {
				t.Errorf("a revoked retry exposed the receipt result: %#v", result)
			}
			if harness.port.callCount() != callsAfterCommit {
				t.Errorf("ingress port calls = %d, want the commit-time %d: the replay was exposed before authority was rechecked", harness.port.callCount(), callsAfterCommit)
			}
		})
	}
}

// TestIngressUnitValidateAcceptsBuiltUnit asserts the unit the orchestrator
// builds is the unit the adapter validates.
func TestIngressUnitValidateAcceptsBuiltUnit(t *testing.T) {
	unit := validIngressUnit(t)
	if err := unit.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestIngressUnitValidateRefusals asserts the adapter-side unit guard refuses
// every shape that could smuggle authority, a settlement or a second effect.
func TestIngressUnitValidateRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*event.IngressUnit)
	}{
		{"wrong event type", func(unit *event.IngressUnit) {
			unit.Event.Type = event.TypeTaskCreated
		}},
		{"verified report type on an observation", func(unit *event.IngressUnit) {
			unit.Event.Type = event.TypeVerifiedReport
			unit.Event.ReportType = "git.ref.updated"
		}},
		{"report type on a custom observation", func(unit *event.IngressUnit) {
			unit.Event.ReportType = "git.ref.updated"
		}},
		{"non-execution source kind", func(unit *event.IngressUnit) {
			unit.Event.Source.Kind = event.SourceShell
		}},
		{"source principal mismatch", func(unit *event.IngressUnit) {
			unit.Event.Source.PrincipalID = otherPrincipal
		}},
		{"event scope mismatch", func(unit *event.IngressUnit) {
			unit.Event.Scope = otherScope()
		}},
		{"owner mismatch", func(unit *event.IngressUnit) {
			unit.Owner = otherScope()
		}},
		{"missing execution id", func(unit *event.IngressUnit) {
			unit.ExecutionID = ""
		}},
		{"receipt operation mismatch", func(unit *event.IngressUnit) {
			unit.Receipt.Key.Operation = "task.create"
		}},
		{"receipt principal mismatch", func(unit *event.IngressUnit) {
			unit.Receipt.Key.PrincipalID = otherPrincipal
		}},
		{"receipt content hash empty", func(unit *event.IngressUnit) {
			unit.Receipt.ContentHash = ""
		}},
		{"missing membership read", func(unit *event.IngressUnit) {
			unit.Authority = unit.Authority[1:]
		}},
		{"missing execution running read", func(unit *event.IngressUnit) {
			unit.Authority = unit.Authority[:1]
		}},
		{"authority operation mismatch", func(unit *event.IngressUnit) {
			unit.Authority[0].Operation = "task.create"
		}},
		{"execution running target mismatch", func(unit *event.IngressUnit) {
			unit.Authority[1].TargetID = "exec-2"
		}},
		{"empty audit", func(unit *event.IngressUnit) {
			unit.Audit = nil
		}},
		{"audit operation mismatch", func(unit *event.IngressUnit) {
			unit.Audit[0].Operation = "task.create"
		}},
		{"empty post-commit", func(unit *event.IngressUnit) {
			unit.PostCommit = nil
		}},
		{"dispatch post-commit", func(unit *event.IngressUnit) {
			unit.PostCommit[0].Kind = event.PostDispatch
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			unit := cloneUnit(validIngressUnit(t))
			testCase.mutate(&unit)
			assertFaultClass(t, unit.Validate(), fault.InvalidInput)
		})
	}
}

// TestIngressUnitValidateHumanSourceRefusals asserts a non-execution unit can
// never carry execution-only authority or an execution id, and an execution
// unit still requires its execution id.
func TestIngressUnitValidateHumanSourceRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*event.IngressUnit)
	}{
		{"human source with execution id", func(unit *event.IngressUnit) {
			unit.ExecutionID = testExecution
		}},
		{"human source with execution running read", func(unit *event.IngressUnit) {
			unit.Authority = append(unit.Authority, authorize.AuthorityRead{
				Operation:   authorize.OperationName(testOperation),
				Required:    unit.Authority[0].Required,
				Check:       authorize.CheckExecutionRunning,
				Scope:       testScope(),
				PrincipalID: humanPrincipal().ID,
				TargetID:    string(testExecution),
			})
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			unit := cloneUnit(validHumanIngressUnit(t))
			testCase.mutate(&unit)
			assertFaultClass(t, unit.Validate(), fault.InvalidInput)
		})
	}
}

// TestNewIngressRefusesPartialComposition asserts the ingress cannot be
// composed with a missing dependency.
func TestNewIngressRefusesPartialComposition(t *testing.T) {
	base := func() event.IngressConfig {
		return event.IngressConfig{
			Credentials:  &fakeCredentials{principal: executionPrincipal()},
			Authority:    &fakeAuthority{denied: map[authorize.AuthorityCheck]bool{}},
			Authorizer:   authorize.NewMatrix(),
			Port:         newFakeIngressPort(),
			Clock:        fakeClock{now: fixedNow},
			NewEventID:   (&idSequence{}).nextEventID,
			NewReceiptID: (&idSequence{}).nextReceiptID,
		}
	}
	cases := []struct {
		name   string
		mutate func(*event.IngressConfig)
	}{
		{"missing credentials", func(config *event.IngressConfig) { config.Credentials = nil }},
		{"missing authority", func(config *event.IngressConfig) { config.Authority = nil }},
		{"missing authorizer", func(config *event.IngressConfig) { config.Authorizer = nil }},
		{"missing port", func(config *event.IngressConfig) { config.Port = nil }},
		{"missing clock", func(config *event.IngressConfig) { config.Clock = nil }},
		{"missing event id generator", func(config *event.IngressConfig) { config.NewEventID = nil }},
		{"missing receipt id generator", func(config *event.IngressConfig) { config.NewReceiptID = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := base()
			testCase.mutate(&config)
			if _, err := event.NewIngress(config); err == nil {
				t.Fatal("NewIngress accepted a partial composition")
			} else {
				assertFaultClass(t, err, fault.InvalidInput)
			}
		})
	}
}
