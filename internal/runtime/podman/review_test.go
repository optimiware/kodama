package podman_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// These tests hold the Podman run resources to the defects an independent review
// found: a creation whose answer was lost, workspace release decided without the
// volume's identity, a stop intent read as a stop, a role whose user namespace
// was never applied, and identities manufactured after a record was lost. They
// run against the double engine, which can lose or delay the answer of a create.

func newSettleEnv(t *testing.T) *rEnv {
	t.Helper()
	e := newREnv(t)
	e.fake.set(t, "exec-code", "0")
	return e
}

// fenceLabel is what a placeholder would carry; the adapter creates none.
const fenceLabel = "kodama.optimi.com/fence"

// objects returns the engine objects of a kind, split into real ones and any that
// carry the placeholder label. The adapter never creates a placeholder: every test
// expects the second count to be zero.
func objects(e *rEnv, t *testing.T, kind string) (real, placeholders int) {
	count := func(labels map[string]string) {
		if labels[fenceLabel] != "" {
			placeholders++
		} else {
			real++
		}
	}
	switch kind {
	case "network":
		for _, n := range e.fake.networks(t) {
			count(n.Labels)
		}
	case "volume":
		for _, v := range e.fake.volumes(t) {
			count(v.Labels)
		}
	default:
		for _, c := range e.fake.created(t) {
			count(c.Labels)
		}
	}
	return
}

// notProven reports launch evidence that claims more than was seen: a container
// found in the created state concretely never started; anything else adopted
// from a lost answer exists, and an unseen creation proves nothing.
func notProven(kind string, ev execution.ResourceStopEvidence) bool {
	if kind == "container" {
		return ev.Launch != execution.LaunchNotStarted && ev.Launch != execution.LaunchStarted
	}
	return ev.Launch == execution.LaunchNotStarted
}

// retire is the operator retiring the unresolved requests of a name, after
// verifying that none can arrive.
func retire(e *rEnv, t *testing.T, name string) {
	t.Helper()
	if err := podman.RetireUnresolvedRequest(e.state, name); err != nil {
		t.Fatal(err)
	}
}

// Test_podman_creation_resolution (review 4): a creation is announced before the
// engine is asked, and nothing turns an outstanding request into evidence. A lost
// answer is adopted when the object is found, however late it lands; until then
// the resource is unresolved (Observe unknown, Stop and Remove not confirmed, no
// second request, no placeholder); an operator retires the request explicitly.
func Test_podman_creation_resolution(t *testing.T) {
	ctx := context.Background()
	kinds := map[string]func(r run) execution.ResourceSpec{
		"network": func(r run) execution.ResourceSpec { return r.network() },
		"volume":  func(r run) execution.ResourceSpec { return r.workspace() },
		"container": func(r run) execution.ResourceSpec {
			return r.service("db", dbBody())
		},
	}
	setup := func(t *testing.T, kind string, fault string) (*rEnv, *podman.Resources, execution.ResourceSpec) {
		e := newSettleEnv(t)
		res := e.open()
		r := newRun(t)
		if kind == "container" {
			e.ensure(res, r.network())
		}
		spec := kinds[kind](r)
		e.fake.set(t, "create-fault", fault)
		if _, err := res.Ensure(ctx, spec); err == nil {
			t.Fatalf("%s: a lost create answer was accepted", kind)
		}
		e.fake.set(t, "create-fault", "")
		return e, res, spec
	}
	stop := func(t *testing.T, res *podman.Resources, h execution.ResourceHandle) execution.ResourceStopEvidence {
		t.Helper()
		ev, err := res.Stop(ctx, h)
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
		return ev
	}
	for kind := range kinds {
		t.Run(kind+": a lost answer is adopted at stop and removed", func(t *testing.T) {
			e, res, spec := setup(t, kind, kind+":lose")
			h := spec.Handle("")
			ev := stop(t, res, h)
			if !ev.Stopped || notProven(kind, ev) {
				t.Fatalf("a stop over a create whose answer was lost: %+v", ev)
			}
			if kind == "volume" && !ev.Released {
				t.Fatalf("%+v", ev)
			}
			if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
				t.Fatalf("remove: %+v %v", rm, err)
			}
			if real, ph := objects(e, t, kind); real != 0 || ph != 0 {
				t.Fatalf("the object the lost answer created leaked: %d real, %d placeholders", real, ph)
			}
		})

		t.Run(kind+": a delayed request keeps everything unconfirmed until it lands, then cleans up normally", func(t *testing.T) {
			e, res, spec := setup(t, kind, kind+":late")
			h := spec.Handle("")
			creates := func() int {
				return len(engineCalls(e.fake.argvs(t), "create", "volume create", "network create"))
			}
			issued := creates()
			// However long it has been, the resource is unresolved, never absent.
			for range 2 {
				if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseUnknown || obs.Reason != "create_unresolved" {
					t.Fatalf("an outstanding creation is %q, never absent: %+v %v", obs.Phase, obs, err)
				}
				time.Sleep(150 * time.Millisecond)
			}
			// Cleanup is attempted and refused, repeatedly.
			for range 2 {
				if ev := stop(t, res, h); ev.Stopped || ev.Launch == execution.LaunchNotStarted {
					t.Fatalf("a stop confirmed over a request that may still land: %+v", ev)
				}
				if _, err := res.Remove(ctx, h); !errors.Is(err, execution.ErrResourceFenced) {
					t.Fatalf("a removal over an unresolved request: %v", err)
				}
			}
			if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
				t.Fatalf("a stopping resource was ensured: %v", err)
			}
			// The adapter issued nothing more and created no placeholder.
			if creates() != issued {
				t.Fatalf("the adapter created something: %d creations, was %d", creates(), issued)
			}
			if real, ph := objects(e, t, kind); real != 0 || ph != 0 {
				t.Fatalf("%d real, %d placeholders before the request landed", real, ph)
			}
			// The request is delivered after the cleanup was attempted: found, adopted and
			// cleaned up like any other object.
			e.fake.ctl(t, "__land")
			ev := stop(t, res, h)
			if !ev.Stopped || notProven(kind, ev) {
				t.Fatalf("the stop after the object landed: %+v", ev)
			}
			if kind == "container" {
				for _, c := range e.fake.created(t) {
					if c.Status == "running" {
						t.Fatalf("the late container runs after the stop was confirmed: %+v", c)
					}
				}
			}
			if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
				t.Fatalf("remove: %+v %v", rm, err)
			}
			if real, ph := objects(e, t, kind); real != 0 || ph != 0 {
				t.Fatalf("leaked %d %s objects and %d placeholders", real, kind, ph)
			}
		})

		t.Run(kind+": an operator's retirement ends an unresolved request, only when nothing of ours exists", func(t *testing.T) {
			e, res, spec := setup(t, kind, kind+":late")
			h := spec.Handle("")
			if ev := stop(t, res, h); ev.Stopped {
				t.Fatalf("%+v", ev)
			}
			retire(e, t, spec.Name)
			ev := stop(t, res, h)
			if !ev.Stopped || ev.Launch != execution.LaunchUnknown {
				t.Fatalf("a retired request: %+v", ev)
			}
			if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
				t.Fatalf("%+v %v", rm, err)
			}
			if real, ph := objects(e, t, kind); real != 0 || ph != 0 {
				t.Fatalf("%d real, %d placeholders", real, ph)
			}

			// The retirement does not hide an object of ours that exists: it is found,
			// adopted and cleaned up.
			e, res, spec = setup(t, kind, kind+":late")
			h = spec.Handle("")
			e.fake.ctl(t, "__land")
			retire(e, t, spec.Name)
			ev = stop(t, res, h)
			if !ev.Stopped || notProven(kind, ev) {
				t.Fatalf("%+v", ev)
			}
			if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
				t.Fatalf("%+v %v", rm, err)
			}
			if real, ph := objects(e, t, kind); real != 0 || ph != 0 {
				t.Fatalf("a retired request's object was left: %d real, %d placeholders", real, ph)
			}
		})

		t.Run(kind+": no second request is issued while one is unresolved", func(t *testing.T) {
			e, res, spec := setup(t, kind, kind+":late")
			creates := func() int {
				return len(engineCalls(e.fake.argvs(t), "create", "volume create", "network create"))
			}
			issued := creates()
			// A retry would race the first request: it is refused, the resource stays
			// unknown, and nothing new is sent.
			for range 2 {
				if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrUnavailable) {
					t.Fatalf("a retry over an unresolved request: %v", err)
				}
			}
			if creates() != issued {
				t.Fatalf("a second creation request was issued: %d, was %d", creates(), issued)
			}
			if obs, err := res.Observe(ctx, spec.Handle("")); err != nil || obs.Phase != execution.PhaseUnknown {
				t.Fatalf("%+v %v", obs, err)
			}
			// The first request lands: the next ensure adopts it, once, and the cleanup
			// needs nothing else.
			e.fake.ctl(t, "__land")
			h := e.ensure(res, spec).Handle
			if n := unanswered(e, t, spec.Name); n != 0 {
				t.Fatalf("the adopted request is still counted: %d", n)
			}
			if ev := stop(t, res, h); !ev.Stopped {
				t.Fatalf("%+v", ev)
			}
			if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
				t.Fatalf("%+v %v", rm, err)
			}
			if real, ph := objects(e, t, kind); real != 0 || ph != 0 {
				t.Fatalf("%d real, %d placeholders", real, ph)
			}
		})
	}

	t.Run("a creation that was never issued proves not_started", func(t *testing.T) {
		e := newSettleEnv(t)
		res := e.open()
		spec := newRun(t).service("db", dbBody())
		// The run has no network: the create is refused before it is issued.
		if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("%v", err)
		}
		if len(engineCalls(e.fake.argvs(t), "create")) != 0 {
			t.Fatal("a create was issued")
		}
		if ev := stop(t, res, spec.Handle("")); !ev.Stopped || ev.Launch != execution.LaunchNotStarted {
			t.Fatalf("%+v", ev)
		}
	})

	t.Run("a foreign object that appears over an announced creation is a collision", func(t *testing.T) {
		e, res, spec := setup(t, "volume", "volume:late")
		e.fake.ctl(t, "volume", "create", spec.Name)
		if _, err := res.Stop(ctx, spec.Handle("")); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("%v", err)
		}
		if len(e.fake.volumes(t)) != 1 {
			t.Fatal("the foreign volume was touched")
		}
	})
}

// Test_podman_workspace_holder_intents (review 3): a preparation or attempt that
// is about to use the run workspace is a durable intent before its create is
// issued; an ambiguous create keeps it, an unresolved holder means the workspace
// is not released, and the late create can neither mount nor recreate the volume.
func Test_podman_workspace_holder_intents(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*rEnv, *podman.Resources, run, execution.ResourceHandle, execution.ResourceHandle) {
		e := newSettleEnv(t)
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		ws := e.ensure(res, r.workspace()).Handle
		return e, res, r, ws, execution.ResourceHandle{}
	}
	markers := func(e *rEnv, ws execution.ResourceHandle) []string {
		entries, _ := os.ReadDir(filepath.Join(e.state, "resources", ws.Name, "holders"))
		var out []string
		for _, f := range entries {
			out = append(out, f.Name())
		}
		return out
	}
	prepare := func(t *testing.T) (*rEnv, *podman.Resources, run, execution.ResourceHandle, execution.ResourceSpec) {
		e, res, r, ws, _ := setup(t)
		prep := r.preparation()
		e.fake.set(t, "create-fault", "container:late")
		if _, err := res.Ensure(ctx, prep); err == nil {
			t.Fatal("an ambiguous create was accepted")
		}
		e.fake.set(t, "create-fault", "")
		if got := markers(e, ws); len(got) != 1 {
			t.Fatalf("the holder intent was not kept across the ambiguous create: %v", got)
		}
		return e, res, r, ws, prep
	}

	t.Run("an ambiguous preparation create holds the workspace until it is observed or retired", func(t *testing.T) {
		e, res, _, ws, prep := prepare(t)
		creates := func() int { return len(engineCalls(e.fake.argvs(t), "create")) }
		issued := creates()
		if obs, err := res.Observe(ctx, ws); err != nil || obs.Released {
			t.Fatalf("an unresolved holder counted as released: %+v %v", obs, err)
		}
		for range 2 {
			if ev, err := res.Stop(ctx, ws); err != nil || !ev.Stopped || ev.Released {
				t.Fatalf("an unresolved holder: %+v %v", ev, err)
			}
			if rm, err := res.Remove(ctx, ws); err != nil || rm.Removed {
				t.Fatalf("the volume was removed under an unresolved holder: %+v %v", rm, err)
			}
		}
		if got := markers(e, ws); len(got) != 1 {
			t.Fatalf("the holder intent was dropped while unresolved: %v", got)
		}
		if creates() != issued || len(e.fake.created(t)) != 0 {
			t.Fatalf("the adapter created something to settle the holder")
		}
		// Delivery: the late preparation container is found, holds the workspace as a
		// bound container, and is cleaned up normally.
		e.fake.ctl(t, "__land")
		if obs, err := res.Observe(ctx, ws); err != nil || obs.Released {
			t.Fatalf("a landed preparation container does not hold the workspace: %+v %v", obs, err)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || rm.Removed {
			t.Fatalf("the volume was removed under a landed holder: %+v %v", rm, err)
		}
		for {
			ev, err := res.Stop(ctx, prep.Handle(""))
			if err != nil {
				t.Fatal(err)
			}
			if ev.Stopped {
				break
			}
		}
		if rm, err := res.Remove(ctx, prep.Handle("")); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
		if ev, err := res.Stop(ctx, ws); err != nil || !ev.Released {
			t.Fatalf("%+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
		if real, ph := objects(e, t, "container"); real != 0 || ph != 0 {
			t.Fatalf("%d containers, %d placeholders", real, ph)
		}
	})

	t.Run("an operator's retirement releases the workspace of a holder that never landed", func(t *testing.T) {
		e, res, _, ws, _ := prepare(t)
		if ev, err := res.Stop(ctx, ws); err != nil || ev.Released {
			t.Fatalf("%+v %v", ev, err)
		}
		retire(e, t, ws.Name)
		ev, err := res.Stop(ctx, ws)
		if err != nil || !ev.Stopped || !ev.Released {
			t.Fatalf("a retired holder: %+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
		if got := markers(e, ws); len(got) != 0 {
			t.Fatalf("%v", got)
		}
	})

	t.Run("a retirement does not hide a holder that exists", func(t *testing.T) {
		e, res, _, ws, _ := prepare(t)
		e.fake.ctl(t, "__land")
		retire(e, t, ws.Name)
		if ev, err := res.Stop(ctx, ws); err != nil || ev.Released {
			t.Fatalf("a retirement released the workspace under a real holder: %+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
	})

	t.Run("an ambiguous attempt create is a holder too", func(t *testing.T) {
		e, res, _, ws, _ := setup(t)
		rt := e.runtime(nil)
		e.fake.set(t, "create-fault", "container:late")
		s := runtimetest.SharedSpec(t, ws, runtimetest.HostImage, "true")
		if _, err := rt.Start(ctx, s); err == nil {
			t.Fatal("an ambiguous create was accepted")
		}
		e.fake.set(t, "create-fault", "")
		if got := markers(e, ws); len(got) != 1 {
			t.Fatalf("%v", got)
		}
		if obs, err := res.Observe(ctx, ws); err != nil || obs.Released {
			t.Fatalf("an unresolved attempt holder counted as released: %+v %v", obs, err)
		}
		for range 2 {
			if ev, err := res.Stop(ctx, ws); err != nil || ev.Released {
				t.Fatalf("%+v %v", ev, err)
			}
		}
		if len(e.fake.created(t)) != 0 {
			t.Fatalf("the adapter created a placeholder for the holder")
		}
		retire(e, t, ws.Name)
		if ev, err := res.Stop(ctx, ws); err != nil || !ev.Released {
			t.Fatalf("%+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
	})
}

// Test_podman_shared_attempt_redrive (re-review 5): a shared attempt has at most
// one create in flight. A redrive while an earlier create is unresolved issues no
// replacement (it cannot bind the holder to a second container, which would let
// the first land after the workspace was cleaned up), keeps the intent across
// redrives and restarts, and adopts the container once it is observed.
func Test_podman_shared_attempt_redrive(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*rEnv, *podman.Resources, *podman.Runtime, execution.AttemptSpec, execution.ResourceHandle) {
		e := newSettleEnv(t)
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		ws := e.ensure(res, r.workspace()).Handle
		rt := e.runtime(nil)
		s := runtimetest.SharedSpec(t, ws, runtimetest.HostImage, "true")
		e.fake.set(t, "create-fault", "container:late")
		if _, err := rt.Start(ctx, s); err == nil {
			t.Fatal("an ambiguous create was accepted")
		}
		e.fake.set(t, "create-fault", "")
		return e, res, rt, s, ws
	}
	creates := func(e *rEnv, t *testing.T) int { return len(engineCalls(e.fake.argvs(t), "create")) }
	markers := func(e *rEnv, ws execution.ResourceHandle) int {
		entries, _ := os.ReadDir(filepath.Join(e.state, "resources", ws.Name, "holders"))
		return len(entries)
	}
	// held: the workspace is not released while the attempt's create is unresolved.
	// Observe changes nothing; a Stop or Remove of the workspace also records its stop
	// intent, after which no attempt can start on it any more.
	held := func(t *testing.T, res *podman.Resources, ws execution.ResourceHandle) {
		t.Helper()
		if obs, err := res.Observe(ctx, ws); err != nil || obs.Released {
			t.Fatalf("an unresolved attempt create counted as released: %+v %v", obs, err)
		}
	}
	unconfirmed := func(t *testing.T, res *podman.Resources, ws execution.ResourceHandle) {
		t.Helper()
		held(t, res, ws)
		if ev, err := res.Stop(ctx, ws); err != nil || ev.Released {
			t.Fatalf("%+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || rm.Removed {
			t.Fatalf("the workspace was removed under an unresolved attempt create: %+v %v", rm, err)
		}
	}

	t.Run("a redrive before delivery issues no second create; delivery is adopted and cleaned up", func(t *testing.T) {
		e, res, rt, s, ws := setup(t)
		issued := creates(e, t)
		for i := range 3 {
			if i == 2 {
				rt = e.runtime(nil) // a restarted controller
			}
			if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrUnavailable) {
				t.Fatalf("a redrive over an unresolved create: %v", err)
			}
			if creates(e, t) != issued || len(e.fake.created(t)) != 0 {
				t.Fatalf("a second container was created: %d creates, was %d", creates(e, t), issued)
			}
			if markers(e, ws) != 1 {
				t.Fatalf("the intent did not survive the redrive: %d", markers(e, ws))
			}
			held(t, res, ws)
		}
		// Delivery: the redrive adopts the container (binding the holder) and starts it.
		e.fake.ctl(t, "__land")
		w, err := rt.Start(ctx, s)
		if err != nil {
			t.Fatalf("the redrive after delivery: %v", err)
		}
		if creates(e, t) != issued {
			t.Fatal("a second create was issued after delivery")
		}
		unconfirmed(t, res, ws) // the container holds the workspace
		if _, err := rt.Stop(ctx, w); err != nil {
			t.Fatal(err)
		}
		if err := rt.Remove(ctx, w); err != nil {
			t.Fatal(err)
		}
		if ev, err := res.Stop(ctx, ws); err != nil || !ev.Released {
			t.Fatalf("%+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
		if n := len(e.fake.created(t)); n != 0 {
			t.Fatalf("%d containers left", n)
		}
	})

	t.Run("the workspace cleanup holds until the create is delivered, then adopts it", func(t *testing.T) {
		e, res, rt, s, ws := setup(t)
		if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		unconfirmed(t, res, ws)
		unconfirmed(t, res, ws)
		if _, err := rt.Start(ctx, s); err == nil {
			t.Fatal("an attempt started on a stopping workspace")
		}
		e.fake.ctl(t, "__land")
		// Found and bound by the workspace's own cleanup; it still holds the volume
		// until the container is gone.
		unconfirmed(t, res, ws)
		var name string
		for _, c := range e.fake.created(t) {
			name = c.Name
		}
		if name == "" {
			t.Fatal("the late attempt container did not land")
		}
		e.fake.ctl(t, "rm", name)
		if ev, err := res.Stop(ctx, ws); err != nil || !ev.Released {
			t.Fatalf("%+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
		if markers(e, ws) != 0 {
			t.Fatalf("%d markers left", markers(e, ws))
		}
	})

	t.Run("an operator's retirement ends the unresolved create; a later ambiguous one is a new intent", func(t *testing.T) {
		e, res, rt, s, ws := setup(t)
		if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		issued := creates(e, t)
		retire(e, t, ws.Name)
		// The redrive may now create anew, once; this create is ambiguous again.
		e.fake.set(t, "create-fault", "container:late")
		if _, err := rt.Start(ctx, s); err == nil {
			t.Fatal("an ambiguous create was accepted")
		}
		e.fake.set(t, "create-fault", "")
		if creates(e, t) != issued+1 {
			t.Fatalf("%d creates, want %d", creates(e, t), issued+1)
		}
		// The new request is not covered by the earlier retirement.
		if markers(e, ws) != 1 {
			t.Fatalf("%d", markers(e, ws))
		}
		if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrUnavailable) || creates(e, t) != issued+1 {
			t.Fatalf("a redrive over the new unresolved create: %v", err)
		}
		unconfirmed(t, res, ws)
		retire(e, t, ws.Name)
		if ev, err := res.Stop(ctx, ws); err != nil || !ev.Released {
			t.Fatalf("%+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, ws); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
	})
}

// Test_podman_retirement_epoch_unavailable (re-review 6): only the genuine absence
// of the retirement file means "no retirement yet". A file that cannot be read, or
// whose contents are not an epoch, blocks every announcement (no create is issued
// under a guessed epoch) and every retirement decision, so that when it recovers
// the old authorization cannot retire a request that was announced during the
// outage. Every path reports unresolved/unavailable and retries.
func Test_podman_retirement_epoch_unavailable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not bind root")
	}
	ctx := context.Background()
	epochFile := func(e *rEnv, name string) string {
		return filepath.Join(e.state, "resources", name, "request-retired")
	}
	// faults: how the epoch file is made unavailable, and how it is restored.
	faults := map[string]func(t *testing.T, path string) (restore func()){
		"unreadable": func(t *testing.T, path string) func() {
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			return func() { _ = os.Chmod(path, 0o600) }
		},
		"malformed": func(t *testing.T, path string) func() {
			orig, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("not-an-epoch\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return func() { _ = os.WriteFile(path, orig, 0o600) }
		},
	}
	creates := func(e *rEnv, t *testing.T) int {
		return len(engineCalls(e.fake.argvs(t), "create", "volume create", "network create"))
	}
	for fault, apply := range faults {
		t.Run(fault+": a resource create is not announced, and the old retirement does not end the next request", func(t *testing.T) {
			e := newSettleEnv(t)
			res := e.open()
			r := newRun(t)
			spec := r.workspace()
			h := spec.Handle("")
			e.fake.set(t, "create-fault", "volume:late")
			if _, err := res.Ensure(ctx, spec); err == nil {
				t.Fatal("an ambiguous create was accepted")
			}
			retire(e, t, spec.Name) // epoch 1 ends request A
			if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseAbsent {
				t.Fatalf("the retired request is not settled: %+v %v", obs, err)
			}
			issued := creates(e, t)

			restore := apply(t, epochFile(e, spec.Name))
			if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrUnavailable) {
				t.Fatalf("a create over an unavailable epoch: %v", err)
			}
			if creates(e, t) != issued || unanswered(e, t, spec.Name) != 0 {
				t.Fatalf("a request was announced or issued without an epoch: %d creates, %d unanswered", creates(e, t), unanswered(e, t, spec.Name))
			}
			restore()

			// Access is back: the new request is announced under epoch 1 and the old
			// retirement does not end it.
			if _, err := res.Ensure(ctx, spec); err == nil {
				t.Fatal("an ambiguous create was accepted")
			}
			if creates(e, t) != issued+1 {
				t.Fatalf("%d creates, want %d", creates(e, t), issued+1)
			}
			if ev, err := res.Stop(ctx, h); err != nil || ev.Stopped {
				t.Fatalf("a retirement made before the request ended it: %+v %v", ev, err)
			}
			// While the epoch is unavailable the outstanding request is not retired
			// either, even by a newer retirement the adapter cannot read.
			retire(e, t, spec.Name)
			restore = apply(t, epochFile(e, spec.Name))
			if ev, err := res.Stop(ctx, h); ev.Stopped || err == nil {
				t.Fatalf("a request retired over an unavailable epoch: %+v %v", ev, err)
			}
			if rm, err := res.Remove(ctx, h); rm.Removed {
				t.Fatalf("a removal over an unavailable epoch: %+v %v", rm, err)
			}
			if obs, err := res.Observe(ctx, h); err == nil && obs.Phase == execution.PhaseAbsent {
				t.Fatalf("an outstanding request was reported absent: %+v", obs)
			}
			restore()
			if ev, err := res.Stop(ctx, h); err != nil || !ev.Stopped {
				t.Fatalf("the operator's retirement did not take effect after recovery: %+v %v", ev, err)
			}
		})

		t.Run(fault+": a probe create is not announced under a guessed epoch", func(t *testing.T) {
			e := newSettleEnv(t)
			res := e.open()
			r := newRun(t)
			e.ensure(res, r.network())
			body := dbBody()
			body.Readiness = execution.ReadinessSpec{Probe: "tcp", Port: "sql", TimeoutSecs: 30, ProbeTimeout: 3}
			spec := r.service("db", body)
			h := e.ensure(res, spec).Handle
			e.fake.set(t, "create-fault", "container:late")
			if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseUnknown {
				t.Fatalf("%+v %v", obs, err)
			}
			retire(e, t, spec.Name)
			e.fake.set(t, "create-fault", "")
			// The outstanding probe create A is settled by the retirement.
			if obs, err := res.Observe(ctx, h); err != nil || obs.Phase == execution.PhaseUnknown {
				t.Fatalf("%+v %v", obs, err)
			}
			issued := len(engineCalls(e.fake.argvs(t), "create"))
			restore := apply(t, epochFile(e, spec.Name))
			e.fake.set(t, "create-fault", "container:late")
			for range 2 {
				_, _ = res.Observe(ctx, h)
			}
			if n := len(engineCalls(e.fake.argvs(t), "create")); n != issued {
				t.Fatalf("a probe create was issued without an epoch: %d, was %d", n, issued)
			}
			restore()
			// A new probe create announced now is under epoch 1: not ended by it.
			_, _ = res.Observe(ctx, h)
			if ev, err := res.Stop(ctx, h); err != nil || ev.Stopped {
				t.Fatalf("a retirement made before the request ended it: %+v %v", ev, err)
			}
		})

		t.Run(fault+": a shared attempt is not created, and a holder is not retired, over an unavailable epoch", func(t *testing.T) {
			e := newSettleEnv(t)
			res := e.open()
			r := newRun(t)
			e.ensure(res, r.network())
			ws := e.ensure(res, r.workspace()).Handle
			rt := e.runtime(nil)
			s := runtimetest.SharedSpec(t, ws, runtimetest.HostImage, "true")
			e.fake.set(t, "create-fault", "container:late")
			if _, err := rt.Start(ctx, s); err == nil {
				t.Fatal("an ambiguous create was accepted")
			}
			e.fake.set(t, "create-fault", "")
			retire(e, t, ws.Name) // epoch 1 covers attempt create A
			if obs, err := res.Observe(ctx, ws); err != nil || !obs.Released {
				t.Fatalf("the retired holder still holds the workspace: %+v %v", obs, err)
			}
			issued := creates(e, t)
			markers := func() int {
				entries, _ := os.ReadDir(filepath.Join(e.state, "resources", ws.Name, "holders"))
				return len(entries)
			}

			restore := apply(t, epochFile(e, ws.Name))
			if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrUnavailable) {
				t.Fatalf("a redrive over an unavailable epoch: %v", err)
			}
			if creates(e, t) != issued || markers() != 0 {
				t.Fatalf("an attempt create was issued under a guessed epoch: %d creates (was %d), %d markers", creates(e, t), issued, markers())
			}
			restore()

			// The redrive now creates B under epoch 1; the old authorization does not
			// retire B.
			e.fake.set(t, "create-fault", "container:late")
			if _, err := rt.Start(ctx, s); err == nil {
				t.Fatal("an ambiguous create was accepted")
			}
			e.fake.set(t, "create-fault", "")
			if creates(e, t) != issued+1 {
				t.Fatalf("%d creates, want %d", creates(e, t), issued+1)
			}
			if obs, err := res.Observe(ctx, ws); err != nil || obs.Released {
				t.Fatalf("the old retirement ended the new create: %+v %v", obs, err)
			}
		})
	}

	t.Run("an absent file is epoch zero and the operator tool refuses an unavailable one", func(t *testing.T) {
		e := newSettleEnv(t)
		res := e.open()
		r := newRun(t)
		spec := r.workspace()
		e.ensure(res, spec)
		retire(e, t, spec.Name)
		path := epochFile(e, spec.Name)
		if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := podman.RetireUnresolvedRequest(e.state, spec.Name); err == nil {
			t.Fatal("a retirement was recorded over a malformed epoch")
		}
		if raw, _ := os.ReadFile(path); string(raw) != "garbage\n" {
			t.Fatalf("the malformed epoch file was overwritten: %q", raw)
		}
	})
}

// Test_podman_probe_containers (review 4): the tcp-probe container is created by
// a recorded intent whose ID is bound as soon as the create is answered, and it is
// deleted only by that ID; an outstanding creation is resolved by the name with
// the recorded nonce, or kept unresolved; and a replacement that copies every label, or a
// foreign container at the name, is never deleted.
func Test_podman_probe_containers(t *testing.T) {
	ctx := context.Background()
	tcpService := func(t *testing.T) (*rEnv, *podman.Resources, execution.ResourceSpec, execution.ResourceHandle) {
		e := newSettleEnv(t)
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		body := dbBody()
		body.Readiness = execution.ReadinessSpec{Probe: "tcp", Port: "sql", TimeoutSecs: 30, ProbeTimeout: 3}
		spec := r.service("db", body)
		return e, res, spec, e.ensure(res, spec).Handle
	}
	probes := func(e *rEnv, t *testing.T, name string) []createdContainer {
		var out []createdContainer
		for _, c := range e.fake.created(t) {
			if c.Name == name {
				out = append(out, c)
			}
		}
		return out
	}
	record := func(e *rEnv, t *testing.T, name string) (nonce, id string) {
		raw, err := os.ReadFile(filepath.Join(e.state, "resources", name, "record.json"))
		if err != nil {
			t.Fatal(err)
		}
		var rec struct {
			ProbeSlot *struct {
				Nonce string `json:"nonce"`
				ID    string `json:"id"`
			} `json:"probeSlot"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil || rec.ProbeSlot == nil {
			t.Fatalf("no probe slot: %s %v", raw, err)
		}
		return rec.ProbeSlot.Nonce, rec.ProbeSlot.ID
	}

	t.Run("a probe is created by intent, bound by ID and deleted by that ID", func(t *testing.T) {
		e, res, spec, h := tcpService(t)
		before := len(e.fake.argvs(t))
		if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhasePending {
			t.Fatalf("%+v %v", obs, err)
		}
		var verbs []string
		var id string
		for _, a := range e.fake.argvs(t)[before:] {
			switch {
			case a[0] == "create" && slices.Contains(a, fakeProbeName(spec)):
				verbs = append(verbs, "create")
				if !slices.Contains(a, podman.LabelProbeNonce+"="+func() string { n, _ := record(e, t, spec.Name); return n }()) {
					t.Fatalf("the probe carries no nonce: %v", a)
				}
			case a[0] == "start" && slices.Contains(a, "--attach"):
				verbs = append(verbs, "start")
				id = a[len(a)-1]
			case a[0] == "rm":
				verbs = append(verbs, "rm")
				if a[len(a)-1] != id || len(id) != 64 {
					t.Fatalf("the probe was not deleted by its bound ID: %v (bound %q)", a, id)
				}
			}
		}
		if !slices.Equal(verbs, []string{"create", "start", "rm"}) {
			t.Fatalf("probe sequence %v", verbs)
		}
		if _, bound := record(e, t, spec.Name); bound != "" {
			t.Fatalf("the probe ID stays bound after its deletion: %q", bound)
		}
		if len(probes(e, t, fakeProbeName(spec))) != 0 {
			t.Fatal("a probe container remains")
		}
	})

	t.Run("a replacement carrying every original label is never deleted", func(t *testing.T) {
		e, res, spec, h := tcpService(t)
		// The probe's deletion fails: its ID stays bound in the record.
		e.fake.set(t, "fail-rm-probe", "1")
		if _, err := res.Observe(ctx, h); err != nil {
			t.Fatal(err)
		}
		nonce, id := record(e, t, spec.Name)
		if id == "" {
			t.Fatal("the probe's ID was not bound")
		}
		var original createdContainer
		for _, c := range probes(e, t, fakeProbeName(spec)) {
			original = c
		}
		if original.ID != id {
			t.Fatalf("the bound ID %q is not the container %q", id, original.ID)
		}
		// The container is replaced by another that copies all of its labels.
		e.fake.set(t, "fail-rm-probe", "")
		e.fake.ctl(t, "rm", id)
		args := []string{}
		for k, v := range original.Labels {
			args = append(args, k+"="+v)
		}
		if original.Labels[podman.LabelProbeNonce] != nonce {
			t.Fatalf("labels %v", original.Labels)
		}
		replacement := e.fake.ctl(t, append([]string{"__plant", fakeProbeName(spec)}, args...)...)
		if replacement == id {
			t.Fatal("the replacement has the same ID")
		}
		for _, step := range []string{"observe", "stop", "remove"} {
			switch step {
			case "observe":
				if obs, err := res.Observe(ctx, h); !errors.Is(err, execution.ErrResourceCollision) && (err != nil || obs.Phase == execution.PhaseReady) {
					t.Fatalf("%s: %+v %v", step, obs, err)
				}
			case "stop":
				if _, err := res.Stop(ctx, h); !errors.Is(err, execution.ErrResourceCollision) {
					t.Fatalf("%s: %v", step, err)
				}
			case "remove":
				_, _ = res.Remove(ctx, h)
			}
			if len(probes(e, t, fakeProbeName(spec))) != 1 || probes(e, t, fakeProbeName(spec))[0].ID != replacement {
				t.Fatalf("%s: the replacement was deleted", step)
			}
		}
		for _, a := range e.fake.argvs(t) {
			if (a[0] == "rm" || a[0] == "stop") && a[len(a)-1] == replacement {
				t.Fatalf("the replacement was addressed: %v", a)
			}
		}
	})

	t.Run("an outstanding probe create whose answer was lost is found by name and nonce", func(t *testing.T) {
		// The probe create's answer is lost although the engine created it.
		e, res, spec, h := tcpService(t)
		e.fake.set(t, "create-fault", "container:lose")
		if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseUnknown {
			t.Fatalf("an unanswered probe create: %+v %v", obs, err)
		}
		e.fake.set(t, "create-fault", "")
		if n := len(probes(e, t, fakeProbeName(spec))); n != 1 {
			t.Fatalf("the engine did not create the probe: %d", n)
		}
		if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhasePending {
			t.Fatalf("%+v %v", obs, err)
		}
		if n := len(probes(e, t, fakeProbeName(spec))); n != 0 {
			t.Fatalf("the landed probe was not deleted: %d", n)
		}
	})

	delayedProbe := func(t *testing.T) (*rEnv, *podman.Resources, execution.ResourceSpec, execution.ResourceHandle) {
		e, res, spec, h := tcpService(t)
		e.fake.set(t, "create-fault", "container:late")
		if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseUnknown {
			t.Fatalf("%+v %v", obs, err)
		}
		e.fake.set(t, "create-fault", "")
		return e, res, spec, h
	}

	t.Run("a delayed probe create keeps stop and removal unconfirmed until it lands", func(t *testing.T) {
		e, res, spec, h := delayedProbe(t)
		creates := func() int { return len(engineCalls(e.fake.argvs(t), "create")) }
		issued := creates()
		for range 2 {
			if ev, err := res.Stop(ctx, h); err != nil || ev.Stopped {
				t.Fatalf("a stop confirmed over an outstanding probe create: %+v %v", ev, err)
			}
			if _, err := res.Remove(ctx, h); !errors.Is(err, execution.ErrResourceFenced) {
				t.Fatalf("a removal over an outstanding probe create: %v", err)
			}
		}
		if creates() != issued || len(probes(e, t, fakeProbeName(spec))) != 0 {
			t.Fatal("the adapter created a placeholder for the probe")
		}
		// Delivery: the probe is found by name and nonce and deleted; then the
		// cleanup completes with nothing left.
		e.fake.ctl(t, "__land")
		if ev, err := res.Stop(ctx, h); err != nil || !ev.Stopped {
			t.Fatalf("%+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
		if n := len(e.fake.created(t)); n != 0 {
			t.Fatalf("containers remain: %d", n)
		}
	})

	t.Run("an operator's retirement ends an outstanding probe create", func(t *testing.T) {
		e, res, spec, h := delayedProbe(t)
		if ev, err := res.Stop(ctx, h); err != nil || ev.Stopped {
			t.Fatalf("%+v %v", ev, err)
		}
		retire(e, t, spec.Name)
		if ev, err := res.Stop(ctx, h); err != nil || !ev.Stopped {
			t.Fatalf("%+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
		if n := len(e.fake.created(t)); n != 0 {
			t.Fatalf("containers remain: %d", n)
		}
	})

	t.Run("a retirement covers only the requests announced before it", func(t *testing.T) {
		e, res, spec, h := delayedProbe(t)
		retire(e, t, spec.Name)
		// A later probe create is ambiguous again: the old retirement does not end it.
		e.fake.set(t, "create-fault", "container:late")
		for range 2 {
			if obs, err := res.Observe(ctx, h); err != nil || obs.Phase == execution.PhaseReady {
				t.Fatalf("%+v %v", obs, err)
			}
		}
		e.fake.set(t, "create-fault", "")
		if ev, err := res.Stop(ctx, h); err != nil || ev.Stopped {
			t.Fatalf("a retirement made before the request ended it: %+v %v", ev, err)
		}
		retire(e, t, spec.Name)
		if ev, err := res.Stop(ctx, h); err != nil || !ev.Stopped {
			t.Fatalf("%+v %v", ev, err)
		}
	})

	t.Run("a foreign container at the probe name is never touched", func(t *testing.T) {
		e, res, spec, h := tcpService(t)
		foreign := e.fake.ctl(t, "__plant", fakeProbeName(spec))
		if obs, err := res.Observe(ctx, h); !errors.Is(err, execution.ErrResourceCollision) && (err != nil || obs.Phase == execution.PhaseReady) {
			t.Fatalf("a foreign probe name: %+v %v", obs, err)
		}
		_, _ = res.Stop(ctx, h)
		_, _ = res.Remove(ctx, h)
		for _, a := range e.fake.argvs(t) {
			if (a[0] == "rm" || a[0] == "stop") && a[len(a)-1] == foreign {
				t.Fatalf("the foreign container was addressed: %v", a)
			}
		}
		if len(probes(e, t, fakeProbeName(spec))) != 1 {
			t.Fatal("the foreign container is gone")
		}
	})
}

// Test_podman_workspace_release_identity (review 5): a workspace is reported
// released only after its recorded volume instance is verified; a replaced or
// uninspectable volume is never an affirmative release.
func Test_podman_workspace_release_identity(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*rEnv, *podman.Resources, execution.ResourceHandle, execution.ResourceSpec) {
		e := newSettleEnv(t)
		res := e.open()
		spec := newRun(t).workspace()
		return e, res, e.ensure(res, spec).Handle, spec
	}
	labelsOf := func(e *rEnv, t *testing.T, name string) []string {
		var args []string
		for k, v := range e.fake.volumes(t)[name].Labels {
			args = append(args, "--label", k+"="+v)
		}
		return args
	}

	t.Run("a replaced volume is a collision, not a release", func(t *testing.T) {
		e, res, h, spec := setup(t)
		e.fake.ctl(t, "volume", "rm", spec.Name)
		e.fake.ctl(t, "volume", "create", "--label", "foreign=1", spec.Name)
		if ev, err := res.Stop(ctx, h); !errors.Is(err, execution.ErrResourceCollision) || ev.Released || ev.Stopped {
			t.Fatalf("stop over a replaced volume: %+v %v", ev, err)
		}
		if _, err := res.Remove(ctx, h); err == nil {
			t.Fatal("remove over a replaced volume")
		}
		if _, ok := e.fake.volumes(t)[spec.Name]; !ok {
			t.Fatal("the replacement volume was removed")
		}
	})

	t.Run("a lookalike with the same labels but another instance is a collision", func(t *testing.T) {
		e, res, h, spec := setup(t)
		args := labelsOf(e, t, spec.Name)
		e.fake.ctl(t, "volume", "rm", spec.Name)
		time.Sleep(5 * time.Millisecond)
		e.fake.ctl(t, append(append([]string{"volume", "create"}, args...), spec.Name)...)
		if ev, err := res.Stop(ctx, h); !errors.Is(err, execution.ErrResourceCollision) || ev.Released {
			t.Fatalf("%+v %v", ev, err)
		}
	})

	t.Run("an uninspectable volume is never released", func(t *testing.T) {
		e, res, h, _ := setup(t)
		restore := e.fake.outage(t)
		defer restore()
		if ev, err := res.Stop(ctx, h); err == nil && (ev.Released || ev.Stopped && ev.Released) {
			t.Fatalf("a release during an outage: %+v", ev)
		}
		restore()
		if ev, err := res.Stop(ctx, h); err != nil || !ev.Stopped || !ev.Released {
			t.Fatalf("after the outage: %+v %v", ev, err)
		}
	})

	t.Run("a volume podman says is gone holds nothing", func(t *testing.T) {
		e, res, h, spec := setup(t)
		e.fake.ctl(t, "volume", "rm", spec.Name)
		if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseAbsent {
			t.Fatalf("%+v %v", obs, err)
		}
		if ev, err := res.Stop(ctx, h); err != nil || !ev.Stopped || !ev.Released {
			t.Fatalf("%+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
	})
}

// Test_podman_stop_intent_is_not_a_stop (review 7): a stop that was requested but
// not verified is not observed as stopped and does not allow removal.
func Test_podman_stop_intent_is_not_a_stop(t *testing.T) {
	ctx := context.Background()

	t.Run("a container the engine could not stop", func(t *testing.T) {
		e := newSettleEnv(t)
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		spec := r.service("db", dbBody())
		h := e.ensure(res, spec).Handle
		e.fake.set(t, "fail-stop", "1")
		if ev, err := res.Stop(ctx, h); err == nil || ev.Stopped {
			t.Fatalf("a failed stop: %+v %v", ev, err)
		}
		obs, err := res.Observe(ctx, h)
		if err != nil || obs.Phase != execution.PhasePending || obs.Reason != execution.ReasonStopUnconfirmed {
			t.Fatalf("a stop intent observed as %q: %+v %v", obs.Phase, obs, err)
		}
		if _, err := res.Remove(ctx, h); !errors.Is(err, execution.ErrResourceFenced) {
			t.Fatalf("remove after an unverified stop: %v", err)
		}
		if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
			t.Fatalf("the intent did not fence: %v", err)
		}
		e.fake.set(t, "fail-stop", "")
		if ev, err := res.Stop(ctx, h); err != nil || !ev.Stopped || ev.Launch != execution.LaunchStarted {
			t.Fatalf("%+v %v", ev, err)
		}
		if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseStopped {
			t.Fatalf("%+v %v", obs, err)
		}
		if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
			t.Fatalf("%+v %v", rm, err)
		}
	})

	t.Run("a workspace whose stop could not be verified", func(t *testing.T) {
		e := newSettleEnv(t)
		res := e.open()
		h := e.ensure(res, newRun(t).workspace()).Handle
		restore := e.fake.outage(t)
		defer restore()
		if ev, err := res.Stop(ctx, h); err == nil && ev.Stopped {
			t.Fatalf("%+v", ev)
		}
		restore()
		obs, err := res.Observe(ctx, h)
		if err != nil || obs.Phase != execution.PhasePending || obs.Reason != execution.ReasonStopUnconfirmed {
			t.Fatalf("an interrupted workspace stop observed as %q: %+v %v", obs.Phase, obs, err)
		}
		if _, err := res.Remove(ctx, h); !errors.Is(err, execution.ErrResourceFenced) {
			t.Fatalf("%v", err)
		}
		if ev, err := res.Stop(ctx, h); err != nil || !ev.Stopped || !ev.Released {
			t.Fatalf("%+v %v", ev, err)
		}
		if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseStopped {
			t.Fatalf("%+v %v", obs, err)
		}
	})
}

// Test_podman_user_namespace (review 8): a role that runs the workload in a user
// namespace of its own gets one (--userns=auto), verified on the created
// container; an engine that did not apply it never starts the container, and a
// role that adds capabilities outside a user namespace is refused.
func Test_podman_user_namespace(t *testing.T) {
	ctx := context.Background()

	t.Run("an attempt with the pod role gets a verified namespace of its own", func(t *testing.T) {
		e := newSettleEnv(t)
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		ws := e.ensure(res, r.workspace())
		rt := e.runtime(nil)
		s := runtimetest.SharedSpec(t, ws.Handle, runtimetest.HostImage, "true")
		w, err := rt.Start(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		var got createdContainer
		for _, c := range e.fake.created(t) {
			if c.Name == w.Name {
				got = c
			}
		}
		if !slices.Contains(got.Flags, "--userns=auto") {
			t.Fatalf("no per-container user namespace: %v", got.Flags)
		}
	})

	t.Run("an engine that ignored it never starts the attempt", func(t *testing.T) {
		e := newSettleEnv(t)
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		ws := e.ensure(res, r.workspace())
		e.fake.set(t, "no-userns", "1")
		rt := e.runtime(nil)
		s := runtimetest.SharedSpec(t, ws.Handle, runtimetest.HostImage, "true")
		if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("an attempt the engine did not isolate: %v", err)
		}
		if slices.Contains(e.fake.calls(t), "start") {
			t.Fatalf("an unisolated container was started: %v", e.fake.calls(t))
		}
		if len(e.fake.created(t)) != 0 {
			t.Fatal("the unisolated container was kept")
		}
	})

	t.Run("an engine that ignored it never starts a service or a preparation", func(t *testing.T) {
		e := newSettleEnv(t)
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		e.ensure(res, r.workspace())
		e.fake.set(t, "no-userns", "1")
		for _, spec := range []execution.ResourceSpec{r.service("db", dbBody()), r.preparation()} {
			if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrNotConfigured) {
				t.Fatalf("%s: %v", spec.Key, err)
			}
		}
		if slices.Contains(e.fake.calls(t), "start") || len(e.fake.created(t)) != 0 {
			t.Fatalf("an unisolated container was started or kept: %v", e.fake.calls(t))
		}
	})

	t.Run("capabilities outside a user namespace are refused, and no namespace is asked without one", func(t *testing.T) {
		e := newSettleEnv(t)
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		ws := e.ensure(res, r.workspace())
		rt := e.runtime(nil)
		set := func(mutate func(*execution.RoleShape)) execution.AttemptSpec {
			s := runtimetest.SharedSpec(t, ws.Handle, runtimetest.HostImage, "true")
			role := s.Shared.Role
			mutate(&role)
			s.Shared.Role = role
			s.Shared.RoleDigest, _ = role.Digest()
			return s
		}
		s := set(func(role *execution.RoleShape) {
			role.UserNamespace, role.CapabilitiesAdd = "", []string{"SETUID", "SETGID"}
		})
		if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("capabilities without a user namespace: %v", err)
		}
		if len(e.fake.created(t)) != 0 {
			t.Fatal("a container was created for a refused role")
		}
		s = set(func(role *execution.RoleShape) { role.UserNamespace = "" })
		if _, err := rt.Start(ctx, s); err != nil {
			t.Fatal(err)
		}
		for _, c := range e.fake.created(t) {
			if c.Name == s.WorkloadName && slices.Contains(c.Flags, "--userns=auto") {
				t.Fatalf("a namespace was asked for a role without one: %v", c.Flags)
			}
		}
	})
}

// Test_podman_record_loss_keeps_identity (review 9): stopping and removing a
// resource nobody recorded keeps the identity the caller supplied, repeats
// without collision and never claims a launch or a not-started proof.
func Test_podman_record_loss_keeps_identity(t *testing.T) {
	ctx := context.Background()
	e := newSettleEnv(t)
	res := e.open()
	spec := newRun(t).service("db", dbBody())
	bound := spec.Handle("66666666-6666-4666-8666-666666666666")

	for range 3 {
		ev, err := res.Stop(ctx, bound)
		if err != nil || !ev.Stopped || ev.Launch != execution.LaunchUnknown {
			t.Fatalf("stop after record loss: %+v %v", ev, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(e.state, "resources", bound.Name, "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		UID       string `json:"uid"`
		Untracked bool   `json:"untracked"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil || rec.UID != bound.UID || !rec.Untracked {
		t.Fatalf("the tombstone does not keep the supplied identity: %s %v", raw, err)
	}
	for range 3 {
		if rm, err := res.Remove(ctx, bound); err != nil || !rm.Removed {
			t.Fatalf("remove after record loss: %+v %v", rm, err)
		}
	}
	if obs, err := res.Observe(ctx, bound); err != nil || obs.Phase != execution.PhaseAbsent {
		t.Fatalf("%+v %v", obs, err)
	}
	if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
		t.Fatalf("a lost-record tombstone did not fence: %v", err)
	}
	other := bound
	other.UID = "55555555-5555-4555-8555-555555555555"
	if _, err := res.Stop(ctx, other); !errors.Is(err, execution.ErrResourceCollision) {
		t.Fatalf("another instance's stop: %v", err)
	}
	// A tombstone made for an unbound handle accepts the instance bound later.
	spec2 := newRun(t).workspace()
	if ev, err := res.Stop(ctx, spec2.Handle("")); err != nil || !ev.Stopped {
		t.Fatalf("%+v %v", ev, err)
	}
	if ev, err := res.Stop(ctx, spec2.Handle("44444444-4444-4444-8444-444444444444")); err != nil || !ev.Stopped || ev.Launch != execution.LaunchUnknown {
		t.Fatalf("a later bound stop: %+v %v", ev, err)
	}
	for _, c := range engineCalls(e.fake.argvs(t), "create", "volume create", "network create", "rm", "stop", "volume rm", "network rm") {
		t.Fatalf("a record-less stop or remove touched the engine: %v", c)
	}
}

// unanswered reads the number of creation requests a record holds unanswered.
func unanswered(e *rEnv, t *testing.T, name string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.state, "resources", name, "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Unanswered int `json:"unanswered"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec.Unanswered
}

// Test_podman_unanswered_requests_are_counted (re-review 1, 2): at most one
// request is in flight for a name; the answer is recorded together with the object
// it made, so a crash between them cannot count a request twice; a request whose
// answer was never recorded is neither retired by a retry (none is issued) nor
// forgotten by a restart, and the cleanup is not confirmed until it is handled.
func Test_podman_unanswered_requests_are_counted(t *testing.T) {
	ctx := context.Background()
	kinds := map[string]func(r run) execution.ResourceSpec{
		"volume":    func(r run) execution.ResourceSpec { return r.workspace() },
		"container": func(r run) execution.ResourceSpec { return r.service("db", dbBody()) },
	}
	crash := func(res *podman.Resources) {
		res.InjectCrash(func(point string) error {
			if point == "sent" {
				return errors.New("the controller died")
			}
			return nil
		})
	}
	for kind, mk := range kinds {
		t.Run(kind+": a request sent before the crash is still outstanding after the restart", func(t *testing.T) {
			e := newSettleEnv(t)
			first := e.open()
			r := newRun(t)
			if kind == "container" {
				e.ensure(first, r.network())
			}
			spec := mk(r)
			crash(first)
			e.fake.set(t, "create-fault", kind+":late")
			if _, err := first.Ensure(ctx, spec); err == nil {
				t.Fatal("the crash was not simulated")
			}
			e.fake.set(t, "create-fault", "")
			if n := unanswered(e, t, spec.Name); n != 1 {
				t.Fatalf("the unanswered request was not kept: %d", n)
			}
			// The restarted controller does not race the first request with a retry.
			res := e.open()
			before := len(engineCalls(e.fake.argvs(t), "create", "volume create"))
			if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrUnavailable) {
				t.Fatalf("a retry over an outstanding request: %v", err)
			}
			if after := len(engineCalls(e.fake.argvs(t), "create", "volume create")); after != before {
				t.Fatal("a second request was issued")
			}
			h := spec.Handle("")
			if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseUnknown {
				t.Fatalf("%+v %v", obs, err)
			}
			// Cleanup is not confirmed while the first request may still land; once it is
			// delivered it is adopted and cleaned up like any other object.
			if ev, err := res.Stop(ctx, h); err != nil || ev.Stopped || ev.Launch == execution.LaunchNotStarted {
				t.Fatalf("%+v %v", ev, err)
			}
			if rm, err := res.Remove(ctx, h); !errors.Is(err, execution.ErrResourceFenced) {
				t.Fatalf("the cleanup completed with the first request unaccounted for: %+v %v", rm, err)
			}
			e.fake.ctl(t, "__land")
			if ev, err := res.Stop(ctx, h); err != nil || !ev.Stopped {
				t.Fatalf("%+v %v", ev, err)
			}
			if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
				t.Fatalf("%+v %v", rm, err)
			}
			if real, ph := objects(e, t, kind); real != 0 || ph != 0 {
				t.Fatalf("%d real, %d placeholders", real, ph)
			}
		})

		t.Run(kind+": a success whose answer was never recorded is adopted once, not twice", func(t *testing.T) {
			e := newSettleEnv(t)
			first := e.open()
			r := newRun(t)
			if kind == "container" {
				e.ensure(first, r.network())
			}
			spec := mk(r)
			// The engine creates the object and the controller dies before it records
			// anything of the answer: the request stays announced and unanswered, the
			// object exists unbound.
			crash(first)
			if _, err := first.Ensure(ctx, spec); err == nil {
				t.Fatal("the crash was not simulated")
			}
			if n := unanswered(e, t, spec.Name); n != 1 {
				t.Fatalf("%d", n)
			}
			if real, _ := objects(e, t, kind); real != 1 {
				t.Fatalf("the engine did not create the object: %d", real)
			}
			res := e.open()
			h := e.ensure(res, spec).Handle
			if n := unanswered(e, t, spec.Name); n != 0 {
				t.Fatalf("the recovered request is counted %d times", n)
			}
			if ev, err := res.Stop(ctx, h); err != nil || !ev.Stopped {
				t.Fatalf("%+v %v", ev, err)
			}
			if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
				t.Fatalf("%+v %v", rm, err)
			}
			if real, fences := objects(e, t, kind); real != 0 || fences != 0 {
				t.Fatalf("%d real, %d placeholders", real, fences)
			}
		})
	}
}

// Test_podman_cleanup_waits_for_delivery (re-review 4): the interleaving a
// placeholder could not make safe. A creation request is delayed; the cleanup is
// attempted over the resource, its holder and its probe and is never confirmed
// while the request can still land; the adapter creates and deletes nothing to
// settle it; after delivery the cleanup completes with nothing left.
func Test_podman_cleanup_waits_for_delivery(t *testing.T) {
	ctx := context.Background()
	e := newSettleEnv(t)
	res := e.open()
	r := newRun(t)
	e.ensure(res, r.network())
	ws := e.ensure(res, r.workspace()).Handle

	prep := r.preparation()
	e.fake.set(t, "create-fault", "container:late")
	if _, err := res.Ensure(ctx, prep); err == nil {
		t.Fatal("an ambiguous create was accepted")
	}
	svcBody := dbBody()
	svcBody.Readiness = execution.ReadinessSpec{Probe: "tcp", Port: "sql", TimeoutSecs: 30, ProbeTimeout: 3}
	svc := r.service("db", svcBody)
	e.fake.set(t, "create-fault", "")
	svcH := e.ensure(res, svc).Handle
	e.fake.set(t, "create-fault", "container:late")
	if obs, err := res.Observe(ctx, svcH); err != nil || obs.Phase != execution.PhaseUnknown {
		t.Fatalf("%+v %v", obs, err)
	}
	e.fake.set(t, "create-fault", "")

	mark := len(e.fake.argvs(t))
	for range 3 {
		for _, h := range []execution.ResourceHandle{svcH, prep.Handle("")} {
			_, _ = res.Observe(ctx, h)
			if ev, err := res.Stop(ctx, h); err != nil || ev.Stopped {
				t.Fatalf("%s: a stop confirmed while a request can still land: %+v %v", h.Key, ev, err)
			}
			if rm, err := res.Remove(ctx, h); err == nil && rm.Removed {
				t.Fatalf("%s: removal confirmed while a request can still land", h.Key)
			}
		}
		if ev, err := res.Stop(ctx, ws); err != nil || ev.Released {
			t.Fatalf("the workspace was released under an unresolved holder: %+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, ws); err == nil && rm.Removed {
			t.Fatal("the workspace was removed under an unresolved holder")
		}
	}
	for _, a := range e.fake.argvs(t)[mark:] {
		switch a[0] {
		case "create":
			t.Fatalf("the adapter created an object to settle a request: %v", a)
		case "volume", "network", "container":
			if len(a) > 1 && a[1] == "create" {
				t.Fatalf("the adapter created an object to settle a request: %v", a)
			}
		}
	}
	for _, c := range e.fake.created(t) {
		if c.Labels[fenceLabel] != "" {
			t.Fatalf("a placeholder exists: %+v", c)
		}
	}

	// Delivery, then the cleanup completes normally with nothing left.
	e.fake.ctl(t, "__land")
	for _, h := range []execution.ResourceHandle{svcH, prep.Handle("")} {
		for {
			ev, err := res.Stop(ctx, h)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Stopped {
				break
			}
		}
		if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
			t.Fatalf("%s: %+v %v", h.Key, rm, err)
		}
	}
	if ev, err := res.Stop(ctx, ws); err != nil || !ev.Stopped || !ev.Released {
		t.Fatalf("%+v %v", ev, err)
	}
	if rm, err := res.Remove(ctx, ws); err != nil || !rm.Removed {
		t.Fatalf("%+v %v", rm, err)
	}
	if n := len(e.fake.created(t)); n != 0 {
		t.Fatalf("%d containers left", n)
	}
}

// Test_podman_foreign_holder_occupation (re-review 3): a foreign object at a
// holder's container name is not ours (its owner can remove it), so the
// holder intent stays and the workspace stays held; a create that lands after the
// foreign object left is still caught.
func Test_podman_foreign_holder_occupation(t *testing.T) {
	ctx := context.Background()
	e := newSettleEnv(t)
	res := e.open()
	r := newRun(t)
	e.ensure(res, r.network())
	ws := e.ensure(res, r.workspace()).Handle
	prep := r.preparation()
	e.fake.set(t, "create-fault", "container:late")
	if _, err := res.Ensure(ctx, prep); err == nil {
		t.Fatal("an ambiguous create was accepted")
	}
	e.fake.set(t, "create-fault", "")
	marker := filepath.Join(e.state, "resources", ws.Name, "holders", prep.Name+".json")

	foreign := e.fake.ctl(t, "__plant", prep.Name)
	for _, step := range []string{"observe", "stop", "remove"} {
		switch step {
		case "observe":
			if obs, err := res.Observe(ctx, ws); err != nil || obs.Released {
				t.Fatalf("a foreign occupant released the workspace: %+v %v", obs, err)
			}
		case "stop":
			if ev, err := res.Stop(ctx, ws); err != nil || ev.Released {
				t.Fatalf("%+v %v", ev, err)
			}
		case "remove":
			if rm, err := res.Remove(ctx, ws); err != nil || rm.Removed {
				t.Fatalf("the volume was removed under a holder whose name a foreign object holds: %+v %v", rm, err)
			}
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("%s: the holder intent was dropped: %v", step, err)
		}
	}
	for _, a := range e.fake.argvs(t) {
		if (a[0] == "rm" || a[0] == "stop") && a[len(a)-1] == foreign {
			t.Fatalf("the foreign container was touched: %v", a)
		}
	}

	// The foreign object goes away; the announced create is delivered afterwards.
	e.fake.ctl(t, "rm", foreign)
	if ev, err := res.Stop(ctx, ws); err != nil || ev.Released {
		t.Fatalf("an unresolved holder: %+v %v", ev, err)
	}
	e.fake.ctl(t, "__land")
	if obs, err := res.Observe(ctx, ws); err != nil || obs.Released {
		t.Fatalf("the landed preparation container does not hold the workspace: %+v %v", obs, err)
	}
	if rm, err := res.Remove(ctx, ws); err != nil || rm.Removed {
		t.Fatalf("%+v %v", rm, err)
	}
	// Once the preparation is stopped and removed the workspace is released.
	for {
		ev, err := res.Stop(ctx, prep.Handle(""))
		if err != nil {
			t.Fatal(err)
		}
		if ev.Stopped {
			break
		}
	}
	if rm, err := res.Remove(ctx, prep.Handle("")); err != nil || !rm.Removed {
		t.Fatalf("%+v %v", rm, err)
	}
	if ev, err := res.Stop(ctx, ws); err != nil || !ev.Released {
		t.Fatalf("%+v %v", ev, err)
	}
	if rm, err := res.Remove(ctx, ws); err != nil || !rm.Removed {
		t.Fatalf("%+v %v", rm, err)
	}
}
