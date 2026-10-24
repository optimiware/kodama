package kit

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/runtime/podman"
)

// Container is one workload container as Podman reports it.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

// Attempt is the attempt the container carries.
func (c Container) Attempt() string { return c.Labels[podman.LabelAttempt] }

func (s *Stack) podman(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Containers lists the runtime's containers (any state) for the tenant, or for
// one attempt when attempt is not empty.
func (s *Stack) Containers(tenant, attempt string) []Container {
	s.T.Helper()
	args := []string{"ps", "-a", "--format", "json", "--filter", "label=" + podman.LabelManagedBy + "=" + podman.ValueManagedBy, "--filter", "label=" + podman.LabelTenant + "=" + tenant}
	if attempt != "" {
		args = append(args, "--filter", "label="+podman.LabelAttempt+"="+attempt)
	}
	out, err := s.podman(args...)
	if err != nil {
		s.T.Fatalf("podman ps: %v: %s", err, out)
	}
	var cs []Container
	if out != "" {
		if err := json.Unmarshal([]byte(out), &cs); err != nil {
			s.T.Fatalf("podman ps output: %v: %s", err, out)
		}
	}
	return cs
}

// Exec runs a command in a running container.
func (s *Stack) Exec(container string, args ...string) (string, error) {
	return s.podman(append([]string{"exec", container}, args...)...)
}

// Inspect returns `podman inspect` of the container.
func (s *Stack) Inspect(container string) map[string]any {
	s.T.Helper()
	out, err := s.podman("inspect", container)
	if err != nil {
		s.T.Fatalf("podman inspect: %v: %s", err, out)
	}
	var docs []map[string]any
	if err := json.Unmarshal([]byte(out), &docs); err != nil || len(docs) != 1 {
		s.T.Fatalf("podman inspect output: %v: %s", err, out)
	}
	return docs[0]
}

// WaitContainer waits for the attempt's container to be in state.
func (s *Stack) WaitContainer(tenant, attempt, state string, within time.Duration) Container {
	s.T.Helper()
	var found Container
	WaitFor(s.T, "the container of "+attempt+" to be "+state, within, func() bool {
		for _, c := range s.Containers(tenant, attempt) {
			if c.State == state {
				found = c
				return true
			}
		}
		return false
	})
	return found
}

// WaitAttempt waits until the run's first step has an attempt and returns it.
func (w *World) WaitAttempt(run string, within time.Duration) string {
	w.S.T.Helper()
	var id string
	WaitFor(w.S.T, "an attempt of "+run, within, func() bool {
		if a := w.Attempts(run); len(a) > 0 {
			id = a[0].Id
			return true
		}
		return false
	})
	return id
}

// RuntimeToken reads the bearer token a running step entrypoint holds, the way
// only an operator with access to the container can. The token is the workload's
// own authority: using it from outside is how the result guard is exercised.
func (s *Stack) RuntimeToken(container string) string {
	s.T.Helper()
	var tok string
	WaitFor(s.T, "the entrypoint's runtime token", 60*time.Second, func() bool {
		out, err := s.Exec(container, "cat", "/kodama/identity/token")
		if err != nil || strings.TrimSpace(out) == "" {
			return false
		}
		tok = strings.TrimSpace(out)
		return true
	})
	return tok
}

// PodmanRun runs a throwaway command in the pinned image on the workload network.
func (s *Stack) PodmanRun(cmd ...string) (string, error) {
	args := []string{"run", "--rm", "--network", s.podmanNet, PodmanImage}
	return s.podman(append(args, cmd...)...)
}

// PodmanLogs returns a container's output.
func (s *Stack) PodmanLogs(container string) (string, error) { return s.podman("logs", container) }

// recordContainerLogs keeps the last output of every workload container of the
// installation's tenants while it exists, and prints it when the test fails: a
// container is removed after its attempt settles, so its log is otherwise gone
// by the time a failure is looked at.
func (s *Stack) recordContainerLogs() {
	var mu sync.Mutex
	logs := map[string]string{}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
			}
			for _, tenant := range []string{TenantA, TenantB} {
				out, err := s.podman("ps", "-a", "--format", "{{.ID}} {{.Names}}", "--filter", "label="+podman.LabelTenant+"="+tenant)
				if err != nil {
					continue
				}
				for _, line := range strings.Split(out, "\n") {
					f := strings.Fields(line)
					if len(f) != 2 {
						continue
					}
					if l, err := s.podman("logs", f[0]); err == nil && l != "" {
						mu.Lock()
						logs[f[1]] = l
						mu.Unlock()
					}
				}
			}
		}
	}()
	s.T.Cleanup(func() {
		close(stop)
		<-done
		if s.T.Failed() {
			mu.Lock()
			defer mu.Unlock()
			for name, l := range logs {
				lines := strings.Split(l, "\n")
				if len(lines) > 14 {
					lines = lines[len(lines)-14:]
				}
				s.T.Logf("container %s output (tail):\n%s", name, strings.Join(lines, "\n"))
			}
		}
	})
}

// removeContainers force-removes every workload container of the test tenants:
// leftovers of an aborted earlier run would otherwise be mistaken for this one's,
// and a failing test must not leave its sleeping workloads behind.
func (s *Stack) removeContainers() {
	for _, tenant := range []string{TenantA, TenantB} {
		out, err := s.podman("ps", "-a", "-q", "--filter", "label="+podman.LabelTenant+"="+tenant)
		if err != nil || out == "" {
			continue
		}
		_, _ = s.podman(append([]string{"rm", "-f", "-t", "0"}, strings.Fields(out)...)...)
	}
}

// WaitLaunched waits until the entrypoint in the container was admitted and
// launched the step's command (an admission refused by an unavailable server is
// final for the attempt; only a launched command has callbacks to retry).
func (s *Stack) WaitLaunched(container string) {
	s.T.Helper()
	WaitFor(s.T, "the entrypoint to launch the command", 60*time.Second, func() bool {
		out, err := s.podman("logs", container)
		return err == nil && strings.Contains(out, "launching command")
	})
}
