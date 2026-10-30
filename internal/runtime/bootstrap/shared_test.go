package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

type registrarV2 struct {
	registrar
	cmds2 []runtimeauth.RegisterCommandV2
}

func (r *registrarV2) RegisterV2(_ context.Context, cmd runtimeauth.RegisterCommandV2) (runtimeauth.Registration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := os.ReadFile(r.file)
	r.cmds2, r.seen = append(r.cmds2, cmd), append(r.seen, string(raw))
	return runtimeauth.Registration{}, r.err
}

// sharedSpec is the spec of a run_shared attempt: a ContextV2 and the SharedLaunch
// execution sets before every Start.
func sharedSpec(t *testing.T) execution.AttemptSpec {
	t.Helper()
	c := runtimev2.ContextV2{
		ContextVersion: 2, RunModel: "run_shared", TenantID: "t086_boot", ProjectID: "p086_boot", RunID: "run_1", StepID: "step", AttemptID: "att_1",
		ActionKey:  execution.ActionKey("run_1", "step"),
		Definition: runtimev1.Definition{ID: "pd_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("2", 64))},
		Command:    runtimev1.Command{Image: "registry.invalid/x@sha256:" + strings.Repeat("0", 64), Command: []string{"true"}, Args: []string{}, Env: map[string]string{}},
		Workspace:  runtimev2.WorkspaceV2{Access: runtimev1.AccessNone, MountPath: "/workspace", Lineage: []runtimev2.LineageEntry{}},
		Endpoints:  map[string]runtimev2.Endpoint{}, Egress: []string{},
		Run:    runtimev2.RunWindow{StartedAt: "2026-10-26T10:00:00Z", DeadlineAt: "2026-10-26T11:00:00Z"},
		Policy: runtimev2.PolicyPin{ID: "run-v1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("f", 64))},
		Inputs: map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: 60, MaxOutputBytes: 1 << 20, MaxOutputFiles: 16, MaxDiagnosticBytes: 1 << 20},
	}
	digest, doc, err := runtimeauth.PinContextV2(c)
	if err != nil {
		t.Fatal(err)
	}
	return execution.AttemptSpec{
		Scope: domain.Scope{TenantID: c.TenantID, ProjectID: c.ProjectID}, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		WorkloadName: execution.WorkloadName(c.TenantID, c.AttemptID), SpecFingerprint: "sha256:" + strings.Repeat("a", 64),
		ContextDigest: "sha256:" + digest.Hex(), Context: doc, Access: execution.AccessNone,
		Shared: &execution.SharedLaunch{},
	}
}

// Test_shared_attempt_admission: a run_shared attempt's one-time credential is
// persisted before it is registered with its ContextV2, through the v2
// registrar; a registrar that cannot serve the wire is refused, and no checkout
// credential is ever issued for it.
func Test_shared_attempt_admission(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	reg := &registrarV2{registrar: registrar{file: filepath.Join(dir, CredentialFile)}}
	s := sharedSpec(t)
	if err := (Issuer{Registrar: reg}).Issue(ctx, s, dir); err != nil {
		t.Fatal(err)
	}
	if len(reg.cmds) != 0 || len(reg.cmds2) != 1 {
		t.Fatalf("the v1 registration was used: v1=%d v2=%d", len(reg.cmds), len(reg.cmds2))
	}
	stored, _ := os.ReadFile(reg.file)
	cmd := reg.cmds2[0]
	if cmd.Credential.Reveal() != string(stored) || reg.seen[0] != string(stored) {
		t.Fatal("the registered credential was not the persisted one")
	}
	if cmd.Context.ContextVersion != 2 || cmd.Context.AttemptID != s.AttemptID || cmd.Scope != s.Scope {
		t.Fatalf("command %+v", cmd)
	}
	// A retry registers the same credential.
	if err := (Issuer{Registrar: reg}).Issue(ctx, s, dir); err != nil || reg.cmds2[1].Credential.Reveal() != string(stored) {
		t.Fatalf("retry: %v", err)
	}
	// A conflict is unrecoverable and classified like v1's.
	reg.err = runtimeauth.ErrConflict
	if err := (Issuer{Registrar: reg}).Issue(ctx, s, dir); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	// A registrar of the v1 wire only cannot admit a run_shared attempt.
	v1only := &registrar{file: filepath.Join(t.TempDir(), CredentialFile)}
	if err := (Issuer{Registrar: v1only}).Issue(ctx, s, filepath.Dir(v1only.file)); !errors.Is(err, execution.ErrNotConfigured) || len(v1only.cmds) != 0 {
		t.Fatalf("a v1-only registrar: %v", err)
	}
	// No checkout credential for a run_shared attempt, whatever the provider holds.
	called := false
	m, err := IssueCheckout(ctx, providerFunc(func(CheckoutRequest) (CheckoutMaterial, error) { called = true; return CheckoutMaterial{}, nil }), s, runtimev1.Context{Workspace: runtimev1.Workspace{Access: runtimev1.AccessRead}})
	if err != nil || m != nil || called {
		t.Fatalf("checkout for a run_shared attempt: %v %v called=%v", m, err, called)
	}
}
