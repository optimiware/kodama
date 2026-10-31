package process

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	kr "github.com/optimiware/kodama/internal/kernel/runtime"
)

// setProbe injects what the system answers about process groups.
func setProbe(k *Kernel, p groupProbe) {
	k.h.mu.Lock()
	k.h.probe = p
	k.h.mu.Unlock()
}

// Test_process_kernel_permission_denied_is_not_termination: a probe that is
// denied permission proves a group exists, not that it ended. A descendant that
// changed its credentials looks exactly so, so such a group keeps its workspace
// held and blocks a confirmed stop or removal unless independent evidence shows
// it is gone. The system's answers are injected, for a launched and an adopted
// group alike.
func Test_process_kernel_permission_denied_is_not_termination(t *testing.T) {
	type outcome int
	const (
		alive outcome = iota
		unknown
		gone
	)
	cases := []struct {
		name        string
		adoptedOnly bool
		want        outcome
		leader      func(recorded string) func(int) (string, bool, error)
		members     func(int) (int, error)
	}{
		{"a descendant with other credentials outlives its leader", false, alive,
			func(string) func(int) (string, bool, error) {
				return func(int) (string, bool, error) { return "", true, nil }
			},
			func(int) (int, error) { return 1, nil }},
		{"the leader is still the recorded process", false, alive,
			func(rec string) func(int) (string, bool, error) {
				return func(int) (string, bool, error) { return rec, false, nil }
			},
			func(int) (int, error) { return 1, nil }},
		{"the process table cannot be read", false, unknown,
			func(string) func(int) (string, bool, error) {
				return func(int) (string, bool, error) { return "", true, nil }
			},
			func(int) (int, error) { return 0, errors.New("ps failed") }},
		{"the leader cannot be examined", false, unknown,
			func(string) func(int) (string, bool, error) {
				return func(int) (string, bool, error) { return "", false, errors.New("ps failed") }
			},
			func(int) (int, error) { return 1, nil }},
		{"the leader's pid was recycled", true, gone,
			func(string) func(int) (string, bool, error) {
				return func(int) (string, bool, error) { return "another start time", false, nil }
			},
			func(int) (int, error) { return 1, nil }},
		{"only zombies remain", true, gone,
			func(string) func(int) (string, bool, error) {
				return func(int) (string, bool, error) { return "", true, nil }
			},
			func(int) (int, error) { return 0, nil }},
	}
	for _, c := range cases {
		for _, path := range []string{"launched", "adopted"} {
			if c.adoptedOnly && path == "launched" {
				continue // a launched leader's status is awaited, which is not what is tested here
			}
			t.Run(path+"/"+c.name, func(t *testing.T) {
				ctx := context.Background()
				e := newKenv(t)
				ws := e.workspace(e.run)
				wsKey := objectKey(kScope, typeWorkspace, ws.Identity)
				b := e.binding("denied", 1)
				if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "exec sleep 300")); err != nil {
					t.Fatal(err)
				}
				wlKey := objectKey(kScope, typeWorkload, b.WorkloadID)
				pgid := e.pgid(typeWorkload, b.WorkloadID)
				t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
				first := e.k
				if path == "adopted" {
					e.restart()
				}
				rec, _, err := e.k.h.st.load(typeWorkload, wlKey)
				if err != nil {
					t.Fatal(err)
				}
				denied := groupProbe{
					signal: func(pg int, sig syscall.Signal) error {
						if pg == pgid && sig == 0 {
							return syscall.EPERM
						}
						return osProbe.signal(pg, sig)
					},
					leader:  c.leader(rec.StartID),
					members: c.members,
				}
				// The first adapter's supervisor, left over from the simulated restart,
				// must see the same system.
				for _, k := range []*Kernel{first, e.k} {
					setProbe(k, denied)
					t.Cleanup(func() { setProbe(k, osProbe) })
				}

				short := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, 300*time.Millisecond) }
				holder := func() (string, bool, error) { return e.k.Resources.WorkspaceHolder(ctx, ws) }
				_, markerErr := os.Stat(e.k.h.st.holderFile(wsKey, wlKey))
				if markerErr != nil {
					t.Fatalf("no holder marker: %v", markerErr)
				}

				switch c.want {
				case gone:
					obs, err := e.k.Runtime.Observe(ctx, b)
					if err != nil || obs.Phase != kr.PhaseExited {
						t.Fatalf("with independent evidence the group is gone: %+v %v", obs, err)
					}
					if _, held, err := holder(); err != nil || held {
						t.Fatalf("the workspace stays held: %v %v", held, err)
					}
					return
				case alive:
					if obs, err := e.k.Runtime.Observe(ctx, b); err != nil || obs.Phase != kr.PhaseRunning {
						t.Fatalf("a group denying permission is %+v %v", obs, err)
					}
					if id, held, err := holder(); err != nil || !held || id != b.WorkloadID {
						t.Fatalf("the workspace was released: %q %v %v", id, held, err)
					}
					if _, err := e.k.Runtime.Start(ctx, e.request(e.binding("next", 1), ws, "true")); !errors.Is(err, ErrWorkspaceBusy) {
						t.Fatalf("a step started on a held workspace: %v", err)
					}
				case unknown:
					if obs, err := e.k.Runtime.Observe(ctx, b); err != nil || obs.Phase != kr.PhaseUnknown {
						t.Fatalf("an unverifiable group is %+v %v", obs, err)
					}
					if _, _, err := holder(); !errors.Is(err, errUnverifiable) {
						t.Fatalf("the holder of an unverifiable group: %v", err)
					}
					if _, err := e.k.Runtime.Start(ctx, e.request(e.binding("next", 1), ws, "true")); !errors.Is(err, kr.ErrNotReached) || errors.Is(err, ErrWorkspaceBusy) {
						t.Fatalf("a step started on an unverifiable workspace: %v", err)
					}
				}

				sctx, cancel := short()
				defer cancel()
				if _, err := e.k.Runtime.Stop(sctx, b); !errors.Is(err, kr.ErrUncertain) {
					t.Fatalf("a stop confirmed while the group is not verified gone: %v", err)
				}
				rctx, cancel2 := short()
				defer cancel2()
				if _, err := e.k.Runtime.Remove(rctx, b); !errors.Is(err, kr.ErrUncertain) {
					t.Fatalf("a removal confirmed while the group is not verified gone: %v", err)
				}
				cur, found, err := e.k.h.st.load(typeWorkload, wlKey)
				if err != nil || !found || cur.Stopped || cur.Removed {
					t.Fatalf("the record claims a stop or removal: %+v %v", cur, err)
				}
				if _, err := os.Stat(e.k.h.st.holderFile(wsKey, wlKey)); err != nil {
					t.Fatalf("the holder marker was dropped: %v", err)
				}
				if exists, _ := e.k.h.st.dirExists(typeWorkload, wlKey); !exists {
					t.Fatal("the workload's files were removed")
				}
				// The workspace cannot be removed while its holder is not verified gone.
				if o, err := e.k.Resources.Remove(ctx, ws); err == nil && o.ConfirmedRemoved() {
					t.Fatalf("the held workspace was removed: %+v", o)
				}
				if exists, _ := e.k.h.st.dirExists(typeWorkspace, wsKey); !exists {
					t.Fatal("the held workspace's files were removed")
				}
			})
		}
	}
}

// Test_process_kernel_denied_service_is_not_lost_or_stopped: the same rule for a
// service's group.
func Test_process_kernel_denied_service_is_not_lost_or_stopped(t *testing.T) {
	ctx := context.Background()
	e := newKenv(t)
	spec := e.service("denied", 1, "exec sleep 300")
	obs, err := e.k.Resources.Ensure(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	h := obs.Handle
	pgid := e.pgid(typeService, spec.Identity)
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	first := e.k
	e.restart()
	denied := groupProbe{
		signal: func(pg int, sig syscall.Signal) error {
			if pg == pgid && sig == 0 {
				return syscall.EPERM
			}
			return osProbe.signal(pg, sig)
		},
		leader:  func(int) (string, bool, error) { return "", true, nil },
		members: func(int) (int, error) { return 1, nil },
	}
	for _, k := range []*Kernel{first, e.k} {
		setProbe(k, denied)
		t.Cleanup(func() { setProbe(k, osProbe) })
	}

	if o, err := e.k.Resources.Observe(ctx, h); err != nil || o.State != kr.ResourceReady {
		t.Fatalf("a service whose group denies permission is lost: %+v %v", o, err)
	}
	sctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := e.k.Resources.Stop(sctx, h); !errors.Is(err, kr.ErrUncertain) {
		t.Fatalf("a stop confirmed while the group is not verified gone: %v", err)
	}
	rctx, cancel2 := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel2()
	if o, err := e.k.Resources.Remove(rctx, h); err == nil || o.ConfirmedRemoved() {
		t.Fatalf("a removal confirmed while the group is not verified gone: %+v %v", o, err)
	}
}
