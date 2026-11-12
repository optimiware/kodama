//go:build integration

package factorytest

// Factory S11 row AC19: local and remote authority agree on a fresh membership
// revocation.
//
// One OIDC engineer holds the project engineer role. Before revocation the same
// principal can read and write through the Shell API (local) and through the
// served Git smart-HTTP edge (remote). The membership is then revoked at its
// observed version and both surfaces are exercised immediately with the same
// still-valid OIDC token: the local operations must refuse with the authority
// class the token now lacks, and the remote smart-HTTP status must be the exact
// status that class maps to (unauthenticated -> 401, forbidden -> 403). A real
// clone and a real work-branch push are both refused, no ref moves, and no
// journal entry or verification row is created. Regranting the engineer role
// restores both surfaces with the same token, proving the decision is current
// committed authority rather than the token.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	authorityParityPipeline = "authority-parity"
	authorityParityWorkRef  = "refs/heads/work/authority-parity"
)

// TestFactoryAuthorityParityAfterRevocation proves the Shell edge and the
// served Git edge derive the same current authority from a fresh revocation.
func TestFactoryAuthorityParityAfterRevocation(t *testing.T) {
	fixture := startGitHarness(t)
	document := marshalPipeline(t, pipelineDoc{Actions: []pipelineActionDoc{{
		Name:     "control",
		Image:    fixture.Image.Reference,
		Commands: []string{"true"},
	}}})
	applied := fixture.applyPipeline(authorityParityPipeline, document)
	if applied.PipelineID == "" {
		t.Fatalf("control pipeline.apply returned %+v", applied)
	}

	// Local control before revocation: the engineer reads the applied pipeline
	// and applies another one through the served API.
	if stored := readAuthorityParityPipeline(t, fixture, fixture.engineerToken, applied.PipelineID); stored.Version != applied.Version {
		t.Fatalf("local control read version = %d; want %d", stored.Version, applied.Version)
	}
	applyAuthorityParityPipeline(t, fixture, fixture.engineerToken, authorityParityPipeline+"-before", document, "apply-parity-before")

	// Remote control before revocation: the same principal clones and pushes a
	// granted work ref through the served Git edge.
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "authority-parity")
	first := fixture.commit(t, directory, "parity before revocation")
	if stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+authorityParityWorkRef); code != 0 {
		t.Fatalf("remote control push before revocation: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, authorityParityWorkRef); remote != first {
		t.Fatalf("remote control ref = %q; want %q", remote, first)
	}
	firstJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, authorityParityWorkRef, first)
	if firstJournal.VerificationID == "" {
		t.Fatalf("remote control push has no verification identity")
	}

	// Revoke the exact observed membership version. Nothing below waits: the
	// next attempt must observe the committed revocation.
	version := fixture.membershipVersion(t, fixture.engineerPrincipal)
	revoked := fixture.MustREST(fixture.AdminToken, "membership.revoke", api.MembershipRevokeRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: fixture.engineerPrincipal,
	}, &version, "revoke-"+gitEngineerSubject)
	var revokedResult api.MembershipRevokeResult
	fixture.Decode(revoked, &revokedResult)
	if revokedResult.Version != version+1 {
		t.Fatalf("membership.revoke version = %d; want %d", revokedResult.Version, version+1)
	}
	requireProtectedEdgeMembershipAbsent(t, fixture, fixture.engineerPrincipal)

	// Local: both a read and a write operation refuse with one authority class.
	_, readError := fixture.REST(fixture.engineerToken, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: applied.PipelineID,
	}, nil, "")
	_, writeError := fixture.REST(fixture.engineerToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      authorityParityPipeline + "-revoked",
		YAML:      document,
	}, nil, "apply-parity-revoked")
	if readError.Code != writeError.Code {
		t.Fatalf("local read/write refusal classes differ: read=%q write=%q",
			readError.Code, writeError.Code)
	}
	localCode := readError.Code
	if localCode != api.CodeUnauthenticated && localCode != api.CodeForbidden {
		t.Fatalf("local refusal code = %q (%s); want an authority refusal class",
			localCode, readError.Message)
	}

	// Remote: the smart-HTTP status is exactly the class's status, and both a
	// real clone and a real push are refused without durable Git facts.
	requireGitAuthorityParity(t, fixture, fixture.engineerToken, localCode)
	revokedCommit := fixture.commit(t, directory, "parity after revocation")
	stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+authorityParityWorkRef)
	requireRefusal(t, "remote push after revocation", stderr, code)
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, authorityParityWorkRef); remote != first {
		t.Fatalf("ref moved after revocation: %q; want the unchanged %q", remote, first)
	}
	requireProtectedRefNoJournal(t, fixture, authorityParityWorkRef, revokedCommit)

	// Regrant restores both surfaces with the same still-valid token.
	restored := fixture.MustREST(fixture.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: fixture.engineerPrincipal,
		Roles:       []string{"engineer"},
	}, nil, "restore-"+gitEngineerSubject)
	var restoredResult api.MembershipGrantResult
	fixture.Decode(restored, &restoredResult)
	if restoredResult.Version == 0 {
		t.Fatalf("membership.grant returned no version after revocation")
	}
	if stored := readAuthorityParityPipeline(t, fixture, fixture.engineerToken, applied.PipelineID); stored.PipelineID != applied.PipelineID {
		t.Fatalf("local read after regrant = %+v; want the applied pipeline", stored)
	}
	applyAuthorityParityPipeline(t, fixture, fixture.engineerToken, authorityParityPipeline+"-after", document, "apply-parity-after")

	second := fixture.commit(t, directory, "parity after regrant")
	if stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+authorityParityWorkRef); code != 0 {
		t.Fatalf("remote control push after regrant: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, authorityParityWorkRef); remote != second {
		t.Fatalf("remote ref after regrant = %q; want %q", remote, second)
	}
	secondJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, authorityParityWorkRef, second)
	if secondJournal.VerificationID == firstJournal.VerificationID {
		t.Fatalf("the regranted push reused the pre-revocation verification %q", firstJournal.VerificationID)
	}
}

// readAuthorityParityPipeline reads one stored pipeline as the given actor.
func readAuthorityParityPipeline(t *testing.T, fixture *gitHarness, token, pipelineID string) api.PipelineReadResult {
	t.Helper()
	var read api.PipelineReadResult
	fixture.Decode(fixture.MustREST(token, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, nil, ""), &read)
	return read
}

// applyAuthorityParityPipeline applies one pipeline as the given actor.
func applyAuthorityParityPipeline(t *testing.T, fixture *gitHarness, token, name string, document []byte, key string) api.PipelineApplyResult {
	t.Helper()
	var applied api.PipelineApplyResult
	fixture.Decode(fixture.MustREST(token, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, key), &applied)
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply %q returned %+v", name, applied)
	}
	return applied
}

// requireGitAuthorityParity probes the served smart-HTTP read endpoint with one
// credential and requires the status the local authority class maps to, a real
// clone refusal for the same reason, and no new verification row. A transport or
// TLS failure fails the test instead of passing as authorization evidence.
func requireGitAuthorityParity(t *testing.T, fixture *gitHarness, credential, localCode string) {
	t.Helper()
	if fixture.TLS == nil {
		t.Fatalf("the Git fixture has no TLS client")
	}
	wantStatus := 0
	switch localCode {
	case api.CodeUnauthenticated:
		wantStatus = http.StatusUnauthorized
	case api.CodeForbidden:
		wantStatus = http.StatusForbidden
	default:
		t.Fatalf("local authority class %q has no remote status mapping", localCode)
	}

	endpoint := fixture.git.HostURL + "/" + factory.GitProjectRepository + ".git/info/refs?service=git-upload-pack"
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("build the smart-HTTP parity probe: %v", err)
	}
	request.Header.Set("Authorization", "Basic "+basicValue(credential))
	response, err := fixture.TLS.Client().Do(request)
	if err != nil {
		t.Fatalf("the smart-HTTP parity probe did not reach the served edge: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != wantStatus {
		t.Fatalf("remote smart-HTTP status = %d; local class %q maps to %d",
			response.StatusCode, localCode, wantStatus)
	}

	before := protectedEdgeVerificationCount(t, fixture)
	stderr, code := fixture.git.Clone(t, credential, factory.GitProjectRepository,
		directoryFor(t, "parity-revoked"))
	if code == 0 {
		t.Fatalf("Git accepted the revoked engineer credential")
	}
	lower := strings.ToLower(stderr)
	for _, transportFailure := range []string{"certificate", "ssl", "could not resolve", "connection refused", "timed out"} {
		if strings.Contains(lower, transportFailure) {
			t.Fatalf("the Git refusal is a transport failure, not an authority decision: %s", stderr)
		}
	}
	if after := protectedEdgeVerificationCount(t, fixture); after != before {
		t.Fatalf("a revoked credential produced %d new verification rows; want none", after-before)
	}
}
