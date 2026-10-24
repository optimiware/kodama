package logs

import (
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs/contract"
)

var (
	t0    = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	scope = domain.Scope{TenantID: "t023_acme", ProjectID: "p023_app"}
)

const dig = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
const dig2 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func newRec(t *testing.T, src Source, gen int) *Record {
	t.Helper()
	r, err := NewRecord(scope, "logrec_1", "att_1", "run_1", "build", src, gen, t0)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ctrlSnap() Snapshot {
	return Snapshot{LogName: "controller-g1.log", MetaName: "controller-g1.log.meta.json", Digest: dig, Size: 10,
		Statement: Statement{Completeness: Unknown, Reasons: []Reason{contract.ReasonControllerCapture}}}
}

func ctrlLink() Link {
	return Link{Log: ArtifactRef{ID: "art_log", Digest: dig, Size: 10}, Meta: &ArtifactRef{ID: "art_meta", Digest: dig2, Size: 200},
		Statement: ctrlSnap().Statement}
}

func wrapperLink(c Completeness, reasons ...Reason) Link {
	return Link{Log: ArtifactRef{ID: "art_w", Digest: dig, Size: 5}, Statement: Statement{Completeness: c, Reasons: reasons}}
}

func TestLogRecordTransitions(t *testing.T) {
	t.Run("identity is validated", func(t *testing.T) {
		for _, bad := range []struct {
			src Source
			gen int
		}{{"bogus", 1}, {SourceController, 0}, {SourceController, MaxGenerations + 1}} {
			if _, err := NewRecord(scope, "logrec_1", "att_1", "run_1", "build", bad.src, bad.gen, t0); !errors.Is(err, ErrInvalid) {
				t.Fatalf("%v: %v", bad, err)
			}
		}
	})

	t.Run("controller happy path", func(t *testing.T) {
		r := newRec(t, SourceController, 1)
		c, err := r.Begin("tok", time.Minute, t0)
		if err != nil || r.State != StateCollecting || c.Fence != 1 {
			t.Fatalf("begin: %v %+v", err, r)
		}
		if _, err := r.Begin("other", time.Minute, t0.Add(time.Second)); !errors.Is(err, ErrBusy) {
			t.Fatalf("a live claim must block another collector: %v", err)
		}
		if err := r.Retain(c, ctrlLink(), t0); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("linking before the capture is frozen: %v", err)
		}
		if ch, err := r.Freeze(c, ctrlSnap(), t0); err != nil || !ch {
			t.Fatalf("freeze: %v %v", ch, err)
		}
		if ch, err := r.Freeze(c, ctrlSnap(), t0); err != nil || ch {
			t.Fatalf("identical freeze must be a no-op: %v %v", ch, err)
		}
		changed := ctrlSnap()
		changed.Digest = dig2
		if _, err := r.Freeze(c, changed, t0); !errors.Is(err, ErrConflict) {
			t.Fatalf("a different snapshot must conflict: %v", err)
		}
		if err := r.Retain(c, ctrlLink(), t0); err != nil || r.State != StateRetained || r.Claim.Token != "" {
			t.Fatalf("retain: %v %+v", err, r)
		}
		if _, err := r.Begin("tok2", time.Minute, t0.Add(time.Hour)); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("a retained capture is never collected again: %v", err)
		}
	})

	t.Run("lease takeover fences the old collector", func(t *testing.T) {
		r := newRec(t, SourceController, 1)
		old, _ := r.Begin("old", time.Second, t0)
		newer, err := r.Begin("new", time.Minute, t0.Add(2*time.Second))
		if err != nil || newer.Fence != old.Fence+1 {
			t.Fatalf("takeover: %v %+v", err, newer)
		}
		if _, err := r.Freeze(old, ctrlSnap(), t0); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("stale freeze: %v", err)
		}
		if err := r.Fail(old, FailureBackend, t0); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("stale fail: %v", err)
		}
		r.Freeze(newer, ctrlSnap(), t0)
		if err := r.Retain(old, ctrlLink(), t0); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("stale linkage must not commit: %v", err)
		}
		if r.State != StateCollecting {
			t.Fatalf("state = %s", r.State)
		}
	})

	t.Run("retained link must be the frozen snapshot", func(t *testing.T) {
		r := newRec(t, SourceController, 1)
		c, _ := r.Begin("t", time.Minute, t0)
		r.Freeze(c, ctrlSnap(), t0)
		l := ctrlLink()
		l.Log.Digest = dig2
		if err := r.Retain(c, l, t0); !errors.Is(err, ErrConflict) {
			t.Fatalf("link of other bytes: %v", err)
		}
		l = ctrlLink()
		l.Statement = Statement{Completeness: Partial, Reasons: []Reason{contract.ReasonControllerCapture, contract.ReasonTruncated}}
		if err := r.Retain(c, l, t0); !errors.Is(err, ErrConflict) {
			t.Fatalf("a changed completeness statement: %v", err)
		}
	})

	t.Run("a controller capture is never complete", func(t *testing.T) {
		r := newRec(t, SourceController, 1)
		c, _ := r.Begin("t", time.Minute, t0)
		s := ctrlSnap()
		s.Statement = Statement{Completeness: Complete}
		if _, err := r.Freeze(c, s, t0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("EOF alone cannot establish complete capture: %v", err)
		}
	})

	t.Run("failure classes gate retries", func(t *testing.T) {
		r := newRec(t, SourceController, 1)
		c, _ := r.Begin("a", time.Minute, t0)
		if err := r.Fail(c, FailureBackend, t0); err != nil || r.State != StateUnavailable {
			t.Fatal(err)
		}
		c2, err := r.Begin("b", time.Minute, t0)
		if err != nil || c2.Fence != 2 || r.Failure != "" {
			t.Fatalf("a retryable failure may be claimed again: %v", err)
		}
		r.Fail(c2, FailureWorkloadMissing, t0)
		if _, err := r.Begin("c", time.Minute, t0); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("a missing workload does not recover by retrying: %v", err)
		}
		if _, err := r.Next("logrec_2", t0); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("a new generation follows only lost frozen bytes: %v", err)
		}
	})

	t.Run("claims per generation are bounded", func(t *testing.T) {
		r := newRec(t, SourceController, 1)
		for i := 0; i < MaxClaims; i++ {
			c, err := r.Begin("t", time.Minute, t0)
			if err != nil {
				t.Fatalf("claim %d: %v", i, err)
			}
			r.Fail(c, FailureStore, t0)
		}
		if _, err := r.Begin("t", time.Minute, t0); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("claims must be bounded: %v", err)
		}
	})

	t.Run("frozen bytes lost fences the capture and bounds generations", func(t *testing.T) {
		r := newRec(t, SourceController, 1)
		for gen := 1; ; gen++ {
			c, _ := r.Begin("t", time.Minute, t0)
			r.Freeze(c, ctrlSnap(), t0)
			if err := r.Fail(c, FailureFrozenLost, t0); err != nil {
				t.Fatal(err)
			}
			if r.Snapshot == nil || r.State != StateUnavailable {
				t.Fatal("the earlier failure and its snapshot stay recorded")
			}
			next, err := r.Next(RecordID("logrec_n"), t0)
			if gen == MaxGenerations {
				if !errors.Is(err, ErrGenerationsExhausted) {
					t.Fatalf("generation %d: %v", gen, err)
				}
				break
			}
			if err != nil || next.Generation != gen+1 || next.State != StatePending || next.Snapshot != nil {
				t.Fatalf("next: %v %+v", err, next)
			}
			r = next
		}
	})

	t.Run("wrapper observation is idempotent and immutable", func(t *testing.T) {
		r := newRec(t, SourceWrapper, 1)
		if _, err := r.Begin("t", time.Minute, t0); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("a wrapper capture is observed, never collected: %v", err)
		}
		l := wrapperLink(Complete)
		if ch, err := r.Observe(l, t0); err != nil || !ch || r.State != StateRetained {
			t.Fatalf("observe: %v %v", ch, err)
		}
		v := r.Version
		if ch, err := r.Observe(wrapperLink(Complete), t0); err != nil || ch || r.Version != v {
			t.Fatalf("an exact duplicate succeeds without change: %v %v", ch, err)
		}
		// No false completeness upgrade or downgrade, no different artifact.
		for _, bad := range []Link{
			wrapperLink(Partial, contract.ReasonTruncated),
			{Log: ArtifactRef{ID: "art_other", Digest: dig, Size: 5}, Statement: Statement{Completeness: Complete}},
		} {
			if _, err := r.Observe(bad, t0); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed linkage must conflict: %v", err)
			}
		}
		if r.Link.Completeness != Complete {
			t.Fatal("evidence changed")
		}
	})

	t.Run("unknown completeness is never upgraded", func(t *testing.T) {
		r := newRec(t, SourceWrapper, 1)
		r.Observe(wrapperLink(Unknown, contract.ReasonUnverified), t0)
		if _, err := r.Observe(wrapperLink(Complete), t0); !errors.Is(err, ErrConflict) {
			t.Fatalf("a legacy log cannot become complete later: %v", err)
		}
	})

	t.Run("a complete statement with reasons is refused", func(t *testing.T) {
		r := newRec(t, SourceWrapper, 1)
		if _, err := r.Observe(wrapperLink(Complete, contract.ReasonTruncated), t0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%v", err)
		}
		bad := wrapperLink(Complete)
		bad.Meta = &ArtifactRef{ID: bad.Log.ID, Digest: dig, Size: 1}
		if _, err := r.Observe(bad, t0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("sidecar must be its own artifact: %v", err)
		}
	})

	t.Run("removal changes availability, not completeness", func(t *testing.T) {
		r := newRec(t, SourceWrapper, 1)
		if _, err := r.MarkRemoved(t0); !errors.Is(err, ErrInvalidTransition) {
			t.Fatal("only a retained capture can be removed")
		}
		r.Observe(wrapperLink(Complete), t0)
		if ch, err := r.MarkRemoved(t0.Add(time.Hour)); err != nil || !ch || r.Available() {
			t.Fatalf("remove: %v %v", ch, err)
		}
		if ch, _ := r.MarkRemoved(t0.Add(2 * time.Hour)); ch {
			t.Fatal("removal is recorded once")
		}
		if r.Link.Completeness != Complete {
			t.Fatal("historical completeness must survive removal")
		}
	})
}

func TestSelectPrefersVerifiedCompleteWrapper(t *testing.T) {
	mk := func(src Source, gen int, l Link) *Record {
		r := newRec(t, src, gen)
		if src == SourceWrapper {
			r.Observe(l, t0)
		} else {
			c, _ := r.Begin("t", time.Minute, t0)
			r.Freeze(c, ctrlSnap(), t0)
			r.Retain(c, l, t0)
		}
		return r
	}
	ctrl := mk(SourceController, 1, ctrlLink())
	partial := mk(SourceWrapper, 1, wrapperLink(Partial, contract.ReasonTruncated))
	complete := mk(SourceWrapper, 1, wrapperLink(Complete))
	if got := Select([]*Record{ctrl, partial, complete}); got != complete {
		t.Fatal("a verified complete wrapper capture is preferred")
	}
	if got := Select([]*Record{ctrl, partial}); got != partial {
		t.Fatal("a wrapper capture beats a controller capture")
	}
	if got := Select([]*Record{ctrl}); got != ctrl {
		t.Fatal("the controller capture is served when it is all there is")
	}
	removed := mk(SourceWrapper, 1, wrapperLink(Complete))
	removed.MarkRemoved(t0)
	if got := Select([]*Record{removed, ctrl}); got != ctrl {
		t.Fatal("a removed capture is not served")
	}
	if Select(nil) != nil {
		t.Fatal("nothing to select")
	}
}
