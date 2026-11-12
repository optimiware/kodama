//go:build integration

package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

const fixtureServiceTwoTok = "kodsvc.service-2.fixture-token"

func mustVerify(t *testing.T, server *idfix.ContractServer, actorToken string) api.VerifyResponse {
	t.Helper()
	verification, err := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	response, err := verification.Verify(context.Background(), validVerifyRequest(actorToken))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return response
}

func TestContractReportReplayAndSingleEvent(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	options := defaultContractOptions()
	options.Services = append(options.Services, idfix.ServiceAccount{
		ID: "service-2", Token: fixtureServiceTwoTok, TenantID: fixtureTenant, ProjectID: fixtureProject, Enabled: true,
	})
	options.Bindings = append(options.Bindings, idfix.Binding{
		ID: "binding-2", ServiceID: "service-2", TenantID: fixtureTenant, ProjectID: fixtureProject,
		ResourceID: fixtureResourceID, Operations: []string{string(api.ResourceOperationGitRefUpdate)},
		ReportableEvents: []string{"git.ref.updated"},
	})
	server := newContract(t, provider, options)
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	verified := mustVerify(t, server, human.Value)
	verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
	request := validReportRequest(verified.VerificationID)

	first, err := verification.Report(context.Background(), request)
	if err != nil {
		t.Fatalf("first Report: %v", err)
	}
	if first.EventID != "event-0001" || first.Replayed {
		t.Fatalf("first report = %+v; want event-0001, not replayed", first)
	}
	if server.EventCount() != 1 {
		t.Fatalf("event count = %d; want 1", server.EventCount())
	}

	retry, err := verification.Report(context.Background(), request)
	if err != nil {
		t.Fatalf("replayed Report: %v", err)
	}
	if !retry.Replayed || retry.EventID != first.EventID || retry.VerificationID != first.VerificationID {
		t.Fatalf("replay = %+v; want the original result marked replayed", retry)
	}
	if server.EventCount() != 1 {
		t.Fatalf("event count after replay = %d; want 1", server.EventCount())
	}

	changed := request
	changed.Subject = gitSubject("refs/heads/main", "1111111", "9999999")
	_, err = verification.Report(context.Background(), changed)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
	if server.EventCount() != 1 {
		t.Fatalf("event count after changed-content refusal = %d; want 1", server.EventCount())
	}

	t.Run("foreign verification cannot be reported", func(t *testing.T) {
		foreignTransport := newTransport(t, server, tokenSource(client.KindService, fixtureServiceTwoTok), 1)
		foreign, err := gitapp.NewVerificationClient(foreignTransport)
		if err != nil {
			t.Fatalf("NewVerificationClient: %v", err)
		}
		foreignVerified, err := foreign.Verify(context.Background(), validVerifyRequest(human.Value))
		if err != nil {
			t.Fatalf("foreign Verify: %v", err)
		}
		foreignReport := validReportRequest(foreignVerified.VerificationID)
		foreignReport.SourceEventID = "source-event-foreign"
		_, err = verification.Report(context.Background(), foreignReport)
		requireFaultClass(t, err, fault.Forbidden)
	})

	t.Run("subject mismatch is refused", func(t *testing.T) {
		mismatch := request
		mismatch.SourceEventID = "source-event-2"
		mismatch.Subject = gitSubject("refs/heads/main", "1111111", "8888888")
		_, err := verification.Report(context.Background(), mismatch)
		requireFaultClass(t, err, fault.Forbidden)
	})

	t.Run("undeclared report type is refused", func(t *testing.T) {
		undeclared := request
		undeclared.SourceEventID = "source-event-3"
		undeclared.EventType = "other.event"
		_, err := verification.Report(context.Background(), undeclared)
		requireFaultClass(t, err, fault.Forbidden)
	})

	t.Run("unknown verification is refused", func(t *testing.T) {
		unknown := request
		unknown.SourceEventID = "source-event-4"
		unknown.VerificationID = "verification-9999"
		_, err := verification.Report(context.Background(), unknown)
		requireFaultClass(t, err, fault.Forbidden)
	})

	t.Run("subject with an extra field is refused", func(t *testing.T) {
		extra := request
		extra.SourceEventID = "source-event-extra"
		extra.Subject = api.Subject{Fields: append(append([]api.SubjectField(nil), request.Subject.Fields...), api.SubjectField{
			Name: "extra", Kind: "string", Value: "smuggled",
		})}
		_, err := verification.Report(context.Background(), extra)
		requireFaultClass(t, err, fault.Forbidden)
	})
}

// TestContractReportRefusesGitReadVerification pins that a repository read
// verification checks access only and can never stand behind resource.report,
// even when the binding declares both git.ref.update and git.read.
func TestContractReportRefusesGitReadVerification(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	options := defaultContractOptions()
	options.Bindings[0].Operations = []string{
		string(api.ResourceOperationGitRefUpdate),
		string(api.ResourceOperationGitRead),
	}
	server := newContract(t, provider, options)
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	transport := newTransport(t, server, serviceSource(), 1)
	verification, err := gitapp.NewVerificationClient(transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	readVerified, err := verification.Verify(context.Background(), api.VerifyRequest{
		ActorCredential: human.Value,
		ResourceID:      fixtureResourceID,
		Operation:       string(api.ResourceOperationGitRead),
	})
	if err != nil {
		t.Fatalf("git.read Verify: %v", err)
	}
	if readVerified.VerificationID == "" || server.VerificationCount() != 1 {
		t.Fatalf("git.read verification = %+v; count = %d; want 1", readVerified, server.VerificationCount())
	}

	t.Run("git.read operation is invalid_input", func(t *testing.T) {
		_, err := transport.Invoke(context.Background(), api.Invocation{
			Operation: "resource.report",
			Request: api.Request{
				IdempotencyKey: "source-event-git-read",
				Input: mustJSON(t, api.ReportRequest{
					ResourceID:     fixtureResourceID,
					SourceEventID:  "source-event-git-read",
					EventType:      "git.ref.updated",
					Operation:      string(api.ResourceOperationGitRead),
					VerificationID: readVerified.VerificationID,
				}),
			},
		})
		requireFaultClass(t, err, fault.InvalidInput)
	})

	t.Run("git.ref.update citing git.read verification is forbidden", func(t *testing.T) {
		for _, subject := range []api.Subject{
			{},
			gitSubject("refs/heads/main", "1111111", "2222222"),
		} {
			sourceID := "source-event-git-read-empty"
			if len(subject.Fields) > 0 {
				sourceID = "source-event-git-read-subject"
			}
			_, err := verification.Report(context.Background(), api.ReportRequest{
				ResourceID:     fixtureResourceID,
				SourceEventID:  sourceID,
				EventType:      "git.ref.updated",
				Operation:      string(api.ResourceOperationGitRefUpdate),
				VerificationID: readVerified.VerificationID,
				Subject:        subject,
			})
			requireFaultClass(t, err, fault.Forbidden)
		}
	})

	if server.EventCount() != 0 || len(server.EventIDs()) != 0 {
		t.Fatalf("event count = %d (ids=%v); want zero events committed", server.EventCount(), server.EventIDs())
	}
}

// TestContractReportReplayKeyIsScopedToCaller pins that an idempotency key is
// scoped to the authenticated service: service-2 reusing service-1's key and
// report content must not observe service-1's committed event or replay flag,
// while the same key remains usable for service-2's own report.
func TestContractReportReplayKeyIsScopedToCaller(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	options := defaultContractOptions()
	options.Services = append(options.Services, idfix.ServiceAccount{
		ID: "service-2", Token: fixtureServiceTwoTok, TenantID: fixtureTenant, ProjectID: fixtureProject, Enabled: true,
	})
	options.Bindings = append(options.Bindings, idfix.Binding{
		ID: "binding-2", ServiceID: "service-2", TenantID: fixtureTenant, ProjectID: fixtureProject,
		ResourceID: fixtureResourceID, Operations: []string{string(api.ResourceOperationGitRefUpdate)},
		ReportableEvents: []string{"git.ref.updated"},
	})
	server := newContract(t, provider, options)
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	firstVerification := mustVerify(t, server, human.Value)
	firstClient, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
	first, err := firstClient.Report(context.Background(), validReportRequest(firstVerification.VerificationID))
	if err != nil {
		t.Fatalf("first Report: %v", err)
	}
	if first.EventID != "event-0001" || server.EventCount() != 1 {
		t.Fatalf("first report = %+v; events = %d", first, server.EventCount())
	}

	foreignClient, _ := gitapp.NewVerificationClient(newTransport(t, server, tokenSource(client.KindService, fixtureServiceTwoTok), 1))
	_, err = foreignClient.Report(context.Background(), validReportRequest(firstVerification.VerificationID))
	requireFaultClass(t, err, fault.Forbidden)
	requireNoLeakedSecrets(t, "cross-principal replay", []string{err.Error()},
		secretRule{label: "service-1 token", value: fixtureServiceTok},
		secretRule{label: "service-2 token", value: fixtureServiceTwoTok},
		secretRule{label: "human token", value: human.Value},
	)
	if server.EventCount() != 1 {
		t.Fatalf("event count after a cross-principal report = %d; want 1", server.EventCount())
	}

	// The same key is still usable by service-2 for its own verification.
	foreignVerification, err := foreignClient.Verify(context.Background(), validVerifyRequest(human.Value))
	if err != nil {
		t.Fatalf("service-2 Verify: %v", err)
	}
	foreign, err := foreignClient.Report(context.Background(), validReportRequest(foreignVerification.VerificationID))
	if err != nil {
		t.Fatalf("service-2 Report with its own key scope: %v", err)
	}
	if foreign.EventID == first.EventID || foreign.EventID != "event-0002" || server.EventCount() != 2 {
		t.Fatalf("service-2 report = %+v; events = %d; want its own event", foreign, server.EventCount())
	}
}

func TestContractLostReportResponseReplaysOnly(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	server := newContract(t, provider, defaultContractOptions())
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	verified := mustVerify(t, server, human.Value)
	server.QueueReportFailure(idfix.Failure{Mode: idfix.FailureDropAfterCommit})

	verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 2))
	response, err := verification.Report(context.Background(), validReportRequest(verified.VerificationID))
	if err != nil {
		t.Fatalf("Report after a lost response: %v", err)
	}
	if !response.Replayed {
		t.Fatalf("response = %+v; want the replayed original", response)
	}
	if response.EventID != "event-0001" || server.EventCount() != 1 {
		t.Fatalf("event id/count = %q/%d; want event-0001/1", response.EventID, server.EventCount())
	}

	var reportAttempts int
	for _, recorded := range server.Recorded() {
		if recorded.Operation == "resource.report" {
			reportAttempts++
		}
	}
	if reportAttempts != 2 {
		t.Fatalf("report attempts = %d; want 2 (lost response then replay)", reportAttempts)
	}
}

func TestContractConcurrentReportReplayCommitsOnce(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	server := newContract(t, provider, defaultContractOptions())
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	verified := mustVerify(t, server, human.Value)
	request := validReportRequest(verified.VerificationID)

	const workers = 8
	var wait sync.WaitGroup
	responses := make([]api.ReportResponse, workers)
	errorsSeen := make([]error, workers)
	for index := range workers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			verification, err := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
			if err != nil {
				errorsSeen[index] = err
				return
			}
			responses[index], errorsSeen[index] = verification.Report(context.Background(), request)
		}(index)
	}
	wait.Wait()

	eventID := ""
	for index := range workers {
		if errorsSeen[index] != nil {
			t.Fatalf("worker %d: %v", index, errorsSeen[index])
		}
		if eventID == "" {
			eventID = responses[index].EventID
		}
		if responses[index].EventID != eventID {
			t.Fatalf("worker %d event id = %q; want %q", index, responses[index].EventID, eventID)
		}
	}
	if server.EventCount() != 1 {
		t.Fatalf("event count = %d; want exactly one accepted report", server.EventCount())
	}
}

func TestContractControllableFailures(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	human, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}

	t.Run("verify 5xx", func(t *testing.T) {
		server := newContract(t, provider, defaultContractOptions())
		server.QueueVerifyFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})
		verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
		_, err := verification.Verify(context.Background(), validVerifyRequest(human.Value))
		requireFaultClass(t, err, fault.DependencyUnavailable)
		// Recovery: the one-shot failure is consumed and the same client and
		// server accept the next verification.
		if _, err := verification.Verify(context.Background(), validVerifyRequest(human.Value)); err != nil {
			t.Fatalf("recovery after verify 5xx: %v", err)
		}
	})

	t.Run("report 5xx", func(t *testing.T) {
		server := newContract(t, provider, defaultContractOptions())
		verified := mustVerify(t, server, human.Value)
		server.QueueReportFailure(idfix.Failure{Mode: idfix.FailureStatus, Code: api.CodeInternalFailure, Status: 500})
		verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
		request := validReportRequest(verified.VerificationID)
		_, err := verification.Report(context.Background(), request)
		requireFaultClass(t, err, fault.InternalFailure)
		if _, err := verification.Report(context.Background(), request); err != nil {
			t.Fatalf("recovery after report 5xx: %v", err)
		}
		if server.EventCount() != 1 {
			t.Fatalf("event count = %d; want the single recovered report", server.EventCount())
		}
	})

	t.Run("timeout", func(t *testing.T) {
		server := newContract(t, provider, defaultContractOptions())
		verified := mustVerify(t, server, human.Value)
		server.QueueReportFailure(idfix.Failure{Mode: idfix.FailureDelay, Delay: 200 * time.Millisecond})
		verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
		request := validReportRequest(verified.VerificationID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := verification.Report(ctx, request)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v; want context deadline exceeded", err)
		}
		// Recovery: the same client and server accept the report once the
		// deadline is no longer imposed.
		if _, err := verification.Report(context.Background(), request); err != nil {
			t.Fatalf("recovery after a report timeout: %v", err)
		}
	})

	t.Run("corrupt success envelope is refused", func(t *testing.T) {
		server := newContract(t, provider, defaultContractOptions())
		server.QueueVerifyFailure(idfix.Failure{Mode: idfix.FailureExtraField})
		verification, _ := gitapp.NewVerificationClient(newTransport(t, server, serviceSource(), 1))
		_, err := verification.Verify(context.Background(), validVerifyRequest(human.Value))
		requireFaultClass(t, err, fault.InternalFailure)
		if _, err := verification.Verify(context.Background(), validVerifyRequest(human.Value)); err != nil {
			t.Fatalf("recovery after a corrupt envelope: %v", err)
		}
	})
}

func TestVerifyReportCodecRoundTripStrictBothWays(t *testing.T) {
	codecs := make(map[string]api.OperationCodec)
	for _, codec := range api.OperationCodecs() {
		codecs[codec.Operation] = codec
	}
	verifyCodec, ok := codecs["actor.verify"]
	if !ok {
		t.Fatal("actor.verify codec is missing")
	}
	reportCodec, ok := codecs["resource.report"]
	if !ok {
		t.Fatal("resource.report codec is missing")
	}
	if verifyCodec.RequiresIdempotencyKey {
		t.Fatal("actor.verify must not require an idempotency key")
	}
	if !reportCodec.RequiresIdempotencyKey {
		t.Fatal("resource.report must require an idempotency key")
	}

	verifyRequest := validVerifyRequest("actor-token")
	encoded, err := verifyCodec.EncodeRequest(verifyRequest)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	decoded, err := verifyCodec.DecodeRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if decoded.(api.VerifyRequest).ActorCredential != verifyRequest.ActorCredential {
		t.Fatalf("decoded verify request = %+v", decoded)
	}
	if _, err := verifyCodec.DecodeRequest([]byte(`{"actor_credential":"token","resource_id":"repo-1","operation":"git.ref.update","subject":{"fields":[]},"unexpected":true}`)); err == nil {
		t.Fatal("the verify request codec accepted an unknown member")
	}

	verifyResponse := api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: "oidc-0123456789abcdef0123456789abcdef", Kind: "human"},
		Roles:          []string{"engineer"},
		VerificationID: "verification-0001",
	}
	encoded, err = verifyCodec.EncodeResult(verifyResponse)
	if err != nil {
		t.Fatalf("EncodeResult: %v", err)
	}
	decodedResult, err := verifyCodec.DecodeResult(encoded)
	if err != nil {
		t.Fatalf("DecodeResult: %v", err)
	}
	if decodedResult.(api.VerifyResponse).VerificationID != verifyResponse.VerificationID {
		t.Fatalf("decoded verify response = %+v", decodedResult)
	}
	if _, err := verifyCodec.DecodeResult([]byte(`{"principal":{"id":"oidc-0123456789abcdef0123456789abcdef","kind":"human"},"verification_id":"verification-0001","unexpected":true}`)); err == nil {
		t.Fatal("the verify result codec accepted an unknown member")
	}

	reportRequest := validReportRequest("verification-0001")
	encoded, err = reportCodec.EncodeRequest(reportRequest)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	decoded, err = reportCodec.DecodeRequest(encoded)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if decoded.(api.ReportRequest).SourceEventID != reportRequest.SourceEventID {
		t.Fatalf("decoded report request = %+v", decoded)
	}
	if _, err := reportCodec.DecodeRequest([]byte(`{"resource_id":"repo-1","source_event_id":"event-1","event_type":"git.ref.updated","operation":"git.ref.update","verification_id":"verification-0001","subject":{"fields":[]},"unexpected":true}`)); err == nil {
		t.Fatal("the report request codec accepted an unknown member")
	}

	reportResponse := api.ReportResponse{VerificationID: "verification-0001", EventID: "event-0001"}
	encoded, err = reportCodec.EncodeResult(reportResponse)
	if err != nil {
		t.Fatalf("EncodeResult: %v", err)
	}
	decodedResult, err = reportCodec.DecodeResult(encoded)
	if err != nil {
		t.Fatalf("DecodeResult: %v", err)
	}
	if decodedResult.(api.ReportResponse).EventID != reportResponse.EventID {
		t.Fatalf("decoded report response = %+v", decodedResult)
	}
	if _, err := reportCodec.DecodeResult([]byte(`{"verification_id":"verification-0001","event_id":"event-0001","unexpected":true}`)); err == nil {
		t.Fatal("the report result codec accepted an unknown member")
	}
}
