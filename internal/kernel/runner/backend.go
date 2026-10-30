package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
)

// Process runs steps directly on the host. It is the development backend: it
// proves orchestration and workspace sharing, never isolation.
type Process struct{ Root string }

func (p Process) Name() string { return "process" }

func (p Process) Prepare(_ context.Context, scope domain.Scope, runID string) (string, func(context.Context) error, error) {
	root := filepath.Join(p.Root, string(scope.TenantID)+"_"+string(scope.ProjectID)+"_"+runID)
	ws := filepath.Join(root, "workspace")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		return "", nil, err
	}
	return ws, func(context.Context) error { return os.RemoveAll(root) }, nil
}

func (p Process) Execute(ctx context.Context, spec Spec, log io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-ec", shellCommands(spec.Commands))
	cmd.Dir = spec.Workspace
	cmd.Env = append(os.Environ(), "KODAMA_SECRETS_DIR="+spec.SecretsDir)
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = log, log
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ctx.Err() != nil {
			return exit.ExitCode(), ctx.Err()
		}
		return exit.ExitCode(), nil
	}
	return 0, err
}

// Podman runs steps in containers sharing one named volume per run. The image
// must provide /bin/sh; digest pinning is enforced at admission.
type Podman struct {
	Root   string
	Binary string
}

func (p Podman) Name() string { return "podman" }

func (p Podman) bin() string {
	if p.Binary != "" {
		return p.Binary
	}
	return "podman"
}

func (p Podman) volume(scope domain.Scope, runID string) string {
	return "kodama-" + sanitize(string(scope.TenantID)) + "-" + sanitize(runID)
}

func (p Podman) Prepare(ctx context.Context, scope domain.Scope, runID string) (string, func(context.Context) error, error) {
	vol := p.volume(scope, runID)
	out, err := exec.CommandContext(ctx, p.bin(), "volume", "create", vol).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("podman volume create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	cleanup := func(cctx context.Context) error {
		out, err := exec.CommandContext(cctx, p.bin(), "volume", "rm", "-f", vol).CombinedOutput()
		if err != nil {
			return fmt.Errorf("podman volume rm: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return vol, cleanup, nil
}

func (p Podman) Execute(ctx context.Context, spec Spec, log io.Writer) (int, error) {
	name := "kodama-" + sanitize(spec.RunID) + "-" + sanitize(spec.StepKey)
	args := []string{"run", "--rm", "--name", name,
		"-v", spec.Workspace + ":/workspace", "-w", "/workspace"}
	if len(spec.SecretsDir) > 0 {
		if _, err := os.Stat(spec.SecretsDir); err == nil {
			args = append(args, "-v", spec.SecretsDir+":/kodama/secrets:ro")
		}
	}
	if os.Getenv("KODAMA_PODMAN_NETWORK") != "" {
		args = append(args, "--network", os.Getenv("KODAMA_PODMAN_NETWORK"))
	}
	for k, v := range spec.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, "-e", "KODAMA_SECRETS_DIR=/kodama/secrets")
	args = append(args, spec.Image, "/bin/sh", "-ec", shellCommands(spec.Commands))

	cmd := exec.CommandContext(ctx, p.bin(), args...)
	cmd.Stdout, cmd.Stderr = log, log
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ctx.Err() != nil {
			return exit.ExitCode(), ctx.Err()
		}
		return exit.ExitCode(), nil
	}
	return 0, err
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
