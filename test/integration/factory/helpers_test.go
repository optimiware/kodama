//go:build integration

package factorytest

// Shared fixtures for the served-approval verification tests.
//
// The tests are deliberately narrow: they prove the Shell verification
// operation against a real factory composition and real request decisions. They
// do not claim protected Git integration or reviewer-only discovery.
//
// The reader/reviewer principal is explicitly dual-role. Reviewers cannot read
// execution versions through execution.inspect (the operation registry grants
// that read to engineer/operator/viewer only), while execution.request.resolve
// requires the assigned human role/reviewer. The suite therefore mints one OIDC
// human and grants exactly engineer+reviewer through the public membership
// operation, then uses that single principal for inspection and resolution. No
// broader role is borrowed from the shared project token.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	identityfixture "github.com/optimiware/kodama/test/support/boundaries/identity"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// approvalOperation is the only declared resource operation and the
	// canonical Git ref-update effect.
	approvalOperation = "git.ref.update"

	// approvalRequestTimeout keeps the request far beyond the real PostgreSQL
	// statement clock used by the deadline guard for the duration of one test.
	approvalRequestTimeout = "1h"
	// approvalContainerTimeout keeps the container action deadline well clear of
	// the same clock while the workload is held open.
	approvalContainerTimeout = "1h"

	// approvalRef is the canonical Git approval subject ref.
	approvalRef = "refs/heads/main"
	// approvalBase and approvalCandidate are distinct immutable 40-hex object
	// identities. A changed candidate/base/ref must never match recorded
	// evidence.
	approvalBase      = "1111111111111111111111111111111111111111"
	approvalCandidate = "2222222222222222222222222222222222222222"
	// approvalOtherCandidate is a different execution's effect identity.
	approvalOtherCandidate = "3333333333333333333333333333333333333333"
	// approvalOutputValue is the resolved request answer. Copying this string
	// into a subject or using it as a claim never confers approval.
	approvalOutputValue = "approved-by-reviewer"

	// approvalTokenPath is the workload-side evidence copy of the live execution
	// credential. It lives only in the retained workspace, is never printed, and
	// is removed with the execution.
	approvalTokenPath = "/workspace/execution-token"
	// approvalReleasePath holds a blocked workload open until the test releases
	// it. The test usually cancels the execution instead.
	approvalReleasePath = "/workspace/release"
)

// approvalFixture is one real factory composition plus the served configuration
// the verification tests need: one enforcing service with a current credential,
// one registered resource binding, and one explicit dual-role reader/reviewer.
type approvalFixture struct {
	t *testing.T

	harness *factory.Harness
	// dualToken authenticates one human holding exactly engineer+reviewer.
	dualToken string
	// serviceToken authenticates the enforcing service.
	serviceToken string
	serviceID    string
	resourceID   string
	bindingID    string
}

// newApprovalFixture starts one TLS+OIDC factory and registers the verification
// configuration through public operations only.
func newApprovalFixture(t *testing.T) *approvalFixture {
	t.Helper()
	harness := factory.Start(t, factory.WithTLS(), factory.WithOIDC())

	// One OIDC human becomes the explicit dual-role reader/reviewer. The minted
	// token is only an identity hint: the OIDC resolver re-reads the committed
	// project membership on every request, so exactly engineer+reviewer is the
	// authority. This is a single principal, not a reviewer borrowing an
	// engineer token.
	const dualSubject = "approval-dual-reader-reviewer"
	dualToken := harness.OIDC.Mint(t, identityfixture.TokenSpec{Subject: dualSubject})
	dualPrincipal, err := shelloidc.PrincipalIDFor(harness.OIDC.Issuer, dualSubject)
	if err != nil {
		t.Fatalf("derive dual-role principal: %v", err)
	}
	harness.MustREST(harness.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(dualPrincipal),
		Roles:       []string{"engineer", "reviewer"},
	}, nil, "grant-dual-reader-reviewer")

	serviceID := "approval-enforcing-service"
	created := harness.MustREST(harness.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: serviceID,
		// The service genuinely holds the reviewer role. That role never
		// substitutes for a human approval and never makes a service actor an
		// execution.
		Roles: []string{"reviewer"},
	}, nil, "create-"+serviceID)
	var service api.ServiceCreateResult
	harness.Decode(created, &service)
	if service.Version == 0 {
		t.Fatalf("service.create returned no committed version")
	}
	issued := harness.MustREST(harness.ProjectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  serviceID,
		TTLSeconds: int((time.Hour) / time.Second),
	}, &service.Version, "issue-"+serviceID)
	var credential api.ServiceCredentialIssueResult
	harness.Decode(issued, &credential)
	if credential.Token == nil || *credential.Token == "" {
		t.Fatalf("service.credential.issue returned no one-time token")
	}

	resourceID := "repository-approval"
	bindingID := "binding-approval"
	harness.MustREST(harness.ProjectToken, "resource.binding.register", api.ResourceBindingRegisterRequest{
		ProjectID:        factory.ProjectID,
		BindingID:        bindingID,
		ResourceID:       resourceID,
		EnforcingService: serviceID,
		TrustedOrigins:   []string{"https://git.approval.invalid"},
		Operations:       []string{approvalOperation},
		ReportableEvents: []string{"git.ref.updated"},
	}, nil, "register-"+bindingID)

	return &approvalFixture{
		t:            t,
		harness:      harness,
		dualToken:    dualToken,
		serviceToken: *credential.Token,
		serviceID:    serviceID,
		resourceID:   resourceID,
		bindingID:    bindingID,
	}
}

// pipelineDoc is the API-posted pipeline document this suite authors.
type pipelineDoc struct {
	Actions []pipelineActionDoc `yaml:"actions"`
}

// pipelineActionDoc is one action. Exactly one of Image (container) or Request
// (request action) is rendered.
type pipelineActionDoc struct {
	Name     string         `yaml:"name"`
	Image    string         `yaml:"image,omitempty"`
	Commands []string       `yaml:"commands,omitempty"`
	Request  string         `yaml:"request,omitempty"`
	Subject  map[string]any `yaml:"subject,omitempty"`
	Outputs  []string       `yaml:"outputs,omitempty"`
	Timeout  string         `yaml:"timeout,omitempty"`
}

// marshalPipeline renders one exact pipeline YAML byte sequence.
func marshalPipeline(t *testing.T, document pipelineDoc) []byte {
	t.Helper()
	encoded, err := yaml.Marshal(document)
	if err != nil {
		t.Fatalf("marshal pipeline yaml: %v", err)
	}
	return encoded
}

// applyPipeline posts one pipeline through the public API.
func (fixture *approvalFixture) applyPipeline(name string, document []byte) api.PipelineApplyResult {
	fixture.t.Helper()
	result := fixture.harness.MustREST(fixture.harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	fixture.harness.Decode(result, &applied)
	return applied
}

// startPipeline starts one stored pipeline at its currently observed version.
func (fixture *approvalFixture) startPipeline(pipelineID, key string) api.PipelineStartResult {
	fixture.t.Helper()
	read := fixture.harness.MustREST(fixture.harness.ProjectToken, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, nil, "")
	var stored api.PipelineReadResult
	fixture.harness.Decode(read, &stored)
	if stored.Version == 0 {
		fixture.t.Fatalf("pipeline.read %s version = 0", pipelineID)
	}
	result := fixture.harness.MustREST(fixture.harness.ProjectToken, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, &stored.Version, key)
	var started api.PipelineStartResult
	fixture.harness.Decode(result, &started)
	return started
}

// inspect reads one execution through the dual-role human credential.
func (fixture *approvalFixture) inspect(t *testing.T, executionID string) api.ExecutionInspectResult {
	t.Helper()
	result := fixture.harness.MustREST(fixture.dualToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, nil, "")
	var inspected api.ExecutionInspectResult
	fixture.harness.Decode(result, &inspected)
	return inspected
}

// waitRequestRunning waits for one named request action to be the running action
// of a running execution.
func (fixture *approvalFixture) waitRequestRunning(t *testing.T, executionID, actionName string) api.ExecutionInspectResult {
	t.Helper()
	var last api.ExecutionInspectResult
	var lastError api.Error
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		result, publicError := fixture.harness.REST(fixture.dualToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			lastError = publicError
			time.Sleep(10 * time.Millisecond)
			continue
		}
		lastError = api.Error{}
		fixture.harness.Decode(result, &last)
		if last.State == "running" {
			for _, action := range last.Actions {
				if action.Name == actionName && action.State == "running" {
					return last
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("request action %q did not run within 2m: %s last_error=%s (%s)",
		actionName, commitSummary(last), lastError.Code, lastError.Message)
	return api.ExecutionInspectResult{}
}

// actionByName returns one action observation by its admitted name.
func actionByName(t *testing.T, execution api.ExecutionInspectResult, name string) api.ExecutionAction {
	t.Helper()
	for _, action := range execution.Actions {
		if action.Name == name {
			return action
		}
	}
	t.Fatalf("execution %s has no action %q; actions=%+v", execution.ExecutionID, name, execution.Actions)
	return api.ExecutionAction{}
}

// approvalSubject returns the canonical immutable Git approval subject the
// enforcing service supplies for one effect. operation and resource travel in
// the request DTO's own fields and are compared separately; the effect subject
// is ref/expected_base/candidate.
func approvalSubject(ref, base, candidate string) []api.SubjectField {
	return []api.SubjectField{
		{Name: "ref", Kind: "string", Value: ref},
		{Name: "expected_base", Kind: "string", Value: base},
		{Name: "candidate", Kind: "string", Value: candidate},
	}
}

// requestSubjectYAML is the immutable request-action subject. operation and
// resource are stored literals too: the evidence reader derives the approval
// operation/resource from them and the verifier compares them exactly.
func requestSubjectYAML(base, candidate string) map[string]any {
	return map[string]any{
		"operation":     approvalOperation,
		"resource":      "repository-approval",
		"ref":           approvalRef,
		"expected_base": base,
		"candidate":     candidate,
	}
}

// blockingCommands copies the live execution credential into the retained
// workspace and then blocks until the test releases the workload. The credential
// is read back only through podman exec; it is never printed or placed in an
// event, receipt or task metadata.
func blockingCommands() []string {
	return []string{
		`test ! -e /kodama/task.json`,
		`cat /kodama/secrets/KODAMA_TOKEN > ` + approvalTokenPath,
		`while [ ! -e ` + approvalReleasePath + ` ]; do sleep 1; done`,
		`exit 0`,
	}
}

// requestPipelinePath is the prepare -> approve -> integrate pipeline. prepare
// is a short container action so the request is not the first action; integrate
// holds the fresh post-approval execution credential live for verification.
func (fixture *approvalFixture) requestPipelinePath(name, candidate string) api.PipelineApplyResult {
	fixture.t.Helper()
	return fixture.applyPipeline(name, marshalPipeline(fixture.t, pipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:     "prepare",
				Image:    fixture.harness.Image.Reference,
				Timeout:  approvalContainerTimeout,
				Commands: []string{`printf 'shared workspace\n' > /workspace/marker`},
			},
			{
				Name:    "approve",
				Request: "role/reviewer",
				Timeout: approvalRequestTimeout,
				Subject: requestSubjectYAML(approvalBase, candidate),
				Outputs: []string{"approved"},
			},
			{
				Name:     "integrate",
				Image:    fixture.harness.Image.Reference,
				Timeout:  approvalContainerTimeout,
				Commands: blockingCommands(),
			},
		},
	}))
}

// plainPipelinePath is one blocking container action with no request action. Its
// execution has a live credential but no approval evidence of its own.
func (fixture *approvalFixture) plainPipelinePath(name string) api.PipelineApplyResult {
	fixture.t.Helper()
	return fixture.applyPipeline(name, marshalPipeline(fixture.t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     "work",
			Image:    fixture.harness.Image.Reference,
			Timeout:  approvalContainerTimeout,
			Commands: blockingCommands(),
		}},
	}))
}

// waitLiveToken waits for one named container action to run and reads the live
// execution credential it was issued. The token is only used as a bearer value.
func (fixture *approvalFixture) waitLiveToken(t *testing.T, executionID, actionName string) string {
	t.Helper()
	fixture.harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := fixture.harness.REST(fixture.dualToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var current api.ExecutionInspectResult
		fixture.harness.Decode(result, &current)
		if current.State != "running" {
			return false
		}
		for _, action := range current.Actions {
			if action.Name == actionName {
				return action.State == "running"
			}
		}
		return false
	})
	var container string
	fixture.harness.Eventually(t, time.Minute, func() bool {
		found, err := fixture.harness.RunningExecutionContainer(t.Context(), executionID)
		if err != nil || found == "" {
			return false
		}
		container = found
		return true
	})
	var token string
	fixture.harness.Eventually(t, time.Minute, func() bool {
		value, err := fixture.harness.ReadContainerFile(t.Context(), container, approvalTokenPath)
		if err != nil {
			return false
		}
		token = strings.TrimSpace(value)
		return token != ""
	})
	return token
}

// resolvePass answers one running request with an exact pass through the public
// resolution operation, using the dual-role reader/reviewer as the assigned
// respondent.
func (fixture *approvalFixture) resolvePass(t *testing.T, executionID, actionName, key string) {
	t.Helper()
	for range 20 {
		current := fixture.inspect(t, executionID)
		if current.State != "running" {
			t.Fatalf("execution %s state = %q before resolve; want running", executionID, current.State)
		}
		action := actionByName(t, current, actionName)
		result, publicError := fixture.harness.REST(fixture.dualToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
			ExecutionID:           executionID,
			ActionID:              action.ActionID,
			ExpectedActionVersion: action.Version,
			Decision:              "pass",
			Outputs:               map[string]string{"approved": approvalOutputValue},
		}, &current.Version, key)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("execution.request.resolve refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var resolved api.ExecutionRequestResolveResult
		fixture.harness.Decode(result, &resolved)
		if resolved.ExecutionVersion <= current.Version || resolved.ActionVersion <= action.Version {
			t.Fatalf("resolve versions = %+v; want both advanced past execution=%d action=%d",
				resolved, current.Version, action.Version)
		}
		return
	}
	t.Fatalf("execution.request.resolve did not converge on a current version")
}

// startApprovedExecution starts the request pipeline, passes its request and
// returns the execution identity and its live post-approval credential.
func (fixture *approvalFixture) startApprovedExecution(t *testing.T, name, key, candidate string) (string, string) {
	t.Helper()
	applied := fixture.requestPipelinePath(name, candidate)
	started := fixture.startPipeline(applied.PipelineID, key)
	fixture.waitRequestRunning(t, started.ExecutionID, "approve")
	fixture.resolvePass(t, started.ExecutionID, "approve", "resolve-"+key)
	token := fixture.waitLiveToken(t, started.ExecutionID, "integrate")
	return started.ExecutionID, token
}

// startPlainExecution starts a request-free blocking execution and returns its
// live credential. It has no approval evidence of its own.
func (fixture *approvalFixture) startPlainExecution(t *testing.T, name, key string) (string, string) {
	t.Helper()
	applied := fixture.plainPipelinePath(name)
	started := fixture.startPipeline(applied.PipelineID, key)
	token := fixture.waitLiveToken(t, started.ExecutionID, "work")
	return started.ExecutionID, token
}

// cancelExecution cancels one execution through the public operation at its
// current version.
func (fixture *approvalFixture) cancelExecution(t *testing.T, executionID, key string) {
	t.Helper()
	for range 20 {
		current := fixture.inspect(t, executionID)
		switch current.State {
		case "succeeded", "failed", "cancelled":
			return
		}
		result, publicError := fixture.harness.REST(fixture.dualToken, "execution.cancel", api.ExecutionCancelRequest{
			ExecutionID: executionID,
		}, &current.Version, key)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("execution.cancel refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var cancelled api.ExecutionCancelResult
		fixture.harness.Decode(result, &cancelled)
		if cancelled.ExecutionVersion == 0 {
			t.Fatalf("execution.cancel returned no committed version")
		}
		return
	}
	t.Fatalf("execution.cancel did not converge on a current version")
}

// verify performs one served actor.verify call as the enforcing service.
func (fixture *approvalFixture) verify(t *testing.T, actorCredential, resourceID string, subject []api.SubjectField, approval string) (api.VerifyResponse, api.Error) {
	t.Helper()
	verified, _, publicError := fixture.verifyRaw(t, actorCredential, resourceID, subject, approval)
	return verified, publicError
}

// verifyRaw performs one served actor.verify call and also returns the exact
// public result so a caller can prove the response body never echoes the
// forwarded credential.
func (fixture *approvalFixture) verifyRaw(t *testing.T, actorCredential, resourceID string, subject []api.SubjectField, approval string) (api.VerifyResponse, api.Result, api.Error) {
	t.Helper()
	result, publicError := fixture.harness.REST(fixture.serviceToken, "actor.verify", api.VerifyRequest{
		ActorCredential: actorCredential,
		ResourceID:      resourceID,
		Operation:       approvalOperation,
		Subject:         api.Subject{Fields: subject},
		Approval:        approval,
	}, nil, "")
	if publicError.Code != "" {
		return api.VerifyResponse{}, api.Result{}, publicError
	}
	var verified api.VerifyResponse
	fixture.harness.Decode(result, &verified)
	return verified, result, api.Error{}
}

// requireApproved asserts one served verification approved the exact execution
// actor and never echoed the forwarded credential.
func requireApproved(t *testing.T, verified api.VerifyResponse) {
	t.Helper()
	if !verified.Approval {
		t.Fatalf("verification %s did not carry the approval fact", verified.VerificationID)
	}
	if verified.Principal.Kind != "execution" {
		t.Fatalf("verified principal kind = %q; want execution", verified.Principal.Kind)
	}
	if verified.Principal.ID == "" || verified.VerificationID == "" {
		t.Fatalf("verification identity is incomplete: %+v", verified)
	}
}

// commitSummary renders a safe failure summary for one inspect result. It never
// contains a credential or a subject value.
func commitSummary(execution api.ExecutionInspectResult) string {
	return fmt.Sprintf("execution=%s state=%s version=%d actions=%d",
		execution.ExecutionID, execution.State, execution.Version, len(execution.Actions))
}
