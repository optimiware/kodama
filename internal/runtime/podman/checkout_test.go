package podman_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

const (
	pinnedRepo   = "https://git.example.test/org/repo.git"
	pinnedCommit = "0123456789abcdef0123456789abcdef01234567"
	tipCommit    = "fedcba9876543210fedcba9876543210fedcba98"
	checkoutPass = "s3cr3t-checkout-password-0001"
	checkoutUser = "kodama-ro"
)

// sourceSpec is a planned spec whose attempt has the given workspace access.
func sourceSpec(t *testing.T, access runtimev1.WorkspaceAccess) execution.AttemptSpec {
	t.Helper()
	s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), "", "true")
	c, err := s.DecodeContext()
	if err != nil {
		t.Fatal(err)
	}
	c.Command.Image = "registry.invalid/step@sha256:" + strings.Repeat("1", 64)
	switch access {
	case runtimev1.AccessRead:
		c.Workspace = runtimev1.Workspace{Access: access, Source: &runtimev1.Source{RepositoryURL: pinnedRepo, Commit: pinnedCommit}}
	case runtimev1.AccessWrite:
		branch, tip := "refs/heads/kodama/work", runtimev1.GitCommit(tipCommit)
		c.Workspace = runtimev1.Workspace{Access: access, Source: &runtimev1.Source{RepositoryURL: pinnedRepo, Commit: pinnedCommit, Branch: &branch, ExpectedTip: &tip}}
	}
	plan, err := execution.DefaultPlanner{}.Plan(s.Scope.TenantID, c)
	if err != nil {
		t.Fatal(err)
	}
	s.WorkloadName, s.SpecFingerprint, s.ContextDigest, s.Context = plan.WorkloadName, plan.SpecFingerprint, plan.ContextDigest, plan.Context
	s.Access = execution.Access(access)
	if access != runtimev1.AccessNone {
		s.WorkspaceID = "ws_" + string(s.AttemptID)
	}
	return s
}

// recordingProvider is a scriptable bootstrap.CheckoutProvider.
type recordingProvider struct {
	mu     sync.Mutex
	reqs   []bootstrap.CheckoutRequest
	answer func(bootstrap.CheckoutRequest) (bootstrap.CheckoutMaterial, error)
}

func (p *recordingProvider) Checkout(_ context.Context, req bootstrap.CheckoutRequest) (bootstrap.CheckoutMaterial, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	return p.answer(req)
}

func (p *recordingProvider) requests() []bootstrap.CheckoutRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.reqs)
}

// issuing answers for exactly the requested source, as the composition's
// provider does for the project's configured repository.
func issuing(ca []byte) *recordingProvider {
	return &recordingProvider{answer: func(r bootstrap.CheckoutRequest) (bootstrap.CheckoutMaterial, error) {
		return bootstrap.CheckoutMaterial{
			Scope: r.Scope, WorkspaceID: r.WorkspaceID, RepositoryURL: r.RepositoryURL,
			Credential: []byte(checkoutUser + ":" + checkoutPass), GitCA: ca,
		}, nil
	}}
}

type registrar struct {
	mu   sync.Mutex
	cmds []runtimeauth.RegisterCommand
}

func (r *registrar) Register(_ context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
	return runtimeauth.Registration{}, nil
}

func (r *registrar) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cmds)
}

type checkoutEnv struct {
	fake  fakeEnv
	state string
	reg   *registrar
	rt    *podman.Runtime
}

func newCheckoutEnv(t *testing.T, p bootstrap.CheckoutProvider) checkoutEnv {
	t.Helper()
	fake := newFake(t)
	reg := &registrar{}
	state := t.TempDir()
	r, err := podman.New(podman.Config{
		Binary: fake.Binary, StateDir: state, StopTimeout: time.Second,
		Program: podman.Entrypoint{
			Binary: "/host/kodama", ServerURL: "https://runtime.invalid", Issuer: bootstrap.Issuer{Registrar: reg}, Checkout: p,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return checkoutEnv{fake: fake, state: state, reg: reg, rt: r}
}

func (e checkoutEnv) bootstrapDir(s execution.AttemptSpec) string {
	return filepath.Join(e.state, s.WorkloadName, "bootstrap")
}

// leaks reports where the secret appears in a created container's command line
// and environment.
func (c createdContainer) leaks(secret string) bool {
	all := append(append(slices.Clone(c.Env), c.Args...), c.Entrypoint)
	all = append(all, c.Volumes...)
	all = append(all, c.Tmpfs...)
	for k, v := range c.Labels {
		all = append(all, k, v)
	}
	return strings.Contains(strings.Join(all, "\x00"), secret)
}

func hasFlag(args []string, prefix string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, prefix) })
}

// Test_podman_checkout_issued: a read and a write attempt receive the
// credential and the Git CA as read-only mounted files of the attempt, not as
// environment variables or arguments; a workspace-free attempt receives nothing
// and does not consult the provider.
func Test_podman_checkout_issued(t *testing.T) {
	ctx := context.Background()
	ca := testCA(t)

	for _, access := range []runtimev1.WorkspaceAccess{runtimev1.AccessRead, runtimev1.AccessWrite} {
		t.Run(string(access), func(t *testing.T) {
			prov := issuing(ca)
			e := newCheckoutEnv(t, prov)
			s := sourceSpec(t, access)
			w, err := e.rt.Start(ctx, s)
			if err != nil {
				t.Fatal(err)
			}

			reqs := prov.requests()
			if len(reqs) != 1 {
				t.Fatalf("the provider was asked %d times", len(reqs))
			}
			want := bootstrap.CheckoutRequest{
				Scope: s.Scope, RunID: s.RunID, StepID: s.StepID, AttemptID: s.AttemptID, WorkspaceID: s.WorkspaceID,
				Access: execution.Access(access), RepositoryURL: pinnedRepo, Commit: pinnedCommit,
			}
			if reqs[0] != want {
				t.Fatalf("request %+v, want %+v", reqs[0], want)
			}

			cs := e.fake.created(t)
			if len(cs) != 1 {
				t.Fatalf("%d containers created", len(cs))
			}
			c := cs[0]
			dir := e.bootstrapDir(s)
			if !slices.Contains(c.Volumes, dir+":/kodama/bootstrap:ro") {
				t.Fatalf("the bootstrap directory is not a read-only mount: %v", c.Volumes)
			}
			for _, v := range c.Volumes {
				if strings.Contains(v, bootstrap.CheckoutCredentialFile) && !strings.HasSuffix(v, ":ro") {
					t.Fatalf("a writable checkout mount: %q", v)
				}
			}
			for file, want := range map[string]string{
				bootstrap.CheckoutCredentialFile: checkoutUser + ":" + checkoutPass,
				bootstrap.GitCAFile:              string(ca),
			} {
				got, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil || string(got) != want {
					t.Fatalf("%s: %q %v", file, got, err)
				}
				if fi, err := os.Stat(filepath.Join(dir, file)); err != nil || fi.Mode().Perm() != 0o600 {
					t.Fatalf("%s mode: %v %v", file, fi, err)
				}
			}
			for _, flag := range []string{
				"--checkout-credential-file=/kodama/bootstrap/checkout-credential", "--git-ca-file=/kodama/bootstrap/git-ca.crt",
			} {
				if !slices.Contains(c.Args, flag) {
					t.Errorf("missing %s in %v", flag, c.Args)
				}
			}
			for _, secret := range []string{checkoutPass, checkoutUser + ":" + checkoutPass, "BEGIN CERTIFICATE"} {
				if c.leaks(secret) {
					t.Errorf("%q reached the container's command line, environment, mounts or labels", secret)
				}
			}
			for _, env := range c.Env {
				if strings.Contains(strings.ToLower(env), "checkout") || strings.Contains(strings.ToLower(env), "git") {
					t.Errorf("checkout material in the environment: %s", env)
				}
			}
			if e.reg.count() != 1 {
				t.Fatalf("admission registered %d times", e.reg.count())
			}

			// A restart adopts the container and never asks the provider again.
			if again, err := e.rt.Start(ctx, s); err != nil || again != w {
				t.Fatalf("restart: %+v %v", again, err)
			}
			if n := len(prov.requests()); n != 1 {
				t.Fatalf("an adopted workload consulted the provider again (%d)", n)
			}

			// The material lives and dies with the attempt's workload directory.
			if err := e.rt.Remove(ctx, w); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{bootstrap.CheckoutCredentialFile, bootstrap.GitCAFile} {
				if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s survives the workload: %v", f, err)
				}
			}
		})
	}

	t.Run("a workspace-free attempt receives nothing", func(t *testing.T) {
		prov := issuing(ca)
		e := newCheckoutEnv(t, prov)
		s := sourceSpec(t, runtimev1.AccessNone)
		if _, err := e.rt.Start(ctx, s); err != nil {
			t.Fatal(err)
		}
		if n := len(prov.requests()); n != 0 {
			t.Fatalf("the provider was consulted %d times", n)
		}
		c := e.fake.created(t)[0]
		if hasFlag(c.Args, "--checkout-credential-file") || hasFlag(c.Args, "--git-ca-file") {
			t.Fatalf("checkout flags without a workspace: %v", c.Args)
		}
		for _, f := range []string{bootstrap.CheckoutCredentialFile, bootstrap.GitCAFile} {
			if _, err := os.Stat(filepath.Join(e.bootstrapDir(s), f)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s delivered to a workspace-free attempt: %v", f, err)
			}
		}
	})

	t.Run("a workspace-free attempt needs no provider", func(t *testing.T) {
		e := newCheckoutEnv(t, nil)
		if _, err := e.rt.Start(ctx, sourceSpec(t, runtimev1.AccessNone)); err != nil {
			t.Fatal(err)
		}
	})
}

// Test_podman_checkout_refuses_foreign_source: a provider refusal, including
// for a repository other than the pinned one, starts no container, registers no
// admission credential and leaves no material; an outage is retryable and
// recovers on the redrive.
func Test_podman_checkout_refuses_foreign_source(t *testing.T) {
	ctx := context.Background()
	ca := testCA(t)

	noEffect := func(t *testing.T, e checkoutEnv, s execution.AttemptSpec) {
		t.Helper()
		if slices.Contains(e.fake.calls(t), "create") || len(e.fake.created(t)) != 0 {
			t.Fatalf("a container was created: %v", e.fake.calls(t))
		}
		if e.reg.count() != 0 {
			t.Fatalf("an admission credential was registered")
		}
		for _, f := range []string{bootstrap.CheckoutCredentialFile, bootstrap.GitCAFile} {
			if _, err := os.Stat(filepath.Join(e.bootstrapDir(s), f)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s was stored: %v", f, err)
			}
		}
	}

	// foreign is the composition's provider rule: it answers only for the
	// project's repository, which is not the one this attempt pins.
	foreign := &recordingProvider{answer: func(r bootstrap.CheckoutRequest) (bootstrap.CheckoutMaterial, error) {
		const configured = "https://git.example.test/org/other.git"
		if r.RepositoryURL != configured {
			return bootstrap.CheckoutMaterial{}, fmt.Errorf("%w: the repository is not the attempt's pinned source", bootstrap.ErrCheckoutDenied)
		}
		return bootstrap.CheckoutMaterial{}, nil
	}}

	for _, access := range []runtimev1.WorkspaceAccess{runtimev1.AccessRead, runtimev1.AccessWrite} {
		t.Run("a provider refusal for "+string(access), func(t *testing.T) {
			e := newCheckoutEnv(t, foreign)
			s := sourceSpec(t, access)
			if _, err := e.rt.Start(ctx, s); !errors.Is(err, bootstrap.ErrCheckoutDenied) {
				t.Fatalf("start: %v", err)
			}
			noEffect(t, e, s)
		})
	}

	answers := map[string]func(r bootstrap.CheckoutRequest) bootstrap.CheckoutMaterial{
		"another repository": func(r bootstrap.CheckoutRequest) bootstrap.CheckoutMaterial {
			m := goodMaterial(r, ca)
			m.RepositoryURL = "https://git.example.test/org/other.git"
			return m
		},
		"another workspace": func(r bootstrap.CheckoutRequest) bootstrap.CheckoutMaterial {
			m := goodMaterial(r, ca)
			m.WorkspaceID = "ws_other"
			return m
		},
		"another tenant": func(r bootstrap.CheckoutRequest) bootstrap.CheckoutMaterial {
			m := goodMaterial(r, ca)
			m.Scope.TenantID = "t_other"
			return m
		},
		"a credential that is not a username:password line": func(r bootstrap.CheckoutRequest) bootstrap.CheckoutMaterial {
			m := goodMaterial(r, ca)
			m.Credential = []byte("token-without-user")
			return m
		},
		"a credential with a trailing newline": func(r bootstrap.CheckoutRequest) bootstrap.CheckoutMaterial {
			m := goodMaterial(r, ca)
			m.Credential = []byte(checkoutUser + ":" + checkoutPass + "\n")
			return m
		},
		"no Git CA": func(r bootstrap.CheckoutRequest) bootstrap.CheckoutMaterial {
			m := goodMaterial(r, ca)
			m.GitCA = nil
			return m
		},
		"a private key as the Git CA": func(r bootstrap.CheckoutRequest) bootstrap.CheckoutMaterial {
			m := goodMaterial(r, ca)
			m.GitCA = []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")
			return m
		},
	}
	for name, answer := range answers {
		t.Run("an answer with "+name, func(t *testing.T) {
			p := &recordingProvider{answer: func(r bootstrap.CheckoutRequest) (bootstrap.CheckoutMaterial, error) { return answer(r), nil }}
			e := newCheckoutEnv(t, p)
			s := sourceSpec(t, runtimev1.AccessRead)
			if _, err := e.rt.Start(ctx, s); !errors.Is(err, bootstrap.ErrCheckoutDenied) {
				t.Fatalf("start: %v", err)
			}
			noEffect(t, e, s)
		})
	}

	t.Run("an outage is not a refusal and recovers on the redrive", func(t *testing.T) {
		var down = true
		p := &recordingProvider{}
		p.answer = func(r bootstrap.CheckoutRequest) (bootstrap.CheckoutMaterial, error) {
			if down {
				return bootstrap.CheckoutMaterial{}, errors.New("dial git-credentials: password=" + checkoutPass)
			}
			return goodMaterial(r, ca), nil
		}
		e := newCheckoutEnv(t, p)
		s := sourceSpec(t, runtimev1.AccessRead)
		_, err := e.rt.Start(ctx, s)
		switch {
		case !errors.Is(err, execution.ErrUnavailable) || errors.Is(err, bootstrap.ErrCheckoutDenied):
			t.Fatalf("an outage: %v", err)
		case strings.Contains(err.Error(), checkoutPass):
			t.Fatalf("the provider's error text leaked: %v", err)
		}
		noEffect(t, e, s)
		down = false
		if _, err := e.rt.Start(ctx, s); err != nil {
			t.Fatalf("redrive: %v", err)
		}
		if len(e.fake.created(t)) != 1 {
			t.Fatal("the redrive created no container")
		}
	})
}

func goodMaterial(r bootstrap.CheckoutRequest, ca []byte) bootstrap.CheckoutMaterial {
	return bootstrap.CheckoutMaterial{
		Scope: r.Scope, WorkspaceID: r.WorkspaceID, RepositoryURL: r.RepositoryURL,
		Credential: []byte(checkoutUser + ":" + checkoutPass), GitCA: ca,
	}
}

// Test_podman_checkout_unconfigured_refuses: a runtime with no provider still
// refuses a source-consuming attempt with ErrNoCheckout and starts nothing.
func Test_podman_checkout_unconfigured_refuses(t *testing.T) {
	ctx := context.Background()
	for _, access := range []runtimev1.WorkspaceAccess{runtimev1.AccessRead, runtimev1.AccessWrite} {
		t.Run(string(access), func(t *testing.T) {
			e := newCheckoutEnv(t, nil)
			s := sourceSpec(t, access)
			if _, err := e.rt.Start(ctx, s); !errors.Is(err, bootstrap.ErrNoCheckout) || !errors.Is(err, execution.ErrNotConfigured) {
				t.Fatalf("start: %v", err)
			}
			if slices.Contains(e.fake.calls(t), "create") || e.reg.count() != 0 {
				t.Fatalf("an unconfigured refusal had effects: %v, %d", e.fake.calls(t), e.reg.count())
			}
			if _, err := os.Stat(filepath.Join(e.bootstrapDir(s), bootstrap.CredentialFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("an admission credential was provisioned: %v", err)
			}
		})
	}
}
