package podman_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	podmantest "github.com/optimiware/kodama/test/support/podman"
)

// commandStderr is the error text of a failed podman command. Stdout is kept
// separate: a platform warning on stderr must not be parsed as a container ID.
func commandStderr(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(exit.Stderr)
	}
	return ""
}

func newRuntime(t *testing.T, dir string) *podman.Runtime {
	t.Helper()
	r, err := podman.New(podman.Config{Program: podman.DeclaredCommand{}, StateDir: dir, StopTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Test_podman_contract runs the runtime contract against the real engine.
func Test_podman_contract(t *testing.T) {
	podmantest.Require(t)
	runtimetest.Contract(t, runtimetest.Harness{
		New:     func(t *testing.T) execution.Runtime { return newRuntime(t, podmantest.SharedDir(t)) },
		Image:   podmantest.Image,
		Timeout: 2 * time.Minute,
	})
}

// Test_podman_durable_identity: a restarted runtime adopts its container from
// the durable record, acts on it by container ID only and never adopts or
// touches a foreign container that holds the workload name.
func Test_podman_durable_identity(t *testing.T) {
	podmantest.Require(t)
	ctx := context.Background()

	t.Run("a restart adopts the recorded container", func(t *testing.T) {
		dir := podmantest.SharedDir(t)
		s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), podmantest.Image, "exec sleep 60")
		first := newRuntime(t, dir)
		w, err := first.Start(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = first.Remove(context.Background(), w) })
		restarted := newRuntime(t, dir)
		if again, err := restarted.Start(ctx, s); err != nil || again != w {
			t.Fatalf("adopt: %+v %v", again, err)
		}
		if obs, err := restarted.Observe(ctx, w); err != nil || obs.WorkloadUID != w.UID {
			t.Fatalf("observe: %+v %v", obs, err)
		}
		if ev, err := restarted.Stop(ctx, w); err != nil || !ev.Stopped || ev.Launch != execution.LaunchStarted {
			t.Fatalf("stop: %+v %v", ev, err)
		}
		if _, err := newRuntime(t, dir).Start(ctx, s); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("a stopped workload was restarted after a restart: %v", err)
		}
	})

	t.Run("a foreign container at the name is never adopted or touched", func(t *testing.T) {
		s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), podmantest.Image, "exec sleep 60")
		out, err := exec.Command("podman", "create", "--name", s.WorkloadName, "--label", podman.LabelManagedBy+"="+podman.ValueManagedBy,
			"--label", podman.LabelAttempt+"="+string(s.AttemptID), podmantest.Image, "sleep", "60").Output()
		if err != nil {
			t.Fatalf("%v: %s", err, commandStderr(err))
		}
		foreign := strings.TrimSpace(string(out))
		t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", "--time", "0", foreign).Run() })
		r := newRuntime(t, podmantest.SharedDir(t))
		if _, err := r.Start(ctx, s); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("start over a foreign container: %v", err)
		}
		if ev, err := r.Stop(ctx, s.Workload("")); err != nil || !ev.Stopped || ev.Launch != execution.LaunchNotStarted {
			t.Fatalf("stop: %+v %v", ev, err)
		}
		if err := r.Remove(ctx, s.Workload("")); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("podman", "container", "exists", foreign).Run(); err != nil {
			t.Fatal("the foreign container was removed")
		}
	})

	t.Run("the replacement of a recorded container is not ours", func(t *testing.T) {
		dir := podmantest.SharedDir(t)
		s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), podmantest.Image, "exec sleep 60")
		r := newRuntime(t, dir)
		w, err := r.Start(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("podman", "rm", "--force", "--time", "0", s.WorkloadName).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		out, err := exec.Command("podman", "run", "--detach", "--name", s.WorkloadName, "--label", podman.LabelUID+"="+w.UID, podmantest.Image, "sleep", "60").Output()
		if err != nil {
			t.Fatalf("%v: %s", err, commandStderr(err))
		}
		replacement := strings.TrimSpace(string(out))
		t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", "--time", "0", replacement).Run() })
		if _, err := r.Observe(ctx, w); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("the recorded container is gone: %v", err)
		}
		if ev, err := r.Stop(ctx, w); err != nil || !ev.Stopped {
			t.Fatalf("stop: %+v %v", ev, err)
		}
		if err := exec.Command("podman", "container", "exists", replacement).Run(); err != nil {
			t.Fatal("the replacement was touched")
		}
	})
}

// Test_podman_job_observer: runtime admission sees the running container as
// both Job and Pod, with the runtime-generated instance UID.
func Test_podman_job_observer(t *testing.T) {
	podmantest.Require(t)
	ctx := context.Background()
	r := newRuntime(t, podmantest.SharedDir(t))
	s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), podmantest.Image, "exec sleep 60")
	w, err := r.Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Remove(context.Background(), w) })
	obs := runtimeauth.JobObservation{Scope: s.Scope, AttemptID: s.AttemptID, Expected: runtimeauth.ExpectedJob{Namespace: podman.Namespace, Name: w.Name}}
	if ev, err := r.ObserveJob(ctx, obs); err != nil || ev != bootstrap.Evidence(podman.Namespace, w) {
		t.Fatalf("evidence %+v %v", ev, err)
	}
	if _, err := r.Stop(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ObserveJob(ctx, obs); !errors.Is(err, runtimeauth.ErrNotObserved) {
		t.Fatalf("a stopped workload: %v", err)
	}
}

// gated is a Program that parks the first container create until released.
type gated struct {
	podman.Program
	entered, release chan struct{}
	once             sync.Once
}

func (g *gated) Container(ctx context.Context, l podman.Launch) (podman.Container, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return podman.Container{}, ctx.Err()
	}
	return g.Program.Container(ctx, l)
}

// Test_podman_shared_state_fencing: a Start deciding to launch and a Stop of
// the same workload exclude each other even through two Runtime instances
// sharing a state directory, so a confirmed Stop is never followed by a launch.
func Test_podman_shared_state_fencing(t *testing.T) {
	podmantest.Require(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := podmantest.SharedDir(t)
	gate := &gated{Program: podman.DeclaredCommand{}, entered: make(chan struct{}), release: make(chan struct{})}
	starter, err := podman.New(podman.Config{Program: gate, StateDir: dir, StopTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stopper := newRuntime(t, dir)
	s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), podmantest.Image, "exec sleep 60")

	type started struct {
		w   execution.Workload
		err error
	}
	result := make(chan started, 1)
	go func() {
		w, err := starter.Start(ctx, s)
		result <- started{w, err}
	}()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("the start never reached the container create")
	}

	waiting, giveUp := context.WithTimeout(ctx, 200*time.Millisecond)
	defer giveUp()
	if _, err := stopper.Stop(waiting, s.Workload("")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stop through another runtime did not wait for the start: %v", err)
	}
	close(gate.release)
	var r started
	select {
	case r = <-result:
	case <-ctx.Done():
		t.Fatal("the start never finished")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { _ = stopper.Remove(context.Background(), r.w) })

	if ev, err := stopper.Stop(ctx, r.w); err != nil || !ev.Stopped || ev.Launch != execution.LaunchStarted {
		t.Fatalf("stop: %+v %v", ev, err)
	}
	out, err := exec.Command("podman", "inspect", "--format", "{{.State.Running}}", r.w.Name).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "false" {
		t.Fatalf("the workload still runs after a confirmed stop: %s %v", out, err)
	}
	if _, err := starter.Start(ctx, s); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("a stopped workload was started again: %v", err)
	}
}
