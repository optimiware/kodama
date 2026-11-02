package workspace_test

import (
	"errors"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
)

// writerFixture is an internal workspace over two repositories whose writer
// att_1 of run run_1 holds the reservation.
type writerFixture struct {
	e        *wt.InternalEnv
	w        *workspace.Workspace
	app, doc *workspace.RepositoryAssociation
	ref      string
}

func newWriter(t *testing.T) writerFixture {
	t.Helper()
	e := wt.NewInternalEnv(t)
	e.Seed(wt.ScopeA, wt.RepoApp)
	e.Seed(wt.ScopeA, wt.RepoDocs)
	w, assocs := e.Provision(wt.ScopeA, "wi_1", wt.RepoApp, wt.RepoDocs)
	w = e.InternalWriter(w, "att_1")
	return writerFixture{e: e, w: w, app: assoc(t, assocs, wt.RepoApp), doc: assoc(t, assocs, wt.RepoDocs), ref: taskBranch("wi_1")}
}

func (f writerFixture) accepted(t *testing.T, repo workspace.RepositoryID) workspace.Commit {
	t.Helper()
	return assoc(t, associations(t, f.e, f.w), repo).AcceptedCommit
}

// Test_unit_workspace_internal_checkpoint_requires_applied_push: WS-19, WS-A15.
// A writer's commit output is accepted only from an applied push record that
// matches it; every other state of the record, and every other actor or ref,
// leaves the accepted commit where it was.
func Test_unit_workspace_internal_checkpoint_requires_applied_push(t *testing.T) {
	scope := wt.ScopeA

	t.Run("an applied push is accepted: the accepted commit, the pin and one event", func(t *testing.T) {
		f := newWriter(t)
		c1 := f.e.Inspector.Add("feature", f.app.AcceptedCommit)
		push := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c1)

		res, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, push)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Advanced || res.Replay || res.Commit != c1 || res.PreviousCommit != f.app.AcceptedCommit || res.PinRef != "refs/kodama/runs/run_1/1" || res.RepositoryID != wt.RepoApp {
			t.Fatalf("result: %+v", res)
		}
		if got := f.accepted(t, wt.RepoApp); got != c1 {
			t.Fatalf("accepted commit %s, want %s", got, c1)
		}
		if got := f.accepted(t, wt.RepoDocs); got != f.doc.AcceptedCommit {
			t.Fatalf("another repository's accepted commit moved: %s", got)
		}
		cur := reload(t, f.e, f.w)
		if cur.AcceptedCommit != c1 || cur.Writer == nil || cur.Writer.AttemptID != "att_1" {
			t.Fatalf("workspace: %+v", cur) // the primary mirrors its repository; the writer keeps its slot
		}
		if pin := f.e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), res.PinRef); pin != c1 {
			t.Fatalf("the retained ref holds %q", pin)
		}
		evs := f.e.Recorder.Committed(t, scope.TenantID, "workspace.checkpoint_created")
		if len(evs) != 1 || !jsonHas(evs[0].Data, `"newCommit":"`+string(c1)+`"`) || !jsonHas(evs[0].Data, `"publishingAttemptId":"att_1"`) ||
			!jsonHas(evs[0].Data, `"repository":"repo_app"`) || !jsonHas(evs[0].Data, `"previousCommit":"`+string(f.app.AcceptedCommit)+`"`) {
			t.Fatalf("events: %+v", evs)
		}
		for action, want := range map[string]int{"workspace.checkpoint_accepted": 1, "workspace.checkpoint_pinned": 1} {
			if got := auditHas(t, f.e, f.w, action); got != want {
				t.Fatalf("audit %s: %d", action, got)
			}
		}

		// A repeated acceptance is the same checkpoint with no new effect.
		pins := len(f.e.Hosts.PinOps)
		again, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, push)
		if err != nil || !again.Replay || again.PinRef != res.PinRef || again.Commit != c1 {
			t.Fatalf("replay: %+v %v", again, err)
		}
		if len(f.e.Hosts.PinOps) != pins || checkpointEventCount(t, f.e, f.w) != 1 || auditHas(t, f.e, f.w, "workspace.checkpoint_accepted") != 1 {
			t.Fatal("a replay repeated an effect")
		}
	})

	t.Run("a second repository's commit is accepted independently under the one writer", func(t *testing.T) {
		f := newWriter(t)
		d1 := f.e.Inspector.Add("docs", f.doc.AcceptedCommit)
		push := pushed(f.e, f.w, "att_1", "run_1", wt.RepoDocs, f.doc.AcceptedCommit, d1)
		res, err := accept(f.e, f.w, "att_1", wt.RepoDocs, f.ref, d1, push)
		if err != nil || !res.Advanced || res.PinRef != "refs/kodama/runs/run_1/1" {
			t.Fatalf("%+v %v", res, err)
		}
		if f.accepted(t, wt.RepoDocs) != d1 || f.accepted(t, wt.RepoApp) != f.app.AcceptedCommit {
			t.Fatal("the accepted commits are not per repository")
		}
		// Not the primary repository: the workspace's own accepted commit is the primary's.
		if cur := reload(t, f.e, f.w); cur.AcceptedCommit != f.app.AcceptedCommit {
			t.Fatalf("%+v", cur)
		}
	})

	t.Run("a push that is not applied keeps or refuses the output", func(t *testing.T) {
		f := newWriter(t)
		c1 := f.e.Inspector.Add("feature", f.app.AcceptedCommit)
		entry := func(state gitv1.EntryState, kind gitv1.UpdateKind, old, new workspace.Commit) wt.PushEntry {
			return wt.PushEntry{Ref: f.ref, Old: old, New: new, State: state, Kind: kind}
		}
		zero := workspace.Commit(strings.Repeat("0", 40))
		for name, tc := range map[string]struct {
			entry wt.PushEntry
			want  error
		}{
			"authorized":    {entry(gitv1.EntryAuthorized, gitv1.KindFastForward, f.app.AcceptedCommit, c1), workspace.ErrPushPending},
			"committing":    {entry(gitv1.EntryCommitting, gitv1.KindFastForward, f.app.AcceptedCommit, c1), workspace.ErrPushPending},
			"unknown":       {entry(gitv1.EntryUnknown, gitv1.KindFastForward, f.app.AcceptedCommit, c1), workspace.ErrPushPending},
			"refused":       {entry(gitv1.EntryRefused, gitv1.KindFastForward, f.app.AcceptedCommit, c1), workspace.ErrPushNotApplied},
			"a rewrite":     {entry(gitv1.EntryApplied, gitv1.KindRewrite, f.app.AcceptedCommit, c1), workspace.ErrPushNotApplied},
			"a delete":      {entry(gitv1.EntryApplied, gitv1.KindDelete, f.app.AcceptedCommit, zero), workspace.ErrPushNotApplied},
			"other commit":  {entry(gitv1.EntryApplied, gitv1.KindFastForward, f.app.AcceptedCommit, f.e.Inspector.Add("another", f.app.AcceptedCommit)), workspace.ErrPushNotApplied},
			"another ref":   {wt.PushEntry{Ref: "refs/heads/kodama/agents/wi_1/run_1/side", Old: f.app.AcceptedCommit, New: c1, State: gitv1.EntryApplied, Kind: gitv1.KindFastForward}, workspace.ErrPushNotApplied},
			"missing entry": {wt.PushEntry{Ref: "refs/heads/kodama/work/wi_9", Old: f.app.AcceptedCommit, New: c1, State: gitv1.EntryApplied, Kind: gitv1.KindFastForward}, workspace.ErrPushNotApplied},
		} {
			push := f.e.Push(scope, wt.RepoApp, "att_1", "run_1", tc.entry)
			if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, push); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
		}
		// One entry applied beside an unknown one of the same record is still pending.
		other := wt.PushEntry{Ref: "refs/heads/kodama/agents/wi_1/run_1/x", Old: f.app.AcceptedCommit, New: c1, State: gitv1.EntryUnknown, Kind: gitv1.KindFastForward}
		push := f.e.Push(scope, wt.RepoApp, "att_1", "run_1", entry(gitv1.EntryApplied, gitv1.KindFastForward, f.app.AcceptedCommit, c1), other)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, push); !errors.Is(err, workspace.ErrPushPending) {
			t.Errorf("a record with an unknown entry: %v", err)
		}
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("a push that is not applied moved the accepted commit to %s", got)
		}
		if checkpointEventCount(t, f.e, f.w) != 0 || len(f.e.Hosts.PinOps) != 0 {
			t.Fatal("a push that is not applied had an effect")
		}
		// The same output is accepted once reconciliation settles the record as applied.
		push = pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c1)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, push); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the push must be this attempt's, in this repository, on the task branch", func(t *testing.T) {
		f := newWriter(t)
		c1 := f.e.Inspector.Add("feature", f.app.AcceptedCommit)
		f.e.Attempts.Register(scope, "att_other", f.w.ID, workspace.AccessWrite)
		cases := map[string]struct {
			push string
			call func(push string) error
			want error
		}{
			"another attempt's push": {push: pushed(f.e, f.w, "att_other", "run_9", wt.RepoApp, f.app.AcceptedCommit, c1), want: workspace.ErrPushNotApplied,
				call: func(p string) error { _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, p); return err }},
			"another repository": {push: pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c1), want: workspace.ErrNotFound,
				call: func(p string) error { _, err := accept(f.e, f.w, "att_1", wt.RepoDocs, f.ref, c1, p); return err }},
			"an unknown push": {push: "push_none", want: workspace.ErrNotFound,
				call: func(p string) error { _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, p); return err }},
			"an agent branch is no checkpoint": {push: f.e.AppliedPush(scope, wt.RepoApp, "att_1", "run_1", "refs/heads/kodama/agents/wi_1/run_1/side", f.app.AcceptedCommit, c1),
				want: workspace.ErrInvalidValue,
				call: func(p string) error {
					_, err := accept(f.e, f.w, "att_1", wt.RepoApp, "refs/heads/kodama/agents/wi_1/run_1/side", c1, p)
					return err
				}},
			"another work item's task branch": {push: f.e.AppliedPush(scope, wt.RepoApp, "att_1", "run_1", "refs/heads/kodama/work/wi_2", f.app.AcceptedCommit, c1),
				want: workspace.ErrInvalidValue,
				call: func(p string) error {
					_, err := accept(f.e, f.w, "att_1", wt.RepoApp, "refs/heads/kodama/work/wi_2", c1, p)
					return err
				}},
		}
		for name, tc := range cases {
			if err := tc.call(tc.push); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
		}
		// Another tenant's record is not found (foreign and unknown are one answer).
		foreign := f.e.AppliedPush(wt.ScopeB, wt.RepoApp, "att_1", "run_1", f.ref, f.app.AcceptedCommit, c1)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, foreign); !errors.Is(err, workspace.ErrNotFound) {
			t.Errorf("another tenant's push: %v", err)
		}
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("accepted commit moved to %s", got)
		}
	})

	t.Run("only the reserved writer with write access, on an admitting workspace", func(t *testing.T) {
		f := newWriter(t)
		c1 := f.e.Inspector.Add("feature", f.app.AcceptedCommit)
		push := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c1)

		// A read attempt cannot publish a checkpoint, even with an applied push of its own.
		f.e.Attempts.Register(scope, "att_read", f.w.ID, workspace.AccessRead)
		readPush := f.e.AppliedPush(scope, wt.RepoApp, "att_read", "run_2", f.ref, f.app.AcceptedCommit, c1)
		if _, err := accept(f.e, f.w, "att_read", wt.RepoApp, f.ref, c1, readPush); !errors.Is(err, workspace.ErrNotWriter) {
			t.Errorf("read attempt: %v", err)
		}
		// A write attempt that does not hold the reservation (superseded, never admitted).
		f.e.Attempts.Register(scope, "att_late", f.w.ID, workspace.AccessWrite)
		latePush := f.e.AppliedPush(scope, wt.RepoApp, "att_late", "run_3", f.ref, f.app.AcceptedCommit, c1)
		if _, err := accept(f.e, f.w, "att_late", wt.RepoApp, f.ref, c1, latePush); !errors.Is(err, workspace.ErrNotWriter) {
			t.Errorf("attempt without the reservation: %v", err)
		}
		// An unknown attempt and a malformed request.
		if _, err := accept(f.e, f.w, "att_none", wt.RepoApp, f.ref, c1, push); !errors.Is(err, workspace.ErrNotFound) {
			t.Errorf("unknown attempt: %v", err)
		}
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, "refs/tags/x", c1, push); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Errorf("a tag: %v", err)
		}
		// An unauthorized caller.
		if _, err := f.e.Svc.AcceptCheckpoint(bg, workspace.Actor{Type: workspace.ActorUser, ID: "usr_nobody"}, workspace.CheckpointInput{
			Scope: scope, AttemptID: "att_1", RepositoryID: wt.RepoApp, Ref: f.ref, Commit: c1, PushID: push}); !errors.Is(err, workspace.ErrDenied) {
			t.Errorf("unauthorized caller: %v", err)
		}
		// Closing admission by an archive request refuses new checkpoints.
		if _, err := f.e.Svc.Archive(bg, wt.Engineer, scope, f.w.ID, 0); err != nil && !errors.Is(err, workspace.ErrArchiveUncertain) {
			t.Fatal(err)
		}
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, push); !errors.Is(err, workspace.ErrAdmissionClosed) && !errors.Is(err, workspace.ErrPushPending) {
			// An archive without a pending publication archives at once: the workspace no longer admits.
			t.Errorf("archived: %v", err)
		}
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("accepted commit moved to %s", got)
		}
	})

	t.Run("a refused diverged history is audited and never accepted", func(t *testing.T) {
		f := newWriter(t)
		rewritten := f.e.Inspector.Add("rewritten", f.e.Inspector.Seed("unrelated root"))
		push := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, rewritten)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, rewritten, push); !errors.Is(err, workspace.ErrDiverged) {
			t.Fatalf("got %v", err)
		}
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("accepted %s", got)
		}
		if auditHas(t, f.e, f.w, "workspace.checkpoint_refused") != 1 || checkpointEventCount(t, f.e, f.w) != 0 || len(f.e.Hosts.PinOps) != 0 {
			t.Fatal("the refusal was not the only effect")
		}
	})

	t.Run("a late acceptance of an earlier push never moves the accepted commit back", func(t *testing.T) {
		f := newWriter(t)
		c1 := f.e.Inspector.Add("first", f.app.AcceptedCommit)
		c2 := f.e.Inspector.Add("second", c1)
		p1 := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c1)
		p2 := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, c1, c2)
		if r, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c2, p2); err != nil || !r.Advanced || f.accepted(t, wt.RepoApp) != c2 {
			t.Fatalf("second push: %+v %v", r, err)
		}
		r, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, p1)
		if err != nil || r.Advanced || r.PinRef != "refs/kodama/runs/run_1/2" {
			t.Fatalf("first push, late: %+v %v", r, err)
		}
		if got := f.accepted(t, wt.RepoApp); got != c2 {
			t.Fatalf("the accepted commit moved back to %s", got)
		}
		if n := checkpointEventCount(t, f.e, f.w); n != 1 {
			t.Fatalf("%d checkpoint events", n)
		}
	})

	t.Run("a dependency outage accepts nothing", func(t *testing.T) {
		f := newWriter(t)
		c1 := f.e.Inspector.Add("feature", f.app.AcceptedCommit)
		push := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c1)
		f.e.Pushes.Err = errors.New("push store down")
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, push); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("push store: %v", err)
		}
		f.e.Pushes.Err = nil
		f.e.Inspector.Err = errors.New("host unreadable")
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, push); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("inspector: %v", err)
		}
		f.e.Inspector.Err = nil
		f.e.Secrets.Set("att_1")
		f.e.Disclosed.Err = errors.New("disclosures down")
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, push); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("disclosures: %v", err)
		}
		f.e.Disclosed.Err = nil
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("an outage moved the accepted commit to %s", got)
		}
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c1, push); err != nil {
			t.Fatalf("after the outage: %v", err)
		}
	})

	t.Run("without the internal ports no checkpoint is accepted", func(t *testing.T) {
		e := wt.NewMemServiceEnv(t)
		e.Attempts.Register(scope, "att_1", "ws_1", workspace.AccessWrite)
		e.Authz.Grant(wt.ExecutionService.ID, scope)
		_, err := e.Svc.AcceptCheckpoint(bg, wt.ExecutionService, workspace.CheckpointInput{Scope: scope, AttemptID: "att_1", RepositoryID: wt.RepoApp,
			Ref: taskBranch("wi_1"), Commit: workspace.Commit(strings.Repeat("a", 40)), PushID: "push_1"})
		if !errors.Is(err, workspace.ErrNotConfigured) {
			t.Fatalf("got %v", err)
		}
	})
}

// Test_unit_workspace_retained_pins_internal: WS-02, WS-12, GR-13. Each accepted
// checkpoint is kept reachable on the internal host by refs/kodama/runs/<run>/<n>,
// created idempotently through the worker, numbered per run and repository,
// tolerant of other modules' pins and of lost responses.
func Test_unit_workspace_retained_pins_internal(t *testing.T) {
	scope := wt.ScopeA

	t.Run("numbered per run and repository on the internal host", func(t *testing.T) {
		f := newWriter(t)
		prev := f.app.AcceptedCommit
		for i := 1; i <= 3; i++ {
			c := f.e.Inspector.Add(strings.Repeat("x", i), prev)
			p := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, prev, c)
			r, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, p)
			want := "refs/kodama/runs/run_1/" + string(rune('0'+i))
			if err != nil || r.PinRef != want || f.e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), want) != c {
				t.Fatalf("checkpoint %d: %+v %v (pin %s)", i, r, err, f.e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), want))
			}
			prev = c
		}
		// Every checkpoint of the run stays pinned: the older refs still hold their commits.
		if got := f.e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), "refs/kodama/runs/run_1/1"); got == "" || got == prev {
			t.Fatalf("the first pin was lost or overwritten: %s", got)
		}
		// Another repository numbers from one again.
		d := f.e.Inspector.Add("docs", f.doc.AcceptedCommit)
		p := pushed(f.e, f.w, "att_1", "run_1", wt.RepoDocs, f.doc.AcceptedCommit, d)
		if r, err := accept(f.e, f.w, "att_1", wt.RepoDocs, f.ref, d, p); err != nil || r.PinRef != "refs/kodama/runs/run_1/1" {
			t.Fatalf("%+v %v", r, err)
		}
		// Pins are named for the pushing run: a later attempt of another run starts at one.
		settle(t, f.e, f.w, "att_1", workspace.OutcomeSucceeded)
		f.w = f.e.InternalWriter(f.w, "att_2")
		c := f.e.Inspector.Add("run two", prev)
		p2 := pushed(f.e, f.w, "att_2", "run_2", wt.RepoApp, prev, c)
		if r, err := accept(f.e, f.w, "att_2", wt.RepoApp, f.ref, c, p2); err != nil || r.PinRef != "refs/kodama/runs/run_2/1" {
			t.Fatalf("%+v %v", r, err)
		}
	})

	t.Run("a number another module used for another commit is skipped", func(t *testing.T) {
		f := newWriter(t)
		other := f.e.Inspector.Add("a source pinned by admission")
		f.e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), "refs/kodama/runs/run_1/1", other)
		c := f.e.Inspector.Add("feature", f.app.AcceptedCommit)
		p := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c)
		r, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, p)
		if err != nil || r.PinRef != "refs/kodama/runs/run_1/2" {
			t.Fatalf("%+v %v", r, err)
		}
		if got := f.e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), "refs/kodama/runs/run_1/1"); got != other {
			t.Fatalf("another module's pin was overwritten: %s", got)
		}
		if f.e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), "refs/kodama/runs/run_1/2") != c {
			t.Fatal("the checkpoint is not pinned at the next number")
		}
	})

	t.Run("a commit already pinned at the number is adopted", func(t *testing.T) {
		f := newWriter(t)
		c := f.e.Inspector.Add("feature", f.app.AcceptedCommit)
		f.e.Hosts.SetRef(scope.TenantID, string(wt.RepoApp), "refs/kodama/runs/run_1/1", c)
		p := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c)
		if r, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, p); err != nil || r.PinRef != "refs/kodama/runs/run_1/1" {
			t.Fatalf("%+v %v", r, err)
		}
	})

	t.Run("a lost pin response is settled by reading the ref", func(t *testing.T) {
		f := newWriter(t)
		f.e.Hosts.FailPin(errors.New("connection reset"), true, 1) // applied, response lost
		c := f.e.Inspector.Add("feature", f.app.AcceptedCommit)
		p := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c)
		r, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, p)
		if err != nil || r.PinRef != "refs/kodama/runs/run_1/1" || f.e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), r.PinRef) != c {
			t.Fatalf("%+v %v", r, err)
		}
	})

	t.Run("an unconfirmed pin leaves the checkpoint accepted and is finished by a repeat or the reconciler", func(t *testing.T) {
		f := newWriter(t)
		f.e.Hosts.FailPin(errors.New("worker unavailable"), false, -1)
		c := f.e.Inspector.Add("feature", f.app.AcceptedCommit)
		p := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, p); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		// The checkpoint itself is durable: the accepted commit moved, once, with its event.
		if f.accepted(t, wt.RepoApp) != c || checkpointEventCount(t, f.e, f.w) != 1 {
			t.Fatal("the acceptance was not recorded before the pin")
		}
		if got := f.e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), "refs/kodama/runs/run_1/1"); got != "" {
			t.Fatalf("the pin exists: %s", got)
		}
		cands, err := f.e.Store.Reconcilable(bg, scope.TenantID, "", 10)
		if err != nil || len(cands) != 1 || cands[0].ID != f.w.ID {
			t.Fatalf("the workspace with an unconfirmed pin is not reconcilable: %+v %v", cands, err)
		}
		f.e.Hosts.FailPin(nil, false, 0)
		if err := f.e.Svc.ReconcileWorkspace(bg, scope, f.w.ID); err != nil {
			t.Fatal(err)
		}
		if f.e.Hosts.Ref(scope.TenantID, string(wt.RepoApp), "refs/kodama/runs/run_1/1") != c {
			t.Fatal("the reconciler did not pin the checkpoint")
		}
		// The retried operations are one operation: the same identifier every time.
		if ops := distinct(f.e.Hosts.PinOps); len(ops) != 1 {
			t.Fatalf("pin operations: %v", f.e.Hosts.PinOps)
		}
		if cands, _ := f.e.Store.Reconcilable(bg, scope.TenantID, "", 10); len(cands) != 0 {
			t.Fatalf("still reconcilable: %+v", cands)
		}
		again, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, p)
		if err != nil || !again.Replay || checkpointEventCount(t, f.e, f.w) != 1 {
			t.Fatalf("%+v %v", again, err)
		}
	})

	t.Run("a pin is only ever created by the worker through the contract", func(t *testing.T) {
		f := newWriter(t)
		c := f.e.Inspector.Add("feature", f.app.AcceptedCommit)
		p := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, p); err != nil {
			t.Fatal(err)
		}
		if len(f.e.Hosts.PinOps) != 1 || !strings.HasPrefix(f.e.Hosts.PinOps[0], "wspin_") {
			t.Fatalf("pin operations: %v", f.e.Hosts.PinOps)
		}
		// The external retained-ref namespace is not used for internal pins.
		if pins, err := f.e.Store.View(bg, scope, f.w.ID, workspace.Load{Pins: true}); err != nil || len(pins.Pins) != 0 {
			t.Fatalf("external pins: %+v %v", pins, err)
		}
	})
}

// Test_unit_workspace_prior_disclosure_scan_on_push: the disclosure scan of
// disclosures.go is applied to internal pushes before a commit output is
// accepted: the attempt's injected values and those the run disclosed to its
// other steps must not appear in any object the push introduced.
func Test_unit_workspace_prior_disclosure_scan_on_push(t *testing.T) {
	scope := wt.ScopeA
	const own, prior = "own-injected-secret-1234", "prior-step-secret-5678"

	setup := func(t *testing.T) writerFixture {
		f := newWriter(t)
		f.e.Secrets.Set("att_1", own)
		f.e.Disclosed.Set("att_1", prior)
		return f
	}
	refusedClean := func(t *testing.T, f writerFixture, c workspace.Commit, push string, wantAudit int) {
		t.Helper()
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, push); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("got %v", err)
		}
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("a commit with a secret moved the accepted commit to %s", got)
		}
		if checkpointEventCount(t, f.e, f.w) != 0 || len(f.e.Hosts.PinOps) != 0 {
			t.Fatal("a refused commit had an effect")
		}
		if got := auditHas(t, f.e, f.w, "workspace.checkpoint_refused"); got != wantAudit {
			t.Fatalf("refusals audited: %d, want %d", got, wantAudit)
		}
	}

	t.Run("the attempt's own injected value is refused", func(t *testing.T) {
		f := setup(t)
		c := f.e.Inspector.Add("config: token="+own, f.app.AcceptedCommit)
		refusedClean(t, f, c, pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c), 1)
	})

	t.Run("a value disclosed to another step of the run is refused", func(t *testing.T) {
		f := setup(t)
		c := f.e.Inspector.Add("generated file with "+prior, f.app.AcceptedCommit)
		refusedClean(t, f, c, pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c), 1)
	})

	t.Run("a value in an earlier commit of the push is refused although the tip is clean", func(t *testing.T) {
		f := setup(t)
		leaky := f.e.Inspector.Add("oops "+own, f.app.AcceptedCommit)
		c := f.e.Inspector.Add("remove it again", leaky)
		refusedClean(t, f, c, pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c), 1)
	})

	t.Run("a clean commit is accepted and the scan used both sets of values", func(t *testing.T) {
		f := setup(t)
		c := f.e.Inspector.Add("a harmless change", f.app.AcceptedCommit)
		p := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, p); err != nil {
			t.Fatal(err)
		}
		reqs := f.e.Inspector.Requests
		last := reqs[len(reqs)-1]
		if last.Since != f.app.AcceptedCommit || last.Commit != c || last.Ref != f.ref || last.Repo.Scope != scope {
			t.Fatalf("inspection: %+v", last)
		}
		var got []string
		for _, n := range last.Needles {
			got = append(got, string(n))
		}
		if !contains(got, own) || !contains(got, prior) {
			t.Fatalf("the scan was given %v, want the own and the prior value", got)
		}
		// The credential reference given to the host is read-only: no publisher reference exists.
		if last.Repo.PublisherSecretRef != workspace.NoPublisherRef || !strings.HasPrefix(last.Repo.CheckoutSecretRef, "kodama-git-read:") {
			t.Fatalf("repo reference: %+v", last.Repo)
		}
	})

	t.Run("a value that was already in the accepted history is not a new finding", func(t *testing.T) {
		f := newWriter(t)
		// The accepted commit already contains it (a human put it there); a later clean push is fine.
		leaked := f.e.Inspector.Add("human wrote "+own, f.app.AcceptedCommit)
		f.e.HumanPush(scope, wt.RepoApp, f.ref, f.app.AcceptedCommit, leaked)
		settle(t, f.e, f.w, "att_1", workspace.OutcomeFailed)
		f.w = f.e.InternalWriter(f.w, "att_2") // admission adopts the human fast-forward
		f.e.Secrets.Set("att_2", own)
		c := f.e.Inspector.Add("clean follow-up", leaked)
		p := pushed(f.e, f.w, "att_2", "run_2", wt.RepoApp, leaked, c)
		if _, err := accept(f.e, f.w, "att_2", wt.RepoApp, f.ref, c, p); err != nil {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("values too short to scan for are ignored, as for bundles", func(t *testing.T) {
		f := newWriter(t)
		f.e.Secrets.Set("att_1", "abc")
		c := f.e.Inspector.Add("contains abc", f.app.AcceptedCommit)
		p := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, c)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, c, p); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a pushed secret stays refused until an engineer rewrites the branch", func(t *testing.T) {
		f := setup(t)
		leaky := f.e.Inspector.Add("oops "+own, f.app.AcceptedCommit)
		p1 := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, f.app.AcceptedCommit, leaky)
		refusedClean(t, f, leaky, p1, 1)
		// Removing the value in a follow-up commit does not help: the push still introduces the leaking one.
		fixed := f.e.Inspector.Add("remove it again", leaky)
		p2 := pushed(f.e, f.w, "att_1", "run_1", wt.RepoApp, leaky, fixed)
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, fixed, p2); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("a follow-up over the leaking commit: %v", err)
		}
		// A writer cannot rewrite the task branch: such a push is a rewrite and is never accepted.
		clean := f.e.Inspector.Add("clean", f.app.AcceptedCommit)
		p3 := f.e.Push(scope, wt.RepoApp, "att_1", "run_1", wt.PushEntry{Ref: f.ref, Old: fixed, New: clean, State: gitv1.EntryApplied, Kind: gitv1.KindRewrite})
		if _, err := accept(f.e, f.w, "att_1", wt.RepoApp, f.ref, clean, p3); !errors.Is(err, workspace.ErrPushNotApplied) {
			t.Fatalf("a rewrite: %v", err)
		}
		if got := f.accepted(t, wt.RepoApp); got != f.app.AcceptedCommit {
			t.Fatalf("accepted %s", got)
		}
	})
}
