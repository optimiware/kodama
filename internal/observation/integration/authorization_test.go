package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/observation"
	ot "github.com/optimiware/kodama/internal/observation/observationtest"
)

// switchAuthority answers like inner until it is switched off.
type switchAuthority struct {
	inner identity.Authority
	down  atomic.Bool
	err   error
}

func (s *switchAuthority) Authorize(ctx context.Context, a identity.Actor, t identity.Target, p identity.Permission) error {
	if s.down.Load() {
		return s.err
	}
	return s.inner.Authorize(ctx, a, t, p)
}

// hookState runs a function right after the snapshot was read.
type hookState struct {
	observation.State
	after func()
}

func (h hookState) Snapshot(ctx context.Context, s observation.Scope, max int) (observation.Snapshot, error) {
	snap, err := h.State.Snapshot(ctx, s, max)
	if err == nil && h.after != nil {
		h.after()
	}
	return snap, err
}

// Test_current_authorization: persisted membership revocation and authority
// failure prevent further queued, backlog and live disclosure; snapshot release
// rechecks access; no local-authority fallback occurs.
func Test_current_authorization(t *testing.T) {
	f := newFixture(t, ot.Options{})
	ctx := context.Background()
	project := scopeOf(t, ot.TenantA, ot.ProjectA, "")

	appendN := func(prefix string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			f.Commit(ot.TenantA, nil, ot.WorkEvent(ot.TenantA, ot.ProjectA, fmt.Sprintf("%s_%d", prefix, i), "wi_auth", int64(i+1)))
		}
	}
	// A user bound only for the duration of one subtest.
	temp := func(name string) (identity.UserID, identity.BindingID) {
		u := f.User(issuer, name)
		return u, f.Bind(ot.TenantA, u, "", identity.RoleViewer)
	}

	t.Run("revocation stops a backlog drain before the next event", func(t *testing.T) {
		u, b := temp("revoked-backlog")
		appendN("t024_au_bl", 12)
		svc := f.Service()
		zero := int64(0)
		st, err := svc.Open(ctx, observation.Request{Actor: observation.Actor{ID: string(u)}, Scope: project, Resume: &zero})
		if err != nil {
			t.Fatal(err)
		}
		c := &collector{}
		c.onEvent = func(d observation.Delivery) error {
			if len(c.events) == 3 { // the fourth event is about to be recorded; revoke now
				f.revoke(b, u, ot.TenantA)
			}
			return nil
		}
		_, done := run(t, st, c)
		if err := waitEnd(t, done, 5*time.Second); !errors.Is(err, observation.ErrAccessRevoked) {
			t.Fatalf("ended with %v, want ErrAccessRevoked", err)
		}
		if n := c.eventCount(); n != 4 {
			t.Fatalf("%d events delivered; the revocation committed after the fourth must stop the fifth", n)
		}
	})

	t.Run("revocation is noticed while idle and later events never leak", func(t *testing.T) {
		u, b := temp("revoked-idle")
		svc := f.Service()
		head, _ := f.Obs.Watermark(ctx, project)
		st, err := svc.Open(ctx, observation.Request{Actor: observation.Actor{ID: string(u)}, Scope: project, Resume: &head.HighWater})
		if err != nil {
			t.Fatal(err)
		}
		c := &collector{}
		_, done := run(t, st, c)
		f.Commit(ot.TenantA, nil, ot.WorkEvent(ot.TenantA, ot.ProjectA, "t024_au_idle_1", "wi_auth", 100))
		waitFor(t, 3*time.Second, "the first live event", func() bool { return c.eventCount() == 1 })
		f.revoke(b, u, ot.TenantA)
		if err := waitEnd(t, done, 3*time.Second); !errors.Is(err, observation.ErrAccessRevoked) {
			t.Fatalf("ended with %v", err)
		}
		f.Commit(ot.TenantA, nil, ot.WorkEvent(ot.TenantA, ot.ProjectA, "t024_au_idle_2", "wi_auth", 101))
		time.Sleep(100 * time.Millisecond)
		if n := c.eventCount(); n != 1 {
			t.Fatalf("%d events delivered after revocation", n)
		}
	})

	t.Run("snapshot release rechecks access", func(t *testing.T) {
		u, b := temp("revoked-snapshot")
		svc := f.Service(func(c *observation.Config) {
			c.State = hookState{State: f.Obs, after: func() { f.revoke(b, u, ot.TenantA) }}
		})
		snap, err := svc.Snapshot(ctx, observation.Actor{ID: string(u)}, project)
		if !errors.Is(err, observation.ErrHidden) || len(snap.Resources) != 0 || snap.Position != 0 {
			t.Fatalf("snapshot = %+v, err = %v: state read before a revocation must not be released after it", snap, err)
		}
		// The same over HTTP: a nondisclosing 404 and no cursor.
		u2, b2 := temp("revoked-snapshot-http")
		svc2 := f.Service(func(c *observation.Config) {
			c.State = hookState{State: f.Obs, after: func() { f.revoke(b2, u2, ot.TenantA) }}
		})
		srv := f.NewServer(svc2, f.Authority(), ot.HandlerOptions())
		code, _, body := ot.GetJSON(t, ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA), string(u2), nil)
		if code != 404 || ot.ErrorCode(body) != "not_found" {
			t.Fatalf("%d %s", code, body)
		}
		// A stream start fails the same way, before any frame.
		u3, b3 := temp("revoked-stream-start")
		svc3 := f.Service(func(c *observation.Config) {
			c.State = hookState{State: f.Obs, after: func() { f.revoke(b3, u3, ot.TenantA) }}
		})
		st, err := svc3.Open(ctx, observation.Request{Actor: observation.Actor{ID: string(u3)}, Scope: project})
		if err != nil {
			t.Fatalf("the snapshot is acquired at open: %v", err)
		}
		c := &collector{}
		_, done := run(t, st, c)
		if err := waitEnd(t, done, 3*time.Second); !errors.Is(err, observation.ErrAccessRevoked) || c.snapshot != nil {
			t.Fatalf("ended with %v, snapshot released = %v", err, c.snapshot != nil)
		}
	})

	t.Run("authority failure closes the stream and never falls back", func(t *testing.T) {
		u, _ := temp("authority-failure")
		sw := &switchAuthority{inner: f.Authority(), err: errors.New("hosted authority unreachable")}
		svc := f.Service(func(c *observation.Config) { c.Access = f.AccessOver(sw) })
		head, _ := f.Obs.Watermark(ctx, project)
		st, err := svc.Open(ctx, observation.Request{Actor: observation.Actor{ID: string(u)}, Scope: project, Resume: &head.HighWater})
		if err != nil {
			t.Fatal(err)
		}
		c := &collector{}
		_, done := run(t, st, c)
		f.Commit(ot.TenantA, nil, ot.WorkEvent(ot.TenantA, ot.ProjectA, "t024_au_out_1", "wi_auth", 200))
		waitFor(t, 3*time.Second, "the first event", func() bool { return c.eventCount() == 1 })
		sw.down.Store(true)
		f.Commit(ot.TenantA, nil, ot.WorkEvent(ot.TenantA, ot.ProjectA, "t024_au_out_2", "wi_auth", 201))
		if err := waitEnd(t, done, 3*time.Second); !errors.Is(err, observation.ErrUnavailable) {
			t.Fatalf("ended with %v, want ErrUnavailable", err)
		}
		if n := c.eventCount(); n != 1 {
			t.Fatalf("%d events delivered; an authority outage is never access", n)
		}
		// Open during the outage fails too.
		if _, err := svc.Open(ctx, observation.Request{Actor: observation.Actor{ID: string(u)}, Scope: project, Resume: &head.HighWater}); !errors.Is(err, observation.ErrUnavailable) {
			t.Fatalf("open during an outage: %v", err)
		}
		if _, err := svc.Snapshot(ctx, observation.Actor{ID: string(u)}, project); !errors.Is(err, observation.ErrUnavailable) {
			t.Fatalf("snapshot during an outage: %v", err)
		}
	})

	t.Run("the selected hosted profile is never replaced by local grants", func(t *testing.T) {
		sa, err := identity.NewStandaloneAuthority(f.Stack.Store)
		if err != nil {
			t.Fatal(err)
		}
		for name, hosted := range map[string]struct {
			err    error
			status int
			code   string
		}{
			"hosted denies":      {identity.ErrDenied, 403, "forbidden"},
			"hosted hides":       {identity.ErrHidden, 404, "not_found"},
			"hosted unavailable": {errors.New("platform core unreachable"), 503, "unavailable"},
		} {
			down := &switchAuthority{inner: sa, err: hosted.err}
			down.down.Store(true)
			sel, err := identity.SelectAuthority(identity.AuthorityConfig{Profile: identity.ProfileHosted, Standalone: sa, Hosted: down})
			if err != nil {
				t.Fatal(err)
			}
			svc := f.Service(func(c *observation.Config) { c.Access = f.AccessOver(sel) })
			srv := f.NewServer(svc, sel, ot.HandlerOptions())
			// The viewer holds a genuine standalone binding; the hosted answer wins.
			st := ot.Connect(t, ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA), string(f.viewer), nil)
			if st.Resp.StatusCode != hosted.status || ot.ErrorCode(st.Body) != hosted.code {
				t.Errorf("%s: %d %s", name, st.Resp.StatusCode, st.Body)
			}
		}
	})

	t.Run("route and stream authorization follow persisted roles", func(t *testing.T) {
		svc := f.Service()
		srv := f.NewServer(svc, f.Authority(), ot.HandlerOptions())
		f.SeedRun(ot.TenantA, ot.ProjectA2, "run_au", "running", 1)
		a2 := ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA2)
		for name, tc := range map[string]struct {
			url, token string
			status     int
			code       string
		}{
			"tenant viewer, project stream":          {ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA), string(f.viewer), 200, ""},
			"engineer":                               {ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA), string(f.otherViewer), 200, ""},
			"admin has neither work nor run read":    {ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA), string(f.admin), 403, "forbidden"},
			"admin, run stream":                      {ot.RunURL(srv.URL, ot.TenantA, ot.ProjectA2, "run_au"), string(f.admin), 403, "forbidden"},
			"project-only member, own project":       {a2, string(f.projectOnly), 200, ""},
			"project-only member, own project's run": {ot.RunURL(srv.URL, ot.TenantA, ot.ProjectA2, "run_au"), string(f.projectOnly), 200, ""},
			"project-only member, another project":   {ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA), string(f.projectOnly), 404, "not_found"},
			"unbound user":                           {ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA), string(f.outsider), 404, "not_found"},
			"no credential":                          {ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA), "", 401, "invalid_credential"},
		} {
			code, _, body := ot.GetJSON(t, tc.url, tc.token, nil)
			if code != tc.status || (tc.code != "" && ot.ErrorCode(body) != tc.code) {
				t.Errorf("%s: %d %s", name, code, body)
			}
		}
	})

	t.Run("HTTP stream ends when membership is revoked", func(t *testing.T) {
		u, b := temp("revoked-http")
		srv := f.NewServer(f.Service(), f.Authority(), ot.HandlerOptions())
		url := ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA)
		s := ot.Connect(t, url, string(u), nil)
		s.NextN(t, 1, 3*time.Second) // snapshot
		f.Commit(ot.TenantA, nil, ot.WorkEvent(ot.TenantA, ot.ProjectA, "t024_au_http_1", "wi_auth", 300))
		if fr := s.NextN(t, 1, 3*time.Second)[0]; fr.Event != "work_item.transitioned" {
			t.Fatalf("frame %+v", fr)
		}
		f.revoke(b, u, ot.TenantA)
		f.Commit(ot.TenantA, nil, ot.WorkEvent(ot.TenantA, ot.ProjectA, "t024_au_http_2", "wi_auth", 301))
		for {
			fr, ok := s.Next(3 * time.Second)
			if !ok {
				break
			}
			if fr.Event == "work_item.transitioned" {
				t.Fatalf("an event was delivered after the revocation: %+v", fr)
			}
		}
		if !s.Ended(3 * time.Second) {
			t.Fatal("the stream stayed open after the revocation")
		}
		// Reconnecting is refused outright.
		if st := ot.Connect(t, url, string(u), nil); st.Resp.StatusCode != 404 {
			t.Fatalf("reconnect after revocation: %d", st.Resp.StatusCode)
		}
	})

}
