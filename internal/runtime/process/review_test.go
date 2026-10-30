package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// These tests hold the Process run resources to the defects an independent
// review found: a preparation that did not hold the workspace, a child started
// before its identity was durable, a workspace recreated or replaced under the
// same UID, credential material left behind, a stop intent read as a stop, and
// identities manufactured after a record was lost.

func resourcesWith(t *testing.T, dir string, prog ResourceProgram, reg ResourceRegistrar, mutate func(*ResourcesConfig)) *Resources {
	t.Helper()
	cfg := ResourcesConfig{
		Dir: dir, Program: prog, Registrar: reg, KillGrace: time.Second, DrainTimeout: 100 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := NewResources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.idle(10 * time.Second) })
	return r
}

// crashAt makes a launch stop dead at the named point, as a controller crash
// would: the hook returns, the gate pipe closes with the adapter, nothing else
// of the launch happens. onPoint, when set, runs first with the child's pid.
func crashAt(point string, onPoint func(pid int)) func(string, int) error {
	return func(p string, pid int) error {
		if p != point {
			return nil
		}
		if onPoint != nil {
			onPoint(pid)
		}
		return errors.New("controller died")
	}
}

func prepFor(ws execution.ResourceHandle) execution.ResourceSpec {
	spec := execution.ResourceSpec{
		Scope: ws.Scope, RunID: ws.RunID, Backend: Backend, Kind: execution.KindPreparation, Key: execution.KeyPreparation,
		Policy:    execution.PolicyPin{ID: "run-contract", Digest: "sha256:" + strings.Repeat("a", 64)},
		RoleShape: "sha256:" + strings.Repeat("b", 64), DeadlineAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		Preparation: &execution.PreparationSpec{Image: testPrepImage, RepositoryURL: "https://git.invalid/org/repo.git", Commit: testCommit, TimeoutSecs: 60, MaxBytes: 1 << 20, MaxAttempts: 1},
	}
	spec.Name = execution.ResourceName(spec.Scope, spec.RunID, spec.Kind, spec.Key)
	return spec
}

// longPrep is a preparation program that runs until it is stopped.
func longPrep() ResourceProgram {
	return testProgram(`echo working > "$KODAMA_WORKSPACE_DIR/started"; exec sleep 300`)
}

// stopNow is one Stop call.
func stopNow(t *testing.T, res execution.RunResources, h execution.ResourceHandle) execution.ResourceStopEvidence {
	t.Helper()
	ev, err := res.Stop(context.Background(), h)
	if err != nil {
		t.Fatalf("stop %s: %v", h.Key, err)
	}
	return ev
}

// stopped repeats Stop until the resource is verified stopped, as execution
// does: a stop that finds a launch still registering is not yet confirmed.
func stopped(t *testing.T, res execution.RunResources, h execution.ResourceHandle) execution.ResourceStopEvidence {
	t.Helper()
	var ev execution.ResourceStopEvidence
	waitFor(t, "the stop of "+h.Key, func() bool {
		ev = stopNow(t, res, h)
		return ev.Stopped
	})
	return ev
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Test_process_preparation_holds_workspace (review 1): a running or pending
// preparation is a holder of the run workspace: Stop reports it not released,
// Remove keeps the scratch, and a launch in progress excludes a concurrent Stop.
func Test_process_preparation_holds_workspace(t *testing.T) {
	ctx := context.Background()

	t.Run("a running preparation", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)
		res := resourcesWith(t, e.dir, longPrep(), nil, nil)
		prep, err := res.Ensure(ctx, prepFor(ws))
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the preparation to write", func() bool {
			_, err := os.Stat(filepath.Join(e.res.st.workspace(ws.Name), "started"))
			return err == nil
		})
		if obs, err := res.Observe(ctx, ws); err != nil || obs.Released {
			t.Fatalf("the workspace is not held by its preparation: %+v %v", obs, err)
		}
		if ev := stopNow(t, res, ws); ev.Released {
			t.Fatalf("a held workspace's stop reported release: %+v", ev)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || rm.Removed {
			t.Fatalf("a held workspace was removed: %+v %v", rm, err)
		}
		if _, err := os.Stat(filepath.Join(e.res.st.workspace(ws.Name), "started")); err != nil {
			t.Fatalf("the scratch was deleted under a running preparation: %v", err)
		}
		stopped(t, res, prep.Handle)
		if ev := stopNow(t, res, ws); !ev.Released {
			t.Fatalf("not released after the preparation stopped: %+v", ev)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
	})

	t.Run("a launch in progress excludes a concurrent stop", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)
		entered, release := make(chan struct{}), make(chan struct{})
		prog := ResourceProgramFunc(func(ctx context.Context, l ResourceLaunch) (Command, error) {
			close(entered)
			<-release
			return testProgram(`exec sleep 300`).Command(ctx, l)
		})
		res := resourcesWith(t, e.dir, prog, nil, nil)
		ensured := make(chan error, 1)
		go func() { _, err := res.Ensure(ctx, prepFor(ws)); ensured <- err }()
		<-entered
		type result struct {
			ev  execution.ResourceStopEvidence
			err error
		}
		stops := make(chan result, 1)
		go func() { ev, err := res.Stop(ctx, ws); stops <- result{ev, err} }()
		select {
		case got := <-stops:
			t.Fatalf("the workspace stop confirmed while a preparation was launching: %+v", got)
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		if err := recv(t, ensured); err != nil {
			t.Fatal(err)
		}
		if got := recv(t, stops); got.err != nil || !got.ev.Stopped || got.ev.Released {
			t.Fatalf("the stop after the launch: %+v", got)
		}
		stopped(t, res, prepFor(ws).Handle(""))
	})

	t.Run("an interrupted launch holds exactly while its command can run", func(t *testing.T) {
		ran := func(e sharedEnv, ws execution.ResourceHandle) bool {
			_, err := os.Stat(filepath.Join(e.res.st.workspace(ws.Name), "started"))
			return err == nil
		}
		// The gate was opened and the group recorded before the crash: the command
		// runs, the workspace is held until the adopted preparation is stopped.
		e := newSharedEnv(t)
		ws := e.workspace(t)
		crashed := resourcesWith(t, e.dir, longPrep(), nil, nil)
		crashed.hook = crashAt("started", nil)
		if _, err := crashed.Ensure(ctx, prepFor(ws)); err == nil {
			t.Fatal("the hook did not interrupt")
		}
		restarted := resourcesWith(t, e.dir, longPrep(), nil, nil)
		waitFor(t, "the preparation to write", func() bool { return ran(e, ws) })
		if obs, err := restarted.Observe(ctx, ws); err != nil || obs.Released {
			t.Fatalf("a running preparation does not hold the workspace: %+v %v", obs, err)
		}
		if rm, _ := restarted.Remove(ctx, ws); rm.Removed {
			t.Fatal("the workspace was removed under a running preparation")
		}
		stopped(t, restarted, prepFor(ws).Handle(""))
		if obs, err := restarted.Observe(ctx, ws); err != nil || !obs.Released {
			t.Fatalf("%+v %v", obs, err)
		}

		// The crash came after the group was recorded but before the gate opened:
		// the process exits at end of file without running the command, and the
		// recorded group is released once it is gone.
		e = newSharedEnv(t)
		ws = e.workspace(t)
		crashed = resourcesWith(t, e.dir, longPrep(), nil, nil)
		crashed.hook = crashAt("registered", nil)
		if _, err := crashed.Ensure(ctx, prepFor(ws)); err == nil {
			t.Fatal("the hook did not interrupt")
		}
		restarted = resourcesWith(t, e.dir, longPrep(), nil, nil)
		waitFor(t, "the gated process to exit", func() bool {
			obs, err := restarted.Observe(ctx, ws)
			return err == nil && obs.Released
		})
		time.Sleep(200 * time.Millisecond)
		if ran(e, ws) {
			t.Fatal("the command ran although the gate never opened")
		}

		// The crash came before any process: nothing was recorded, nothing can run.
		e = newSharedEnv(t)
		ws = e.workspace(t)
		crashed = resourcesWith(t, e.dir, longPrep(), nil, nil)
		crashed.hook = crashAt("issued", nil)
		if _, err := crashed.Ensure(ctx, prepFor(ws)); err == nil {
			t.Fatal("the hook did not interrupt")
		}
		restarted = resourcesWith(t, e.dir, longPrep(), nil, nil)
		if obs, err := restarted.Observe(ctx, ws); err != nil || !obs.Released {
			t.Fatalf("%+v %v", obs, err)
		}
	})
}

// Test_process_launch_interruption (review 2): a workload is launched behind a
// gate and its group is recorded before the gate opens, so a controller that dies
// between the start and the record leaves a child that exits at end of file and
// can never run the command, however long it is paused; a child that was let run
// is adopted, stopped and never duplicated; and evidence never claims a stop or a
// release while the command could run.
func Test_process_launch_interruption(t *testing.T) {
	ctx := context.Background()
	newService := func(t *testing.T, e sharedEnv, launches *int) (*Resources, execution.ResourceSpec, ResourceProgram) {
		prog := ResourceProgramFunc(func(ctx context.Context, l ResourceLaunch) (Command, error) {
			*launches++
			return DeclaredService{}.Command(ctx, l)
		})
		spec := serviceSpec(t, e, "db", serviceBody("touch ready; exec sleep 300", 5432))
		return resourcesWith(t, e.dir, prog, nil, nil), spec, prog
	}

	t.Run("a child that was let run is adopted, not duplicated", func(t *testing.T) {
		e := newSharedEnv(t)
		launches := 0
		crashed, spec, prog := newService(t, e, &launches)
		crashed.hook = crashAt("started", nil)
		if _, err := crashed.Ensure(ctx, spec); err == nil {
			t.Fatal("the hook did not interrupt")
		}
		if rec, _, _ := crashed.st.load(spec.Name); rec.Pgid == 0 || rec.IssuedAt == nil {
			t.Fatalf("the group must be recorded before the gate opens: %+v", rec)
		}
		restarted := resourcesWith(t, e.dir, prog, nil, nil)
		obs, err := restarted.Ensure(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if launches != 1 {
			t.Fatalf("the interrupted launch was repeated: %d launches", launches)
		}
		ready := awaitPhase(t, restarted, obs.Handle, execution.PhaseReady)
		if ready.ProbeKind != "exec" {
			t.Fatalf("%+v", ready)
		}
		ev := stopped(t, restarted, obs.Handle)
		if !ev.Stopped || ev.Launch != execution.LaunchStarted {
			t.Fatalf("the adopted child's stop: %+v", ev)
		}
		rec, _, _ := restarted.st.load(spec.Name)
		if alive, err := groupAlive(rec.Pgid, rec.StartID); err != nil || alive {
			t.Fatalf("the child survived the stop: alive=%v %v", alive, err)
		}
	})

	t.Run("no process was started: nothing can run", func(t *testing.T) {
		e := newSharedEnv(t)
		launches := 0
		crashed, spec, prog := newService(t, e, &launches)
		crashed.hook = crashAt("issued", nil)
		if _, err := crashed.Ensure(ctx, spec); err == nil {
			t.Fatal("the hook did not interrupt")
		}
		restarted := resourcesWith(t, e.dir, prog, nil, nil)
		h := spec.Handle("")
		if _, err := restarted.Ensure(ctx, spec); err != nil || launches != 1 {
			t.Fatalf("ensure repeated the announced launch: %v, %d launches", err, launches)
		}
		ev := stopNow(t, restarted, h)
		if !ev.Stopped || ev.Launch != execution.LaunchUnknown {
			t.Fatalf("an announced launch that left no process: %+v", ev)
		}
		if _, err := restarted.Ensure(ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
			t.Fatalf("a stopped announced launch was started again: %v", err)
		}
		if launches != 1 {
			t.Fatalf("%d launches", launches)
		}
	})

	t.Run("a child paused before it is recorded can never run the command", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)
		var paused int
		crashed := resourcesWith(t, e.dir, longPrep(), nil, nil)
		// The child is stopped dead right after it was spawned, before it reads the
		// gate, and the controller dies before it records the group.
		crashed.hook = crashAt("spawned", func(pid int) {
			paused = pid
			if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
		})
		if _, err := crashed.Ensure(ctx, prepFor(ws)); err == nil {
			t.Fatal("the hook did not interrupt")
		}
		t.Cleanup(func() { _ = syscall.Kill(-paused, syscall.SIGKILL) })
		rec, _, _ := crashed.st.load(prepFor(ws).Name)
		if rec.Pgid != 0 {
			t.Fatalf("the group was recorded: %+v", rec)
		}
		// Time passes with the child paused and unrecorded. Whatever the evidence
		// says, it is only ever true because the command cannot run.
		time.Sleep(600 * time.Millisecond)
		restarted := resourcesWith(t, e.dir, longPrep(), nil, nil)
		if obs, err := restarted.Observe(ctx, ws); err != nil || !obs.Released {
			t.Fatalf("%+v %v", obs, err)
		}
		if ev := stopNow(t, restarted, prepFor(ws).Handle("")); !ev.Stopped || ev.Launch != execution.LaunchUnknown {
			t.Fatalf("%+v", ev)
		}
		// It resumes: it reads end of file at its gate and exits without the command.
		if err := syscall.Kill(paused, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the resumed child to exit", func() bool {
			return syscall.Kill(paused, 0) != nil || crashed.idle(10*time.Millisecond)
		})
		time.Sleep(300 * time.Millisecond)
		if _, err := os.Stat(filepath.Join(e.res.st.workspace(ws.Name), "started")); err == nil {
			t.Fatal("the resumed child ran the command although the controller died before opening the gate")
		}
	})

	t.Run("a gate that is never opened runs nothing", func(t *testing.T) {
		e := newSharedEnv(t)
		ran := filepath.Join(e.dir, "ran")
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		c := gate(Command{Path: "/bin/sh", Args: []string{"-c", "touch " + ran}, Env: []string{"PATH=" + os.Getenv("PATH")}})
		cmd := exec.Command(c.Path, c.Args...)
		cmd.Env = c.Env
		cmd.ExtraFiles = []*os.File{pr}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pr.Close()
		pw.Close() // the adapter died: end of file
		if err := cmd.Wait(); err == nil {
			t.Fatal("the gated command exited successfully without the gate opening")
		}
		if _, err := os.Stat(ran); err == nil {
			t.Fatal("the command ran")
		}
	})

	t.Run("a marker without a group is not a holder, one with a live group is", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)
		res := resourcesWith(t, e.dir, nil, nil, nil)
		if err := e.res.st.saveHolder(ws.Name, holder{Workload: "attempt-x"}); err != nil {
			t.Fatal(err)
		}
		if obs, err := res.Observe(ctx, ws); err != nil || !obs.Released {
			t.Fatalf("%+v %v", obs, err)
		}
		cmd := exec.Command("sleep", "300")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
		start, _, err := startOf(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.res.st.saveHolder(ws.Name, holder{Workload: "attempt-x", Pgid: cmd.Process.Pid, StartID: start}); err != nil {
			t.Fatal(err)
		}
		if obs, err := res.Observe(ctx, ws); err != nil || obs.Released {
			t.Fatalf("a live recorded group is not a holder: %+v %v", obs, err)
		}
		// A recycled pid (another start time) is not ours and holds nothing.
		if err := e.res.st.saveHolder(ws.Name, holder{Workload: "attempt-x", Pgid: cmd.Process.Pid, StartID: "Mon Jan  1 00:00:00 2001"}); err != nil {
			t.Fatal(err)
		}
		if obs, err := res.Observe(ctx, ws); err != nil || !obs.Released {
			t.Fatalf("%+v %v", obs, err)
		}
	})

	t.Run("a failed start withdraws the announcement", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)
		// A start that fails outright (no process exists) is simulated by a missing
		// working directory.
		prog := ResourceProgramFunc(func(ctx context.Context, l ResourceLaunch) (Command, error) {
			c, _ := testProgram("true").Command(ctx, l)
			c.Dir = "/definitely/not/a/dir"
			return c, nil
		})
		res := resourcesWith(t, e.dir, prog, nil, nil)
		if _, err := res.Ensure(ctx, prepFor(ws)); err == nil {
			t.Fatal("a start that failed was accepted")
		}
		rec, _, _ := res.st.load(prepFor(ws).Name)
		if rec.IssuedAt != nil {
			t.Fatal("the announcement of a launch that never started was kept")
		}
		if obs, err := res.Observe(ctx, ws); err != nil || !obs.Released {
			t.Fatalf("a withdrawn launch holds the workspace: %+v %v", obs, err)
		}
	})
}

// Test_process_workspace_identity (review 3, 5): the workspace is created once and
// pinned by its filesystem identity; lost scratch stays lost, a replacement is a
// collision that is never adopted, reported released or deleted.
func Test_process_workspace_identity(t *testing.T) {
	ctx := context.Background()

	t.Run("lost scratch is never recreated", func(t *testing.T) {
		e := newSharedEnv(t)
		ws, spec := e.workspaceSpec(t)
		if err := os.RemoveAll(e.res.st.workspace(ws.Name)); err != nil {
			t.Fatal(err)
		}
		if obs, err := e.res.Observe(ctx, ws); err != nil || obs.Phase != execution.PhaseAbsent {
			t.Fatalf("lost scratch: %+v %v", obs, err)
		}
		for _, r := range []*Resources{e.res, resourcesWith(t, e.dir, nil, nil, nil)} {
			obs, err := r.Ensure(ctx, spec)
			if err != nil || obs.Phase != execution.PhaseAbsent || obs.Handle != ws {
				t.Fatalf("ensure recreated or adopted lost scratch: %+v %v", obs, err)
			}
		}
		if _, err := os.Stat(e.res.st.workspace(ws.Name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the workspace directory was recreated: %v", err)
		}
		if _, err := e.rt.Start(ctx, runtimetest.SharedSpec(t, ws, "", "true")); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("an attempt ran on lost scratch: %v", err)
		}
		if ev := stopped(t, e.res, ws); !ev.Stopped {
			t.Fatalf("%+v", ev)
		}
		if rm, err := e.res.Remove(ctx, ws); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
	})

	t.Run("a replacement is a collision and is never deleted", func(t *testing.T) {
		e := newSharedEnv(t)
		ws, spec := e.workspaceSpec(t)
		dir := e.res.st.workspace(ws.Name)
		if err := os.WriteFile(filepath.Join(dir, "mine"), []byte("scratch"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The scratch is deleted and another directory takes its place.
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "theirs"), []byte("not ours"), 0o600); err != nil {
			t.Fatal(err)
		}
		intact := func() {
			t.Helper()
			if b, err := os.ReadFile(filepath.Join(dir, "theirs")); err != nil || string(b) != "not ours" {
				t.Fatalf("the replacement was touched: %q %v", b, err)
			}
		}
		if _, err := e.res.Observe(ctx, ws); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("observe: %v", err)
		}
		if _, err := e.res.Ensure(ctx, spec); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("ensure adopted a replacement: %v", err)
		}
		if _, err := e.rt.Start(ctx, runtimetest.SharedSpec(t, ws, "", "true")); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("an attempt ran in a replacement: %v", err)
		}
		if _, err := e.res.Stop(ctx, ws); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("a stop reported release of a replacement: %v", err)
		}
		if rm, err := e.res.Remove(ctx, ws); err == nil && rm.Removed || !errors.Is(err, execution.ErrResourceCollision) && !errors.Is(err, execution.ErrResourceFenced) {
			t.Fatalf("remove of a replacement: %+v %v", rm, err)
		}
		intact()
	})

	t.Run("a replaced resource directory is not ours", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)
		stopped(t, e.res, ws)
		res := e.res.st.resDir(ws.Name)
		if err := os.RemoveAll(res); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(res, workspaceSub), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(res, "theirs"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if rm, err := e.res.Remove(ctx, ws); err == nil && rm.Removed {
			t.Fatalf("a replaced directory was removed: %+v", rm)
		}
		if _, err := os.Stat(filepath.Join(res, "theirs")); err != nil {
			t.Fatalf("the replacement was deleted: %v", err)
		}
	})
}

// Test_process_credential_cleanup_after_failed_launch (review 6): the credential
// material a registration left is destroyed at stop, whatever the launch reached,
// and a cleanup that fails is an error, not a confirmed stop.
func Test_process_credential_cleanup_after_failed_launch(t *testing.T) {
	ctx := context.Background()
	e := newSharedEnv(t)
	reg := &resRegistrar{}
	prog := ResourceProgramFunc(func(context.Context, ResourceLaunch) (Command, error) {
		return Command{}, errors.New("the command cannot be built")
	})
	res := resourcesWith(t, e.dir, prog, reg, nil)
	spec := serviceSpec(t, e, "db", serviceBody("exec sleep 300", 5432))
	if _, err := res.Ensure(ctx, spec); err == nil {
		t.Fatal("a failing program was accepted")
	}
	cred := filepath.Join(res.st.resDir(spec.Name), "identity", CredentialFile)
	if _, err := os.Stat(cred); err != nil || len(reg.commands()) != 1 {
		t.Fatalf("the credential was not registered and persisted: %v %d", err, len(reg.commands()))
	}
	h := spec.Handle("")

	// A cleanup that cannot complete is not a confirmed stop.
	dir := res.st.resDir(spec.Name)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if ev, err := res.Stop(ctx, h); err == nil || ev.Stopped {
		t.Fatalf("a stop whose credential cleanup failed: %+v %v", ev, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ev := stopped(t, res, h)
	if !ev.Stopped || ev.Launch != execution.LaunchNotStarted {
		t.Fatalf("%+v", ev)
	}
	if _, err := os.Stat(cred); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the credential survived the stop: %v", err)
	}
	if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
		t.Fatalf("%+v %v", rm, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the resource directory survived the removal: %v", err)
	}
}

// Test_process_stop_intent_is_not_a_stop (review 7): a stop that was requested but
// not completed is not observed as stopped and does not allow removal.
func Test_process_stop_intent_is_not_a_stop(t *testing.T) {
	ctx := context.Background()
	e := newSharedEnv(t)
	res := resourcesWith(t, e.dir, testProgram(prepScript), nil, nil)
	// A service that ignores SIGTERM keeps running through a cancelled stop.
	spec := serviceSpec(t, e, "db", serviceBody(`trap '' TERM; touch ready; while :; do sleep 0.1; done`, 5432))
	obs, err := res.Ensure(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	awaitPhase(t, res, obs.Handle, execution.PhaseReady)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if ev, err := res.Stop(cancelled, obs.Handle); err == nil || ev.Stopped {
		t.Fatalf("a cancelled stop: %+v %v", ev, err)
	}
	got, err := res.Observe(ctx, obs.Handle)
	if err != nil || got.Phase == execution.PhaseStopped || got.Phase != execution.PhasePending || got.Reason != execution.ReasonStopUnconfirmed {
		t.Fatalf("a stop intent was observed as %q: %+v %v", got.Phase, got, err)
	}
	if _, err := res.Remove(ctx, obs.Handle); !errors.Is(err, execution.ErrResourceFenced) {
		t.Fatalf("remove after an unverified stop: %v", err)
	}
	if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
		t.Fatalf("the intent did not fence: %v", err)
	}
	ev := stopped(t, res, obs.Handle)
	if !ev.Stopped || ev.Launch != execution.LaunchStarted {
		t.Fatalf("%+v", ev)
	}
	if got, err := res.Observe(ctx, obs.Handle); err != nil || got.Phase != execution.PhaseStopped {
		t.Fatalf("%+v %v", got, err)
	}
	if rm, err := res.Remove(ctx, obs.Handle); err != nil || !rm.Removed {
		t.Fatalf("%+v %v", rm, err)
	}
}

// Test_process_record_loss_keeps_identity (review 9): stopping and removing a
// resource nobody recorded keeps the identity the caller supplied, repeats
// without collision and never claims a launch or a not-started proof.
func Test_process_record_loss_keeps_identity(t *testing.T) {
	ctx := context.Background()
	e := newSharedEnv(t)
	run := domain.RunID("run_" + string(runtimetest.NewAttemptID(t)))
	spec := serviceSpec(t, e, "db", serviceBody("exec sleep 300", 5432))
	spec.RunID = run
	spec.Name = execution.ResourceName(spec.Scope, run, spec.Kind, spec.Key)
	bound := spec.Handle("66666666-6666-4666-8666-666666666666")

	for range 3 {
		ev, err := e.res.Stop(ctx, bound)
		if err != nil || !ev.Stopped || ev.Launch != execution.LaunchUnknown {
			t.Fatalf("stop after record loss: %+v %v", ev, err)
		}
	}
	rec, found, err := e.res.st.load(bound.Name)
	if err != nil || !found || rec.UID != bound.UID || !rec.Untracked {
		t.Fatalf("the tombstone does not keep the supplied identity: %+v %v", rec, err)
	}
	for range 3 {
		if rm, err := e.res.Remove(ctx, bound); err != nil || !rm.Removed {
			t.Fatalf("remove after record loss: %+v %v", rm, err)
		}
	}
	if obs, err := e.res.Observe(ctx, bound); err != nil || obs.Phase != execution.PhaseAbsent {
		t.Fatalf("%+v %v", obs, err)
	}
	if _, err := e.res.Ensure(ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
		t.Fatalf("a lost-record tombstone did not fence: %v", err)
	}
	other := bound
	other.UID = "55555555-5555-4555-8555-555555555555"
	if _, err := e.res.Stop(ctx, other); !errors.Is(err, execution.ErrResourceCollision) {
		t.Fatalf("another instance's stop: %v", err)
	}

	// A tombstone made for an unbound handle accepts the instance bound later.
	spec2 := serviceSpec(t, e, "cache", serviceBody("exec sleep 300", 5433))
	if ev, err := e.res.Stop(ctx, spec2.Handle("")); err != nil || !ev.Stopped {
		t.Fatalf("%+v %v", ev, err)
	}
	if ev, err := e.res.Stop(ctx, spec2.Handle("44444444-4444-4444-8444-444444444444")); err != nil || !ev.Stopped || ev.Launch != execution.LaunchUnknown {
		t.Fatalf("a later bound stop: %+v %v", ev, err)
	}
}
