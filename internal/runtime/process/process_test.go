package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// The test binary doubles as helper programs, selected by environment.
func TestMain(m *testing.M) {
	switch os.Getenv("PROCESS_TEST_HELPER") {
	case "escape":
		// Leave a descendant in its own session holding the output open.
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "PROCESS_TEST_HELPER=hold")
		cmd.Stdout = os.Stdout
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Printf("leader-done pid=%d\n", cmd.Process.Pid)
		os.Exit(0)
	case "hold":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func newRuntime(t *testing.T, p Program) *Runtime {
	t.Helper()
	r, err := New(Config{Program: p, Dir: t.TempDir(), KillGrace: time.Second, DrainTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func spec(t *testing.T, script string) execution.AttemptSpec {
	return runtimetest.Spec(t, runtimetest.NewAttemptID(t), "", script)
}

func Test_process_contract(t *testing.T) {
	runtimetest.Contract(t, runtimetest.Harness{New: func(t *testing.T) execution.Runtime { return newRuntime(t, DeclaredCommand{}) }})
}

// Test_stop_while_starting: a stop never confirms while a launch can still
// arrive, and a launch that has not happened yet never happens.
func Test_stop_while_starting(t *testing.T) {
	ctx := context.Background()
	gate := func() (Program, chan struct{}, chan struct{}) {
		entered, release := make(chan struct{}), make(chan struct{})
		return ProgramFunc(func(ctx context.Context, l Launch) (Command, error) {
			close(entered)
			<-release
			return DeclaredCommand{}.Command(ctx, l)
		}), entered, release
	}
	type stopped struct {
		ev  execution.StopEvidence
		err error
	}

	t.Run("a stop during the launch decision prevents the launch", func(t *testing.T) {
		p, entered, release := gate()
		r := newRuntime(t, p)
		s := spec(t, "touch launched; exec sleep 60")
		started := make(chan error, 1)
		go func() { _, err := r.Start(ctx, s); started <- err }()
		recv(t, entered)
		stops := make(chan stopped, 1)
		go func() { ev, err := r.Stop(ctx, s.Workload("")); stops <- stopped{ev, err} }()
		r.awaitStopping(t, s.WorkloadName)
		select {
		case got := <-stops:
			t.Fatalf("the stop confirmed while the launch was pending: %+v", got)
		default:
		}
		close(release)
		if err := recv(t, started); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("start: %v", err)
		}
		if got := recv(t, stops); got.err != nil || !got.ev.Stopped || got.ev.Launch != execution.LaunchNotStarted {
			t.Fatalf("stop: %+v", got)
		}
		if _, err := os.Stat(filepath.Join(r.cfg.Dir, s.WorkloadName, "launched")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the workload ran: %v", err)
		}
	})

	t.Run("a stop after the launch terminates the process", func(t *testing.T) {
		r := newRuntime(t, DeclaredCommand{})
		s := spec(t, "trap 'exit 0' TERM; echo ready; while :; do sleep 0.05; done")
		w, err := r.Start(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := r.Stop(ctx, w)
		if err != nil || !ev.Stopped || ev.Launch != execution.LaunchStarted {
			t.Fatalf("stop: %+v %v", ev, err)
		}
		if !r.ended(w) {
			t.Fatal("the stop confirmed a running process")
		}
	})
}

// awaitBound bounds every wait of a test on another goroutine.
const awaitBound = 30 * time.Second

// recv receives from ch or fails the test after awaitBound.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(awaitBound):
		t.Fatal("timed out waiting for another goroutine")
		panic("unreachable")
	}
}

// awaitStopping yields until Stop marked the workload; Stop does so before
// it waits for the launch decision.
func (r *Runtime) awaitStopping(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(awaitBound)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		w := r.workloads[name]
		marked := w != nil && w.stopped
		r.mu.Unlock()
		if marked {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("Stop never marked %s", name)
}

func (r *Runtime) ended(h execution.Workload) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workloads[h.Name].ended()
}

// Test_output_is_drained: everything a workload wrote before it ended is
// captured; output an escaped descendant holds open is cut off and marked.
func Test_output_is_drained(t *testing.T) {
	ctx := context.Background()
	t.Run("all output before the exit", func(t *testing.T) {
		r := newRuntime(t, DeclaredCommand{})
		w, err := r.Start(ctx, spec(t, "head -c 600000 /dev/zero | tr '\\0' x; exit 3"))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Wait(ctx, w); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := r.Logs(ctx, w, &b); err != nil || b.Len() != 600000 || strings.Contains(b.String(), "[kodama:") {
			t.Fatalf("captured %d bytes: %v", b.Len(), err)
		}
		if obs, err := r.Observe(ctx, w); err != nil || obs.Kind != execution.ObservedFailed {
			t.Fatalf("observe: %+v %v", obs, err)
		}
	})
	t.Run("an escaped descendant", func(t *testing.T) {
		helper := ProgramFunc(func(_ context.Context, _ Launch) (Command, error) {
			return Command{Path: os.Args[0], Env: []string{"PROCESS_TEST_HELPER=escape"}}, nil
		})
		r := newRuntime(t, helper)
		w, err := r.Start(ctx, spec(t, "unused"))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Wait(ctx, w); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		_ = r.Logs(ctx, w, &b)
		out := b.String()
		if !strings.Contains(out, "leader-done") || !strings.HasSuffix(out, "[kodama: output incomplete]\n") {
			t.Fatalf("output %q", out)
		}
		if _, pid, ok := strings.Cut(strings.Fields(out)[1], "="); ok {
			if n, err := strconv.Atoi(pid); err == nil {
				_ = syscall.Kill(n, syscall.SIGKILL)
			}
		}
	})
	t.Run("output beyond the bound is truncated", func(t *testing.T) {
		r, err := New(Config{Program: DeclaredCommand{}, Dir: t.TempDir(), MaxLogBytes: 10})
		if err != nil {
			t.Fatal(err)
		}
		w, err := r.Start(ctx, spec(t, "echo 0123456789abcdef"))
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Wait(ctx, w)
		var b bytes.Buffer
		_ = r.Logs(ctx, w, &b)
		if b.String() != "0123456789\n[kodama: output truncated]\n" {
			t.Fatalf("%q", b.String())
		}
	})
}

// Test_entrypoint_program: the entrypoint is launched with its private layout
// and the identity runtime admission verifies, and a retried launch reuses
// the registered credential.
func Test_entrypoint_program(t *testing.T) {
	ctx := context.Background()
	reg := &registrar{}
	p := Entrypoint{Binary: "/opt/kodama", ServerURL: "https://runtime.invalid", CAFile: "/etc/ca.pem", Issuer: bootstrap.Issuer{Registrar: reg}}
	s := spec(t, "true")
	dir := t.TempDir()
	c, err := s.DecodeContext()
	if err != nil {
		t.Fatal(err)
	}
	l := Launch{Spec: s, Context: c, Workload: s.Workload("11111111-2222-4333-8444-555555555555"), Dir: dir}
	first, err := p.Command(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Command(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path != "/opt/kodama" || !slices.Equal(first.Args, second.Args) || first.Args[0] != "step" {
		t.Fatalf("command %+v", first)
	}
	for _, want := range []string{"--server-url https://runtime.invalid", "--attempt-id " + string(s.AttemptID), "--identity-dir " + filepath.Join(dir, "identity"), "--ca-file /etc/ca.pem"} {
		if !strings.Contains(strings.Join(first.Args, " "), want) {
			t.Errorf("missing %q in %v", want, first.Args)
		}
	}
	for _, want := range bootstrap.Env(l.Workload) {
		if !slices.Contains(first.Env, want) {
			t.Errorf("missing %q", want)
		}
	}
	if len(reg.cmds) != 2 || reg.cmds[0].Credential.Digest() != reg.cmds[1].Credential.Digest() {
		t.Fatalf("registrations %d", len(reg.cmds))
	}
	if fi, err := os.Stat(filepath.Join(dir, "identity", bootstrap.CredentialFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credential file: %v %v", fi, err)
	}
	l.Context.Workspace.Access = runtimev1.AccessRead
	if _, err := p.Command(ctx, l); !errors.Is(err, bootstrap.ErrNoCheckout) {
		t.Fatalf("a source-consuming attempt: %v", err)
	}
}

// Test_job_observer: runtime admission sees the running instance as both Job
// and Pod, and nothing for another attempt or a finished workload.
func Test_job_observer(t *testing.T) {
	ctx := context.Background()
	r := newRuntime(t, DeclaredCommand{})
	s := spec(t, "exec sleep 60")
	w, err := r.Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Remove(ctx, w)
	obs := runtimeauth.JobObservation{Scope: s.Scope, AttemptID: s.AttemptID, Expected: runtimeauth.ExpectedJob{Namespace: Namespace, Name: w.Name}}
	ev, err := r.ObserveJob(ctx, obs)
	if err != nil || ev != bootstrap.Evidence(Namespace, w) {
		t.Fatalf("evidence %+v %v", ev, err)
	}
	other := obs
	other.AttemptID = "att_other"
	if ev, err := r.ObserveJob(ctx, other); err != nil || ev.JobUID != "" {
		t.Fatalf("another attempt: %+v %v", ev, err)
	}
	if _, err := r.Stop(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ObserveJob(ctx, obs); !errors.Is(err, runtimeauth.ErrNotObserved) {
		t.Fatalf("a stopped workload: %v", err)
	}
}

type registrar struct{ cmds []runtimeauth.RegisterCommand }

func (r *registrar) Register(_ context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
	r.cmds = append(r.cmds, cmd)
	return runtimeauth.Registration{}, nil
}
