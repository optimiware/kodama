package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
)

const (
	checkoutRepo      = "https://git.example.test/org/repo.git"
	checkoutOtherRepo = "https://git.example.test/org/other.git"
	checkoutCommit    = "0123456789abcdef0123456789abcdef01234567"
	checkoutPassword  = "t101-checkout-PASSWORD-SENTINEL"
)

// checkoutAttempt is the committed attempt of a step whose pinned context has
// the given workspace access and source repository.
func checkoutAttempt(t *testing.T, access runtimev1.WorkspaceAccess, repo string) execution.AttemptSpec {
	t.Helper()
	s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), "", "true")
	c, err := s.DecodeContext()
	if err != nil {
		t.Fatal(err)
	}
	c.Command.Image = "registry.invalid/step@sha256:" + strings.Repeat("1", 64)
	switch access {
	case runtimev1.AccessRead:
		c.Workspace = runtimev1.Workspace{Access: access, Source: &runtimev1.Source{RepositoryURL: repo, Commit: checkoutCommit}}
	case runtimev1.AccessWrite:
		branch, tip := "refs/heads/kodama/work", runtimev1.GitCommit(checkoutCommit)
		c.Workspace = runtimev1.Workspace{Access: access, Source: &runtimev1.Source{RepositoryURL: repo, Commit: checkoutCommit, Branch: &branch, ExpectedTip: &tip}}
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

type countingRepos struct {
	fxRepos
	calls atomic.Int64
}

func (r *countingRepos) Repository(ctx context.Context, scope domain.Scope) (workspace.RepositoryConfig, error) {
	r.calls.Add(1)
	return r.fxRepos.Repository(ctx, scope)
}

type bootRegistrar struct {
	mu   sync.Mutex
	cmds []runtimeauth.RegisterCommand
}

func (r *bootRegistrar) Register(_ context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
	return runtimeauth.Registration{}, nil
}

func (r *bootRegistrar) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cmds)
}

// Test_app_podman_checkout_bound: the Podman runtime the core composes asks the
// composition's checkout provider (the project's repository and the
// installation's credential) for every source-consuming attempt, and a
// container gets the material of the attempt's pinned repository only.
func Test_app_podman_checkout_bound(t *testing.T) {
	ca := kubetest.CA(t)
	repos := &countingRepos{fxRepos: fxRepos{cfg: workspace.RepositoryConfig{URL: checkoutRepo, CheckoutSecretRef: "secret://installation/checkout"}}}
	provider := checkoutProvider{
		repos: repos, creds: fxCreds{cred: gitad.Credential{Username: "kodama-ro", Password: checkoutPassword}}, gitCA: ca,
	}
	launch := func(t *testing.T, p kube.CheckoutProvider, spec execution.AttemptSpec) (podman.Container, string, *bootRegistrar, error) {
		t.Helper()
		reg := &bootRegistrar{}
		prog := podmanProgram(RuntimeSettings{
			Config:    RuntimeConfig{EntrypointBinary: "/usr/bin/kodama", ServerURL: "https://runtime.example.test"},
			Registrar: reg, Checkout: p,
		}, nil)
		c, err := spec.DecodeContext()
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		got, err := prog.Container(context.Background(), podman.Launch{Spec: spec, Context: c, Workload: execution.Workload{Name: "kdm-t101", UID: "11111111-2222-4333-8444-555555555555"}, Dir: dir})
		return got, dir, reg, err
	}
	nothingLeft := func(t *testing.T, dir string, reg *bootRegistrar) {
		t.Helper()
		if reg.count() != 0 {
			t.Fatalf("%d admission credentials were registered for a refused attempt", reg.count())
		}
		for _, f := range []string{bootstrap.CheckoutCredentialFile, bootstrap.GitCAFile} {
			if _, err := os.Stat(filepath.Join(dir, "bootstrap", f)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s exists for a refused attempt: %v", f, err)
			}
		}
	}

	t.Run("a source-consuming attempt receives its pinned repository's material", func(t *testing.T) {
		for _, access := range []runtimev1.WorkspaceAccess{runtimev1.AccessRead, runtimev1.AccessWrite} {
			before := repos.calls.Load()
			c, dir, reg, err := launch(t, provider, checkoutAttempt(t, access, checkoutRepo))
			if err != nil {
				t.Fatalf("%s: %v", access, err)
			}
			if repos.calls.Load() != before+1 {
				t.Fatalf("%s: the provider was asked %d times", access, repos.calls.Load()-before)
			}
			cred, err := os.ReadFile(filepath.Join(dir, "bootstrap", bootstrap.CheckoutCredentialFile))
			if err != nil || string(cred) != "kodama-ro:"+checkoutPassword {
				t.Fatalf("%s: credential %q, %v", access, cred, err)
			}
			gotCA, err := os.ReadFile(filepath.Join(dir, "bootstrap", bootstrap.GitCAFile))
			if err != nil || string(gotCA) != string(ca) {
				t.Fatalf("%s: git CA %v", access, err)
			}
			if reg.count() != 1 {
				t.Fatalf("%s: %d registrations", access, reg.count())
			}
			args := strings.Join(c.Args, " ")
			if !strings.Contains(args, "--checkout-credential-file=/kodama/bootstrap/"+bootstrap.CheckoutCredentialFile) ||
				!strings.Contains(args, "--git-ca-file=/kodama/bootstrap/"+bootstrap.GitCAFile) {
				t.Fatalf("%s: the entrypoint is not pointed at the material: %s", access, args)
			}
			// Never an argument, environment variable or mount source.
			surfaces := append(slices.Clone(c.Args), c.Env...)
			for _, m := range c.Mounts {
				surfaces = append(surfaces, m.Source, m.Target)
			}
			if strings.Contains(strings.Join(surfaces, "\n"), checkoutPassword) {
				t.Fatalf("%s: the credential reached the container's command line or environment", access)
			}
		}
	})

	t.Run("an attempt pinned to another repository receives nothing", func(t *testing.T) {
		_, dir, reg, err := launch(t, provider, checkoutAttempt(t, runtimev1.AccessRead, checkoutOtherRepo))
		if !errors.Is(err, bootstrap.ErrCheckoutDenied) || !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("an attempt of another repository: %v", err)
		}
		nothingLeft(t, dir, reg)
	})

	t.Run("a refusal and an outage start nothing, and only an outage is retried", func(t *testing.T) {
		noRepo := provider
		noRepo.repos = fxRepos{err: workspace.ErrNotFound}
		_, dir, reg, err := launch(t, noRepo, checkoutAttempt(t, runtimev1.AccessRead, checkoutRepo))
		if !errors.Is(err, bootstrap.ErrCheckoutDenied) {
			t.Fatalf("a project without a repository: %v", err)
		}
		nothingLeft(t, dir, reg)

		outage := errors.New("the database is down")
		down := provider
		down.repos = fxRepos{err: outage}
		_, dir, reg, err = launch(t, down, checkoutAttempt(t, runtimev1.AccessRead, checkoutRepo))
		if !errors.Is(err, outage) || errors.Is(err, bootstrap.ErrCheckoutDenied) {
			t.Fatalf("an outage must stay an outage: %v", err)
		}
		nothingLeft(t, dir, reg)

		noCA := provider
		noCA.gitCA = nil
		_, dir, reg, err = launch(t, noCA, checkoutAttempt(t, runtimev1.AccessRead, checkoutRepo))
		if !errors.Is(err, bootstrap.ErrCheckoutDenied) {
			t.Fatalf("an installation without a Git CA: %v", err)
		}
		nothingLeft(t, dir, reg)
	})

	t.Run("a source-consuming attempt is refused when no provider is bound", func(t *testing.T) {
		if podmanCheckout(nil) != nil {
			t.Fatal("a nil provider became a non-nil adapter")
		}
		_, dir, reg, err := launch(t, nil, checkoutAttempt(t, runtimev1.AccessRead, checkoutRepo))
		if !errors.Is(err, bootstrap.ErrNoCheckout) {
			t.Fatalf("an attempt without a provider: %v", err)
		}
		nothingLeft(t, dir, reg)
	})

	t.Run("a workspace-free attempt neither consults the provider nor receives material", func(t *testing.T) {
		before := repos.calls.Load()
		c, dir, reg, err := launch(t, provider, checkoutAttempt(t, runtimev1.AccessNone, ""))
		if err != nil {
			t.Fatal(err)
		}
		if repos.calls.Load() != before {
			t.Fatal("the provider was consulted for a workspace-free attempt")
		}
		if reg.count() != 1 || strings.Contains(strings.Join(c.Args, " "), "checkout") {
			t.Fatalf("%d registrations, args %v", reg.count(), c.Args)
		}
		if _, err := os.Stat(filepath.Join(dir, "bootstrap", bootstrap.CheckoutCredentialFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a workspace-free attempt has a credential file: %v", err)
		}
	})

	t.Run("the Podman constructor binds the provider and the ceiling", func(t *testing.T) {
		cons, err := NewConstructors(Test)
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewCeilingBinding(ceilingConfig(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		set, err := cons.Runtime(RuntimeSettings{
			Config:    RuntimeConfig{StateDir: t.TempDir(), EntrypointBinary: "/usr/bin/kodama", ServerURL: "http://127.0.0.1:8080"},
			Registrar: &LateRegistrar{}, Checkout: provider, Ceiling: b,
		})
		if err != nil {
			t.Fatal(err)
		}
		if set.Runtime.Capabilities().Backend != podman.Backend || set.Authority == nil {
			t.Fatalf("test runtime: %+v", set.Runtime.Capabilities())
		}
	})
}
