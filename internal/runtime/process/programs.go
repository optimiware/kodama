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
	return Command{Path: argv[0], Args: argv[1:], Env: env}, nil
}

// Entrypoint runs the real step entrypoint, `kodama step`, from a host
// binary. The entrypoint is admitted with the one-time credential Issuer
// provisions in the workload's identity directory, reads its pinned context,
// runs the declared command on the host and submits the result through the
// runtime API, as it does in a container. Its workspace, outputs, secrets,
// identity and scratch directories live in the workload's directory. An
// attempt that consumes the workspace is refused (bootstrap.ErrNoCheckout).
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
	if err := bootstrap.RequireNoCheckout(l.Context); err != nil {
		return Command{}, err
	}
	dir := func(name string) string { return filepath.Join(l.Dir, name) }
	for _, d := range []string{"identity", "workspace", "outputs", "secrets", "tmp"} {
		if err := os.MkdirAll(dir(d), 0o700); err != nil {
			return Command{}, err
		}
	}
	if err := e.Issuer.Issue(ctx, l.Spec, dir("identity")); err != nil {
		return Command{}, err
	}
	args := []string{
		"step", "--server-url", e.ServerURL, "--attempt-id", string(l.Spec.AttemptID),
		"--identity-dir", dir("identity"), "--workspace-dir", dir("workspace"), "--outputs-dir", dir("outputs"),
		"--secrets-dir", dir("secrets"), "--results-file", dir("results.json"), "--scratch-dir", dir("tmp"),
	}
	if e.CAFile != "" {
		args = append(args, "--ca-file", e.CAFile)
	}
	env := e.Env
	if env == nil {
		env = []string{"PATH=" + os.Getenv("PATH")}
	}
	return Command{Path: e.Binary, Args: args, Env: slices.Concat(env, bootstrap.Env(l.Workload))}, nil
}
