package app

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// TestVerificationClientRefusesNonStrictResultBodies pins that the public
// verification/report wrappers decode result bodies with the public operation
// codec: an unknown member at any nesting level, a non-object body or trailing
// data is refused as a dependency-class invalid response instead of being
// silently accepted by json.Unmarshal.
func TestVerificationClientRefusesNonStrictResultBodies(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		body      string
	}{
		{
			name:      "verify top-level unknown member",
			operation: "actor.verify",
			body:      `{"principal":{"id":"principal-1","kind":"human"},"verification_id":"verification-1","unexpected_member":true}`,
		},
		{
			name:      "verify nested unknown member",
			operation: "actor.verify",
			body:      `{"principal":{"id":"principal-1","kind":"human","unexpected_member":true},"verification_id":"verification-1"}`,
		},
		{
			name:      "verify trailing data",
			operation: "actor.verify",
			body:      `{"principal":{"id":"principal-1","kind":"human"},"verification_id":"verification-1"}{}`,
		},
		{
			name:      "report top-level unknown member",
			operation: "resource.report",
			body:      `{"verification_id":"verification-1","event_id":"event-2","unexpected_member":true}`,
		},
		{
			name:      "report trailing data",
			operation: "resource.report",
			body:      `{"verification_id":"verification-1","event_id":"event-2"}[]`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{result: api.Result{Operation: testCase.operation, Body: []byte(testCase.body)}}
			verification, err := NewVerificationClient(transport)
			if err != nil {
				t.Fatalf("NewVerificationClient: %v", err)
			}
			switch testCase.operation {
			case "actor.verify":
				verified, verifyErr := verification.Verify(context.Background(), validVerifyRequest())
				if verifyErr == nil {
					t.Fatalf("Verify accepted %s", testCase.name)
				}
				if verified.Principal.ID != "" || verified.Principal.Kind != "" || verified.VerificationID != "" || len(verified.Roles) != 0 || verified.Approval {
					t.Fatalf("Verify returned a partially applied response: %+v", verified)
				}
				err = verifyErr
			case "resource.report":
				reported, reportErr := verification.Report(context.Background(), validReportRequest())
				if reportErr == nil {
					t.Fatalf("Report accepted %s", testCase.name)
				}
				if reported != (api.ReportResponse{}) {
					t.Fatalf("Report returned a partially applied response: %+v", reported)
				}
				err = reportErr
			default:
				t.Fatalf("unhandled operation %q", testCase.operation)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.DependencyUnavailable {
				t.Fatalf("error class = %q/%v (%v); want dependency_unavailable", class, ok, err)
			}
		})
	}
}

// TestVerificationClientAcceptsWellFormedStrictResults pins that the strict
// codec still accepts a fully populated, schema-conforming result.
func TestVerificationClientAcceptsWellFormedStrictResults(t *testing.T) {
	verifyBody := []byte(`{"principal":{"id":"principal-1","kind":"human"},"roles":["engineer"],"approval":true,"verification_id":"verification-1"}`)
	verification, err := NewVerificationClient(&fakeTransport{result: api.Result{Operation: "actor.verify", Body: verifyBody}})
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	response, err := verification.Verify(context.Background(), validVerifyRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if response.VerificationID != "verification-1" || !response.Approval || len(response.Roles) != 1 {
		t.Fatalf("response = %+v", response)
	}

	reportBody := []byte(`{"verification_id":"verification-1","event_id":"event-2","replayed":true}`)
	reporter, err := NewVerificationClient(&fakeTransport{result: api.Result{Operation: "resource.report", Body: reportBody}})
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	report, err := reporter.Report(context.Background(), validReportRequest())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if report.EventID != "event-2" || !report.Replayed {
		t.Fatalf("report = %+v", report)
	}
}
