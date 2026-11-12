package app

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// fixedActorPrincipals resolves every forwarded credential to one principal and
// records the presented value.
type fixedActorPrincipals struct {
	principal   identity.Principal
	err         error
	credentials []string
}

func (resolver *fixedActorPrincipals) Resolve(_ context.Context, credential string) (identity.Principal, error) {
	resolver.credentials = append(resolver.credentials, credential)
	if resolver.err != nil {
		return identity.Principal{}, resolver.err
	}
	return resolver.principal, nil
}

// recordingExecutionIdentities is the current-action reader seam.
type recordingExecutionIdentities struct {
	identity   kernelengine.EffectIdentity
	err        error
	executions []id.ExecutionID
}

func (resolver *recordingExecutionIdentities) ResolveExecutionIdentity(_ context.Context, executionID id.ExecutionID) (kernelengine.EffectIdentity, error) {
	resolver.executions = append(resolver.executions, executionID)
	if resolver.err != nil {
		return kernelengine.EffectIdentity{}, resolver.err
	}
	return resolver.identity, nil
}

// TestActorActionsResolvesTheExecutionCurrentAction proves the composition seam
// authenticates the credential and returns the current running container action
// of that execution, never a client value.
func TestActorActionsResolvesTheExecutionCurrentAction(t *testing.T) {
	principals := &fixedActorPrincipals{principal: identity.Principal{
		ID:          "execution-1",
		Kind:        identity.KindExecution,
		Scope:       tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"},
		Enabled:     true,
		Version:     1,
		ExecutionID: "execution-1",
	}}
	identities := &recordingExecutionIdentities{identity: kernelengine.EffectIdentity{
		ExecutionID: "execution-1",
		ActionID:    "action-current",
	}}
	action, err := actorActions{principals: principals, identities: identities}.
		ResolveActorAction(context.Background(), "execution-token")
	if err != nil {
		t.Fatalf("ResolveActorAction: %v", err)
	}
	if action != "action-current" {
		t.Fatalf("resolved action = %q; want the current running action", action)
	}
	if len(principals.credentials) != 1 || principals.credentials[0] != "execution-token" {
		t.Fatalf("principal resolver credentials = %q; want the presented credential", principals.credentials)
	}
	if len(identities.executions) != 1 || identities.executions[0] != "execution-1" {
		t.Fatalf("identity resolver executions = %q; want the resolved actor execution", identities.executions)
	}
}

// TestActorActionsHumanAndServiceNameNoAction proves a non-execution actor
// never reaches the execution action reader.
func TestActorActionsHumanAndServiceNameNoAction(t *testing.T) {
	for _, kind := range []identity.PrincipalKind{identity.KindHuman, identity.KindService} {
		t.Run(string(kind), func(t *testing.T) {
			principals := &fixedActorPrincipals{principal: identity.Principal{
				ID:      "actor-1",
				Kind:    kind,
				Scope:   tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"},
				Enabled: true,
				Version: 1,
			}}
			identities := &recordingExecutionIdentities{}
			action, err := actorActions{principals: principals, identities: identities}.
				ResolveActorAction(context.Background(), "actor-token")
			if err != nil {
				t.Fatalf("ResolveActorAction: %v", err)
			}
			if action != "" {
				t.Fatalf("%s action = %q; want none", kind, action)
			}
			if len(identities.executions) != 0 {
				t.Fatalf("%s reached the execution action reader: %q", kind, identities.executions)
			}
		})
	}
}

// TestActorActionsRefusesAnExecutionWithoutACurrentAction proves fail-closed:
// a resolution error is returned with no action instead of an empty target.
func TestActorActionsRefusesAnExecutionWithoutACurrentAction(t *testing.T) {
	principals := &fixedActorPrincipals{principal: identity.Principal{
		ID:          "execution-1",
		Kind:        identity.KindExecution,
		Scope:       tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"},
		Enabled:     true,
		Version:     1,
		ExecutionID: "execution-1",
	}}
	identities := &recordingExecutionIdentities{
		err: fault.New(fault.DependencyUnavailable, "execution has no running container workload"),
	}
	action, err := actorActions{principals: principals, identities: identities}.
		ResolveActorAction(context.Background(), "execution-token")
	if err == nil {
		t.Fatal("ResolveActorAction accepted an execution with no current action")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("refusal class = %v; want dependency_unavailable (%v)", class, err)
	}
	if action != "" {
		t.Fatalf("refused action = %q; want none", action)
	}
}

// TestActorActionsPropagatesAuthenticationRefusal proves an unknown credential
// never reaches the execution action reader.
func TestActorActionsPropagatesAuthenticationRefusal(t *testing.T) {
	principals := &fixedActorPrincipals{err: fault.New(fault.Unauthenticated, "unknown credential")}
	identities := &recordingExecutionIdentities{}
	action, err := actorActions{principals: principals, identities: identities}.
		ResolveActorAction(context.Background(), "unknown-token")
	if err == nil {
		t.Fatal("ResolveActorAction accepted an unknown credential")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
		t.Fatalf("refusal class = %v; want unauthenticated (%v)", class, err)
	}
	if action != "" {
		t.Fatalf("refused action = %q; want none", action)
	}
	if len(identities.executions) != 0 {
		t.Fatalf("unknown credential reached the execution action reader: %q", identities.executions)
	}
}
