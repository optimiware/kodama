package m1_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// TestM1WriterCAS (m1_writer_cas): competing writers and the compare-and-swap
// that decides them, over real PostgreSQL and a real Git remote. Concurrent edits
// of one version yield one winner; concurrent provisioning one workspace; a
// writer attempt holds the workspace's single slot while a competitor waits; a
// human push is observed as the accepted tip and never overwritten, a rewritten
// branch is refused, readers keep the commit their run pinned; a lost push
// acknowledgment is reconciled without a second branch; and archive and restore
// keep the retained source and the audit trail. AC-A02, WS-A01..WS-A08, PX-A04.
//
// A container workload cannot check out or publish source outside Kubernetes
// (ADR 0008), so the publication of a writer's bundle itself is exercised by the
// T-022 integration suites on the same adapters and, for a running writer, by
// T-089; what this test owns is the reservation and the expected-tip preconditions
// the publication compares against.
func TestM1WriterCAS(t *testing.T) {
	s := kit.New(t, kit.Options{Tune: func(tn *app.Tuning) { tn.Limit.StartupBudget = 8 * time.Second }})
	s.Start()
	s.WaitReady()
	w := s.Seed(true)
	ctx := t.Context()

	t.Run("concurrent edits of one version yield exactly one winner (AC-A02)", func(t *testing.T) {
		item := w.WorkItem("Contended", "# v1")
		var wg sync.WaitGroup
		codes := make([]int, 8)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				title := fmt.Sprintf("Edited by writer %d", i)
				r, err := s.API("engineer").UpdateWorkItemWithResponse(ctx, kit.TenantA, w.Project, item.Id,
					&client.UpdateWorkItemParams{IfMatch: fmt.Sprintf(`"%d"`, item.Version)}, client.UpdateWorkItem{Title: &title})
				if err != nil {
					t.Error(err)
					return
				}
				codes[i] = r.StatusCode()
			}()
		}
		wg.Wait()
		wins, stale := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				wins++
			case http.StatusPreconditionFailed:
				stale++
			}
		}
		if wins != 1 || stale != len(codes)-1 {
			t.Fatalf("outcomes %v: %d winners, %d stale", codes, wins, stale)
		}
		got, _ := s.API("engineer").GetWorkItemWithResponse(ctx, kit.TenantA, w.Project, item.Id)
		if got.JSON200.Version != item.Version+1 {
			t.Fatalf("version %d after one winning edit of %d", got.JSON200.Version, item.Version)
		}
		if r, _ := s.API("engineer").UpdateWorkItemWithResponse(ctx, kit.TenantA, w.Project, item.Id, &client.UpdateWorkItemParams{IfMatch: fmt.Sprintf(`"%d"`, item.Version)}, client.UpdateWorkItem{}); r.StatusCode() != http.StatusPreconditionFailed || kit.ErrorCode(r.Body) != "version_conflict" {
			t.Fatalf("a stale edit: %d %s", r.StatusCode(), r.Body)
		}
	})

	t.Run("concurrent provisioning yields one workspace and one branch (WS-A01, WS-A02)", func(t *testing.T) {
		item := w.WorkItem("Provisioned together", "# p")
		var wg sync.WaitGroup
		ids := make([]string, 8)
		codes := make([]int, 8)
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := s.API("engineer").ProvisionWorkspaceWithResponse(ctx, kit.TenantA, w.Project, item.Id, client.EmptyRequest{})
				if err != nil {
					t.Error(err)
					return
				}
				codes[i] = r.StatusCode()
				for _, ws := range []*client.Workspace{r.JSON200, r.JSON201, r.JSON202} {
					if ws != nil {
						ids[i] = ws.Id
					}
				}
			}()
		}
		wg.Wait()
		created := 0
		for i, c := range codes {
			if c == http.StatusCreated {
				created++
			}
			if ids[i] != ids[0] || ids[i] == "" {
				t.Fatalf("callers saw workspaces %v (codes %v)", ids, codes)
			}
		}
		if created > 1 {
			t.Fatalf("%d callers created the workspace (codes %v)", created, codes)
		}
		ws := w.Provision(item.Id)
		if n := s.OwnerCount(`SELECT count(*) FROM workspaces WHERE work_item_id = $1`, item.Id); n != 1 {
			t.Fatalf("%d workspaces", n)
		}
		if got := s.Repo.Git(t, "for-each-ref", "--format=%(refname)", ws.Branch); strings.Count(got, "\n") != 0 || strings.TrimSpace(got) != ws.Branch {
			t.Fatalf("the remote has %q for the branch %s", got, ws.Branch)
		}
	})

	t.Run("a human push is accepted at sync, never overwritten, and readers keep their pinned commit (WS-A04, WS-A07)", func(t *testing.T) {
		item := w.WorkItem("Human push", "# h")
		ws := w.Provision(item.Id)
		def := w.PodmanDefinition("reader-pin", kit.Step{Script: `true`})
		run := w.Run("human-push-run", item, def.Id)
		w.WaitSourceRetained(run.Id)
		w.WaitRun(run.Id, 2*time.Minute)

		human := s.Repo.HumanPush(t, ws.Branch, "a human change", gitfx.File{Path: "human.txt", Content: "human"})
		synced, err := s.API("engineer").SyncWorkspaceWithResponse(ctx, kit.TenantA, w.Project, ws.Id, &client.SyncWorkspaceParams{IfMatch: fmt.Sprintf(`"%d"`, ws.Version)}, client.EmptyRequest{})
		if err != nil || synced.StatusCode() != http.StatusOK || synced.JSON200.AcceptedCommit == nil || *synced.JSON200.AcceptedCommit != human {
			t.Fatalf("sync: %v %s", err, synced.Body)
		}
		if got := w.GetRun(run.Id); got.SourceCommit == nil || *got.SourceCommit != s.Repo.Initial {
			t.Fatalf("the branch advanced and the run's pinned commit became %v", got.SourceCommit)
		}
		if refs := s.Repo.Git(t, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/kodama/retained/"); !strings.Contains(refs, s.Repo.Initial) {
			t.Fatalf("the retained source reference does not resolve the pinned commit: %q", refs)
		}
		// A stale version is a precondition failure, not a silent overwrite.
		if r, _ := s.API("engineer").SyncWorkspaceWithResponse(ctx, kit.TenantA, w.Project, ws.Id, &client.SyncWorkspaceParams{IfMatch: fmt.Sprintf(`"%d"`, ws.Version)}, client.EmptyRequest{}); r.StatusCode() != http.StatusPreconditionFailed {
			t.Fatalf("a stale sync: %d %s", r.StatusCode(), r.Body)
		}

		// A rewritten branch is never adopted: no rewind, no merge.
		orphan := s.Repo.OrphanCommit(t, "unrelated history")
		s.Repo.SetRef(t, ws.Branch, orphan)
		cur, _ := s.API("engineer").GetWorkspaceWithResponse(ctx, kit.TenantA, w.Project, ws.Id)
		r, err := s.API("engineer").SyncWorkspaceWithResponse(ctx, kit.TenantA, w.Project, ws.Id, &client.SyncWorkspaceParams{IfMatch: fmt.Sprintf(`"%d"`, cur.JSON200.Version)}, client.EmptyRequest{})
		if err != nil || r.StatusCode() != http.StatusConflict || kit.ErrorCode(r.Body) != "branch_diverged" {
			t.Fatalf("a rewritten branch: %v %d %s", err, r.StatusCode(), r.Body)
		}
		after, _ := s.API("engineer").GetWorkspaceWithResponse(ctx, kit.TenantA, w.Project, ws.Id)
		if after.JSON200.AcceptedCommit == nil || *after.JSON200.AcceptedCommit != human {
			t.Fatalf("the accepted commit became %v after a diverged branch", after.JSON200.AcceptedCommit)
		}
		s.Repo.SetRef(t, ws.Branch, human)
	})

	t.Run("a writer attempt holds the single slot and a competitor waits (WS-A02, WS-A03, PX-A04)", func(t *testing.T) {
		item := w.WorkItem("Competing writers", "# c")
		ws := w.Provision(item.Id)
		def := w.PodmanDefinition("writer", kit.Step{Script: `true`, Access: "write"})
		first := w.Run("writer-one", item, def.Id)
		second := w.Run("writer-two", item, def.Id)

		// Exactly one run claims the slot; the other is never given an attempt while it is held.
		var holder string
		kit.WaitFor(t, "a writer attempt to hold the slot", 60*time.Second, func() bool {
			cur, err := s.API("engineer").GetWorkspaceWithResponse(ctx, kit.TenantA, w.Project, ws.Id)
			if err != nil || cur.JSON200 == nil || cur.JSON200.WriterAttemptId == nil {
				return false
			}
			holder = *cur.JSON200.WriterAttemptId
			return true
		})
		var holderRun string
		s.MustScan(&holderRun, `SELECT run_id FROM step_attempts WHERE id = $1`, holder)
		other := first.Id
		if holderRun == first.Id {
			other = second.Id
		}
		if holderRun != first.Id && holderRun != second.Id {
			t.Fatalf("the slot is held by %s of run %s", holder, holderRun)
		}
		var expectedTip string
		s.MustScan(&expectedTip, `SELECT expected_tip FROM step_attempts WHERE id = $1`, holder)
		if expectedTip != s.Repo.Initial {
			t.Fatalf("the reservation compares against %s, not the accepted tip %s", expectedTip, s.Repo.Initial)
		}
		if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, other); n != 0 {
			t.Fatalf("the competing writer run was given %d attempts while the slot was held", n)
		}
		// A human push between the reservation and a publication is what the
		// expected-tip comparison is for: the remote moves and the reservation
		// keeps the tip it was admitted against.
		human := s.Repo.HumanPush(t, ws.Branch, "a human change during the attempt", gitfx.File{Path: "h.txt", Content: "x"})
		if tip := s.Repo.Tip(t, ws.Branch); tip != human || tip == expectedTip {
			t.Fatalf("remote tip %s", tip)
		}

		// A revoked writer never keeps the slot: cancelling the holder releases it and the
		// competitor is admitted against the tip the human left (WS-A03: reassignment).
		holderRunID := holderRun
		if r := w.CancelRun(holderRunID); r.StatusCode() != http.StatusAccepted {
			t.Fatalf("cancel: %d %s", r.StatusCode(), r.Body)
		}
		if final := w.WaitRun(holderRunID, 2*time.Minute); final.Status != client.RunStatusCancelled {
			t.Fatalf("the holder's run is %s", final.Status)
		}
		kit.WaitFor(t, "the competitor to be admitted", 90*time.Second, func() bool {
			return s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, other) > 0
		})
		var next string
		s.MustScan(&next, `SELECT id FROM step_attempts WHERE run_id = $1 ORDER BY created_at LIMIT 1`, other)
		if next == holder {
			t.Fatal("the revoked attempt kept the slot")
		}
	})

	t.Run("a lost push acknowledgment is reconciled without a second branch (WS-A05)", func(t *testing.T) {
		item := w.WorkItem("Lost ack", "# l")
		s.Git.DropNextReceivePackAcks(1)
		r, err := s.API("engineer").ProvisionWorkspaceWithResponse(ctx, kit.TenantA, w.Project, item.Id, client.EmptyRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode() != http.StatusAccepted && r.StatusCode() != http.StatusCreated && r.StatusCode() != http.StatusOK {
			t.Fatalf("provision with a lost acknowledgment: %d %s", r.StatusCode(), r.Body)
		}
		ws := w.Provision(item.Id) // repeating observes the remote before pushing again
		if ws.AcceptedCommit == nil || *ws.AcceptedCommit != s.Repo.Initial || s.Repo.Tip(t, ws.Branch) != s.Repo.Initial {
			t.Fatalf("workspace %+v", ws)
		}
		if n := s.OwnerCount(`SELECT count(*) FROM workspaces WHERE work_item_id = $1`, item.Id); n != 1 {
			t.Fatalf("%d workspaces", n)
		}
	})

	t.Run("archive and restore keep the retained source and the audit trail (WS-A08)", func(t *testing.T) {
		item := w.WorkItem("Archived", "# a")
		ws := w.Provision(item.Id)
		def := w.PodmanDefinition("archive-pin", kit.Step{Script: `true`})
		run := w.Run("archive-run", item, def.Id)
		w.WaitSourceRetained(run.Id)
		w.WaitRun(run.Id, 2*time.Minute)

		ar, err := s.API("engineer").ArchiveWorkspaceWithResponse(ctx, kit.TenantA, w.Project, ws.Id, &client.ArchiveWorkspaceParams{IfMatch: fmt.Sprintf(`"%d"`, ws.Version)}, client.EmptyRequest{})
		if err != nil || (ar.StatusCode() != http.StatusOK && ar.StatusCode() != http.StatusAccepted) {
			t.Fatalf("archive: %v %d %s", err, ar.StatusCode(), ar.Body)
		}
		var archived client.Workspace
		kit.WaitFor(t, "the workspace to be archived", 60*time.Second, func() bool {
			cur, err := s.API("engineer").GetWorkspaceWithResponse(ctx, kit.TenantA, w.Project, ws.Id)
			if err != nil || cur.JSON200 == nil {
				return false
			}
			archived = *cur.JSON200
			return archived.State == "archived"
		})
		if refs := s.Repo.Git(t, "for-each-ref", "--format=%(objectname)", "refs/heads/kodama/retained/"); !strings.Contains(refs, s.Repo.Initial) {
			t.Fatalf("archiving dropped the retained source: %q", refs)
		}
		rr, err := s.API("engineer").RestoreWorkspaceWithResponse(ctx, kit.TenantA, w.Project, ws.Id, &client.RestoreWorkspaceParams{IfMatch: fmt.Sprintf(`"%d"`, archived.Version)}, client.EmptyRequest{})
		if err != nil || rr.StatusCode() != http.StatusOK && rr.StatusCode() != http.StatusAccepted {
			t.Fatalf("restore: %v %d %s", err, rr.StatusCode(), rr.Body)
		}
		kit.WaitFor(t, "the workspace to be restored", 60*time.Second, func() bool {
			cur, err := s.API("engineer").GetWorkspaceWithResponse(ctx, kit.TenantA, w.Project, ws.Id)
			return err == nil && cur.JSON200 != nil && cur.JSON200.State == "available" && cur.JSON200.AcceptedCommit != nil && *cur.JSON200.AcceptedCommit == s.Repo.Initial
		})
		if n := s.TenantCount(kit.TenantA, `SELECT count(*) FROM audit WHERE action LIKE 'workspace.%' AND resource_id = $1`, ws.Id); n < 3 {
			t.Fatalf("%d workspace audit rows after provision, archive and restore", n)
		}
		if got := w.GetRun(run.Id); got.SourceCommit == nil || *got.SourceCommit != s.Repo.Initial {
			t.Fatalf("the run's pinned commit is %v", got.SourceCommit)
		}
	})
}
