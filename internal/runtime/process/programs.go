package process

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
)

// DeclaredCommand runs the pinned context's command and arguments on the host,
// in the workload's directory, with the declared environment over PATH. It
// never looks at the image: it is a host fixture standing in for one.
type DeclaredCommand struct{}

// Command implements Program.
func (DeclaredCommand) Command(_ context.Context, l Launch) (Command, error) {
	argv := slices.Concat(l.Context.Command.Command, l.Context.Command.Args)
	if len(argv) == 0 {
		return Command{}, fmt.Errorf("%w: %w", execution.ErrInvalid, errNoCommand)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.Dir, "TMPDIR=" + l.Dir}
	for _, k := range slices.Sorted(maps.Keys(l.Context.Command.Env)) {
		env = append(env, k+"="+l.Context.Command.Env[k])
	}
	cmd := Command{Path: argv[0], Args: argv[1:], Env: env}
	if l.Workspace != "" {
		// A run_shared attempt runs in the run workspace, as the entrypoint runs
		// its command in /workspace.
		cmd.Dir = l.Workspace
		cmd.Env = append(cmd.Env, "KODAMA_WORKSPACE_DIR="+l.Workspace)
	}
	return cmd, nil
}

// Entrypoint runs the real step entrypoint, `kodama step`, from a host
// binary. The entrypoint is admitted with the one-time credential Issuer
// provisions in the workload's identity directory, reads its pinned context,
// runs the declared command on the host and submits the result through the
// runtime API, as it does in a container. Its workspace, outputs, secrets,
// identity and scratch directories live in the workload's directory. An
// attempt that consumes the workspace is refused (bootstrap.ErrNoCheckout),
// unless it is a run_shared attempt: that one runs against the run workspace
// the preparation filled, selects the v2 wire (KODAMA_RUNTIME_API=v2) and never
// receives a checkout credential.
type Entrypoint struct {
	// Binary is the host kodama binary.
	Binary string
	// ServerURL is the runtime API origin and CAFile optional extra PEM roots.
	ServerURL string
	CAFile    string
	// Issuer provisions and registers the admission credential.
	Issuer bootstrap.Issuer
	// Env is the entrypoint's base environment (default: PATH).
	Env []string
}

// Command implements Program.
func (e Entrypoint) Command(ctx context.Context, l Launch) (Command, error) {
	shared := l.Spec.Shared != nil
	if !shared {
		if err := bootstrap.RequireNoCheckout(l.Context); err != nil {
			return Command{}, err
		}
	} else if l.Workspace == "" {
		return Command{}, fmt.Errorf("%w: a run_shared attempt has no run workspace", execution.ErrConflict)
	}
	dir := func(name string) string { return filepath.Join(l.Dir, name) }
	private := []string{"identity", "outputs", "secrets", "tmp"}
	if !shared {
		private = append(private, "workspace")
	}
	for _, d := range private {
		if err := os.MkdirAll(dir(d), 0o700); err != nil {
			return Command{}, err
		}
	}
	workspace := dir("workspace")
	if shared {
		workspace = l.Workspace
	}
	if err := e.Issuer.Issue(ctx, l.Spec, dir("identity")); err != nil {
		return Command{}, err
	}
	args := []string{
		"step", "--server-url", e.ServerURL, "--attempt-id", string(l.Spec.AttemptID),
		"--identity-dir", dir("identity"), "--workspace-dir", workspace, "--outputs-dir", dir("outputs"),
		"--secrets-dir", dir("secrets"), "--results-file", dir("results.json"), "--scratch-dir", dir("tmp"),
	}
	if e.CAFile != "" {
		args = append(args, "--ca-file", e.CAFile)
	}
	env := e.Env
	if env == nil {
		env = []string{"PATH=" + os.Getenv("PATH")}
	}
	env = slices.Concat(env, bootstrap.Env(l.Workload))
	if shared {
		env = append(env, "KODAMA_RUNTIME_API=v2", "KODAMA_WORKSPACE_DIR="+workspace)
	}
	return Command{Path: e.Binary, Args: args, Env: env}, nil
}

// DeclaredService runs a service's declared command and arguments on the host,
// in the service's private directory, with the declared nonsecret environment
// and the workload's identity environment over PATH. It never looks at the
// image: it is a host fixture standing in for one. A workload that is not a
// service is refused; a preparation is an explicit program of the test.
type DeclaredService struct{}

// Command implements ResourceProgram.
func (DeclaredService) Command(_ context.Context, l ResourceLaunch) (Command, error) {
	if l.Spec.Service == nil {
		return Command{}, fmt.Errorf("%w: %s is not a service", execution.ErrInvalid, l.Spec.Key)
	}
	s := l.Spec.Service
	argv := slices.Concat(s.Command, s.Args)
	if len(argv) == 0 {
		return Command{}, fmt.Errorf("%w: the service declares no command", execution.ErrInvalid)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.Dir, "TMPDIR=" + l.Dir}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		env = append(env, k+"="+s.Env[k])
	}
	return Command{Path: argv[0], Args: argv[1:], Env: slices.Concat(env, l.Env)}, nil
}
