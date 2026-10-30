package gitops_test

import (
	"context"
	"errors"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/gitops"
)

func day(n int) time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n) }

// Test_unit_gitops_prune_keeps_pinned: with real Git, a merged task, agent or run
// branch is pruned only when its tip is reachable from the default branch or a
// retained ref and older than retention; a branch a run, evidence or deployment
// references, an unmerged or a recent one stays; and after the most aggressive
// maintenance every pinned commit still exists (GR-13, GR-A09).
func Test_unit_gitops_prune_keeps_pinned(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	const id = "r_prune"
	rp := repo(tenantA, id)
	r.catalog.Add(rp)
	r.vol.Init(tenantA, id)
	r.clock.Advance(0)
	now := r.clock.Now()
	old := now.AddDate(0, 0, -90)

	base := r.vol.Commit(tenantA, id, "", "base", old)
	m1 := r.vol.Commit(tenantA, id, base, "main 1", old.Add(time.Hour))
	r.vol.SetRef(tenantA, id, "refs/heads/main", m1)

	task := func(n string) string { return "refs/heads/kodama/work/" + n }
	agent := "refs/heads/kodama/agents/wi_1/run_1/work"
	runBranch := "refs/heads/kodama/runs/run_9/build"

	// Merged and old: ancestors of main.
	merged := r.vol.Commit(tenantA, id, base, "merged work", old.Add(2*time.Hour))
	m2 := r.vol.Commit(tenantA, id, m1, "main 2 (merge of merged work)", old.Add(3*time.Hour))
	_ = merged
	r.vol.SetRef(tenantA, id, "refs/heads/main", m2)
	r.vol.SetRef(tenantA, id, task("wi_merged"), m1)   // tip m1 is an ancestor of main
	r.vol.SetRef(tenantA, id, agent, base)             // tip base is an ancestor of main
	r.vol.SetRef(tenantA, id, task("wi_evidence"), m1) // merged, but evidence names the branch
	r.vol.SetRef(tenantA, id, task("wi_heldcommit"), base)
	// Unmerged and old, not retained.
	unmerged := r.vol.Commit(tenantA, id, base, "unmerged", old.Add(4*time.Hour))
	r.vol.SetRef(tenantA, id, task("wi_unmerged"), unmerged)
	// Unmerged and old, but a run pinned it with a retained ref: reachable from a retained ref.
	pinned := r.vol.Commit(tenantA, id, base, "pinned by a run", old.Add(5*time.Hour))
	r.vol.SetRef(tenantA, id, runBranch, pinned)
	r.vol.SetRef(tenantA, id, "refs/kodama/runs/run_9/1", pinned)
	// Merged but recent.
	recent := r.vol.Commit(tenantA, id, m2, "recent", now.Add(-time.Hour))
	r.vol.SetRef(tenantA, id, "refs/heads/main", recent)
	r.vol.SetRef(tenantA, id, task("wi_recent"), recent)
	// Not managed: an ordinary branch and a tag are never pruned.
	r.vol.SetRef(tenantA, id, "refs/heads/feature/x", base)
	r.vol.SetRef(tenantA, id, "refs/tags/v1", base)

	r.holds.Set(id, gitops.Held{Refs: []string{task("wi_evidence")}, Commits: []string{base}})
	s := r.scheduler(gitops.Options{BranchRetention: 30 * 24 * time.Hour, MaintenanceInterval: time.Hour})

	res, err := s.PruneRepository(ctx, rp)
	if err != nil {
		t.Fatal(err)
	}
	// base is held: every branch whose tip is base stays, even merged and old.
	wantPruned := []string{runBranch, task("wi_merged")}
	if !equal(res.Pruned, wantPruned) {
		t.Fatalf("pruned %v, want %v; kept %v", res.Pruned, wantPruned, res.Kept)
	}
	wantKept := map[string]string{
		task("wi_evidence"):   gitops.KeptHeldRef,
		agent:                 gitops.KeptHeldCommit,
		task("wi_heldcommit"): gitops.KeptHeldCommit,
		task("wi_unmerged"):   gitops.KeptUnmerged,
		task("wi_recent"):     gitops.KeptRetention,
	}
	for ref, why := range wantKept {
		if res.Kept[ref] != why {
			t.Errorf("%s kept as %q, want %q", ref, res.Kept[ref], why)
		}
	}
	for ref, c := range map[string]string{"refs/heads/feature/x": base, "refs/tags/v1": base, "refs/heads/main": recent, "refs/kodama/runs/run_9/1": pinned} {
		if got := r.vol.Ref(tenantA, id, ref); got != c {
			t.Errorf("%s = %q, want %q", ref, got, c)
		}
	}
	if r.vol.Ref(tenantA, id, task("wi_merged")) != "" || r.vol.Ref(tenantA, id, runBranch) != "" {
		t.Fatal("a pruned branch still exists")
	}

	// The most aggressive maintenance removes nothing a pin keeps: the commit of
	// the pruned run branch survives through its retained ref.
	if _, err := r.worker.Maintain(ctx, tenantA, gitv1.MaintainRequest{RepositoryID: id, OperationID: "op_gc"}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]string{"pinned": pinned, "main tip": recent, "held base": base, "unmerged": unmerged, "merged tip m1": m1} {
		if !r.vol.HasObject(tenantA, id, c) {
			t.Errorf("%s commit %s was lost", name, c)
		}
	}

	// A second pass is idempotent: nothing left to prune, nothing fails.
	if res, err = s.PruneRepository(ctx, rp); err != nil || len(res.Pruned) != 0 {
		t.Fatalf("second pass: %v %v", res.Pruned, err)
	}
	// Without the hold, the held-by-commit branches go too (their tips are merged and old).
	r.holds.Set(id, gitops.Held{})
	if res, err = s.PruneRepository(ctx, rp); err != nil || len(res.Pruned) != 3 {
		t.Fatalf("after the hold ended: %v %v", res.Pruned, err)
	}
	// The unmerged branch and the recent one never.
	if r.vol.Ref(tenantA, id, task("wi_unmerged")) != unmerged || r.vol.Ref(tenantA, id, task("wi_recent")) != recent {
		t.Fatal("an unmerged or recent branch was pruned")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Test_unit_gitops_prune_fails_closed: unknown holds, an archived repository, a
// branch that moved and a worker error prune nothing wrong.
func Test_unit_gitops_prune_fails_closed(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	const id = "r_closed"
	rp := repo(tenantA, id)
	r.vol.Init(tenantA, id)
	now := r.clock.Now()
	old := now.AddDate(0, 0, -90)
	base := r.vol.Commit(tenantA, id, "", "base", old)
	r.vol.SetRef(tenantA, id, "refs/heads/main", base)
	ref := "refs/heads/kodama/work/wi_c"
	r.vol.SetRef(tenantA, id, ref, base)
	s := r.scheduler(gitops.Options{BranchRetention: time.Hour, MaintenanceInterval: time.Hour})

	r.holds.Fail = errors.New("execution unavailable")
	if res, err := s.PruneRepository(ctx, rp); !errors.Is(err, gitops.ErrUnavailable) || len(res.Pruned) != 0 {
		t.Fatalf("unknown holds: %v %v", res.Pruned, err)
	}
	r.holds.Fail = nil

	archived := rp
	archived.Archived = true
	if res, err := s.PruneRepository(ctx, archived); err != nil || len(res.Pruned) != 0 || r.vol.Ref(tenantA, id, ref) == "" {
		t.Fatalf("archived: %v %v", res.Pruned, err)
	}

	// The branch moves between the read and the deletion: the expected value protects it.
	moved := r.vol.Commit(tenantA, id, base, "moved", old)
	r.worker.Hook = func(op, _ string) {
		if op == "deleteBranch" {
			r.vol.SetRef(tenantA, id, ref, moved)
		}
	}
	res, err := s.PruneRepository(ctx, rp)
	if err != nil || len(res.Pruned) != 0 || res.Kept[ref] != gitops.KeptMoved || r.vol.Ref(tenantA, id, ref) != moved {
		t.Fatalf("moved: %+v %v", res, err)
	}
	r.worker.Hook = nil
	r.vol.SetRef(tenantA, id, ref, base) // merged again

	// A worker error is returned and the branch stays for the next pass.
	r.worker.Fail["deleteBranch"] = errors.New("worker reset")
	if res, err = s.PruneRepository(ctx, rp); err == nil || len(res.Pruned) != 0 || r.vol.Ref(tenantA, id, ref) == "" {
		t.Fatalf("worker error: %+v %v", res, err)
	}
	delete(r.worker.Fail, "deleteBranch")
	if res, err = s.PruneRepository(ctx, rp); err != nil || len(res.Pruned) != 1 {
		t.Fatalf("retry: %+v %v", res, err)
	}
	// Bounded per pass.
	for _, n := range []string{"a", "b", "c"} {
		r.vol.SetRef(tenantA, id, "refs/heads/kodama/work/wi_"+n, base)
	}
	bounded := r.scheduler(gitops.Options{BranchRetention: time.Hour, MaxPrunePerRepository: 2})
	if res, err = bounded.PruneRepository(ctx, rp); err != nil || len(res.Pruned) != 2 {
		t.Fatalf("bounded: %+v %v", res, err)
	}
}

// pruneRaceRig is a repository with one merged, old task branch and a rig whose
// pruning declares its intent through the in-memory reservation.
func pruneRaceRig(t *testing.T) (r *rig, s *gitops.Scheduler, rp gitops.Repository, ref, tip string) {
	r = newRig(t)
	const id = "r_race"
	rp = repo(tenantA, id)
	r.vol.Init(tenantA, id)
	old := r.clock.Now().AddDate(0, 0, -90)
	base := r.vol.Commit(tenantA, id, "", "base", old)
	r.vol.SetRef(tenantA, id, "refs/heads/main", base)
	ref, tip = "refs/heads/kodama/work/wi_race", base
	r.vol.SetRef(tenantA, id, ref, base)
	s = r.scheduler(gitops.Options{BranchRetention: time.Hour, MaintenanceInterval: time.Hour, IntentTTL: 5 * time.Minute})
	return r, s, rp, ref, tip
}

// Test_unit_gitops_prune_hold_race: a hold introduced between pruning's
// inspection and its deletion, with the branch itself unchanged, is honoured: the
// intent is durable before the holds are read again, the fresh read sees the hold
// and nothing is deleted. A hold acquirer that checks the intent after committing
// its hold is refused while the branch is being pruned; an unknown deletion
// outcome keeps the reservation until it expires (GR-13).
func Test_unit_gitops_prune_hold_race(t *testing.T) {
	ctx := context.Background()

	t.Run("a hold taken after the inspection keeps the branch", func(t *testing.T) {
		r, s, rp, ref, tip := pruneRaceRig(t)
		r.intents.OnDeclare = func(string) { // the acquirer commits its hold (and does not check the intent)
			r.holds.Set(rp.ID, gitops.Held{Refs: []string{ref}})
		}
		res, err := s.PruneRepository(ctx, rp)
		if err != nil || len(res.Pruned) != 0 || res.Kept[ref] != gitops.KeptHeldRef || r.vol.Ref(tenantA, rp.ID, ref) != tip {
			t.Fatalf("%+v %v", res, err)
		}
		if r.worker.Count("deleteBranch") != 0 || r.intents.Count() != 0 {
			t.Fatalf("a held branch was deleted or its intent leaked: deletes=%d intents=%d", r.worker.Count("deleteBranch"), r.intents.Count())
		}
	})

	t.Run("a hold on the tip commit keeps the branch", func(t *testing.T) {
		r, s, rp, ref, tip := pruneRaceRig(t)
		r.intents.OnDeclare = func(string) { r.holds.Set(rp.ID, gitops.Held{Commits: []string{tip}}) }
		res, err := s.PruneRepository(ctx, rp)
		if err != nil || len(res.Pruned) != 0 || res.Kept[ref] != gitops.KeptHeldCommit || r.vol.Ref(tenantA, rp.ID, ref) != tip {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("an acquirer that checks the intent is refused and withdraws", func(t *testing.T) {
		r, s, rp, ref, tip := pruneRaceRig(t)
		refused := false
		r.intents.OnDeclare = func(string) {
			// Protocol: commit the hold, then ask whether the branch is being pruned.
			r.holds.Set(rp.ID, gitops.Held{Refs: []string{ref}})
			if busy, err := r.intents.Conflicts(ctx, rp.Scope, rp.ID, ref, tip); err != nil || !busy {
				t.Errorf("the acquirer does not see the declared intent: %v %v", busy, err)
				return
			}
			refused = true
			r.holds.Set(rp.ID, gitops.Held{}) // withdrawn: the operation is refused retryably
		}
		res, err := s.PruneRepository(ctx, rp)
		if err != nil || !refused || len(res.Pruned) != 1 || r.vol.Ref(tenantA, rp.ID, ref) != "" {
			t.Fatalf("the refused acquirer does not stop the pruning: %+v %v", res, err)
		}
		if busy, _ := r.intents.Conflicts(ctx, rp.Scope, rp.ID, ref, tip); busy {
			t.Fatal("the intent outlived a definite outcome")
		}
	})

	t.Run("a hold committed before the intent is seen by the fresh read", func(t *testing.T) {
		r, s, rp, ref, tip := pruneRaceRig(t)
		// The hold is committed after pruning's first read of the holds and before
		// the intent is declared: the read after the declaration sees it.
		r.worker.Hook = func(op, _ string) {
			if op == "readRefValues" {
				r.holds.Set(rp.ID, gitops.Held{Refs: []string{ref}})
			}
		}
		res, err := s.PruneRepository(ctx, rp)
		if err != nil || len(res.Pruned) != 0 || res.Kept[ref] != gitops.KeptHeldRef || r.vol.Ref(tenantA, rp.ID, ref) != tip {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("the intent is declared before the deletion", func(t *testing.T) {
		r, s, rp, _, _ := pruneRaceRig(t)
		declared := false
		r.intents.OnDeclare = func(string) { declared = true }
		r.worker.Hook = func(op, _ string) {
			if op == "deleteBranch" && !declared {
				t.Error("a deletion without a declared intent")
			}
		}
		if res, err := s.PruneRepository(ctx, rp); err != nil || len(res.Pruned) != 1 || !declared {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("a recovery that begins mid-pass stops the deletions", func(t *testing.T) {
		r, s, rp, ref, tip := pruneRaceRig(t)
		second := "refs/heads/kodama/work/wi_race2"
		r.vol.SetRef(tenantA, rp.ID, second, tip)
		var began bool
		r.intents.OnDeclare = func(string) { // the first candidate's reservation is declared; a restore begins
			if !began {
				began = true
				if _, err := r.recoveries.Begin(ctx, tenantA, "rcv_mid", gitops.RestoreSource{BackupID: "bk_9"}, r.clock.Now()); err != nil {
					t.Error(err)
				}
			}
		}
		res, err := s.PruneRepository(ctx, rp)
		if !errors.Is(err, gitops.ErrTenantClosed) || len(res.Pruned) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
		if r.worker.Count("deleteBranch") != 0 || r.vol.Ref(tenantA, rp.ID, ref) != tip || r.vol.Ref(tenantA, rp.ID, second) != tip {
			t.Fatal("a branch was deleted after the recovery began")
		}
		if r.intents.Count() != 0 {
			t.Fatal("the intent of the stopped deletion leaked")
		}
		// Through the scheduler the pass reports the tenant closed, not a failure,
		// and does not go on to maintenance.
		rep := s.MaintainRepository(ctx, rp)
		if !errors.Is(rep.Closed, gitops.ErrTenantClosed) || rep.Err != nil || rep.Maintained {
			t.Fatalf("%+v", rep)
		}
	})

	t.Run("an unknown outcome keeps the reservation until it expires", func(t *testing.T) {
		r, s, rp, ref, tip := pruneRaceRig(t)
		r.worker.Fail["deleteBranch"] = errors.New("worker reset")
		if _, err := s.PruneRepository(ctx, rp); err == nil {
			t.Fatal("the unknown outcome is returned")
		}
		if busy, err := r.intents.Conflicts(ctx, rp.Scope, rp.ID, ref, tip); err != nil || !busy {
			t.Fatalf("the reservation of an unknown outcome: %v %v", busy, err)
		}
		r.clock.Advance(6 * time.Minute)
		if busy, _ := r.intents.Conflicts(ctx, rp.Scope, rp.ID, ref, tip); busy {
			t.Fatal("the reservation outlived its lifetime")
		}
		delete(r.worker.Fail, "deleteBranch")
		if res, err := s.PruneRepository(ctx, rp); err != nil || len(res.Pruned) != 1 {
			t.Fatalf("the retry: %+v %v", res, err)
		}
	})

	t.Run("another pruner's live reservation leaves the branch alone", func(t *testing.T) {
		r, s, rp, ref, tip := pruneRaceRig(t)
		if err := r.intents.Declare(ctx, rp.Scope, rp.ID, ref, tip, "pruner_other_instance", 5*time.Minute); err != nil {
			t.Fatal(err)
		}
		res, err := s.PruneRepository(ctx, rp)
		if err != nil || len(res.Pruned) != 0 || res.Kept[ref] != gitops.KeptBusy || r.vol.Ref(tenantA, rp.ID, ref) != tip {
			t.Fatalf("%+v %v", res, err)
		}
		// This pruner did not release the other's reservation.
		if busy, _ := r.intents.Conflicts(ctx, rp.Scope, rp.ID, ref, tip); !busy {
			t.Fatal("a pruner released another instance's reservation")
		}
		// Once it expires this pruner takes the branch.
		r.clock.Advance(6 * time.Minute)
		if res, err := s.PruneRepository(ctx, rp); err != nil || len(res.Pruned) != 1 {
			t.Fatalf("after the reservation expired: %+v %v", res, err)
		}
	})

	t.Run("without the reservation port nothing is pruned", func(t *testing.T) {
		r, _, rp, ref, tip := pruneRaceRig(t)
		s, err := gitops.NewScheduler(gitops.SchedulerConfig{Worker: r.worker, Catalog: r.catalog, Holds: r.holds, Objects: r.objects, Fence: r.fence, Recoveries: r.recoveries,
			Options: gitops.Options{BranchRetention: time.Hour, Clock: r.clock.Now}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PruneRepository(ctx, rp); !errors.Is(err, gitops.ErrNotConfigured) || r.vol.Ref(tenantA, rp.ID, ref) != tip {
			t.Fatalf("pruning without intents: %v", err)
		}
	})
}
