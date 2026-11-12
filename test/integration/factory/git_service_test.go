//go:build integration

package factorytest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestFactoryWithGitServiceProvisionsTenantAndRegistersRepositories proves the
// host-only composition: the product provisioning path created the tenant's
// admin repository with its parentless policy commit, the fixture policy is a
// served successor commit, both repositories have their own registered
// resource binding, and the real managed batch hooks are installed.
func TestFactoryWithGitServiceProvisionsTenantAndRegistersRepositories(t *testing.T) {
	fixture := startGitHarness(t)
	gitFixture := fixture.git

	history := gitFixture.History(t, factory.GitAdminRepository)
	if len(history) < 2 {
		t.Fatalf("admin history has %d commits; want the provisioned root and the served policy successor", len(history))
	}
	root := history[len(history)-1]
	if parents := gitFixture.CommitParents(t, factory.GitAdminRepository, root); len(parents) != 0 {
		t.Fatalf("provisioned admin root has parents %v; want a parentless first commit", parents)
	}
	if initial := gitFixture.PolicyAt(t, factory.GitAdminRepository, root); initial != "repo admin\n    RW = @admin\n" {
		t.Fatalf("provisioned initial policy = %q", initial)
	}
	if policy := gitFixture.AdminPolicy(t); policy != factory.GitPolicy {
		t.Fatalf("accepted admin policy differs from the served policy:\n%s", policy)
	}
	if head := gitFixture.RefCommit(t, factory.GitAdminRepository, "refs/heads/main"); head != history[0] {
		t.Fatalf("admin main = %s; want the policy successor %s", head, history[0])
	}

	if !strings.HasPrefix(gitFixture.HostURL, "https://127.0.0.1:") {
		t.Fatalf("host Git URL = %q; want the TLS loopback listener", gitFixture.HostURL)
	}
	if !strings.HasPrefix(gitFixture.WorkloadURL, "https://host.containers.internal:") {
		t.Fatalf("workload Git URL = %q; want the host workload alias through the relay", gitFixture.WorkloadURL)
	}
	if !strings.HasPrefix(gitFixture.GitVersion, "git version ") {
		t.Fatalf("recorded host Git version = %q", gitFixture.GitVersion)
	}
	t.Logf("host Git: %s", gitFixture.GitVersion)
	binaryInfo, err := os.Stat(gitFixture.BinaryPath)
	if err != nil {
		t.Fatalf("stat fixture hook binary: %v", err)
	}
	if binaryInfo.Mode().Perm()&0o111 == 0 {
		t.Fatalf("fixture hook binary is not executable")
	}
	credentialInfo, err := os.Stat(gitFixture.CredentialFile)
	if err != nil {
		t.Fatalf("stat service credential file: %v", err)
	}
	if mode := credentialInfo.Mode().Perm(); mode != 0o600 {
		t.Fatalf("service credential mode = %04o; want 0600", mode)
	}

	adminBinding := fixture.readBinding(t, gitFixture.AdminBindingID)
	projectBinding := fixture.readBinding(t, gitFixture.ProjectBindingID)
	if adminBinding.ResourceID == projectBinding.ResourceID {
		t.Fatalf("admin and project repositories share resource id %q", adminBinding.ResourceID)
	}
	if adminBinding.BindingID == projectBinding.BindingID {
		t.Fatalf("admin and project repositories share binding id %q", adminBinding.BindingID)
	}
	for _, binding := range []api.ResourceBindingReadResult{adminBinding, projectBinding} {
		if binding.EnforcingService != gitFixture.ServiceID {
			t.Fatalf("binding %s enforces service %q; want %q", binding.BindingID, binding.EnforcingService, gitFixture.ServiceID)
		}
		if !slices.Contains(binding.Operations, string(api.ResourceOperationGitRead)) ||
			!slices.Contains(binding.Operations, string(api.ResourceOperationGitRefUpdate)) {
			t.Fatalf("binding %s operations = %v", binding.BindingID, binding.Operations)
		}
		if !slices.Contains(binding.ReportableEvents, "git.ref.updated") {
			t.Fatalf("binding %s reportable events = %v", binding.BindingID, binding.ReportableEvents)
		}
		if !strings.HasPrefix(binding.TrustedOrigins[0], "https://127.0.0.1:") {
			t.Fatalf("binding %s trusted origin = %v; want the served HTTPS origin", binding.BindingID, binding.TrustedOrigins)
		}
		if len(binding.TrustedOrigins) != 2 ||
			binding.TrustedOrigins[0] != gitFixture.HostURL ||
			binding.TrustedOrigins[1] != gitFixture.WorkloadURL {
			t.Fatalf("binding %s trusted origins = %v; want the host and workload origins %q/%q",
				binding.BindingID, binding.TrustedOrigins, gitFixture.HostURL, gitFixture.WorkloadURL)
		}
	}
	if adminBinding.ResourceID != gitFixture.AdminResourceID || projectBinding.ResourceID != gitFixture.ProjectResourceID {
		t.Fatalf("binding resources = %q/%q; fixture = %q/%q",
			adminBinding.ResourceID, projectBinding.ResourceID, gitFixture.AdminResourceID, gitFixture.ProjectResourceID)
	}
	if adminBinding.Metadata["repository"] != factory.GitAdminRepository ||
		projectBinding.Metadata["repository"] != factory.GitProjectRepository {
		t.Fatalf("binding repository metadata = %#v / %#v", adminBinding.Metadata, projectBinding.Metadata)
	}

	for _, repository := range []string{factory.GitAdminRepository, factory.GitProjectRepository} {
		for _, hook := range []string{"pre-receive", "post-receive"} {
			path := filepath.Join(gitFixture.RepositoryPath(repository), "hooks", hook)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s hook of %s: %v", hook, repository, err)
			}
			if !strings.Contains(string(content), "# managed by kodama") {
				t.Fatalf("%s hook of %s is not the managed product hook", hook, repository)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s hook: %v", hook, err)
			}
			if info.Mode().Perm()&0o111 == 0 {
				t.Fatalf("%s hook of %s is not executable", hook, repository)
			}
		}
		// The single-ref update hook mutates the ref before receive-pack, so the
		// batch installer must never write it.
		if _, err := os.Stat(filepath.Join(gitFixture.RepositoryPath(repository), "hooks", "update")); !os.IsNotExist(err) {
			t.Fatalf("%s has an update hook; want only the managed pre/post-receive batch hooks", repository)
		}
	}

	// The served policy push is durable: it has one accepted journal entry, a
	// real verification row and a linked resource.verified event.
	entry := fixture.waitJournal(t, "refs/heads/main", history[0])
	if entry.Repository != factory.GitAdminRepository || entry.ResourceID != gitFixture.AdminResourceID {
		t.Fatalf("admin policy journal entry = %+v", entry)
	}
	verification := fixture.verificationByID(t, entry.VerificationID)
	if verification.Operation != "git.ref.update" || verification.Result != "approved" {
		t.Fatalf("admin policy verification = %+v", verification)
	}
	event := fixture.waitReportEvent(t, entry.VerificationID)
	if event.Type != "resource.verified" || event.ReportType != "git.ref.updated" {
		t.Fatalf("admin policy event = %+v", event)
	}
	if event.CausationID != entry.VerificationID || event.SubjectID != gitFixture.AdminResourceID {
		t.Fatalf("admin policy event linkage = %+v", event)
	}
}

// TestFactoryWithGitServiceClonePushUsesServedShellFacts proves the host path:
// an OIDC engineer clone and work-branch push move the real ref, create durable
// read/update verification rows, and produce one reported journal entry whose
// verification ID causes the resource.verified event.
func TestFactoryWithGitServiceClonePushUsesServedShellFacts(t *testing.T) {
	fixture := startGitHarness(t)
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "engineer")

	// The clone itself is a served repository read: one real git.read
	// verification with no subject and no approval.
	read := fixture.waitVerifications(t, "git.read")
	if read.ResourceID != fixture.git.ProjectResourceID || read.BindingID != fixture.git.ProjectBindingID {
		t.Fatalf("git.read verification = %+v", read)
	}
	if read.ActorID != fixture.engineerPrincipal || read.ServiceID != fixture.git.ServiceID {
		t.Fatalf("git.read actor/service = %+v", read)
	}
	if read.Result != "approved" {
		t.Fatalf("git.read result = %q", read.Result)
	}
	var readSubject api.Subject
	if err := json.Unmarshal([]byte(read.Subject), &readSubject); err != nil {
		t.Fatalf("decode git.read subject %s: %v", read.Subject, err)
	}
	if len(readSubject.Fields) != 0 {
		t.Fatalf("git.read verification carries subject %s; want no subject fields", read.Subject)
	}

	serviceToken, err := fixture.git.ServiceToken(t)
	if err != nil {
		t.Fatalf("read service credential: %v", err)
	}
	projectBinding := fixture.readBinding(t, fixture.git.ProjectBindingID)
	if !slices.Contains(projectBinding.Operations, string(api.ResourceOperationGitRefUpdate)) ||
		!slices.Contains(projectBinding.Operations, string(api.ResourceOperationGitRead)) {
		t.Fatalf("project binding operations = %v; want both git.ref.update and git.read", projectBinding.Operations)
	}
	t.Run("git.read verification cannot stand behind resource.report", func(t *testing.T) {
		verifyResult := fixture.MustREST(serviceToken, "actor.verify", api.VerifyRequest{
			ActorCredential: fixture.engineerToken,
			ResourceID:      fixture.git.ProjectResourceID,
			Operation:       string(api.ResourceOperationGitRead),
		}, nil, "")
		var readVerified api.VerifyResponse
		fixture.Decode(verifyResult, &readVerified)
		if readVerified.VerificationID == "" {
			t.Fatal("git.read actor.verify returned an empty verification_id")
		}

		eventsBeforeResult := fixture.MustREST(fixture.ProjectToken, "event.read", api.EventReadRequest{
			ProjectID: factory.ProjectID,
			Types:     []string{"resource.verified"},
			Page:      &api.Page{Limit: api.MaxPageLimit},
		}, nil, "")
		var eventsBefore api.EventReadResult
		fixture.Decode(eventsBeforeResult, &eventsBefore)

		_, invalidRefusal := fixture.REST(serviceToken, "resource.report", api.ReportRequest{
			ResourceID:     fixture.git.ProjectResourceID,
			SourceEventID:  "git-read-report-invalid",
			EventType:      "git.ref.updated",
			Operation:      string(api.ResourceOperationGitRead),
			VerificationID: readVerified.VerificationID,
		}, nil, "git-read-report-invalid")
		if invalidRefusal.Code != api.CodeInvalidInput {
			t.Fatalf("resource.report with git.read operation code = %q (%s); want %q",
				invalidRefusal.Code, invalidRefusal.Message, api.CodeInvalidInput)
		}

		for _, attempt := range []struct {
			key     string
			subject api.Subject
		}{
			{key: "git-read-report-empty-subject", subject: api.Subject{}},
			{
				key: "git-read-report-update-subject",
				subject: api.Subject{Fields: []api.SubjectField{
					{Name: "operation", Kind: "string", Value: string(api.ResourceOperationGitRefUpdate)},
					{Name: "resource", Kind: "string", Value: fixture.git.ProjectResourceID},
					{Name: "ref", Kind: "string", Value: gitWorkRef},
					{Name: "expected_base", Kind: "string", Value: ""},
					{Name: "candidate", Kind: "string", Value: "1111111111111111111111111111111111111111"},
				}},
			},
		} {
			_, forbiddenRefusal := fixture.REST(serviceToken, "resource.report", api.ReportRequest{
				ResourceID:     fixture.git.ProjectResourceID,
				SourceEventID:  attempt.key,
				EventType:      "git.ref.updated",
				Operation:      string(api.ResourceOperationGitRefUpdate),
				VerificationID: readVerified.VerificationID,
				Subject:        attempt.subject,
			}, nil, attempt.key)
			if forbiddenRefusal.Code != api.CodeForbidden {
				t.Fatalf("resource.report (%s) citing git.read verification code = %q (%s); want %q",
					attempt.key, forbiddenRefusal.Code, forbiddenRefusal.Message, api.CodeForbidden)
			}
		}

		if eventRow, reported, err := fixture.git.ReportEvent(t.Context(), readVerified.VerificationID); err != nil {
			t.Fatalf("query report event for %s: %v", readVerified.VerificationID, err)
		} else if reported {
			t.Fatalf("git.read verification %s committed report event %+v; want none", readVerified.VerificationID, eventRow)
		}
		eventsAfterResult := fixture.MustREST(fixture.ProjectToken, "event.read", api.EventReadRequest{
			ProjectID: factory.ProjectID,
			Types:     []string{"resource.verified"},
			Page:      &api.Page{Limit: api.MaxPageLimit},
		}, nil, "")
		var eventsAfter api.EventReadResult
		fixture.Decode(eventsAfterResult, &eventsAfter)
		if len(eventsAfter.Events) != len(eventsBefore.Events) {
			t.Fatalf("resource.verified events after refused git.read reports = %d; want unchanged %d",
				len(eventsAfter.Events), len(eventsBefore.Events))
		}
		for _, committed := range eventsAfter.Events {
			if committed.SubjectID == fixture.git.ProjectResourceID || committed.CausationID == readVerified.VerificationID {
				t.Fatalf("unexpected resource.verified event committed for project resource or git.read verification: %+v", committed)
			}
		}
	})

	commit := fixture.commit(t, directory, "git-service work branch")
	stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+gitWorkRef)
	if code != 0 {
		t.Fatalf("push %s: exit %d: %s", gitWorkRef, code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitWorkRef); remote != commit {
		t.Fatalf("remote %s = %q; want %q", gitWorkRef, remote, commit)
	}

	entry := fixture.waitJournal(t, gitWorkRef, commit)
	if entry.State != git.JournalReported {
		t.Fatalf("journal state = %q", entry.State)
	}
	if entry.Repository != factory.GitProjectRepository || entry.ResourceID != fixture.git.ProjectResourceID {
		t.Fatalf("journal entry = %+v", entry)
	}
	if entry.VerificationID == "" || entry.SourceEventID == "" {
		t.Fatalf("journal entry has no verification/source identity: %+v", entry)
	}

	verification := fixture.verificationByID(t, entry.VerificationID)
	if verification.Operation != "git.ref.update" || verification.Result != "approved" {
		t.Fatalf("update verification = %+v", verification)
	}
	if verification.ActorID != fixture.engineerPrincipal ||
		verification.ResourceID != fixture.git.ProjectResourceID ||
		verification.BindingID != fixture.git.ProjectBindingID {
		t.Fatalf("update verification identity = %+v", verification)
	}
	if verification.Approved {
		t.Fatalf("an unprotected work-branch update requested an approval fact")
	}
	var updateSubject api.Subject
	if err := json.Unmarshal([]byte(verification.Subject), &updateSubject); err != nil {
		t.Fatalf("decode update subject %s: %v", verification.Subject, err)
	}
	gotSubject := make(map[string]string, len(updateSubject.Fields))
	for _, field := range updateSubject.Fields {
		if field.Kind != "string" {
			t.Fatalf("verification subject field %q kind = %q; want string", field.Name, field.Kind)
		}
		gotSubject[field.Name] = field.Value
	}
	wantSubject := map[string]string{
		"operation":     "git.ref.update",
		"resource":      fixture.git.ProjectResourceID,
		"ref":           gitWorkRef,
		"expected_base": "",
		"candidate":     commit,
	}
	if len(gotSubject) != len(wantSubject) {
		t.Fatalf("verification subject %s has %d fields; want %d", verification.Subject, len(gotSubject), len(wantSubject))
	}
	for name, want := range wantSubject {
		if got, ok := gotSubject[name]; !ok || got != want {
			t.Fatalf("verification subject %s field %q = %q (present %t); want %q", verification.Subject, name, got, ok, want)
		}
	}

	event := fixture.waitReportEvent(t, entry.VerificationID)
	if event.Type != "resource.verified" || event.ReportType != "git.ref.updated" {
		t.Fatalf("report event = %+v", event)
	}
	if event.CausationID != entry.VerificationID ||
		event.SubjectID != fixture.git.ProjectResourceID ||
		event.SourceKind != "service" {
		t.Fatalf("report event linkage = %+v", event)
	}
	if event.CorrelationID != "" {
		t.Fatalf("a human push reported execution correlation %q", event.CorrelationID)
	}

	// No credential may be durable in the repositories, journal or logs. The
	// Basic encoding is scanned too: a leaked header is as bad as a raw token.
	leaks := fixture.git.ScanCredentialLeaks(t,
		serviceToken,
		fixture.engineerToken,
		"Basic "+basicValue(serviceToken),
		"Basic "+basicValue(fixture.engineerToken))
	if len(leaks) != 0 {
		t.Fatalf("credentials leaked into %v", leaks)
	}
}

// TestFactoryWithGitServiceRefusesInvalidAuthority proves the served Shell is
// the authority: an invalid actor, removed membership, a withdrawn binding
// operation and revoked service credential each refuse, and a restored control
// succeeds.
func TestFactoryWithGitServiceRefusesInvalidAuthority(t *testing.T) {
	fixture := startGitHarness(t)

	invalidDirectory := directoryFor(t, "invalid")
	text, code := fixture.git.Clone(t, "not-a-credential", factory.GitProjectRepository, invalidDirectory)
	requireRefusal(t, "invalid actor credential", text, code)

	// Control before: the engineer can clone and push.
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "engineer")
	control := fixture.commit(t, directory, "control before")
	if text, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+gitWorkRef); code != 0 {
		t.Fatalf("control push: exit %d: %s", code, text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitWorkRef); remote != control {
		t.Fatalf("control ref = %q; want %q", remote, control)
	}

	// Current human membership removal: the same live token no longer carries
	// the project role the committed policy requires. membership.revoke is a
	// versioned mutation, so it must quote the membership's current version.
	membershipVersion := fixture.membershipVersion(t, fixture.engineerPrincipal)
	fixture.MustREST(fixture.AdminToken, "membership.revoke", api.MembershipRevokeRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: fixture.engineerPrincipal,
	}, &membershipVersion, "revoke-"+gitEngineerSubject)
	removed := fixture.commit(t, directory, "removed membership")
	text, code = fixture.push(directory, fixture.engineerToken, "HEAD:refs/heads/work/removed")
	requireRefusal(t, "removed membership", text, code)
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/removed"); remote != "" {
		t.Fatalf("ref moved under removed membership: %q", remote)
	}
	requireNoAcceptedJournal(t, fixture, "refs/heads/work/removed", removed)

	// Control after: a fresh grant restores the exact authority.
	fixture.MustREST(fixture.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: fixture.engineerPrincipal,
		Roles:       []string{"engineer"},
	}, nil, "regrant-"+gitEngineerSubject)
	restored := fixture.commit(t, directory, "restored membership")
	if text, code := fixture.push(directory, fixture.engineerToken, "HEAD:refs/heads/work/restored"); code != 0 {
		t.Fatalf("restored control push: exit %d: %s", code, text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/restored"); remote != restored {
		t.Fatalf("restored control ref = %q; want %q", remote, restored)
	}

	// Withdrawn binding operation: resource.binding.update replaces the whole
	// declared value at an expected version, so it can drop git.ref.update while
	// keeping the repository read. The served verification then refuses the push
	// for the Git actor because the registered binding no longer declares the
	// operation, before any ref or journal effect. The binding route is id-only,
	// so use the project-scoped credential whose project the binding belongs to.
	//
	// Deleting a verified binding is exercised separately by
	// TestFactoryDeletesVerifiedBinding, so this scenario stays focused on the
	// in-place operation withdrawal.
	before := fixture.readBinding(t, fixture.git.ProjectBindingID)
	if !slices.Contains(before.Operations, string(api.ResourceOperationGitRefUpdate)) {
		t.Fatalf("project binding operations = %v; want git.ref.update before withdrawal", before.Operations)
	}
	withdrawn := fixture.MustREST(fixture.ProjectToken, "resource.binding.update", api.ResourceBindingUpdateRequest{
		BindingID:        fixture.git.ProjectBindingID,
		ResourceID:       fixture.git.ProjectResourceID,
		EnforcingService: fixture.git.ServiceID,
		TrustedOrigins:   []string{fixture.git.HostURL},
		Operations:       []string{string(api.ResourceOperationGitRead)},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"repository": factory.GitProjectRepository, "default_branch": "main"},
	}, &before.Version, "withdraw-"+fixture.git.ProjectBindingID)
	var withdrawal api.ResourceBindingUpdateResult
	fixture.Decode(withdrawn, &withdrawal)
	if withdrawal.Version != before.Version+1 {
		t.Fatalf("withdrawal version = %d; want %d", withdrawal.Version, before.Version+1)
	}
	after := fixture.readBinding(t, fixture.git.ProjectBindingID)
	if slices.Contains(after.Operations, string(api.ResourceOperationGitRefUpdate)) {
		t.Fatalf("withdrawn binding still declares git.ref.update: %v", after.Operations)
	}
	if !slices.Contains(after.Operations, string(api.ResourceOperationGitRead)) {
		t.Fatalf("withdrawn binding dropped the repository read too: %v", after.Operations)
	}

	// The hook attempts a git.ref.update verification, but the current binding
	// no longer declares it. That refusal precedes verification identity
	// allocation, so the Shell commits no git.ref.update row for this attempt.
	updateRowsBefore := fixture.countVerificationRows(t, "git.ref.update")
	undeclared := fixture.commit(t, directory, "undeclared operation")
	text, code = fixture.push(directory, fixture.engineerToken, "HEAD:refs/heads/work/undeclared")
	text = requireRefusal(t, "withdrawn git.ref.update", text, code)
	if !strings.Contains(text, "update rejected") {
		t.Fatalf("withdrawn operation did not reach the managed hook: %s", text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/undeclared"); remote != "" {
		t.Fatalf("ref moved without a declared git.ref.update: %q", remote)
	}
	requireNoAcceptedJournal(t, fixture, "refs/heads/work/undeclared", undeclared)
	if updateRowsAfter := fixture.countVerificationRows(t, "git.ref.update"); updateRowsAfter != updateRowsBefore {
		t.Fatalf("withdrawn operation committed a git.ref.update verification row: %d -> %d", updateRowsBefore, updateRowsAfter)
	}

	// Control after: a second binding update restores the operation, and the
	// exact same refused push now succeeds with the same candidate.
	fixture.MustREST(fixture.ProjectToken, "resource.binding.update", api.ResourceBindingUpdateRequest{
		BindingID:        fixture.git.ProjectBindingID,
		ResourceID:       fixture.git.ProjectResourceID,
		EnforcingService: fixture.git.ServiceID,
		TrustedOrigins:   []string{fixture.git.HostURL},
		Operations:       []string{string(api.ResourceOperationGitRead), string(api.ResourceOperationGitRefUpdate)},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"repository": factory.GitProjectRepository, "default_branch": "main"},
	}, &withdrawal.Version, "restore-"+fixture.git.ProjectBindingID)
	if text, code := fixture.push(directory, fixture.engineerToken, "HEAD:refs/heads/work/undeclared"); code != 0 {
		t.Fatalf("restored operation control push: exit %d: %s", code, text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/undeclared"); remote != undeclared {
		t.Fatalf("restored operation ref = %q; want %q", remote, undeclared)
	}

	// Service credential revocation: the Git service cannot verify any actor
	// until the mounted credential file is replaced with a fresh issue. The
	// service credential routes are id-only, so they are called with the
	// project-scoped credential; a tenant-wide one is refused by the scope rule.
	revoked := fixture.MustREST(fixture.ProjectToken, "service.credential.revoke", api.ServiceCredentialRevokeRequest{
		ServiceID: fixture.git.ServiceID,
	}, &fixture.git.ServiceVersion, "revoke-"+fixture.git.ServiceID)
	var revokeResult api.ServiceCredentialRevokeResult
	fixture.Decode(revoked, &revokeResult)
	revokedCandidate := fixture.commit(t, directory, "revoked service credential")
	text, code = fixture.push(directory, fixture.engineerToken, "HEAD:refs/heads/work/revoked")
	requireRefusal(t, "revoked service credential", text, code)
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/revoked"); remote != "" {
		t.Fatalf("ref moved with a revoked service credential: %q", remote)
	}
	requireNoAcceptedJournal(t, fixture, "refs/heads/work/revoked", revokedCandidate)

	issued := fixture.MustREST(fixture.ProjectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  fixture.git.ServiceID,
		TTLSeconds: int((time.Hour) / time.Second),
	}, &revokeResult.Version, "reissue-"+fixture.git.ServiceID)
	var issueResult api.ServiceCredentialIssueResult
	fixture.Decode(issued, &issueResult)
	if issueResult.Token == nil || *issueResult.Token == "" {
		t.Fatalf("service.credential.issue returned no fresh token")
	}
	fixture.git.WriteServiceCredential(t, *issueResult.Token)
	reissued := fixture.commit(t, directory, "reissued service credential")
	if text, code := fixture.push(directory, fixture.engineerToken, "HEAD:refs/heads/work/reissued"); code != 0 {
		t.Fatalf("reissued credential control push: exit %d: %s", code, text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/reissued"); remote != reissued {
		t.Fatalf("reissued credential ref = %q; want %q", remote, reissued)
	}
}

// TestFactoryWithGitServiceHooksRefuseUnauthorizedRef proves the managed batch
// hooks are the enforcement point: a writable actor reaching receive-pack still
// cannot move an unauthorized exact ref, and a protected ref refuses an update
// without its approval, with the ref unchanged and no accepted report.
func TestFactoryWithGitServiceHooksRefuseUnauthorizedRef(t *testing.T) {
	fixture := startGitHarness(t)
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "engineer")

	control := fixture.commit(t, directory, "hook control")
	if text, code := fixture.push(directory, fixture.engineerToken, "HEAD:refs/heads/work/hook-control"); code != 0 {
		t.Fatalf("control push: exit %d: %s", code, text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/hook-control"); remote != control {
		t.Fatalf("control ref = %q; want %q", remote, control)
	}

	mainBefore := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if mainBefore == "" {
		t.Fatalf("fixture project repository has no seeded main ref")
	}

	// The engineer is a writable actor in this repository, but the exact main
	// ref grants only @execution.
	unauthorized := fixture.commit(t, directory, "unauthorized main")
	text, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+gitMainRef)
	text = requireRefusal(t, "unauthorized exact ref", text, code)
	if !strings.Contains(text, "update rejected") {
		t.Fatalf("unauthorized exact ref did not reach the managed hook: %s", text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); remote != mainBefore {
		t.Fatalf("main moved after an unauthorized update: %q; want %q", remote, mainBefore)
	}
	requireNoAcceptedJournal(t, fixture, gitMainRef, unauthorized)

	// A live execution may update main under the rules, but the committed
	// approval requirement is not satisfied, so pre-receive refuses before the
	// receive transaction moves anything.
	executionID, executionToken := fixture.startExecution(t, "protected")

	// Control before: the same live execution may push the granted work ref, so
	// the main refusal below is about the exact ref and its approval, not about
	// the execution credential itself.
	if text, code := fixture.push(directory, executionToken, "HEAD:refs/heads/work/execution-control"); code != 0 {
		t.Fatalf("execution work control push: exit %d: %s", code, text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/execution-control"); remote != unauthorized {
		t.Fatalf("execution work control ref = %q; want %q", remote, unauthorized)
	}

	text, code = fixture.push(directory, executionToken, "HEAD:"+gitMainRef)
	text = requireRefusal(t, "unapproved protected update", text, code)
	if !strings.Contains(text, "update rejected") {
		t.Fatalf("unapproved protected update did not reach the managed hook: %s", text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); remote != mainBefore {
		t.Fatalf("main moved after an unapproved protected update: %q; want %q", remote, mainBefore)
	}
	requireNoAcceptedJournal(t, fixture, gitMainRef, unauthorized)

	// The missing approval is a durable refused git.ref.update fact for this
	// execution, not a silent non-event.
	refused := fixture.refusedVerification(t, executionID, "git.ref.update")
	if refused.Result != "refused" || refused.Approved {
		t.Fatalf("execution main refusal verification = %+v", refused)
	}
	var refusedSubject api.Subject
	if err := json.Unmarshal([]byte(refused.Subject), &refusedSubject); err != nil {
		t.Fatalf("decode refused subject %s: %v", refused.Subject, err)
	}
	refusedFields := make(map[string]string, len(refusedSubject.Fields))
	for _, field := range refusedSubject.Fields {
		refusedFields[field.Name] = field.Value
	}
	if refusedFields["operation"] != "git.ref.update" || refusedFields["ref"] != gitMainRef || refusedFields["candidate"] != unauthorized {
		t.Fatalf("execution main refusal subject = %v", refusedFields)
	}

	// Control after: the same repository still accepts the granted work ref.
	after := fixture.commit(t, directory, "hook control after")
	if text, code := fixture.push(directory, fixture.engineerToken, "HEAD:refs/heads/work/hook-control-after"); code != 0 {
		t.Fatalf("control-after push: exit %d: %s", code, text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/hook-control-after"); remote != after {
		t.Fatalf("control-after ref = %q; want %q", remote, after)
	}
}

// TestFactoryWithGitServiceClosePreservesSibling proves each fixture owns its
// own Git lifetime: closing one joins its Git service and image resources while
// a sibling composition keeps serving and accepting pushes.
func TestFactoryWithGitServiceClosePreservesSibling(t *testing.T) {
	first := startGitHarness(t)
	second := startGitHarness(t)

	first.Close()
	select {
	case <-first.git.ServingDone():
	default:
		t.Fatalf("Git service was not joined by Close")
	}
	probeDirectory := directoryFor(t, "closed-probe")
	if _, code := first.git.Clone(t, first.engineerToken, factory.GitProjectRepository, probeDirectory); code == 0 {
		t.Fatalf("a closed fixture still serves the Git edge")
	}

	directory := second.clone(t, second.engineerToken, factory.GitProjectRepository, "sibling")
	commit := second.commit(t, directory, "sibling push")
	if text, code := second.push(directory, second.engineerToken, "HEAD:refs/heads/work/sibling"); code != 0 {
		t.Fatalf("sibling push after close: exit %d: %s", code, text)
	}
	if remote := second.git.RemoteRef(t, factory.GitProjectRepository, "refs/heads/work/sibling"); remote != commit {
		t.Fatalf("sibling ref = %q; want %q", remote, commit)
	}
}

// requireNoAcceptedJournal asserts no accepted or reported journal entry exists
// for one ref and candidate.
func requireNoAcceptedJournal(t *testing.T, fixture *gitHarness, ref, candidate string) {
	t.Helper()
	for _, entry := range fixture.git.JournalEntries(t) {
		if entry.Ref != ref || entry.Candidate != candidate {
			continue
		}
		if entry.State == git.JournalAccepted || entry.State == git.JournalReported {
			t.Fatalf("ref %s candidate %s was accepted despite the refusal: %+v", ref, candidate, entry)
		}
	}
}
