package git_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	git "github.com/optimiware/kodama/internal/userland/git"
)

func updateRequestFor(repository, ref, expected, candidate, sourceEventID string) git.UpdateRequest {
	return git.UpdateRequest{
		ResourceID:      testResourceID,
		Repository:      repository,
		Ref:             ref,
		ExpectedBase:    expected,
		Candidate:       candidate,
		ActorCredential: testCredential,
		SourceEventID:   sourceEventID,
	}
}

func executionScriptWithApproval(principalID string, approval bool) verificationScript {
	script := executionScript(principalID)
	script.approval = approval
	return script
}

func humanScriptWithApproval(principalID string, approval bool, roles ...string) verificationScript {
	script := humanScript(principalID, roles...)
	script.approval = approval
	return script
}

func assertCanonicalSubject(t *testing.T, subject api.Subject, resourceID, ref, expectedBase, candidate string) {
	t.Helper()
	want := []api.SubjectField{
		{Name: "operation", Kind: "string", Value: "git.ref.update"},
		{Name: "resource", Kind: "string", Value: resourceID},
		{Name: "ref", Kind: "string", Value: ref},
		{Name: "expected_base", Kind: "string", Value: expectedBase},
		{Name: "candidate", Kind: "string", Value: candidate},
	}
	if !reflect.DeepEqual(subject.Fields, want) {
		t.Fatalf("subject fields = %+v; want %+v", subject.Fields, want)
	}
	for _, field := range subject.Fields {
		if field.Name == "repository" {
			t.Fatal("subject uses the repository alias; the canonical field is resource")
		}
	}
	if err := subject.Validate(); err != nil {
		t.Fatalf("canonical subject refused: %v", err)
	}
}

// TestUpdateVerifiesCanonicalSubjectBeforeEffect pins the receive sequence for
// one successful push: verify the exact `git.ref.update` subject, persist the
// intent with the stable source/verification IDs, compare-and-swap the ref,
// durably record acceptance, then report the verified effect.
func TestUpdateVerifiesCanonicalSubjectBeforeEffect(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)
	f.executor.setRef("shop/web", testRef, testBase)

	result, err := f.service.Update(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.Entry.State != git.JournalReported {
		t.Fatalf("settled state = %q; want reported", result.Entry.State)
	}
	if !result.Reported {
		t.Fatal("Update reported=false for a reported entry")
	}
	if result.Entry.SourceEventID != testSourceEventID || result.Entry.VerificationID != testVerificationID {
		t.Fatalf("stable identifiers = %q/%q", result.Entry.SourceEventID, result.Entry.VerificationID)
	}
	if result.Entry.Version != git.JournalVersion || !result.Entry.At.Equal(testNow) {
		t.Fatalf("journal format = version %d at %v", result.Entry.Version, result.Entry.At)
	}
	if result.Ref != (git.RefState{Ref: testRef, Commit: testCandidate}) {
		t.Fatalf("observed ref = %+v; want the candidate", result.Ref)
	}
	if result.Report.VerificationID != testVerificationID {
		t.Fatalf("report verification id = %q", result.Report.VerificationID)
	}
	if err := result.Report.Validate(); err != nil {
		t.Fatalf("report response refused: %v", err)
	}

	verifications := f.verifier.verifyRequests()
	if len(verifications) != 1 {
		t.Fatalf("verification calls = %d; want exactly one", len(verifications))
	}
	verification := verifications[0]
	if err := verification.Validate(); err != nil {
		t.Fatalf("built verify request refused: %v", err)
	}
	if verification.ActorCredential != testCredential {
		t.Fatal("verify request did not forward the actor credential")
	}
	if verification.ResourceID != testResourceID || verification.Operation != "git.ref.update" {
		t.Fatalf("verify request identity = %+v", verification)
	}
	if verification.Approval != "role/reviewer" {
		t.Fatalf("verify approval = %q; want role/reviewer for the guarded main branch", verification.Approval)
	}
	assertCanonicalSubject(t, verification.Subject, testResourceID, testRef, testBase, testCandidate)

	reports := f.verifier.reportRequests()
	if len(reports) != 1 {
		t.Fatalf("report calls = %d; want exactly one", len(reports))
	}
	report := reports[0]
	if err := report.Validate(); err != nil {
		t.Fatalf("built report request refused: %v", err)
	}
	if report.ResourceID != testResourceID || report.SourceEventID != testSourceEventID ||
		report.VerificationID != testVerificationID || report.Operation != "git.ref.update" ||
		report.EventType != "git.ref.updated" {
		t.Fatalf("report identity = %+v", report)
	}
	if !reflect.DeepEqual(report.Subject, verification.Subject) {
		t.Fatalf("report subject %+v does not match the verified subject %+v", report.Subject, verification.Subject)
	}
	assertCanonicalSubject(t, report.Subject, testResourceID, testRef, testBase, testCandidate)
	resourceValue := ""
	for _, field := range report.Subject.Fields {
		if field.Name == "resource" {
			resourceValue = field.Value
		}
	}
	if resourceValue != testResourceID {
		t.Fatalf("report subject resource = %q; want the registered resource id %q", resourceValue, testResourceID)
	}

	if f.executor.updateRefCount() != 1 {
		t.Fatalf("update-ref commands = %d; want one", f.executor.updateRefCount())
	}

	order := []string{
		"verify",
		"journal:record:intended",
		"exec:update-ref",
		"journal:record:accepted",
		"report",
		"journal:record:reported",
	}
	previous := -1
	for _, entry := range order {
		index := f.log.index(entry)
		if index < 0 {
			t.Fatalf("call %q missing from %v", entry, f.log.snapshot())
		}
		if index < previous {
			t.Fatalf("call %q out of order in %v", entry, f.log.snapshot())
		}
		previous = index
	}
}

// TestUpdateRuleBoundaries pins ordered rule evaluation against the computed
// update kind: RW permits create and fast-forward, RW+ additionally permits a
// rewind, a deny or no match refuses, and a ref prefix only matches at a slash
// boundary.
func TestUpdateRuleBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		repository string
		ref        string
		expected   string
		candidate  string
		script     verificationScript
		setup      func(*fixture)
		wantErr    error
	}{
		{
			name: "engineer creates a work branch", repository: "shop/web", ref: "refs/heads/work/x",
			expected: "", candidate: "work-1", script: humanScript("prn-eng", "engineer"),
		},
		{
			name: "engineer rewinds a work branch with RW+", repository: "shop/web", ref: "refs/heads/work/x",
			expected: "work-2", candidate: "work-1", script: humanScript("prn-eng", "engineer"),
			setup: func(f *fixture) {
				f.executor.setRef("shop/web", "refs/heads/work/x", "work-2")
				f.executor.setAncestor("shop/web", "work-2", "work-1", false)
			},
		},
		{
			name: "execution fast-forwards protected main", repository: "shop/web", ref: testRef,
			expected: testBase, candidate: testCandidate, script: executionScript("exe-1"),
			setup: func(f *fixture) {
				f.executor.setRef("shop/web", testRef, testBase)
				f.executor.setAncestor("shop/web", testBase, testCandidate, true)
			},
		},
		{
			name: "execution cannot rewind protected main", repository: "shop/web", ref: testRef,
			expected: testBase, candidate: testCandidate, script: executionScript("exe-1"),
			setup: func(f *fixture) {
				f.executor.setRef("shop/web", testRef, testBase)
				f.executor.setAncestor("shop/web", testBase, testCandidate, false)
			},
			wantErr: git.ErrPolicyDenied,
		},
		{
			name: "engineer cannot create protected main", repository: "shop/web", ref: testRef,
			expected: "", candidate: testCandidate, script: humanScript("prn-eng", "engineer"),
			wantErr: git.ErrPolicyDenied,
		},
		{
			name: "viewer cannot create", repository: "shop/web", ref: "refs/heads/work/x",
			expected: "", candidate: "work-1", script: humanScript("prn-view", "viewer"),
			wantErr: git.ErrPolicyDenied,
		},
		{
			name: "prefix does not match without a slash boundary", repository: "shop/web", ref: "refs/heads/work-notes/x",
			expected: "", candidate: "notes-1", script: humanScript("prn-eng", "engineer"),
			wantErr: git.ErrPolicyDenied,
		},
		{
			name: "pipeline service account creates guarded svc", repository: "shop/guarded", ref: "refs/heads/svc",
			expected: "", candidate: "svc-1", script: serviceScript("ci-pipeline"),
		},
		{
			name: "unlisted service account refuses", repository: "shop/guarded", ref: "refs/heads/svc",
			expected: "", candidate: "svc-1", script: serviceScript("other-svc"),
			wantErr: git.ErrPolicyDenied,
		},
		{
			name: "no matching ref refuses", repository: "shop/guarded", ref: "refs/tags/v1",
			expected: "", candidate: "tag-1", script: humanScript("prn-eng", "engineer"),
			wantErr: git.ErrPolicyDenied,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t)
			f.verifier.script(testCredential, testCase.script)
			if testCase.setup != nil {
				testCase.setup(f)
			}
			request := updateRequestFor(testCase.repository, testCase.ref, testCase.expected, testCase.candidate, testSourceEventID)
			result, err := f.service.Update(context.Background(), request)

			if testCase.wantErr != nil {
				if !errors.Is(err, testCase.wantErr) {
					t.Fatalf("Update error = %v; want %v", err, testCase.wantErr)
				}
				if f.executor.updateRefCount() != 0 {
					t.Fatalf("denied update ran %d update-ref commands", f.executor.updateRefCount())
				}
				if calls := f.verifier.reportRequests(); len(calls) != 0 {
					t.Fatalf("denied update reported %d times", len(calls))
				}
				state, ok := f.journal.state(testSourceEventID)
				if !ok || state != git.JournalRefused {
					t.Fatalf("denied journal state = %q,%v; want refused", state, ok)
				}
				entry, _ := f.journal.entry(testSourceEventID)
				if entry.VerificationID != testVerificationID {
					t.Fatalf("refused entry verification id = %q; want the verified id", entry.VerificationID)
				}
				if result.Entry.State != git.JournalRefused {
					t.Fatalf("returned state = %q; want refused", result.Entry.State)
				}
				return
			}

			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if result.Entry.State != git.JournalReported {
				t.Fatalf("state = %q; want reported", result.Entry.State)
			}
			if got := f.executor.commit(testCase.repository, testCase.ref); got != testCase.candidate {
				t.Fatalf("ref = %q; want candidate %q", got, testCase.candidate)
			}
			if f.executor.updateRefCount() != 1 {
				t.Fatalf("update-ref commands = %d; want one", f.executor.updateRefCount())
			}
		})
	}
}

// TestUpdateApprovalGuardedRefs pins approval handling: only an eligible
// execution can satisfy `option approve.<ref>`, humans and services never carry
// approval evidence even if a verifier claims it, and an unguarded ref sends no
// approval request.
func TestUpdateApprovalGuardedRefs(t *testing.T) {
	t.Run("execution with current approval updates", func(t *testing.T) {
		f := newFixture(t)
		f.verifier.script(testCredential, executionScriptWithApproval("exe-1", true))
		f.executor.setRef("shop/web", testRef, testBase)
		f.executor.setAncestor("shop/web", testBase, testCandidate, true)
		result, err := f.service.Update(context.Background(), f.updateRequest())
		if err != nil || result.Entry.State != git.JournalReported {
			t.Fatalf("Update = %+v,%v; want reported", result.Entry.State, err)
		}
	})

	t.Run("execution without approval refuses", func(t *testing.T) {
		f := newFixture(t)
		f.verifier.script(testCredential, executionScriptWithApproval("exe-1", false))
		f.executor.setRef("shop/web", testRef, testBase)
		f.executor.setAncestor("shop/web", testBase, testCandidate, true)
		_, err := f.service.Update(context.Background(), f.updateRequest())
		if !errors.Is(err, git.ErrApprovalRequired) {
			t.Fatalf("Update error = %v; want ErrApprovalRequired", err)
		}
		if f.executor.updateRefCount() != 0 {
			t.Fatalf("unapproved update ran %d update-ref commands", f.executor.updateRefCount())
		}
		verifications := f.verifier.verifyRequests()
		if len(verifications) != 1 || verifications[0].Approval != "role/reviewer" {
			t.Fatalf("verify approval = %+v; want role/reviewer", verifications)
		}
	})

	t.Run("human cannot carry approval", func(t *testing.T) {
		f := newFixture(t)
		// The verifier maliciously claims approval; the service must still
		// refuse a human on an approval-guarded ref.
		f.verifier.script(testCredential, humanScriptWithApproval("prn-eng", true, "engineer"))
		request := updateRequestFor("shop/guarded", "refs/heads/release/2026", "", "rel-1", testSourceEventID)
		_, err := f.service.Update(context.Background(), request)
		if !errors.Is(err, git.ErrApprovalRequired) {
			t.Fatalf("Update error = %v; want ErrApprovalRequired", err)
		}
		if f.executor.updateRefCount() != 0 {
			t.Fatalf("human update ran %d update-ref commands", f.executor.updateRefCount())
		}
		verifications := f.verifier.verifyRequests()
		if len(verifications) != 1 || verifications[0].Approval != "role/reviewer" {
			t.Fatalf("verify approval = %+v; want role/reviewer", verifications)
		}
	})

	t.Run("service account cannot carry approval", func(t *testing.T) {
		f := newFixtureWithConfig(t, "repo shop/web\n    RW refs/heads/main = shop-svc\n    option approve.refs/heads/main = reviewer\n")
		f.verifier.script(testCredential, serviceScript("shop-svc"))
		f.verifier.mu.Lock()
		script := f.verifier.scripts[testCredential]
		script.approval = true
		f.verifier.scripts[testCredential] = script
		f.verifier.mu.Unlock()
		f.executor.setRef("shop/web", testRef, testBase)
		f.executor.setAncestor("shop/web", testBase, testCandidate, true)
		_, err := f.service.Update(context.Background(), f.updateRequest())
		if !errors.Is(err, git.ErrApprovalRequired) {
			t.Fatalf("Update error = %v; want ErrApprovalRequired", err)
		}
		if f.executor.updateRefCount() != 0 {
			t.Fatalf("service update ran %d update-ref commands", f.executor.updateRefCount())
		}
	})

	t.Run("unguarded ref sends no approval", func(t *testing.T) {
		f := newFixture(t)
		// The script claims approval, but an unguarded ref must not request it.
		f.verifier.script(testCredential, humanScriptWithApproval("prn-eng", true, "engineer"))
		request := updateRequestFor("shop/web", "refs/heads/work/x", "", "work-1", testSourceEventID)
		result, err := f.service.Update(context.Background(), request)
		if err != nil || result.Entry.State != git.JournalReported {
			t.Fatalf("Update = %+v,%v; want reported", result.Entry.State, err)
		}
		verifications := f.verifier.verifyRequests()
		if len(verifications) != 1 || verifications[0].Approval != "" {
			t.Fatalf("verify approval = %+v; want empty", verifications)
		}
	})
}

// TestUpdateValidatesVerificationFacts pins that the service refuses an
// unusable verification: an unknown credential, a transport failure, a missing
// verification ID or verified roles/IDs the declared vocabulary rejects never
// produce an intent or an effect.
func TestUpdateValidatesVerificationFacts(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*fixture)
	}{
		{"unknown credential", func(f *fixture) {}},
		{"verification transport failure", func(f *fixture) {
			f.verifier.mu.Lock()
			f.verifier.verifyErr = errors.New("identity service unavailable")
			f.verifier.mu.Unlock()
		}},
		{"missing verification id", func(f *fixture) {
			f.verifier.script(testCredential, verificationScript{
				principal: api.VerifiedPrincipal{ID: "exe-1", Kind: "execution"},
			})
		}},
		{"unknown verified role", func(f *fixture) {
			f.verifier.script(testCredential, verificationScript{
				principal:      api.VerifiedPrincipal{ID: "prn-1", Kind: "human"},
				roles:          []string{"root"},
				verificationID: testVerificationID,
			})
		}},
		{"invalid service id", func(f *fixture) {
			f.verifier.script(testCredential, verificationScript{
				principal:      api.VerifiedPrincipal{ID: "@bad", Kind: "service"},
				verificationID: testVerificationID,
			})
		}},
		{"execution with roles", func(f *fixture) {
			f.verifier.script(testCredential, verificationScript{
				principal:      api.VerifiedPrincipal{ID: "exe-1", Kind: "execution"},
				roles:          []string{"engineer"},
				verificationID: testVerificationID,
			})
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t)
			f.executor.setRef("shop/web", testRef, testBase)
			f.executor.setAncestor("shop/web", testBase, testCandidate, true)
			testCase.prepare(f)
			if _, err := f.service.Update(context.Background(), f.updateRequest()); err == nil {
				t.Fatal("Update accepted an unusable verification")
			}
			if f.executor.updateRefCount() != 0 {
				t.Fatalf("unusable verification ran %d update-ref commands", f.executor.updateRefCount())
			}
			if state, ok := f.journal.state(testSourceEventID); ok {
				t.Fatalf("unusable verification persisted a %q journal entry", state)
			}
		})
	}
}

// TestUpdateStaleBaseRefusals pins compare-and-swap intent: a ref that is not
// the expected base is refused before any effect, and a concurrent move between
// observation and update is refused rather than force-updated.
func TestUpdateStaleBaseRefusals(t *testing.T) {
	t.Run("moved before the attempt", func(t *testing.T) {
		f := newFixture(t)
		f.verifier.script(testCredential, executionScript("exe-1"))
		f.executor.setRef("shop/web", testRef, "someone-else")
		f.executor.setAncestor("shop/web", testBase, testCandidate, true)
		_, err := f.service.Update(context.Background(), f.updateRequest())
		if !errors.Is(err, git.ErrStaleBase) {
			t.Fatalf("Update error = %v; want ErrStaleBase", err)
		}
		if f.executor.updateRefCount() != 0 {
			t.Fatalf("stale attempt ran %d update-ref commands", f.executor.updateRefCount())
		}
		if state, ok := f.journal.state(testSourceEventID); !ok || state != git.JournalRefused {
			t.Fatalf("stale journal state = %q,%v; want refused", state, ok)
		}
	})

	t.Run("create when the ref exists", func(t *testing.T) {
		f := newFixture(t)
		f.verifier.script(testCredential, humanScript("prn-eng", "engineer"))
		f.executor.setRef("shop/web", "refs/heads/work/x", "existing")
		request := updateRequestFor("shop/web", "refs/heads/work/x", "", "work-1", testSourceEventID)
		_, err := f.service.Update(context.Background(), request)
		if !errors.Is(err, git.ErrStaleBase) {
			t.Fatalf("Update error = %v; want ErrStaleBase", err)
		}
		if f.executor.updateRefCount() != 0 {
			t.Fatalf("stale create ran %d update-ref commands", f.executor.updateRefCount())
		}
	})

	t.Run("concurrent move loses the compare-and-swap", func(t *testing.T) {
		f := newFixture(t)
		f.verifier.script(testCredential, executionScript("exe-1"))
		f.executor.setRef("shop/web", testRef, "moved-by-someone-else")
		f.executor.setAncestor("shop/web", testBase, testCandidate, true)
		// Observation still reports the expected base, so the service must
		// rely on the compare-and-swap and never force the update.
		f.observer.overrides = map[string]string{"shop/web|" + testRef: testBase}
		_, err := f.service.Update(context.Background(), f.updateRequest())
		if !errors.Is(err, git.ErrStaleBase) {
			t.Fatalf("Update error = %v; want ErrStaleBase", err)
		}
		if f.executor.updateRefCount() != 1 {
			t.Fatalf("update-ref commands = %d; want one failed compare-and-swap", f.executor.updateRefCount())
		}
		if got := f.executor.commit("shop/web", testRef); got != "moved-by-someone-else" {
			t.Fatalf("ref = %q; the service overwrote a concurrent move", got)
		}
		if state, ok := f.journal.state(testSourceEventID); !ok || state != git.JournalRefused {
			t.Fatalf("journal state = %q,%v; want refused", state, ok)
		}
	})
}

// TestUpdateRejectsUnrepresentableRequests pins request validation before any
// verification or repository command. The journal cannot carry a delete
// (empty candidate), so a delete attempt refuses instead of fabricating one.
func TestUpdateRejectsUnrepresentableRequests(t *testing.T) {
	base := git.UpdateRequest{
		ResourceID:      testResourceID,
		Repository:      "shop/web",
		Ref:             testRef,
		ExpectedBase:    testBase,
		Candidate:       testCandidate,
		ActorCredential: testCredential,
		SourceEventID:   testSourceEventID,
	}
	cases := []struct {
		name   string
		mutate func(*git.UpdateRequest)
		want   error
	}{
		{"delete has no journal representation", func(request *git.UpdateRequest) { request.Candidate = "" }, nil},
		{"empty attempt", func(request *git.UpdateRequest) { request.ExpectedBase = ""; request.Candidate = "" }, nil},
		{"missing source event id", func(request *git.UpdateRequest) { request.SourceEventID = "" }, nil},
		{"missing resource id", func(request *git.UpdateRequest) { request.ResourceID = "" }, nil},
		{"missing credential", func(request *git.UpdateRequest) { request.ActorCredential = "" }, nil},
		{"escaped repository", func(request *git.UpdateRequest) { request.Repository = "../escape" }, git.ErrInvalidRepository},
		{"absolute repository", func(request *git.UpdateRequest) { request.Repository = "/srv/git" }, git.ErrInvalidRepository},
		{"unanchored ref", func(request *git.UpdateRequest) { request.Ref = "heads/main" }, nil},
		{"prefix ref", func(request *git.UpdateRequest) { request.Ref = "refs/heads/work/" }, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t)
			f.verifier.script(testCredential, executionScript("exe-1"))
			request := base
			testCase.mutate(&request)
			_, err := f.service.Update(context.Background(), request)
			if err == nil {
				t.Fatal("Update accepted an unrepresentable request")
			}
			if testCase.want != nil && !errors.Is(err, testCase.want) {
				t.Fatalf("Update error = %v; want %v", err, testCase.want)
			}
			if calls := f.verifier.verifyRequests(); len(calls) != 0 {
				t.Fatalf("invalid request verified %d times", len(calls))
			}
			if f.executor.updateRefCount() != 0 {
				t.Fatalf("invalid request ran %d update-ref commands", f.executor.updateRefCount())
			}
			if len(f.journal.recorded()) != 0 {
				t.Fatalf("invalid request persisted %d journal entries", len(f.journal.recorded()))
			}
		})
	}
}

// TestUpdateNeverPersistsActorCredential pins credential hygiene: the forwarded
// actor credential is the only credential field on the verification request and
// appears in no journal entry, report request, command or repository metadata.
func TestUpdateNeverPersistsActorCredential(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)
	if _, err := f.service.Update(context.Background(), f.updateRequest()); err != nil {
		t.Fatalf("Update: %v", err)
	}

	verifications := f.verifier.verifyRequests()
	if len(verifications) != 1 || verifications[0].ActorCredential != testCredential {
		t.Fatal("the verification request must carry the forwarded credential")
	}

	encoded, err := json.Marshal(f.journal.recorded())
	if err != nil {
		t.Fatalf("marshal journal records: %v", err)
	}
	reports, err := json.Marshal(f.verifier.reportRequests())
	if err != nil {
		t.Fatalf("marshal report requests: %v", err)
	}
	commands, err := json.Marshal(f.executor.commandsSnapshot())
	if err != nil {
		t.Fatalf("marshal commands: %v", err)
	}
	for name, blob := range map[string][]byte{"journal": encoded, "reports": reports, "commands": commands} {
		if strings.Contains(string(blob), testCredential) {
			t.Fatalf("%s contains the actor credential: %s", name, blob)
		}
	}

	for _, record := range f.journal.recorded() {
		assertNoCredentialField(t, reflect.TypeOf(record), "journal")
	}
	for _, report := range f.verifier.reportRequests() {
		assertNoCredentialField(t, reflect.TypeOf(report), "report")
	}
}

func assertNoCredentialField(t *testing.T, recordType reflect.Type, name string) {
	t.Helper()
	for index := 0; index < recordType.NumField(); index++ {
		field := strings.ToLower(recordType.Field(index).Name)
		for _, forbidden := range []string{"credential", "password", "secret", "token"} {
			if strings.Contains(field, forbidden) {
				t.Fatalf("%s record has forbidden field %q", name, recordType.Field(index).Name)
			}
		}
	}
}

// TestUpdateStableIdentifiersAndDuplicateDelivery pins idempotent redelivery:
// an identical source event settles the existing entry without a second
// verification, effect or report, while changed content under the same source
// event id refuses.
func TestUpdateStableIdentifiersAndDuplicateDelivery(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	request := f.updateRequest()
	first, err := f.service.Update(context.Background(), request)
	if err != nil || first.Entry.State != git.JournalReported {
		t.Fatalf("first update = %+v,%v", first.Entry.State, err)
	}

	second, err := f.service.Update(context.Background(), request)
	if err != nil {
		t.Fatalf("duplicate delivery: %v", err)
	}
	if second.Entry.SourceEventID != first.Entry.SourceEventID || second.Entry.VerificationID != first.Entry.VerificationID {
		t.Fatalf("duplicate identifiers = %q/%q", second.Entry.SourceEventID, second.Entry.VerificationID)
	}
	if second.Entry.State != git.JournalReported || !second.Reported {
		t.Fatalf("duplicate state = %q,%v; want reported", second.Entry.State, second.Reported)
	}
	if calls := f.verifier.verifyRequests(); len(calls) != 1 {
		t.Fatalf("duplicate delivery verified %d times; want one", len(calls))
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("duplicate delivery ran %d update-ref commands", f.executor.updateRefCount())
	}
	if calls := f.verifier.reportRequests(); len(calls) != 1 {
		t.Fatalf("duplicate delivery reported %d times; want one", len(calls))
	}

	changed := request
	changed.Candidate = "cand-2"
	if _, err := f.service.Update(context.Background(), changed); !errors.Is(err, git.ErrSourceEventConflict) {
		t.Fatalf("changed replay error = %v; want ErrSourceEventConflict", err)
	}
	changed = request
	changed.ResourceID = "res-other"
	if _, err := f.service.Update(context.Background(), changed); !errors.Is(err, git.ErrSourceEventConflict) {
		t.Fatalf("changed resource replay error = %v; want ErrSourceEventConflict", err)
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("changed replay ran %d update-ref commands", f.executor.updateRefCount())
	}
	if calls := f.verifier.verifyRequests(); len(calls) != 1 {
		t.Fatalf("changed replay verified %d times; want one", len(calls))
	}

	recovered, err := f.service.Recover(context.Background(), first.Entry.SourceEventID)
	if err != nil {
		t.Fatalf("Recover(reported): %v", err)
	}
	if recovered.Entry.State != git.JournalReported || recovered.Outcome != "" {
		t.Fatalf("recovered reported entry = %q,%q; want reported with no outcome", recovered.Entry.State, recovered.Outcome)
	}
	if calls := f.verifier.reportRequests(); len(calls) != 1 {
		t.Fatalf("settled recovery reported %d times; want one", len(calls))
	}

	if _, err := f.service.Recover(context.Background(), "git-event-missing"); !errors.Is(err, git.ErrNoJournalEntry) {
		t.Fatalf("Recover(missing) error = %v; want ErrNoJournalEntry", err)
	}
}

// TestUpdateRefusedDuplicateIsSettled pins that a permanent refusal is recorded
// once and a duplicate delivery returns the settled refusal without a second
// verification or effect.
func TestUpdateRefusedDuplicateIsSettled(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, humanScript("prn-view", "viewer"))
	request := updateRequestFor("shop/web", "refs/heads/work/x", "", "work-1", testSourceEventID)

	result, err := f.service.Update(context.Background(), request)
	if !errors.Is(err, git.ErrPolicyDenied) {
		t.Fatalf("Update error = %v; want ErrPolicyDenied", err)
	}
	if result.Entry.State != git.JournalRefused {
		t.Fatalf("returned state = %q; want refused", result.Entry.State)
	}

	duplicate, err := f.service.Update(context.Background(), request)
	if err != nil {
		t.Fatalf("duplicate refusal: %v", err)
	}
	if duplicate.Entry.State != git.JournalRefused {
		t.Fatalf("duplicate state = %q; want refused", duplicate.Entry.State)
	}
	if calls := f.verifier.verifyRequests(); len(calls) != 1 {
		t.Fatalf("duplicate refusal verified %d times; want one", len(calls))
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatalf("duplicate refusal ran %d update-ref commands", f.executor.updateRefCount())
	}
}

// TestUpdateDuplicateIntentDoesNotApplyStaleAuthority pins that a duplicate
// delivery which finds a pre-effect `intended` entry never applies the stored
// compare-and-swap. The actor is revoked, the rules now deny the same attempt
// and the duplicate carries a garbage credential, so applying the intent would
// push the ref from stale authority.
func TestUpdateDuplicateIntentDoesNotApplyStaleAuthority(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	request := f.updateRequest()
	prepared, err := f.service.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.State != git.JournalIntended {
		t.Fatalf("prepared state = %q; want intended", prepared.State)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatal("Prepare performed a ref effect")
	}

	// The crash left only the intent. The actor is now revoked (its credential
	// is unknown to verification) and the rules deny the attempt.
	f.policies.set(mustConfig(t, "repo shop/web\n    - refs/heads/main = @execution\n"))
	duplicate := request
	duplicate.ActorCredential = "revoked-garbage-credential"

	result, err := f.service.Update(context.Background(), duplicate)
	if err != nil {
		t.Fatalf("duplicate Update: %v", err)
	}
	if result.Entry.State != git.JournalRefused {
		t.Fatalf("duplicate state = %q; want refused from observation", result.Entry.State)
	}
	if result.Reported {
		t.Fatal("duplicate delivery reported a stale-authority effect")
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatalf("duplicate delivery ran %d update-ref commands; want zero", f.executor.updateRefCount())
	}
	if got := f.executor.commit("shop/web", testRef); got != testBase {
		t.Fatalf("ref = %q; want the unchanged base %q", got, testBase)
	}
	if calls := f.verifier.reportRequests(); len(calls) != 0 {
		t.Fatalf("duplicate delivery reported %d times; want zero", len(calls))
	}
	if calls := f.verifier.verifyRequests(); len(calls) != 1 {
		t.Fatalf("duplicate delivery verified %d times; want only the Prepare verification", len(calls))
	}
}

// TestUpdateDuplicateIntentReconcilesObservedCandidate pins that a duplicate
// which finds a pre-effect `intended` entry still settles an effect that
// actually happened: an observed candidate is accepted and reported without a
// second push or verification.
func TestUpdateDuplicateIntentReconcilesObservedCandidate(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	request := f.updateRequest()
	prepared, err := f.service.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.State != git.JournalIntended {
		t.Fatalf("prepared state = %q; want intended", prepared.State)
	}
	// The effect happened but the acceptance write was lost with the crash.
	f.executor.setRef("shop/web", testRef, testCandidate)

	duplicate := request
	duplicate.ActorCredential = "revoked-garbage-credential"
	result, err := f.service.Update(context.Background(), duplicate)
	if err != nil {
		t.Fatalf("duplicate Update: %v", err)
	}
	if result.Entry.State != git.JournalReported || !result.Reported {
		t.Fatalf("duplicate state = %q,%v; want reported", result.Entry.State, result.Reported)
	}
	if result.Ref != (git.RefState{Ref: testRef, Commit: testCandidate}) {
		t.Fatalf("observed ref = %+v; want the candidate", result.Ref)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatalf("duplicate delivery re-pushed: %d update-ref commands", f.executor.updateRefCount())
	}
	if calls := f.verifier.reportRequests(); len(calls) != 1 {
		t.Fatalf("duplicate delivery reports = %d; want one", len(calls))
	}
	if calls := f.verifier.verifyRequests(); len(calls) != 1 {
		t.Fatalf("duplicate delivery verified %d times; want only the Prepare verification", len(calls))
	}
}

// TestUpdateReadsCurrentPolicy pins that the next attempt uses the rules
// accepted by the admin push instead of a cached config.
func TestUpdateReadsCurrentPolicy(t *testing.T) {
	f := newFixtureWithConfig(t, "repo shop/web\n    RW refs/heads/work/ = @engineer\n")
	f.verifier.script(testCredential, humanScript("prn-eng", "engineer"))

	first := updateRequestFor("shop/web", "refs/heads/work/a", "", "a-1", "git-event-a")
	if _, err := f.service.Update(context.Background(), first); err != nil {
		t.Fatalf("first update: %v", err)
	}

	f.policies.set(mustConfig(t, "repo shop/web\n    - refs/heads/work/ = @engineer\n"))
	second := updateRequestFor("shop/web", "refs/heads/work/b", "", "b-1", "git-event-b")
	if _, err := f.service.Update(context.Background(), second); !errors.Is(err, git.ErrPolicyDenied) {
		t.Fatalf("update after rule change = %v; want ErrPolicyDenied", err)
	}
	if f.executor.commit("shop/web", "refs/heads/work/b") != "" {
		t.Fatal("denied update created a ref")
	}
}
