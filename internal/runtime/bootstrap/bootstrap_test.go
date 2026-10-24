package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

type registrar struct {
	mu   sync.Mutex
	err  error
	cmds []runtimeauth.RegisterCommand
	// seen is the credential file's content when Register ran.
	seen []string
	file string
}

func (r *registrar) Register(_ context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := os.ReadFile(r.file)
	r.cmds, r.seen = append(r.cmds, cmd), append(r.seen, string(raw))
	return runtimeauth.Registration{}, r.err
}

func spec(t *testing.T) execution.AttemptSpec { return specWith(t, nil) }

// specWith builds the attempt spec of a context that mutate has adjusted.
func specWith(t *testing.T, mutate func(*runtimev1.Context)) execution.AttemptSpec {
	t.Helper()
	c := runtimev1.Context{
		TenantID: "t086_boot", ProjectID: "p086_boot", RunID: "run_1", StepID: "step", AttemptID: "att_1",
		ActionKey:  execution.ActionKey("run_1", "step"),
		Definition: runtimev1.Definition{ID: "pd_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("2", 64))},
		Command:    runtimev1.Command{Image: "registry.invalid/x@sha256:" + strings.Repeat("0", 64), Command: []string{"true"}, Args: []string{}, Env: map[string]string{}},
		Workspace:  runtimev1.Workspace{Access: runtimev1.AccessNone},
		Inputs:     map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: 60, MaxOutputBytes: 1 << 20, MaxOutputFiles: 16, MaxDiagnosticBytes: 1 << 20},
	}
	if mutate != nil {
		mutate(&c)
	}
	plan, err := execution.DefaultPlanner{}.Plan(c.TenantID, c)
	if err != nil {
		t.Fatal(err)
	}
	return execution.AttemptSpec{
		Scope: c.Scope(), RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		WorkloadName: plan.WorkloadName, SpecFingerprint: plan.SpecFingerprint, ContextDigest: plan.ContextDigest, Context: plan.Context,
		Access: execution.AccessNone,
	}
}

// Test_admission_credential: the credential is generated once and persisted
// before it is registered; every retry, also a concurrent one, registers the
// same value.
func Test_admission_credential(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	reg := &registrar{file: filepath.Join(dir, CredentialFile)}
	issuer := Issuer{Registrar: reg}
	s := spec(t)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := issuer.Issue(ctx, s, dir); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	stored, err := os.ReadFile(reg.file)
	if err != nil {
		t.Fatal(err)
	}
	for i, cmd := range reg.cmds {
		if cmd.Credential.Reveal() != string(stored) || reg.seen[i] != string(stored) {
			t.Fatalf("registration %d used a credential that was not the persisted one", i)
		}
		if cmd.Scope != s.Scope || cmd.AttemptID != s.AttemptID || cmd.Context.AttemptID != s.AttemptID || !cmd.Actions.Contains(runtimeauth.ActionResultSubmit) {
			t.Fatalf("registration %+v", cmd)
		}
	}
	if fi, _ := os.Stat(reg.file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files remain: %v", entries)
	}

	reg.err = runtimeauth.ErrConflict
	if err := issuer.Issue(ctx, s, dir); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("a contradicting registration: %v", err)
	}
	if err := (Issuer{}).Issue(ctx, s, dir); !errors.Is(err, execution.ErrNotConfigured) {
		t.Fatalf("without a registrar: %v", err)
	}
}

// Test_workload_identity: one instance stands for both Job and Pod, and the
// evidence a runtime reports satisfies runtime admission's ownership check.
func Test_workload_identity(t *testing.T) {
	w := execution.Workload{Name: "kdm-x", UID: "11111111-2222-4333-8444-555555555555"}
	id, ev := Identity(w), Evidence("kodama-test", w)
	if id.PodName != w.Name || id.PodUID != w.UID || ev.PodOwnerUID != ev.JobUID || ev.JobUID != id.UID || ev.PodNamespace != ev.Namespace {
		t.Fatalf("%+v %+v", id, ev)
	}
	if got := strings.Join(Env(w), " "); got != "KODAMA_JOB_NAME=kdm-x KODAMA_JOB_UID="+w.UID+" KODAMA_POD_NAME=kdm-x KODAMA_POD_UID="+w.UID {
		t.Fatal(got)
	}
	if RequireNoCheckout(runtimev1.Context{Workspace: runtimev1.Workspace{Access: runtimev1.AccessRead}}) == nil {
		t.Fatal("a source-consuming attempt was accepted")
	}
}

type profiles map[string]runtimeauth.ProfileCapabilities

func (p profiles) ProfileCapabilities(_ context.Context, _ domain.Scope, pin runtimev1.Profile) (runtimeauth.ProfileCapabilities, error) {
	c, ok := p[pin.Name]
	if !ok {
		return runtimeauth.ProfileCapabilities{}, errors.New("no such profile")
	}
	return c, nil
}

type brokenSource struct{}

func (brokenSource) Ceiling(context.Context) (runtimeauth.CeilingConfig, error) {
	return runtimeauth.CeilingConfig{}, errors.New("unreadable")
}

func ceilingIssuer(t *testing.T, reg *registrar, src runtimeauth.CeilingSource) Issuer {
	t.Helper()
	pol, err := runtimeauth.NewCeilingPolicy(src, profiles{"both": {Model: true, Tools: true}, "none": {}})
	if err != nil {
		t.Fatal(err)
	}
	return Issuer{Registrar: reg, Ceiling: &pol}
}

func operatorCeiling() runtimeauth.CeilingSource {
	return runtimeauth.StaticCeiling{Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{
		runtimeauth.StepAgent:     {Grants: []runtimeauth.Action{runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke}},
		runtimeauth.StepContainer: {Grants: []runtimeauth.Action{runtimeauth.ActionDeploymentCall}},
	}}
}

// Test_issuer_action_ceiling: the registered ceiling is chosen from the pinned
// context's facts and the operator configuration, and every degraded input
// registers the defaults only.
func Test_issuer_action_ceiling(t *testing.T) {
	agent := func(profile string) func(*runtimev1.Context) {
		return func(c *runtimev1.Context) {
			c.Profile = &runtimev1.Profile{Name: profile, Version: 1, WrapperVersion: "1.0.0"}
		}
	}
	deploy := func(c *runtimev1.Context) {
		c.Environment = &runtimev1.EnvironmentPin{ID: "env_1", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("3", 64))}
	}
	base := runtimeauth.DefaultActions(false)
	with := func(extra ...runtimeauth.Action) []string {
		set, err := runtimeauth.NewActionSet(append(slices.Clone(base), extra...)...)
		if err != nil {
			t.Fatal(err)
		}
		return set.Strings()
	}
	kindOf := func(k runtimeauth.StepKind, err error) func(context.Context, execution.AttemptSpec) (runtimeauth.StepKind, error) {
		return func(context.Context, execution.AttemptSpec) (runtimeauth.StepKind, error) { return k, err }
	}
	for name, tc := range map[string]struct {
		mutate func(*runtimev1.Context)
		src    runtimeauth.CeilingSource
		kind   func(context.Context, execution.AttemptSpec) (runtimeauth.StepKind, error)
		want   []string
	}{
		"agent naming a model and a tool": {agent("both"), operatorCeiling(), nil, with(runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke)},
		"agent naming neither":            {agent("none"), operatorCeiling(), nil, with()},
		"agent with an unknown profile":   {agent("ghost"), operatorCeiling(), nil, with()},
		"deployment-declaring step":       {deploy, operatorCeiling(), nil, with(runtimeauth.ActionDeploymentCall)},
		"ordinary step":                   {nil, operatorCeiling(), nil, with()},
		"unreadable source":               {agent("both"), brokenSource{}, nil, with()},
		"reported agent without a pin":    {nil, operatorCeiling(), kindOf(runtimeauth.StepAgent, nil), with()},
		"reported kind failing":           {deploy, operatorCeiling(), kindOf("", errors.New("down")), with()},
		"reported unknown kind":           {agent("both"), operatorCeiling(), kindOf("paused", nil), with()},
		"reported container with a pin":   {agent("both"), operatorCeiling(), kindOf(runtimeauth.StepContainer, nil), with()},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			reg := &registrar{file: filepath.Join(dir, CredentialFile)}
			issuer := ceilingIssuer(t, reg, tc.src)
			issuer.StepKind = tc.kind
			if err := issuer.Issue(context.Background(), specWith(t, tc.mutate), dir); err != nil {
				t.Fatal(err)
			}
			if got := reg.cmds[0].Actions.Strings(); !slices.Equal(got, tc.want) {
				t.Fatalf("registered %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("both an actions function and a ceiling is a configuration error", func(t *testing.T) {
		dir := t.TempDir()
		reg := &registrar{file: filepath.Join(dir, CredentialFile)}
		issuer := ceilingIssuer(t, reg, operatorCeiling())
		issuer.Actions = func(execution.Access) (runtimeauth.ActionSet, error) {
			return runtimeauth.NewActionSet(runtimeauth.DefaultActions(false)...)
		}
		if err := issuer.Issue(context.Background(), spec(t), dir); !errors.Is(err, execution.ErrNotConfigured) || len(reg.cmds) != 0 {
			t.Fatalf("%v, %d registrations", err, len(reg.cmds))
		}
	})
}
