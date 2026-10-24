package podman

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
)

// Program decides what an attempt's container runs. It is called once per
// container create and may provision files under the workload's directory.
type Program interface {
	Container(ctx context.Context, l Launch) (Container, error)
}

// Launch is the workload a Program is asked for.
type Launch struct {
	Spec     execution.AttemptSpec
	Context  runtimev1.Context
	Workload execution.Workload
	// Dir is the workload's host directory, removed with it.
	Dir string
}

// Container is what runs in the step's image.
type Container struct {
	Entrypoint []string
	Args       []string
	Env        []string
	Mounts     []Mount
	// Tmpfs lists writable in-memory mount points.
	Tmpfs []string
}

// Mount bind-mounts a host path.
type Mount struct {
	Source, Target string
	ReadOnly       bool
}

// DeclaredCommand runs the pinned context's command and arguments as the
// container's entrypoint, with the declared environment.
type DeclaredCommand struct{}

// Container implements Program.
func (DeclaredCommand) Container(_ context.Context, l Launch) (Container, error) {
	c := l.Context.Command
	if len(c.Command) == 0 {
		return Container{}, fmt.Errorf("%w: the pinned context declares no command", execution.ErrInvalid)
	}
	env := []string{"HOME=/tmp", "TMPDIR=/tmp"}
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		env = append(env, k+"="+c.Env[k])
	}
	return Container{Entrypoint: c.Command, Args: c.Args, Env: env, Tmpfs: []string{"/tmp"}}, nil
}

// The entrypoint's layout inside the container, as in a Kubernetes Pod.
const (
	pathTools     = "/kodama/bin"
	pathBootstrap = "/kodama/bootstrap"
)

// Entrypoint runs the real step entrypoint, `kodama step`, from a Linux
// kodama binary bind-mounted read-only into the step's image. The admission
// credential Issuer provisions and the runtime CA are mounted read-only;
// workspace, outputs, results, secrets, identity and scratch are in-memory.
// The container needs a network that reaches ServerURL.
//
// An attempt that consumes the workspace (access read or write) needs checkout
// material from Checkout, asked once per container create for the repository
// the attempt's pinned context names. The credential and the Git CA are then
// two more read-only files in the attempt's bootstrap mount, which is removed
// with the workload: never an environment variable, argument or image layer. A
// refusal, an outage, or no configured provider (bootstrap.ErrNoCheckout)
// creates no container and registers no admission credential. The publisher
// credential is never requested. An attempt without a workspace receives
// nothing and the provider is not consulted.
type Entrypoint struct {
	// Binary is the host path of a static Linux kodama binary for the
	// machine's architecture.
	Binary string
	// ServerURL is the runtime API origin as the container reaches it.
	ServerURL string
	// CA is PEM trust roots for the runtime API (optional).
	CA []byte
	// Issuer provisions and registers the admission credential.
	Issuer bootstrap.Issuer
	// Checkout issues the read-only checkout material of source-consuming
	// attempts. nil refuses them with bootstrap.ErrNoCheckout.
	Checkout bootstrap.CheckoutProvider
}

// Container implements Program.
func (e Entrypoint) Container(ctx context.Context, l Launch) (Container, error) {
	// Resolve the material first: a refusal must leave nothing behind, not even
	// a registered admission credential.
	checkout, err := bootstrap.IssueCheckout(ctx, e.Checkout, l.Spec, l.Context)
	if err != nil {
		return Container{}, err
	}
	dir := filepath.Join(l.Dir, "bootstrap")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Container{}, err
	}
	if err := e.Issuer.Issue(ctx, l.Spec, dir); err != nil {
		return Container{}, err
	}
	args := []string{
		"step", "--server-url=" + e.ServerURL, "--attempt-id=" + string(l.Spec.AttemptID),
		"--identity-dir=/kodama/identity", "--admission-credential-file=" + pathBootstrap + "/" + bootstrap.CredentialFile,
		"--workspace-dir=/workspace", "--outputs-dir=/kodama/outputs", "--secrets-dir=/kodama/secrets",
		"--results-file=/kodama/results/results.json", "--scratch-dir=/tmp",
	}
	if len(e.CA) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "ca.crt"), e.CA, 0o600); err != nil {
			return Container{}, err
		}
		args = append(args, "--ca-file="+pathBootstrap+"/ca.crt")
	}
	if checkout != nil {
		if err := bootstrap.WriteCheckout(dir, *checkout); err != nil {
			return Container{}, err
		}
		args = append(args,
			"--checkout-credential-file="+pathBootstrap+"/"+bootstrap.CheckoutCredentialFile,
			"--git-ca-file="+pathBootstrap+"/"+bootstrap.GitCAFile)
	}
	return Container{
		Entrypoint: []string{pathTools + "/kodama"},
		Args:       args,
		Env:        append(bootstrap.Env(l.Workload), "HOME=/tmp", "TMPDIR=/tmp"),
		Mounts: []Mount{
			{Source: e.Binary, Target: pathTools + "/kodama", ReadOnly: true},
			{Source: dir, Target: pathBootstrap, ReadOnly: true},
		},
		Tmpfs: []string{"/workspace", "/kodama/outputs", "/kodama/results", "/kodama/secrets", "/kodama/identity", "/tmp"},
	}, nil
}
