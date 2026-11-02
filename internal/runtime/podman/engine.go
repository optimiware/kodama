package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/optimiware/kodama/internal/execution"
)

// engine runs podman commands for the run resources. It distinguishes what
// podman SAID (an exit code) from what could not be asked at all (the binary,
// the machine, the context): only the former is evidence. `podman X exists`
// exits 0 for present, 1 for absent and 125 or more for any failure, so an
// outage is never mistaken for absence.
type engine struct{ binary string }

// result is one finished podman command.
type result struct {
	stdout, stderr string
	code           int
}

func (r result) ok() bool { return r.code == 0 }

// run executes podman. The error is non-nil only when the command could not be
// run or was interrupted; a non-zero exit is reported in the result.
func (e engine) run(ctx context.Context, args ...string) (result, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.binary, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return result{stdout: stdout.String(), stderr: stderr.String()}, nil
	case ctx.Err() != nil:
		return result{}, ctx.Err()
	case errors.As(err, &exit):
		return result{stdout: stdout.String(), stderr: stderr.String(), code: exit.ExitCode()}, nil
	}
	return result{}, fmt.Errorf("%w: podman %s: %w", execution.ErrUnavailable, args[0], err)
}

// must runs podman and fails on a non-zero exit with its (trimmed) message.
func (e engine) must(ctx context.Context, args ...string) (result, error) {
	res, err := e.run(ctx, args...)
	if err != nil {
		return res, err
	}
	if !res.ok() {
		return res, fmt.Errorf("%w: podman %s: %s", execution.ErrUnavailable, args[0], strings.TrimSpace(res.stderr))
	}
	return res, nil
}

// exists asks whether a volume, network or container exists. Only exit codes 0
// and 1 are answers.
func (e engine) exists(ctx context.Context, kind, ref string) (bool, error) {
	res, err := e.run(ctx, kind, "exists", ref)
	if err != nil {
		return false, err
	}
	switch res.code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, fmt.Errorf("%w: podman %s exists: %s", execution.ErrUnavailable, kind, strings.TrimSpace(res.stderr))
}

// volume is the part of `podman volume inspect` the adapter decides on.
type volume struct {
	Name      string            `json:"Name"`
	Labels    map[string]string `json:"Labels"`
	CreatedAt string            `json:"CreatedAt"`
}

// network is the part of `podman network inspect` the adapter decides on.
type network struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Labels   map[string]string `json:"labels"`
	Internal bool              `json:"internal"`
}

// inspectOne decodes the single-element array `podman <kind> inspect` prints;
// found is false only when podman definitely has no such object.
func inspectOne[T any](ctx context.Context, e engine, kind, ref string) (v T, found bool, err error) {
	ok, err := e.exists(ctx, kind, ref)
	if err != nil || !ok {
		return v, false, err
	}
	res, err := e.must(ctx, kind, "inspect", "--format", "json", ref)
	if err != nil {
		return v, false, err
	}
	var all []T
	if err := json.Unmarshal([]byte(res.stdout), &all); err != nil || len(all) != 1 {
		return v, false, fmt.Errorf("%w: unreadable podman %s inspect output", execution.ErrUnavailable, kind)
	}
	return all[0], true, nil
}

func (e engine) volume(ctx context.Context, name string) (volume, bool, error) {
	return inspectOne[volume](ctx, e, "volume", name)
}

func (e engine) network(ctx context.Context, name string) (network, bool, error) {
	return inspectOne[network](ctx, e, "network", name)
}

func (e engine) container(ctx context.Context, ref string) (container, bool, error) {
	return inspectOne[container](ctx, e, "container", ref)
}

// holders returns the state of every container that references the volume (by
// name), running or not. An empty result is an answer only when podman said so.
func (e engine) holders(ctx context.Context, volumeName string) (map[string]string, error) {
	res, err := e.must(ctx, "ps", "--all", "--no-trunc", "--filter", "volume="+volumeName, "--format", "{{.ID}} {{.State}}")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(res.stdout), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		id, state, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("%w: unreadable podman ps output", execution.ErrUnavailable)
		}
		out[id] = strings.ToLower(strings.TrimSpace(state))
	}
	return out, nil
}

// usesVolume reports whether a container in this state can still use its
// volumes: only a container that has exited cannot.
func usesVolume(state string) bool {
	switch state {
	case "exited", "stopped", "dead":
		return false
	}
	return true
}
