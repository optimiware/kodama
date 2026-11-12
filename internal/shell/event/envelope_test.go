package event_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestNewEnvelopeCanonicalizesInputAlias asserts the canonical public lifecycle
// name is emitted while the internal alias is accepted only on input.
func TestNewEnvelopeCanonicalizesInputAlias(t *testing.T) {
	canonical, ok := api.CanonicalEventType(string(event.TypeRequestActivated))
	if !ok || canonical != string(event.TypeActionRequested) {
		t.Fatalf("declared vector: api.CanonicalEventType(request.activated) = %q,%v; want %q,true", canonical, ok, event.TypeActionRequested)
	}

	envelope, err := event.NewEnvelope(id.EventID("event-1"), testSource(), testScope(), fixedNow, event.EnvelopeInput{
		Type:      event.TypeRequestActivated,
		SubjectID: "subject-1",
	})
	if err != nil {
		t.Fatalf("NewEnvelope(alias): %v", err)
	}
	if envelope.Type != event.TypeActionRequested {
		t.Errorf("envelope type = %q, want the canonical %q", envelope.Type, event.TypeActionRequested)
	}
	if string(envelope.Type) == string(event.TypeRequestActivated) {
		t.Error("the internal alias was emitted as a public event type")
	}

	alreadyCanonical, err := event.NewEnvelope(id.EventID("event-1"), testSource(), testScope(), fixedNow, event.EnvelopeInput{
		Type:      event.TypeActionRequested,
		SubjectID: "subject-1",
	})
	if err != nil {
		t.Fatalf("NewEnvelope(canonical): %v", err)
	}
	if alreadyCanonical.Type != event.TypeActionRequested {
		t.Errorf("envelope type = %q, want %q", alreadyCanonical.Type, event.TypeActionRequested)
	}
}

// TestNewEnvelopeRejectsUnknownType asserts an unregistered lifecycle name is
// refused rather than stored as an event type.
func TestNewEnvelopeRejectsUnknownType(t *testing.T) {
	cases := []event.Type{"", "bogus", "task.created.extra", "git.ref.updated"}
	for _, eventType := range cases {
		t.Run(string(eventType), func(t *testing.T) {
			_, err := event.NewEnvelope(id.EventID("event-1"), testSource(), testScope(), fixedNow, event.EnvelopeInput{
				Type:      eventType,
				SubjectID: "subject-1",
			})
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
}

// TestNewEnvelopeReportTypeSeparation asserts only a verified resource report
// may carry a binding-declared report type, and that a lifecycle name (or its
// alias) can never be declared as a report type. This is what stops a custom
// observation from impersonating a trusted git report.
func TestNewEnvelopeReportTypeSeparation(t *testing.T) {
	cases := []struct {
		name       string
		eventType  event.Type
		reportType event.ReportType
		wantClass  fault.Class
		wantReport event.ReportType
	}{
		{
			name:       "verified report with declared type",
			eventType:  event.TypeVerifiedReport,
			reportType: "git.ref.updated",
			wantReport: "git.ref.updated",
		},
		{
			name:      "verified report without report type",
			eventType: event.TypeVerifiedReport,
			wantClass: fault.InvalidInput,
		},
		{
			name:       "verified report with lifecycle type",
			eventType:  event.TypeVerifiedReport,
			reportType: "observation.custom",
			wantClass:  fault.InvalidInput,
		},
		{
			name:       "verified report with lifecycle alias",
			eventType:  event.TypeVerifiedReport,
			reportType: "request.activated",
			wantClass:  fault.InvalidInput,
		},
		{
			name:       "verified report with canonical lifecycle type",
			eventType:  event.TypeVerifiedReport,
			reportType: "action.requested",
			wantClass:  fault.InvalidInput,
		},
		{
			name:       "verified report with malformed type",
			eventType:  event.TypeVerifiedReport,
			reportType: "Git Ref Updated",
			wantClass:  fault.InvalidInput,
		},
		{
			name:       "lifecycle event with report type",
			eventType:  event.TypeTaskCreated,
			reportType: "git.ref.updated",
			wantClass:  fault.InvalidInput,
		},
		{
			name:       "custom observation with report type",
			eventType:  event.TypeCustomObservation,
			reportType: "git.ref.updated",
			wantClass:  fault.InvalidInput,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			envelope, err := event.NewEnvelope(id.EventID("event-1"), testSource(), testScope(), fixedNow, event.EnvelopeInput{
				Type:       testCase.eventType,
				ReportType: testCase.reportType,
				SubjectID:  "subject-1",
			})
			if testCase.wantClass != "" {
				assertFaultClass(t, err, testCase.wantClass)
				return
			}
			if err != nil {
				t.Fatalf("NewEnvelope: %v", err)
			}
			if envelope.ReportType != testCase.wantReport {
				t.Errorf("report type = %q, want %q", envelope.ReportType, testCase.wantReport)
			}
			if envelope.Type != event.TypeVerifiedReport {
				t.Errorf("verified report envelope type = %q, want %q", envelope.Type, event.TypeVerifiedReport)
			}
		})
	}
}

// TestNewEnvelopePayloadBound asserts the declared 64 KiB event bound: exactly
// at the bound is accepted and one byte over is refused.
func TestNewEnvelopePayloadBound(t *testing.T) {
	if event.DefaultBounds.MaxDataBytes != 64<<10 {
		t.Fatalf("DefaultBounds.MaxDataBytes = %d, want the declared 64 KiB", event.DefaultBounds.MaxDataBytes)
	}

	atLimit := make([]byte, event.DefaultBounds.MaxDataBytes)
	envelope, err := event.NewEnvelope(id.EventID("event-1"), testSource(), testScope(), fixedNow, event.EnvelopeInput{
		Type:      event.TypeCustomObservation,
		SubjectID: "subject-1",
		Data:      atLimit,
	})
	if err != nil {
		t.Fatalf("NewEnvelope at the exact bound: %v", err)
	}
	if len(envelope.Data) != event.DefaultBounds.MaxDataBytes {
		t.Errorf("data length = %d, want %d", len(envelope.Data), event.DefaultBounds.MaxDataBytes)
	}

	overLimit := make([]byte, event.DefaultBounds.MaxDataBytes+1)
	_, err = event.NewEnvelope(id.EventID("event-1"), testSource(), testScope(), fixedNow, event.EnvelopeInput{
		Type:      event.TypeCustomObservation,
		SubjectID: "subject-1",
		Data:      overLimit,
	})
	assertFaultClass(t, err, fault.InvalidInput)
}

// TestNewEnvelopeRefusesInvalidTrustedContext asserts every trusted field the
// constructor receives is validated.
func TestNewEnvelopeRefusesInvalidTrustedContext(t *testing.T) {
	cases := []struct {
		name    string
		eventID id.EventID
		source  event.Source
		scope   tenancy.Scope
		at      time.Time
		input   event.EnvelopeInput
	}{
		{
			name:  "empty event id",
			input: event.EnvelopeInput{Type: event.TypeCustomObservation, SubjectID: "subject-1"},
		},
		{
			name:    "unknown source kind",
			eventID: id.EventID("event-1"),
			source:  event.Source{Kind: event.SourceKind("bogus"), PrincipalID: testPrincipal},
			scope:   testScope(),
			at:      fixedNow,
			input:   event.EnvelopeInput{Type: event.TypeCustomObservation, SubjectID: "subject-1"},
		},
		{
			name:    "empty source principal",
			eventID: id.EventID("event-1"),
			source:  event.Source{Kind: event.SourceExecution},
			scope:   testScope(),
			at:      fixedNow,
			input:   event.EnvelopeInput{Type: event.TypeCustomObservation, SubjectID: "subject-1"},
		},
		{
			name:    "zero time",
			eventID: id.EventID("event-1"),
			source:  testSource(),
			scope:   testScope(),
			input:   event.EnvelopeInput{Type: event.TypeCustomObservation, SubjectID: "subject-1"},
		},
		{
			name:    "invalid scope",
			eventID: id.EventID("event-1"),
			source:  testSource(),
			scope:   tenancy.Scope{},
			at:      fixedNow,
			input:   event.EnvelopeInput{Type: event.TypeCustomObservation, SubjectID: "subject-1"},
		},
		{
			name:    "empty subject",
			eventID: id.EventID("event-1"),
			source:  testSource(),
			scope:   testScope(),
			at:      fixedNow,
			input:   event.EnvelopeInput{Type: event.TypeCustomObservation},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := event.NewEnvelope(testCase.eventID, testCase.source, testCase.scope, testCase.at, testCase.input)
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
}

// TestNewEnvelopeRetainsSuppliedStableID asserts the constructor never
// generates or rewrites the identity: the caller's stable event ID survives,
// so a publication retry republishes the same event.
func TestNewEnvelopeRetainsSuppliedStableID(t *testing.T) {
	eventID := id.EventID("event-stable")
	first, err := event.NewEnvelope(eventID, testSource(), testScope(), fixedNow, event.EnvelopeInput{
		Type:      event.TypeCustomObservation,
		SubjectID: "subject-1",
	})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	second, err := event.NewEnvelope(eventID, testSource(), testScope(), fixedNow.Add(time.Hour), event.EnvelopeInput{
		Type:      event.TypeCustomObservation,
		SubjectID: "subject-2",
	})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if first.ID != eventID || second.ID != eventID {
		t.Errorf("envelope ids = %q/%q, want the supplied %q", first.ID, second.ID, eventID)
	}
}

// TestNewEnvelopeOutputValidates asserts a constructed envelope satisfies the
// declared Envelope.Validate contract.
func TestNewEnvelopeOutputValidates(t *testing.T) {
	envelope, err := event.NewEnvelope(id.EventID("event-1"), testSource(), testScope(), fixedNow, event.EnvelopeInput{
		Type:          event.TypeCustomObservation,
		SubjectID:     "subject-1",
		CorrelationID: "correlation-1",
		CausationID:   "causation-1",
		Data:          []byte(`{"detail":"built"}`),
	})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("constructed envelope does not validate: %v", err)
	}
	if envelope.Source != testSource() {
		t.Errorf("source = %#v, want the trusted %#v", envelope.Source, testSource())
	}
	if envelope.Scope != testScope() {
		t.Errorf("scope = %#v, want the trusted %#v", envelope.Scope, testScope())
	}
	if !envelope.At.Equal(fixedNow) {
		t.Errorf("time = %v, want the trusted %v", envelope.At, fixedNow)
	}
}
