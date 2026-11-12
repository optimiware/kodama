package app

// Current-authority and attribution tests for the optional configured model
// gateway composition.
//
// TestModelCompositionAuthorityAndAttribution is the named behavioral test. It
// builds the same gateway dependency shape as wireApp directly, so the gateway
// boundary is exercised even when a bypass would also be refused by the public
// router: the real permission source, the real platform execution reader, the
// shared authorizer and the request-local binding authenticator.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// deniedModelAuthority refuses exactly one current-authority check and allows
// every other requested fact. It is the revocation boundary under test.
type deniedModelAuthority struct {
	check authorize.AuthorityCheck
}

func (authority deniedModelAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	facts := make([]authorize.AuthorityFact, 0, len(reads))
	for _, read := range reads {
		facts = append(facts, authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Allowed:     read.Check != authority.check,
		})
	}
	return facts, nil
}

// modelTestServicePrincipal is one project-scoped service holding the engineer
// grant model.invoke requires.
func modelTestServicePrincipal() identity.Principal {
	return identity.Principal{
		ID:      "service-1",
		Kind:    identity.KindService,
		Scope:   modelTestScope("project-1"),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
}

// modelTestExecutionPrincipal is one execution identity in project-1.
func modelTestExecutionPrincipal() identity.Principal {
	return identity.Principal{
		ID:          "exec-1",
		Kind:        identity.KindExecution,
		Scope:       modelTestScope("project-1"),
		Enabled:     true,
		Version:     1,
		ExecutionID: "execution-models-1",
	}
}

// modelTestExecutionContext builds the controller fixture the platform
// execution reader reads for one execution.
func modelTestExecutionContext(state, actionKind, actionState string, deadline time.Time) *toolTestController {
	controller := &toolTestController{
		execution: executionFactRow("execution-models-1", "exec-1", state),
	}
	if actionKind != "" {
		controller.actions = []store.Row{
			actionFactRow("execution-models-1", "action-models-1", actionKind, actionState, deadline),
		}
	}
	return controller
}

// TestModelCompositionAuthorityAndAttribution covers the current-authority and
// attribution boundaries the composed gateway must re-read at call time. Each
// refusal proves zero upstream calls and zero usage rows; each success proves
// the usage row is attributed to the caller kind that was authorized.
func TestModelCompositionAuthorityAndAttribution(t *testing.T) {
	clock := newFakeClock()
	scope := modelTestScope("project-1")
	request := modelTestRequest(modelTestAlias)

	build := func(t *testing.T, principal identity.Principal, controller store.Controller, authority authorize.AuthorityReader) (opsapp.ModelInvocation, *modelTestTransport, *recordingToolExecutor) {
		t.Helper()
		transport := &modelTestTransport{response: modelTestResponse(&modelTestBody{reader: strings.NewReader(modelTestCompletion)})}
		usage := &recordingToolExecutor{}
		models, stop := modelTestDependencies(t, principal, controller, authority, usage, transport, clock)
		t.Cleanup(func() { _ = stop(context.Background()) })
		return models, transport, usage
	}

	t.Run("human caller is attributed without an execution identity", func(t *testing.T) {
		principal := modelTestWidePrincipal()
		models, transport, usage := build(t, principal, &toolTestController{}, allowAllToolAuthority{})

		if _, err := models.Invoke(context.Background(), principal, scope.ProjectID, request); err != nil {
			t.Fatalf("human model invocation: %v", err)
		}
		if attempts := transport.attemptCount(); attempts != 1 {
			t.Fatalf("human upstream attempts = %d; want 1", attempts)
		}
		values := modelTestSingleUsage(t, usage)
		if values["principal_id"] != "admin-1" {
			t.Fatalf("human usage principal = %v; want admin-1", values["principal_id"])
		}
		if values["execution_id"] != nil {
			t.Fatalf("human usage execution_id = %v; want nil", values["execution_id"])
		}
		if values["outcome"] != "succeeded" {
			t.Fatalf("human usage outcome = %v; want succeeded", values["outcome"])
		}
	})

	t.Run("service caller is attributed without an execution identity", func(t *testing.T) {
		principal := modelTestServicePrincipal()
		models, transport, usage := build(t, principal, &toolTestController{}, allowAllToolAuthority{})

		if _, err := models.Invoke(context.Background(), principal, scope.ProjectID, request); err != nil {
			t.Fatalf("service model invocation: %v", err)
		}
		if attempts := transport.attemptCount(); attempts != 1 {
			t.Fatalf("service upstream attempts = %d; want 1", attempts)
		}
		values := modelTestSingleUsage(t, usage)
		if values["principal_id"] != "service-1" {
			t.Fatalf("service usage principal = %v; want service-1", values["principal_id"])
		}
		if values["execution_id"] != nil {
			t.Fatalf("service usage execution_id = %v; want nil", values["execution_id"])
		}
	})

	t.Run("execution caller is attributed to its execution", func(t *testing.T) {
		principal := modelTestExecutionPrincipal()
		controller := modelTestExecutionContext("running", "container", "running", clock.Now().Add(time.Hour))
		models, transport, usage := build(t, principal, controller, allowAllToolAuthority{})

		if _, err := models.Invoke(context.Background(), principal, scope.ProjectID, request); err != nil {
			t.Fatalf("execution model invocation: %v", err)
		}
		if attempts := transport.attemptCount(); attempts != 1 {
			t.Fatalf("execution upstream attempts = %d; want 1", attempts)
		}
		values := modelTestSingleUsage(t, usage)
		if values["principal_id"] != "exec-1" {
			t.Fatalf("execution usage principal = %v; want exec-1", values["principal_id"])
		}
		if values["execution_id"] != "execution-models-1" {
			t.Fatalf("execution usage execution_id = %v; want execution-models-1", values["execution_id"])
		}
		if values["outcome"] != "succeeded" {
			t.Fatalf("execution usage outcome = %v; want succeeded", values["outcome"])
		}
	})

	refusals := []struct {
		name       string
		principal  identity.Principal
		projectID  string
		controller store.Controller
		authority  authorize.AuthorityReader
		class      fault.Class
	}{
		{
			name:      "foreign project scope",
			principal: modelTestWidePrincipal(),
			projectID: "project-2",
			class:     fault.Forbidden,
		},
		{
			name: "foreign tenant",
			principal: identity.Principal{
				ID:      "admin-3",
				Kind:    identity.KindHuman,
				Scope:   tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-9"},
				Roles:   []tenancy.Role{tenancy.RoleEngineer},
				Enabled: true,
				Version: 1,
			},
			projectID: "project-1",
			class:     fault.Forbidden,
		},
		{
			name: "tenant-wide principal naming an unconfigured project",
			principal: identity.Principal{
				ID:      "admin-1",
				Kind:    identity.KindHuman,
				Scope:   tenancy.Scope{TenantID: "tenant-1"},
				Roles:   []tenancy.Role{tenancy.RoleEngineer},
				Enabled: true,
				Version: 1,
			},
			projectID: "project-2",
			class:     fault.NotFound,
		},
		{
			name:      "revoked membership",
			principal: modelTestWidePrincipal(),
			projectID: "project-1",
			authority: deniedModelAuthority{check: authorize.CheckMembership},
			class:     fault.Forbidden,
		},
		{
			name:       "revoked execution state",
			principal:  modelTestExecutionPrincipal(),
			projectID:  "project-1",
			controller: modelTestExecutionContext("running", "container", "running", clock.Now().Add(time.Hour)),
			authority:  deniedModelAuthority{check: authorize.CheckExecutionRunning},
			class:      fault.Forbidden,
		},
		{
			name:       "request wait action",
			principal:  modelTestExecutionPrincipal(),
			projectID:  "project-1",
			controller: modelTestExecutionContext("running", "request", "running", clock.Now().Add(time.Hour)),
			class:      fault.Forbidden,
		},
		{
			name:       "expired action deadline",
			principal:  modelTestExecutionPrincipal(),
			projectID:  "project-1",
			controller: modelTestExecutionContext("running", "container", "running", clock.Now().Add(-time.Minute)),
			class:      fault.Forbidden,
		},
		{
			name:       "terminal execution",
			principal:  modelTestExecutionPrincipal(),
			projectID:  "project-1",
			controller: modelTestExecutionContext("failed", "", "", time.Time{}),
			class:      fault.Forbidden,
		},
		{
			name:       "execution without a running action",
			principal:  modelTestExecutionPrincipal(),
			projectID:  "project-1",
			controller: modelTestExecutionContext("running", "", "", time.Time{}),
			class:      fault.Forbidden,
		},
	}

	for _, test := range refusals {
		t.Run(test.name, func(t *testing.T) {
			controller := test.controller
			if controller == nil {
				controller = &toolTestController{}
			}
			authority := test.authority
			if authority == nil {
				authority = allowAllToolAuthority{}
			}
			models, transport, usage := build(t, test.principal, controller, authority)

			_, err := models.Invoke(context.Background(), test.principal, id.ProjectID(test.projectID), request)
			if err == nil {
				t.Fatalf("%s reached a configured model", test.name)
			}
			if class, ok := fault.ClassOf(err); !ok || class != test.class {
				t.Fatalf("%s refusal class = %v (ok=%v); want %v", test.name, class, ok, test.class)
			}
			if attempts := transport.attemptCount(); attempts != 0 {
				t.Fatalf("%s upstream attempts = %d; want 0", test.name, attempts)
			}
			if units := modelTestUsageUnits(t, usage); len(units) != 0 {
				t.Fatalf("%s committed %d usage rows; want 0", test.name, len(units))
			}
		})
	}
}

// modelTestSingleUsage returns the values of exactly one recorded usage row.
func modelTestSingleUsage(t *testing.T, executor *recordingToolExecutor) map[string]any {
	t.Helper()
	units := modelTestUsageUnits(t, executor)
	if len(units) != 1 {
		t.Fatalf("usage rows = %d; want exactly 1", len(units))
	}
	return modelTestUsageValues(t, units[0])
}
