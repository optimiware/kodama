package postgres

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

func TestDefaultActionsNarrowByAccess(t *testing.T) {
	reader, err := DefaultActions(execution.AccessRead)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := DefaultActions(execution.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	if reader.Contains(runtimeauth.ActionSourcePropose) || !writer.Contains(runtimeauth.ActionSourcePropose) {
		t.Fatalf("source.propose: reader %v writer %v", reader, writer)
	}
	for _, a := range []runtimeauth.Action{runtimeauth.ActionResultSubmit, runtimeauth.ActionArtifactReserve, runtimeauth.ActionArtifactComplete, runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew, runtimeauth.ActionSecretResolve} {
		if !reader.Contains(a) || !writer.Contains(a) {
			t.Errorf("%s missing", a)
		}
	}
	for _, a := range []runtimeauth.Action{runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke, runtimeauth.ActionDeploymentCall} {
		if reader.Contains(a) || writer.Contains(a) {
			t.Errorf("%s is granted on another interface, not by result finalization", a)
		}
	}
}

func TestSessionContext(t *testing.T) {
	if _, ok := SessionFrom(context.Background()); ok {
		t.Fatal("an unbound context has a session")
	}
	s := Session{Tenant: "t022_acme", ID: "ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Generation: 2, Route: "att_1", Action: runtimeauth.ActionResultSubmit}
	got, ok := SessionFrom(WithSession(context.Background(), s))
	if !ok || got != s {
		t.Fatalf("%+v %v", got, ok)
	}
}

func TestConstructorsFailClosed(t *testing.T) {
	if _, err := NewGuard(nil, nil, nil); err == nil {
		t.Error("guard without dependencies")
	}
	if _, err := NewAttemptAuthority(nil, nil, nil); err == nil {
		t.Error("attempt authority without dependencies")
	}
	if _, err := NewExecutionStore(nil, nil, nil); err == nil {
		t.Error("execution store without dependencies")
	}
	if _, err := NewWorkspaceStore(nil, nil, nil); err == nil {
		t.Error("workspace store without dependencies")
	}
}
