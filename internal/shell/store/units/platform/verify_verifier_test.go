package platform_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// canonicalApprovalBase and canonicalApprovalCandidate are the immutable effect
// identities the canonical Git approval subject carries.
const (
	canonicalApprovalBase      = "1111111111111111111111111111111111111111"
	canonicalApprovalCandidate = "2222222222222222222222222222222222222222"
)

// canonicalStoredRequestSubject is the exact five-field immutable request
// subject a Git approval action records: operation, resource (the registered
// repository resource identity), ref, expected_base and candidate.
func canonicalStoredRequestSubject(resourceID string) []byte {
	return storedRequestSubjectFields(map[string]string{
		"operation":     "git.ref.update",
		"resource":      resourceID,
		"ref":           "refs/heads/main",
		"expected_base": canonicalApprovalBase,
		"candidate":     canonicalApprovalCandidate,
	})
}

// approvalAttempt is the enforcing service's verification attempt. The subject
// is the canonical five-field `git.ref.update` subject the Git service
// supplies; every field is required and the recorded evidence preserves the
// operation and resource fields.
func approvalAttempt(resourceID resource.ResourceID) verify.Attempt {
	return verify.Attempt{
		Scope:      testScope(),
		Service:    testServicePrincipal(),
		Credential: "kodex.exec-1.credential-material",
		ResourceID: resourceID,
		Operation:  resource.OperationGitRefUpdate,
		Subject: verify.Subject{Fields: []verify.SubjectField{
			{Name: "operation", Kind: verify.KindString, Value: "git.ref.update"},
			{Name: "resource", Kind: verify.KindString, Value: string(resourceID)},
			{Name: "ref", Kind: verify.KindString, Value: "refs/heads/main"},
			{Name: "expected_base", Kind: verify.KindString, Value: canonicalApprovalBase},
			{Name: "candidate", Kind: verify.KindString, Value: canonicalApprovalCandidate},
		}},
		Approval: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
		ActionID: testAction,
	}
}

type approvalPrincipals struct{}

func (approvalPrincipals) Resolve(context.Context, string) (identity.Principal, error) {
	return executionActorPrincipal(), nil
}

type approvalBindings struct{}

// ReadBinding answers for whichever resource the attempt names, so a
// resource-mismatch case fails on the evidence match rather than the binding.
func (approvalBindings) ReadBinding(_ context.Context, query verify.BindingQuery) (resource.Binding, error) {
	return resource.Binding{
		ID:               resource.BindingID(testBinding),
		ResourceID:       query.ResourceID,
		Scope:            query.Scope,
		EnforcingService: service.ServiceID(query.ServiceID),
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Version:          7,
	}, nil
}

type approvalRecords struct {
	units []verify.RecordUnit
}

func (records *approvalRecords) Record(_ context.Context, unit verify.RecordUnit) (verify.RecordResult, error) {
	records.units = append(records.units, unit)
	return verify.RecordResult{ID: unit.Record.ID}, nil
}

type approvalClock struct{}

func (approvalClock) Now() time.Time { return fixedNow }

// approvalVerifier builds the real verifier with the platform evidence reader,
// so an approval decision is proven through the store-backed read.
func approvalVerifier(t *testing.T, store *memoryStoreAlias) (*verify.Verifier, *approvalRecords) {
	t.Helper()
	reader, err := platform.NewEvidenceReader(store)
	if err != nil {
		t.Fatalf("NewEvidenceReader: %v", err)
	}
	records := &approvalRecords{}
	next := 0
	verifier, err := verify.New(verify.Config{
		Principals: approvalPrincipals{},
		Bindings:   approvalBindings{},
		Evidence:   reader,
		Records:    records,
		Clock:      approvalClock{},
		NewID: func() (verify.VerificationID, error) {
			next++
			return verify.VerificationID(fmt.Sprintf("verification-%d", next)), nil
		},
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	return verifier, records
}

// TestEvidenceReaderFeedsRealApprovalMatch proves a stored canonical request
// subject carrying operation, resource and every effect field matches a real
// verification attempt through this reader end to end.
func TestEvidenceReaderFeedsRealApprovalMatch(t *testing.T) {
	s := newStore()
	seedActionSubject(t, s, testScope(), testAction, testExecution, canonicalStoredRequestSubject(testResource))
	verifier, records := approvalVerifier(t, s)

	record, err := verifier.Verify(context.Background(), approvalAttempt(resource.ResourceID(testResource)))
	if err != nil {
		t.Fatalf("Verify with matching evidence: %v", err)
	}
	if record.Result != verify.ResultApproved || !record.Approved {
		t.Errorf("record = %#v, want an approved verification", record)
	}
	if len(records.units) != 1 || !records.units[0].Record.Approved {
		t.Fatalf("committed units = %d, want one approved record", len(records.units))
	}
}

// TestEvidenceReaderEmptySubjectIsNotAWildcard proves an empty required subject
// never matches a recorded request subject that carries the operation and
// resource approval fields.
func TestEvidenceReaderEmptySubjectIsNotAWildcard(t *testing.T) {
	s := newStore()
	seedActionSubject(t, s, testScope(), testAction, testExecution, storedRequestSubjectFields(map[string]string{
		"operation": "git.ref.update",
		"resource":  testResource,
	}))
	verifier, records := approvalVerifier(t, s)
	attempt := approvalAttempt(resource.ResourceID(testResource))
	attempt.Subject = verify.Subject{}
	_, err := verifier.Verify(context.Background(), attempt)
	requireClass(t, err, fault.Forbidden)
	if len(records.units) != 1 || records.units[0].Record.Approved {
		t.Errorf("committed units = %#v, want one refused record", records.units)
	}
}

// TestEvidenceReaderApprovalMismatches asserts a stored operation or resource
// that differs from the attempt cannot match, and a subject missing either
// approval field is returned as unmatched evidence rather than failing the
// whole read.
func TestEvidenceReaderApprovalMismatches(t *testing.T) {
	cases := []struct {
		name       string
		subject    []byte
		resourceID resource.ResourceID
		wantOp     string
		wantRes    resource.ResourceID
	}{
		{
			name: "different operation",
			subject: storedRequestSubjectFields(map[string]string{
				"operation": "git.ref.other",
				"resource":  testResource,
				"ref":       "refs/heads/main",
			}),
			resourceID: resource.ResourceID(testResource),
			wantOp:     "git.ref.other",
			wantRes:    resource.ResourceID(testResource),
		},
		{
			name: "different resource",
			subject: storedRequestSubjectFields(map[string]string{
				"operation": "git.ref.update",
				"resource":  "repo-2",
				"ref":       "refs/heads/main",
			}),
			resourceID: resource.ResourceID(testResource),
			wantOp:     "git.ref.update",
			wantRes:    resource.ResourceID("repo-2"),
		},
		{
			name: "missing operation",
			subject: storedRequestSubjectFields(map[string]string{
				"resource": testResource,
				"ref":      "refs/heads/main",
			}),
			resourceID: resource.ResourceID(testResource),
			wantOp:     "",
			wantRes:    resource.ResourceID(testResource),
		},
		{
			name: "missing resource",
			subject: storedRequestSubjectFields(map[string]string{
				"operation": "git.ref.update",
				"ref":       "refs/heads/main",
			}),
			resourceID: resource.ResourceID(testResource),
			wantOp:     "git.ref.update",
			wantRes:    "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			seedActionSubject(t, s, testScope(), testAction, testExecution, testCase.subject)
			reader, err := platform.NewEvidenceReader(s)
			if err != nil {
				t.Fatalf("NewEvidenceReader: %v", err)
			}
			evidence, err := reader.ReadEvidence(context.Background(), verify.EvidenceQuery{
				Scope:       testScope(),
				ExecutionID: testExecution,
				Assignment:  request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
				Operation:   "git.ref.update",
				ResourceID:  testCase.resourceID,
				Subject:     testSubject(),
			})
			if err != nil {
				t.Fatalf("ReadEvidence: %v", err)
			}
			if len(evidence) != 1 {
				t.Fatalf("evidence = %d, want 1", len(evidence))
			}
			if evidence[0].Operation != testCase.wantOp || evidence[0].ResourceID != testCase.wantRes {
				t.Errorf("evidence operation/resource = %q/%q, want %q/%q",
					evidence[0].Operation, evidence[0].ResourceID, testCase.wantOp, testCase.wantRes)
			}

			verifier, records := approvalVerifier(t, s)
			_, err = verifier.Verify(context.Background(), approvalAttempt(testCase.resourceID))
			requireClass(t, err, fault.Forbidden)
			if len(records.units) != 1 || records.units[0].Record.Approved {
				t.Errorf("committed units = %#v, want one refused record", records.units)
			}
		})
	}
}
