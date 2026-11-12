//go:build integration

package composition

import (
	"testing"

	"github.com/optimiware/kodama/api"
)

// TestBindingAndVerificationHappyPath proves the resource ownership path
// through the composed server: a service account registers a new
// resource, the composed verifier and reporter accept the service's forwarded
// actor credential, and the committed resource.verified event is readable.
func TestBindingAndVerificationHappyPath(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)
	environment.mustInvoke(adminToken, "project.create", api.ProjectCreateRequest{
		TenantID:  tenantID,
		ProjectID: otherID,
		Name:      "Project Two",
	}, nil, "verification-project-two")
	environment.grantEngineer("admin-2", otherID)

	// A service account and its credential lifecycle.
	serviceCreated := environment.mustInvoke(projectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: projectID,
		ServiceID: "service-1",
		Roles:     []string{"operator"},
	}, nil, "verification-service-create")
	var service api.ServiceCreateResult
	environment.decode(serviceCreated, &service)
	serviceVersion := service.Version
	issuedResult := environment.mustInvoke(projectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  "service-1",
		TTLSeconds: 3600,
	}, &serviceVersion, "verification-credential-issue")
	var issued api.ServiceCredentialIssueResult
	environment.decode(issuedResult, &issued)
	if issued.Token == nil {
		t.Fatal("service.credential.issue returned no one-time token")
	}
	// The development authenticator maps the static serviceToken to the created
	// service-1 principal; the issued one-time token proves the credential
	// lifecycle but is not resolved by the static dev authenticator. The acting
	// service credential for verify/report is therefore serviceToken.

	const (
		resourceID = "repo-verify-1"
		bindingID  = "binding-verify-1"
		reportType = "git.ref.updated"
		operation  = "git.ref.update"
	)

	// Registering a new resource id creates the resources row in the same atomic
	// unit as the binding.
	environment.mustInvoke(projectToken, "resource.binding.register", api.ResourceBindingRegisterRequest{
		ProjectID:        projectID,
		BindingID:        bindingID,
		ResourceID:       resourceID,
		EnforcingService: "service-1",
		TrustedOrigins:   []string{"https://git.example.com"},
		Operations:       []string{operation},
		ReportableEvents: []string{reportType},
	}, nil, "verification-binding-register")

	read := environment.mustInvoke(projectToken, "resource.binding.read", api.ResourceBindingReadRequest{
		BindingID: bindingID,
	}, nil, "")
	var binding api.ResourceBindingReadResult
	environment.decode(read, &binding)
	if binding.ResourceID != resourceID || binding.EnforcingService != "service-1" || binding.Version != 1 {
		t.Fatalf("registered binding = %+v", binding)
	}

	// The service's actor.verify succeeds through the public client and commits
	// an approved verification fact for the forwarded human actor.
	verifyResult, err := clientInvoke(t, environment, serviceToken, "actor.verify", api.VerifyRequest{
		ActorCredential: projectToken,
		ResourceID:      resourceID,
		Operation:       operation,
	}, nil, "")
	if err != nil {
		t.Fatalf("actor.verify through the public client: %v", err)
	}
	var verified api.VerifyResponse
	environment.decode(verifyResult, &verified)
	if verified.Principal.ID != "admin-1" || verified.Principal.Kind != "human" || verified.VerificationID == "" {
		t.Fatalf("actor.verify result = %+v", verified)
	}

	// A committed source event supplies the report deduplication identity.
	source := environment.mustInvoke(projectToken, "event.publish", api.EventPublishRequest{
		ProjectID: projectID,
		Type:      "observation.custom",
		SubjectID: resourceID,
		Data:      []byte(`{"ref":"refs/heads/main"}`),
	}, nil, "verification-source-event")
	var published api.EventPublishResult
	environment.decode(source, &published)

	reportResult, err := clientInvoke(t, environment, serviceToken, "resource.report", api.ReportRequest{
		ResourceID:     resourceID,
		SourceEventID:  published.EventID,
		EventType:      reportType,
		Operation:      operation,
		VerificationID: verified.VerificationID,
	}, nil, "verification-report")
	if err != nil {
		t.Fatalf("resource.report through the public client: %v", err)
	}
	var report api.ReportResponse
	environment.decode(reportResult, &report)
	if report.EventID == "" || report.VerificationID != verified.VerificationID {
		t.Fatalf("resource.report result = %+v", report)
	}

	// The trusted resource.verified event is readable with its declared report
	// type and the resource subject.
	page := readAllEventPage(t, environment)
	verifiedEvent := false
	for _, envelope := range page.Events {
		if envelope.Type != "resource.verified" {
			continue
		}
		if envelope.ReportType == reportType && envelope.SubjectID == resourceID {
			verifiedEvent = true
		}
	}
	if !verifiedEvent {
		t.Fatalf("event.read has no %s resource.verified event", reportType)
	}

	// A same-resource registration from another project and from another tenant
	// is the uniform not_found, and the winner stays intact.
	for _, testCase := range []struct {
		name    string
		token   string
		project string
		binding string
		key     string
	}{
		// The other project names its own project, so the handler runs and the
		// resources-row conflict resolves to the uniform foreign-scope not_found.
		{"other project", otherToken, otherID, "binding-loser-project", "verification-loser-project"},
		// The foreign tenant names the winner's project, so the scope resolver
		// refuses before any store read.
		{"foreign tenant", foreignToken, projectID, "binding-loser-tenant", "verification-loser-tenant"},
	} {
		t.Run("same resource "+testCase.name, func(t *testing.T) {
			_, publicError := environment.call(testCase.token, "resource.binding.register", api.ResourceBindingRegisterRequest{
				ProjectID:        testCase.project,
				BindingID:        testCase.binding,
				ResourceID:       resourceID,
				EnforcingService: "service-1",
				TrustedOrigins:   []string{"https://git.example.com"},
				Operations:       []string{operation},
				ReportableEvents: []string{reportType},
			}, nil, testCase.key)
			if publicError.Code != api.CodeNotFound {
				t.Fatalf("cross-scope register code = %q; want %q", publicError.Code, api.CodeNotFound)
			}
		})
	}

	read = environment.mustInvoke(projectToken, "resource.binding.read", api.ResourceBindingReadRequest{
		BindingID: bindingID,
	}, nil, "")
	environment.decode(read, &binding)
	if binding.ResourceID != resourceID || binding.Version != 1 {
		t.Fatalf("winner binding changed after refused registrations: %+v", binding)
	}
	if _, err := clientInvoke(t, environment, serviceToken, "actor.verify", api.VerifyRequest{
		ActorCredential: projectToken,
		ResourceID:      resourceID,
		Operation:       operation,
	}, nil, ""); err != nil {
		t.Fatalf("verification after refused registrations: %v", err)
	}
}
