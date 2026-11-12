// Behavior tests for the composition-backed platform.PermissionSource.
//
// A source is built for exactly one closed execution permission and returns it
// only for current facts that show a scoped, running execution whose single
// current action is a running container action before its authoritative
// deadline. Every other fact fails closed to ExecNone; the source never reads a
// request, URL, environment or stored permission column.
package bridges_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("project-a")}
}

func validFacts(now time.Time) platform.ExecutionFacts {
	return platform.ExecutionFacts{
		ExecutionID:      id.ExecutionID("exec-1"),
		ExecutionState:   "running",
		ExecutionVersion: 3,
		PrincipalID:      id.PrincipalID("exec-1"),
		Scope:            testScope(),
		ActionID:         id.ActionID("action-1"),
		ActionKind:       "container",
		ActionState:      "running",
		Deadline:         now.Add(time.Minute),
	}
}

func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("fault class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

func TestPermissionSourceGrantsOnlyCurrentRunningContainerFacts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	clock := &fakeClock{now: now}
	source, err := bridges.NewPermissionSource(authorize.ExecConfiguredTools, clock)
	if err != nil {
		t.Fatalf("NewPermissionSource: %v", err)
	}
	permission, err := source.Permission(context.Background(), validFacts(now))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if permission != authorize.ExecConfiguredTools {
		t.Fatalf("permission = %q; want %q", permission, authorize.ExecConfiguredTools)
	}
}

func TestPermissionSourceFailsClosedForEveryOtherFact(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	clock := &fakeClock{now: now}
	source, err := bridges.NewPermissionSource(authorize.ExecConfiguredModels, clock)
	if err != nil {
		t.Fatalf("NewPermissionSource: %v", err)
	}

	base := validFacts(now)
	cases := []struct {
		name   string
		mutate func(*platform.ExecutionFacts)
	}{
		{"execution queued", func(f *platform.ExecutionFacts) { f.ExecutionState = "queued" }},
		{"execution succeeded", func(f *platform.ExecutionFacts) { f.ExecutionState = "succeeded" }},
		{"request wait action kind", func(f *platform.ExecutionFacts) { f.ActionKind = "request" }},
		{"unknown action kind", func(f *platform.ExecutionFacts) { f.ActionKind = "mystery" }},
		{"action waiting", func(f *platform.ExecutionFacts) { f.ActionState = "waiting" }},
		{"action succeeded", func(f *platform.ExecutionFacts) { f.ActionState = "succeeded" }},
		{"between actions", func(f *platform.ExecutionFacts) {
			f.ActionID = ""
			f.ActionKind = ""
			f.ActionState = ""
			f.Deadline = time.Time{}
		}},
		{"expired deadline", func(f *platform.ExecutionFacts) { f.Deadline = now.Add(-time.Second) }},
		{"deadline at now", func(f *platform.ExecutionFacts) { f.Deadline = now }},
		{"zero deadline", func(f *platform.ExecutionFacts) { f.Deadline = time.Time{} }},
		{"empty scope", func(f *platform.ExecutionFacts) { f.Scope = tenancy.Scope{} }},
		{"invalid project scope", func(f *platform.ExecutionFacts) { f.Scope.ProjectID = "bad project" }},
		{"empty execution id", func(f *platform.ExecutionFacts) { f.ExecutionID = "" }},
		{"invalid execution id", func(f *platform.ExecutionFacts) { f.ExecutionID = "bad id" }},
		{"empty action id", func(f *platform.ExecutionFacts) { f.ActionID = "" }},
		{"empty principal id", func(f *platform.ExecutionFacts) { f.PrincipalID = "" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			facts := base
			testCase.mutate(&facts)
			permission, err := source.Permission(context.Background(), facts)
			if err != nil {
				t.Fatalf("Permission returned error %v; want ExecNone", err)
			}
			if permission != authorize.ExecNone {
				t.Fatalf("permission = %q; want ExecNone", permission)
			}
		})
	}
}

func TestPermissionSourceRefusesInvalidConstruction(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0).UTC()}
	for _, permission := range []authorize.ExecutionPermission{authorize.ExecNone, "widened"} {
		source, err := bridges.NewPermissionSource(permission, clock)
		if err == nil {
			t.Fatalf("NewPermissionSource(%q) = %v, nil; want refusal", permission, source)
		}
		requireFaultClass(t, err, fault.InvalidInput)
	}
	if _, err := bridges.NewPermissionSource(authorize.ExecOwnOutputs, nil); err == nil {
		t.Fatal("NewPermissionSource accepted a nil clock")
	}
}

func TestPermissionSourcePropagatesCancellation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	source, err := bridges.NewPermissionSource(authorize.ExecOwnEvents, &fakeClock{now: now})
	if err != nil {
		t.Fatalf("NewPermissionSource: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	permission, err := source.Permission(ctx, validFacts(now))
	if permission != authorize.ExecNone {
		t.Fatalf("permission = %q; want ExecNone", permission)
	}
	requireFaultClass(t, err, fault.DeadlineExpired)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not wrap context.Canceled: %v", err)
	}
}

type fakeClock struct {
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	if clock == nil {
		return time.Time{}
	}
	return clock.now
}
