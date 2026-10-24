package logstest

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// World is the fixture surface a Contract subject provides: attempts the
// adapters know, and the diagnostic files a wrapper uploads. The memory adapter
// implements it over memdb; the PostgreSQL composition implements it over the
// execution and artifact fixtures of Env.
type World interface {
	// Attempt creates an attempt in scope that the adapters know and returns
	// it as the log module sees it.
	Attempt(tb testing.TB, scope domain.Scope) logs.AttemptInfo
	// Upload stores a usable diagnostic file of the attempt and returns its ID.
	Upload(tb testing.TB, a logs.AttemptInfo, name, mediaType string, data []byte) domain.ArtifactID
	// Audits counts the audit entries of the record, which every change writes
	// in its own transaction.
	Audits(tb testing.TB, scope domain.Scope, id logs.RecordID) int
	// Remove applies retention to the artifact. It reports false when the
	// backend cannot model removal, which skips the cases that need it.
	Remove(tb testing.TB, a logs.AttemptInfo, id domain.ArtifactID) bool
}

// Subject is one adapter set under the Contract.
type Subject struct {
	Store    logs.Store
	Retained logs.Retained
	World    World
	// Scopes are the tenants and projects the subject has seeded; the Contract
	// creates attempts only in these.
	Scopes Scopes
}

// Scopes are two projects of one tenant and a project of another tenant.
type Scopes struct{ A, A2, B domain.Scope }

// Subject returns the memory adapters as a Contract subject.
func (m *Memory) Subject() Subject {
	return Subject{Store: m.Store(), Retained: m.Retained(), World: memWorld{m}, Scopes: Scopes(ContractScopes)}
}

type memWorld struct{ m *Memory }

func (w memWorld) Attempt(tb testing.TB, scope domain.Scope) logs.AttemptInfo {
	tb.Helper()
	a := w.m.NewAttempt(tb, scope, AttemptOptions{Observed: true})
	info, err := w.m.Attempts().ByID(context.Background(), scope, a.ID)
	if err != nil {
		tb.Fatal(err)
	}
	return info
}

func (w memWorld) Upload(tb testing.TB, a logs.AttemptInfo, name, media string, data []byte) domain.ArtifactID {
	tb.Helper()
	return w.m.Upload(tb, MemoryAttempt{Scope: a.Scope, Run: a.Run, ID: a.ID}, name, media, data)
}

func (w memWorld) Audits(tb testing.TB, scope domain.Scope, id logs.RecordID) int {
	tb.Helper()
	return len(w.m.Audit(tb, scope, id))
}

func (w memWorld) Remove(tb testing.TB, a logs.AttemptInfo, id domain.ArtifactID) bool {
	tb.Helper()
	w.m.RemoveArtifact(tb, MemoryAttempt{Scope: a.Scope, Run: a.Run, ID: a.ID}, id)
	return true
}

// ContractScopes are the scopes of the memory subject.
var ContractScopes = struct{ A, A2, B domain.Scope }{
	A:  domain.Scope{TenantID: "t081_acme", ProjectID: "p081_web"},
	A2: domain.Scope{TenantID: "t081_acme", ProjectID: "p081_api"},
	B:  domain.Scope{TenantID: "t081_globex", ProjectID: "p081_ops"},
}

func refOf(id domain.ArtifactID, data []byte) logs.ArtifactRef {
	return logs.ArtifactRef{ID: id, Digest: sum(data), Size: int64(len(data))}
}

func sameLink(a, b logs.Link) bool {
	if a.Log != b.Log || a.Completeness != b.Completeness || len(a.Reasons) != len(b.Reasons) || (a.Meta == nil) != (b.Meta == nil) {
		return false
	}
	for i := range a.Reasons {
		if a.Reasons[i] != b.Reasons[i] {
			return false
		}
	}
	return a.Meta == nil || *a.Meta == *b.Meta
}

func template(tb testing.TB, a logs.AttemptInfo, src logs.Source, id logs.RecordID, now time.Time) logs.Record {
	tb.Helper()
	r, err := logs.NewRecord(a.Scope, id, a.ID, a.Run, a.Step, src, 1, now)
	if err != nil {
		tb.Fatal(err)
	}
	return *r
}

// Contract is the behavior every adapter set of the log ports promises (SD-13):
// capture records are tenant and project scoped and ensured once, a failed
// mutation changes nothing, claims are fenced, verified linkage never changes,
// generations are bounded and follow lost bytes only, and retained logs are
// opened only for the scoped attempt that owns them with the exact verified
// content. It asserts what callers rely on, not how a backend does it: SQL,
// row-level security and lock order belong to the PostgreSQL checkpoints
// (SD-14).
func Contract(t *testing.T, open func(testing.TB) Subject) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC).Truncate(time.Microsecond)

	run := func(name string, body func(t *testing.T, s Subject)) {
		t.Run(name, func(t *testing.T) { body(t, open(t)) })
	}
	ensure := func(t *testing.T, s Subject, a logs.AttemptInfo, src logs.Source) *logs.Record {
		t.Helper()
		rec, err := s.Store.Ensure(ctx, template(t, a, src, logs.RandomIDs{}.NewRecordID(), t0))
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
		return rec
	}
	observe := func(s Subject, scope domain.Scope, id logs.RecordID, l logs.Link) (*logs.Record, error) {
		return s.Store.Update(ctx, scope, id, func(r *logs.Record) (bool, logs.Audit, error) {
			ch, err := r.Observe(l, t0)
			return ch, logs.Audit{Action: "log.linked"}, err
		})
	}
	complete := logs.Statement{Completeness: logs.Complete}
	unknown := logs.Statement{Completeness: logs.Unknown, Reasons: []logs.Reason{contract.ReasonControllerCapture}}
	snapshot := func(gen int, data string) logs.Snapshot {
		name := contract.ControllerLogName(gen)
		meta, _ := contract.MetadataName(name)
		return logs.Snapshot{LogName: name, MetaName: meta, Digest: sum([]byte(data)), Size: int64(len(data)), Statement: unknown}
	}

	run("a capture is ensured once, whoever asks", func(t *testing.T, s Subject) {
		scopes := s.Scopes
		a := s.World.Attempt(t, scopes.A)
		first := ensure(t, s, a, logs.SourceWrapper)
		if first.State != logs.StatePending || first.Generation != 1 || first.Version != 1 || first.Scope != a.Scope || first.Attempt != a.ID {
			t.Fatalf("a new record is pending generation 1: %+v", first)
		}
		var wg sync.WaitGroup
		ids := make([]logs.RecordID, 8)
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec, err := s.Store.Ensure(ctx, template(t, a, logs.SourceWrapper, logs.RandomIDs{}.NewRecordID(), t0))
				if err != nil {
					t.Error(err)
					return
				}
				ids[i] = rec.ID
			}()
		}
		wg.Wait()
		for i, id := range ids {
			if id != first.ID {
				t.Fatalf("caller %d got record %s, want %s", i, id, first.ID)
			}
		}
		other := ensure(t, s, a, logs.SourceController)
		if other.ID == first.ID || other.Source != logs.SourceController {
			t.Fatalf("each source has its own record: %+v", other)
		}
		got, err := s.Store.ForAttempt(ctx, a.Scope, a.ID)
		if err != nil || len(got) != 2 || got[0].Source != logs.SourceController || got[1].Source != logs.SourceWrapper {
			t.Fatalf("records are ordered by source: %+v %v", got, err)
		}
	})

	run("records never cross tenant, project or attempt", func(t *testing.T, s Subject) {
		scopes := s.Scopes
		a := s.World.Attempt(t, scopes.A)
		b := s.World.Attempt(t, scopes.B)
		rec := ensure(t, s, a, logs.SourceWrapper)
		if got, err := s.Store.Get(ctx, a.Scope, rec.ID); err != nil || got.ID != rec.ID {
			t.Fatalf("own record: %+v %v", got, err)
		}
		for name, sc := range map[string]domain.Scope{
			"other tenant":  scopes.B,
			"other project": scopes.A2,
			"mixed":         {TenantID: scopes.A.TenantID, ProjectID: scopes.B.ProjectID},
		} {
			if _, err := s.Store.Get(ctx, sc, rec.ID); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("%s: Get = %v", name, err)
			}
			if rs, err := s.Store.ForAttempt(ctx, sc, a.ID); err != nil || len(rs) != 0 {
				t.Errorf("%s: ForAttempt = %v %v", name, rs, err)
			}
			if _, err := s.Store.Update(ctx, sc, rec.ID, func(*logs.Record) (bool, logs.Audit, error) { return false, logs.Audit{}, nil }); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("%s: Update = %v", name, err)
			}
		}
		// A record cannot bind an attempt of another scope.
		foreign := template(t, a, logs.SourceWrapper, "logrec_foreign", t0)
		foreign.Scope = b.Scope
		if _, err := s.Store.Ensure(ctx, foreign); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("a record binding another scope's attempt must be refused: %v", err)
		}
		if rs, err := s.Store.ForAttempt(ctx, b.Scope, b.ID); err != nil || len(rs) != 0 {
			t.Fatalf("the refused record left a trace: %v %v", rs, err)
		}
	})

	run("a record identifier names one capture and is never reused", func(t *testing.T, s Subject) {
		scopes := s.Scopes
		a := s.World.Attempt(t, scopes.A)
		b := s.World.Attempt(t, scopes.A)
		data := []byte("alpha\n")
		const shared = logs.RecordID("logrec_shared")
		first, err := s.Store.Ensure(ctx, template(t, a, logs.SourceWrapper, shared, t0))
		if err != nil {
			t.Fatal(err)
		}
		link := logs.Link{Log: refOf(s.World.Upload(t, a, contract.CommandLogName, contract.LogMediaType, data), data), Statement: complete}
		retained, err := observe(s, a.Scope, first.ID, link)
		if err != nil || retained.State != logs.StateRetained {
			t.Fatalf("observe: %+v %v", retained, err)
		}
		audits := s.World.Audits(t, a.Scope, shared)

		// Another capture of the same project cannot take the identifier.
		for name, tmpl := range map[string]logs.Record{
			"another attempt":                template(t, b, logs.SourceWrapper, shared, t0),
			"another source":                 template(t, b, logs.SourceController, shared, t0),
			"the same attempt, other source": template(t, a, logs.SourceController, shared, t0),
		} {
			if _, err := s.Store.Ensure(ctx, tmpl); !errors.Is(err, logs.ErrConflict) {
				t.Errorf("ensure with %s: %v", name, err)
			}
		}
		if rs, err := s.Store.ForAttempt(ctx, b.Scope, b.ID); err != nil || len(rs) != 0 {
			t.Fatalf("a refused record left a trace: %v %v", rs, err)
		}
		got, err := s.Store.Get(ctx, a.Scope, shared)
		if err != nil || got.Attempt != a.ID || got.State != logs.StateRetained || got.Version != retained.Version || got.Link == nil || !sameLink(*got.Link, link) {
			t.Fatalf("the original record was changed: %+v %v", got, err)
		}
		if n := s.World.Audits(t, a.Scope, shared); n != audits {
			t.Fatalf("the original audit history changed: %d entries, was %d", audits, n)
		}

		// A later generation cannot take it either.
		ctrl := ensure(t, s, b, logs.SourceController)
		var claim logs.Claim
		step := func(fn logs.Mutation) {
			t.Helper()
			if _, err := s.Store.Update(ctx, b.Scope, ctrl.ID, fn); err != nil {
				t.Fatal(err)
			}
		}
		step(func(r *logs.Record) (bool, logs.Audit, error) {
			c, err := r.Begin("tok", time.Minute, t0)
			claim = c
			return err == nil, logs.Audit{Action: "log.collecting"}, err
		})
		step(func(r *logs.Record) (bool, logs.Audit, error) {
			ch, err := r.Freeze(claim, snapshot(1, "bytes\n"), t0)
			return ch, logs.Audit{Action: "log.frozen"}, err
		})
		step(func(r *logs.Record) (bool, logs.Audit, error) {
			err := r.Fail(claim, logs.FailureFrozenLost, t0)
			return err == nil, logs.Audit{Action: "log.unavailable"}, err
		})
		lost, _ := s.Store.Get(ctx, b.Scope, ctrl.ID)
		next, err := lost.Next(shared, t0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.Create(ctx, next); !errors.Is(err, logs.ErrConflict) {
			t.Fatalf("create with a used identifier: %v", err)
		}
		if rs, _ := s.Store.ForAttempt(ctx, b.Scope, b.ID); len(rs) != 1 {
			t.Fatalf("a refused generation left a trace: %d records", len(rs))
		}
		if got, _ := s.Store.Get(ctx, a.Scope, shared); got.Attempt != a.ID || got.Version != retained.Version {
			t.Fatalf("the original record was changed: %+v", got)
		}
	})

	run("a failed or empty mutation changes nothing", func(t *testing.T, s Subject) {
		scopes := s.Scopes
		a := s.World.Attempt(t, scopes.A)
		rec := ensure(t, s, a, logs.SourceController)
		boom := errors.New("boom")
		_, err := s.Store.Update(ctx, a.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			if _, err := r.Begin("tok", time.Minute, t0); err != nil {
				return false, logs.Audit{}, err
			}
			return true, logs.Audit{Action: "log.collecting"}, boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if got, _ := s.Store.Get(ctx, a.Scope, rec.ID); got.State != logs.StatePending || got.Version != 1 || got.Claims != 0 {
			t.Fatalf("a failed mutation changed state: %+v", got)
		}
		got, err := s.Store.Update(ctx, a.Scope, rec.ID, func(*logs.Record) (bool, logs.Audit, error) { return false, logs.Audit{}, nil })
		if err != nil || got.Version != 1 {
			t.Fatalf("an unchanged mutation writes nothing: %+v %v", got, err)
		}
		if _, err := s.Store.Update(ctx, a.Scope, "logrec_missing", func(*logs.Record) (bool, logs.Audit, error) { return false, logs.Audit{}, nil }); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("an unknown record: %v", err)
		}
	})

	run("collection claims are fenced", func(t *testing.T, s Subject) {
		scopes := s.Scopes
		a := s.World.Attempt(t, scopes.A)
		rec := ensure(t, s, a, logs.SourceController)
		var first, second logs.Claim
		if _, err := s.Store.Update(ctx, a.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			c, err := r.Begin("tok1", time.Minute, t0)
			first = c
			return err == nil, logs.Audit{Action: "log.collecting"}, err
		}); err != nil || first.Fence != 1 {
			t.Fatalf("first claim: %+v %v", first, err)
		}
		_, err := s.Store.Update(ctx, a.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			_, err := r.Begin("tok2", time.Minute, t0.Add(10*time.Second))
			return err == nil, logs.Audit{Action: "log.collecting"}, err
		})
		if !errors.Is(err, logs.ErrBusy) {
			t.Fatalf("a live claim blocks another collector: %v", err)
		}
		if _, err := s.Store.Update(ctx, a.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			c, err := r.Begin("tok2", time.Minute, t0.Add(2*time.Minute))
			second = c
			return err == nil, logs.Audit{Action: "log.collecting"}, err
		}); err != nil || second.Fence != 2 {
			t.Fatalf("an expired claim is taken over with a higher fence: %+v %v", second, err)
		}
		_, err = s.Store.Update(ctx, a.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			ch, err := r.Freeze(first, snapshot(1, "stale\n"), t0.Add(3*time.Minute))
			return ch, logs.Audit{Action: "log.frozen"}, err
		})
		if !errors.Is(err, logs.ErrClaimLost) {
			t.Fatalf("a fenced-out collector must not commit: %v", err)
		}
		if got, _ := s.Store.Get(ctx, a.Scope, rec.ID); got.Snapshot != nil || got.Claim.Fence != 2 {
			t.Fatalf("the stale collector left a trace: %+v", got)
		}
	})

	run("verified linkage never changes", func(t *testing.T, s Subject) {
		scopes := s.Scopes
		a := s.World.Attempt(t, scopes.A)
		rec := ensure(t, s, a, logs.SourceWrapper)
		data := []byte("alpha\n")
		link := logs.Link{Log: refOf(s.World.Upload(t, a, contract.CommandLogName, contract.LogMediaType, data), data), Statement: complete}
		got, err := observe(s, a.Scope, rec.ID, link)
		if err != nil || got.State != logs.StateRetained || got.Link == nil || !sameLink(*got.Link, link) {
			t.Fatalf("observe: %+v %v", got, err)
		}
		again, err := observe(s, a.Scope, rec.ID, link)
		if err != nil || again.Version != got.Version {
			t.Fatalf("the identical linkage is a no-op: %+v %v", again, err)
		}
		if err := func() error {
			shrunk := link
			shrunk.Statement = logs.Statement{Completeness: logs.Unknown, Reasons: []logs.Reason{contract.ReasonUnverified}}
			_, err := observe(s, a.Scope, rec.ID, shrunk)
			return err
		}(); !errors.Is(err, logs.ErrConflict) {
			t.Fatalf("a different linkage must not replace the evidence: %v", err)
		}
		if cur, _ := s.Store.Get(ctx, a.Scope, rec.ID); cur.Link.Completeness != logs.Complete || cur.Version != got.Version {
			t.Fatalf("completeness was rewritten: %+v", cur)
		}
		removed, err := s.Store.Update(ctx, a.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			ch, err := r.MarkRemoved(t0)
			return ch, logs.Audit{Action: "log.removed"}, err
		})
		if err != nil || removed.Available() || removed.Link == nil || removed.Link.Completeness != logs.Complete {
			t.Fatalf("removal changes availability only: %+v %v", removed, err)
		}
	})

	run("generations are bounded and follow lost frozen bytes only", func(t *testing.T, s Subject) {
		scopes := s.Scopes
		a := s.World.Attempt(t, scopes.A)
		rec := ensure(t, s, a, logs.SourceController)
		premature, err := rec.Next("logrec_premature", t0)
		if !errors.Is(err, logs.ErrInvalidTransition) || premature != nil {
			t.Fatalf("an unfinished capture has no next generation: %v", err)
		}
		skip := template(t, a, logs.SourceController, "logrec_skip", t0)
		skip.Generation = 2
		if _, err := s.Store.Create(ctx, &skip); !errors.Is(err, logs.ErrConflict) {
			t.Fatalf("a generation above 1 needs lost frozen bytes: %v", err)
		}
		loseBytes := func(id logs.RecordID, gen int) {
			var claim logs.Claim
			if _, err := s.Store.Update(ctx, a.Scope, id, func(r *logs.Record) (bool, logs.Audit, error) {
				c, err := r.Begin("tok", time.Minute, t0)
				claim = c
				return err == nil, logs.Audit{Action: "log.collecting"}, err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.Update(ctx, a.Scope, id, func(r *logs.Record) (bool, logs.Audit, error) {
				ch, err := r.Freeze(claim, snapshot(gen, "bytes\n"), t0)
				return ch, logs.Audit{Action: "log.frozen"}, err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.Update(ctx, a.Scope, id, func(r *logs.Record) (bool, logs.Audit, error) {
				err := r.Fail(claim, logs.FailureFrozenLost, t0)
				return err == nil, logs.Audit{Action: "log.unavailable"}, err
			}); err != nil {
				t.Fatal(err)
			}
		}
		cur := rec
		for gen := 1; gen < logs.MaxGenerations; gen++ {
			loseBytes(cur.ID, gen)
			lost, _ := s.Store.Get(ctx, a.Scope, cur.ID)
			next, err := lost.Next(logs.RandomIDs{}.NewRecordID(), t0)
			if err != nil {
				t.Fatalf("generation %d: %v", gen, err)
			}
			created, err := s.Store.Create(ctx, next)
			if err != nil || created.Generation != gen+1 || created.State != logs.StatePending {
				t.Fatalf("create generation %d: %+v %v", gen+1, created, err)
			}
			if again, err := s.Store.Create(ctx, next); err != nil || again.ID != created.ID {
				t.Fatalf("an identical create returns the existing generation: %+v %v", again, err)
			}
			if latest := ensure(t, s, a, logs.SourceController); latest.ID != created.ID {
				t.Fatalf("ensure returns the latest generation: %+v", latest)
			}
			cur = created
		}
		loseBytes(cur.ID, cur.Generation)
		lost, _ := s.Store.Get(ctx, a.Scope, cur.ID)
		if _, err := lost.Next("logrec_beyond", t0); !errors.Is(err, logs.ErrGenerationsExhausted) {
			t.Fatalf("generations are bounded: %v", err)
		}
		if all, err := s.Store.ForAttempt(ctx, a.Scope, a.ID); err != nil || len(all) != logs.MaxGenerations || all[0].Generation != 1 || all[len(all)-1].Generation != logs.MaxGenerations {
			t.Fatalf("the earlier failures stay recorded: %d %v", len(all), err)
		}
	})

	run("the wrapper's log is found only for its own scoped attempt", func(t *testing.T, s Subject) {
		scopes := s.Scopes
		a := s.World.Attempt(t, scopes.A)
		if _, err := s.Retained.WrapperLog(ctx, a.Scope, a.ID); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("an attempt without a log: %v", err)
		}
		data := []byte("hello\nworld\n")
		id := s.World.Upload(t, a, contract.CommandLogName, contract.LogMediaType, data)
		link, err := s.Retained.WrapperLog(ctx, a.Scope, a.ID)
		if err != nil || link.Log != refOf(id, data) || link.Meta != nil || link.Completeness != logs.Unknown || len(link.Reasons) != 1 || link.Reasons[0] != contract.ReasonUnverified {
			t.Fatalf("a log without a sidecar has unknown completeness: %+v %v", link, err)
		}
		for name, sc := range map[string]domain.Scope{"other tenant": scopes.B, "other project": scopes.A2} {
			if _, err := s.Retained.WrapperLog(ctx, sc, a.ID); !errors.Is(err, logs.ErrNoLog) && !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		other := s.World.Attempt(t, scopes.A)
		if _, err := s.Retained.WrapperLog(ctx, other.Scope, other.ID); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("another attempt's log is not this attempt's: %v", err)
		}

		sidecar := func(doc contract.Metadata) {
			b, err := contract.Encode(doc)
			if err != nil {
				t.Fatal(err)
			}
			name, _ := contract.MetadataName(contract.CommandLogName)
			s.World.Upload(t, a, name, contract.MediaType, b)
		}
		wrong := contract.Metadata{Version: contract.Version, Source: contract.SourceWrapper, LogName: contract.CommandLogName,
			Log: contract.Ref{ArtifactID: id, Digest: sum([]byte("different")), SizeBytes: int64(len(data))}, Completeness: contract.Complete}
		sidecar(wrong)
		if l, err := s.Retained.WrapperLog(ctx, a.Scope, a.ID); err != nil || l.Completeness != logs.Unknown || l.Meta != nil {
			t.Fatalf("a sidecar that describes another artifact is never trusted: %+v %v", l, err)
		}

		b := s.World.Attempt(t, scopes.A)
		bdata := []byte("bound\n")
		bid := s.World.Upload(t, b, contract.CommandLogName, contract.LogMediaType, bdata)
		doc := contract.Metadata{Version: contract.Version, Source: contract.SourceWrapper, LogName: contract.CommandLogName,
			Log: contract.Ref{ArtifactID: bid, Digest: sum(bdata), SizeBytes: int64(len(bdata))}, Completeness: contract.Partial, Reasons: []contract.Reason{contract.ReasonTruncated}}
		enc, _ := contract.Encode(doc)
		name, _ := contract.MetadataName(contract.CommandLogName)
		s.World.Upload(t, b, name, contract.MediaType, enc)
		l, err := s.Retained.WrapperLog(ctx, b.Scope, b.ID)
		if err != nil || l.Completeness != logs.Partial || l.Meta == nil || len(l.Reasons) != 1 || l.Reasons[0] != contract.ReasonTruncated {
			t.Fatalf("a bound sidecar states completeness: %+v %v", l, err)
		}
	})

	run("retained bytes open only for the exact verified artifact of the scoped attempt", func(t *testing.T, s Subject) {
		scopes := s.Scopes
		a := s.World.Attempt(t, scopes.A)
		other := s.World.Attempt(t, scopes.A)
		data := []byte("one\ntwo\n")
		id := s.World.Upload(t, a, contract.CommandLogName, contract.LogMediaType, data)
		ref := refOf(id, data)
		read := func(sc domain.Scope, at domain.AttemptID, r logs.ArtifactRef) ([]byte, error) {
			rc, err := s.Retained.Open(ctx, sc, at, r)
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
		if got, err := read(a.Scope, a.ID, ref); err != nil || string(got) != string(data) {
			t.Fatalf("own artifact: %q %v", got, err)
		}
		badDigest, badSize := ref, ref
		badDigest.Digest = sum([]byte("other"))
		badSize.Size++
		for name, c := range map[string]struct {
			scope   domain.Scope
			attempt domain.AttemptID
			ref     logs.ArtifactRef
		}{
			"a digest the artifact does not have": {a.Scope, a.ID, badDigest},
			"a size the artifact does not have":   {a.Scope, a.ID, badSize},
			"another attempt":                     {other.Scope, other.ID, ref},
			"another project":                     {scopes.A2, a.ID, ref},
			"another tenant":                      {scopes.B, a.ID, ref},
			"an unknown artifact":                 {a.Scope, a.ID, logs.ArtifactRef{ID: "art_missing", Digest: ref.Digest, Size: ref.Size}},
		} {
			if _, err := read(c.scope, c.attempt, c.ref); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if ok, err := s.Retained.Usable(ctx, a.Scope, a.ID, ref); err != nil || !ok {
			t.Fatalf("usable: %v %v", ok, err)
		}
		if _, err := s.Retained.Usable(ctx, scopes.B, a.ID, ref); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("a foreign scope cannot probe artifacts: %v", err)
		}
		if !s.World.Remove(t, a, id) {
			return
		}
		if ok, err := s.Retained.Usable(ctx, a.Scope, a.ID, ref); err != nil || ok {
			t.Fatalf("a removed artifact is not usable: %v %v", ok, err)
		}
		if _, err := read(a.Scope, a.ID, ref); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("a removed artifact has no bytes: %v", err)
		}
	})

	run("a controller capture is retained idempotently with its sidecar", func(t *testing.T, s Subject) {
		scopes := s.Scopes
		a := s.World.Attempt(t, scopes.A)
		const text = "captured\nlines\n"
		snap := snapshot(1, text)
		link, err := s.Retained.Store(ctx, a, snap, readSeeker(text))
		if err != nil || link.Meta == nil || link.Log.Digest != snap.Digest || link.Log.Size != snap.Size || link.Completeness != logs.Unknown {
			t.Fatalf("store: %+v %v", link, err)
		}
		again, err := s.Retained.Store(ctx, a, snap, readSeeker(text))
		if err != nil || !sameLink(again, link) {
			t.Fatalf("repeating the same snapshot returns the same link: %+v %v", again, err)
		}
		recovered, err := s.Retained.Store(ctx, a, snap, nil)
		if err != nil || !sameLink(recovered, link) {
			t.Fatalf("a restart recovers the uploaded capture: %+v %v", recovered, err)
		}
		rc, err := s.Retained.Open(ctx, a.Scope, a.ID, link.Log)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != text {
			t.Fatalf("retained bytes: %q", got)
		}
		if _, err := s.Retained.WrapperLog(ctx, a.Scope, a.ID); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("a controller capture is never the wrapper's log: %v", err)
		}
		fresh := s.World.Attempt(t, scopes.A)
		if _, err := s.Retained.Store(ctx, fresh, snapshot(1, "never uploaded\n"), nil); !errors.Is(err, logs.ErrFrozenBytesLost) {
			t.Fatalf("without bytes there is nothing to recover: %v", err)
		}
		if _, err := s.Retained.Store(ctx, a, logs.Snapshot{LogName: "evil.log", MetaName: "evil.log.meta.json", Digest: snap.Digest, Size: snap.Size, Statement: unknown}, readSeeker(text)); !errors.Is(err, logs.ErrInvalid) {
			t.Fatalf("only controller capture names are written: %v", err)
		}
	})
}

func readSeeker(s string) io.ReadSeeker { return strings.NewReader(s) }
