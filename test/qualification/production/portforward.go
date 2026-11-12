//go:build qualification

package production

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// defaultPortForwardGrace is how long Close waits for a TERM to take effect
// before killing the port-forward.
const defaultPortForwardGrace = 5 * time.Second

// PortForward is one joined kubectl port-forward child. Its output is discarded
// so a subprocess diagnostic can never reach test or evidence output.
type PortForward struct {
	Grace time.Duration

	cmd       *exec.Cmd
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// StartPortForward starts one guarded `kubectl port-forward svc/<service>`.
// Readiness is the caller's responsibility; Close always joins and kills.
func StartPortForward(ctx context.Context, path string, guard *Guard, namespace, service string, localPort, remotePort int) (*PortForward, error) {
	if guard == nil {
		return nil, fmt.Errorf("port-forward has no confinement guard")
	}
	if path == "" {
		path = "kubectl"
	}
	if err := guard.CheckExecutable(path); err != nil {
		return nil, err
	}
	if !dnsLabelPattern.MatchString(service) {
		return nil, fmt.Errorf("service %q is not a kubernetes service name", service)
	}
	if localPort < 1 || localPort > 65535 || remotePort < 1 || remotePort > 65535 {
		return nil, fmt.Errorf("port-forward ports must be in 1..65535")
	}
	if err := guard.Admit(Access{
		Context:   guard.Context(),
		Namespace: namespace,
		Verb:      "port-forward",
		Resource:  "services",
	}); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(
		runCtx,
		path,
		"--context", guard.Context(),
		"--namespace", namespace,
		"port-forward",
		"svc/"+service,
		fmt.Sprintf("%d:%d", localPort, remotePort),
	)
	command.Env = safeChildEnvironment()
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Signal(syscall.SIGTERM)
	}
	command.WaitDelay = defaultPortForwardGrace
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start kubectl port-forward: %s", safeExitStatus(err))
	}
	forward := &PortForward{
		Grace:  defaultPortForwardGrace,
		cmd:    command,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() {
		_ = command.Wait()
		close(forward.done)
	}()
	return forward, nil
}

// Done reports when the port-forward process has exited and been reaped.
func (forward *PortForward) Done() <-chan struct{} {
	if forward == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return forward.done
}

// PID returns the child process id, or zero before it starts.
func (forward *PortForward) PID() int {
	if forward == nil || forward.cmd == nil || forward.cmd.Process == nil {
		return 0
	}
	return forward.cmd.Process.Pid
}

// Close sends TERM, waits for the process to exit, escalates to KILL after the
// grace bound, and joins the wait. It is idempotent.
func (forward *PortForward) Close() error {
	if forward == nil {
		return nil
	}
	forward.closeOnce.Do(func() {
		if forward.cancel != nil {
			forward.cancel()
		}
		if forward.done == nil {
			return
		}
		grace := forward.Grace
		if grace <= 0 {
			grace = defaultPortForwardGrace
		}
		select {
		case <-forward.done:
			return
		case <-time.After(grace):
		}
		if forward.cmd != nil && forward.cmd.Process != nil {
			_ = forward.cmd.Process.Kill()
		}
		select {
		case <-forward.done:
		case <-time.After(defaultPortForwardGrace):
			forward.closeErr = fmt.Errorf("kubectl port-forward did not exit after KILL")
		}
	})
	return forward.closeErr
}
