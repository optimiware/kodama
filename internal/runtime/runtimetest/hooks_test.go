package runtimetest

import (
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
)

// Test_missing_hooks: the contract demands the hooks of every capability a
// backend advertises, and only those.
func Test_missing_hooks(t *testing.T) {
	full := ResourcesFixture{
		Service:       func(string) execution.ServiceSpec { return execution.ServiceSpec{} },
		Preparation:   func() execution.PreparationSpec { return execution.PreparationSpec{} },
		Foreign:       func(execution.ResourceHandle) {},
		ForeignIntact: func(execution.ResourceHandle) bool { return true },
		Exists:        func(execution.ResourceHandle) bool { return true },
		Fault:         func() func() { return func() {} },
		HoldWorkspace: func(execution.ResourceHandle) func() { return func() {} },
	}
	all := execution.Set(execution.CapRunWorkspace, execution.CapWorkspaceFence, execution.CapWorkspacePreparation, execution.CapResourceRecovery, execution.CapRunServices)

	if got := missingHooks(full, all); len(got) != 0 {
		t.Fatalf("a complete fixture is missing %v", got)
	}
	for name, c := range map[string]struct {
		mutate func(*ResourcesFixture)
		want   string
	}{
		"services":    {func(f *ResourcesFixture) { f.Service = nil }, "Service"},
		"preparation": {func(f *ResourcesFixture) { f.Preparation = nil }, "Preparation"},
		"fence":       {func(f *ResourcesFixture) { f.HoldWorkspace = nil }, "HoldWorkspace"},
		"foreign":     {func(f *ResourcesFixture) { f.Foreign = nil }, "Foreign"},
		"intact":      {func(f *ResourcesFixture) { f.ForeignIntact = nil }, "ForeignIntact"},
		"exists":      {func(f *ResourcesFixture) { f.Exists = nil }, "Exists"},
		"fault":       {func(f *ResourcesFixture) { f.Fault = nil }, "Fault"},
	} {
		fx := full
		c.mutate(&fx)
		got := missingHooks(fx, all)
		if len(got) != 1 || !strings.HasPrefix(got[0], c.want+" ") {
			t.Fatalf("%s: %v", name, got)
		}
	}
	// A capability the backend does not advertise needs no hook.
	bare := ResourcesFixture{}
	if got := missingHooks(bare, execution.Set(execution.CapResourceRecovery)); len(got) != 0 {
		t.Fatalf("hooks demanded for undeclared capabilities: %v", got)
	}
	got := missingHooks(bare, execution.Set(execution.CapRunServices))
	if !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "Service ") }) || len(got) != 1 {
		t.Fatalf("%v", got)
	}
}
