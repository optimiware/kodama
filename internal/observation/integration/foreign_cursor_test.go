package integration_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	ot "github.com/optimiware/kodama/internal/observation/observationtest"
)

// errorShape is what a client can observe of a refusal, minus the request ID.
func errorShape(t testing.TB, status int, body []byte) string {
	t.Helper()
	var s struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := jsonUnmarshal(body, &s); err != nil {
		t.Fatalf("status %d is not an error envelope: %v: %s", status, err, body)
	}
	return strings.Join([]string{http.StatusText(status), s.Error.Code, s.Error.Message}, "|")
}

// Test_foreign_cursor: copied cursors cannot change tenant, project, run,
// endpoint or filters; unauthorized scopes and foreign resource references
// disclose no content; authorized-scope cursor mismatches use the generic
// invalid-cursor response.
func Test_foreign_cursor(t *testing.T) {
	f := newFixture(t, ot.Options{})
	svc := f.Service()
	srv := f.NewServer(svc, f.Authority(), ot.HandlerOptions())
	viewer, viewerB := string(f.viewer), string(f.tenantBViewer)

	// Content in every scope, so a leak would be visible.
	for _, c := range []struct {
		tenant  domain.TenantID
		project domain.ProjectID
		item    string
	}{{ot.TenantA, ot.ProjectA, "wi_secret_a"}, {ot.TenantA, ot.ProjectA2, "wi_secret_a2"}, {ot.TenantB, ot.ProjectB, "wi_secret_b"}} {
		if err := createWork(f.Env, c.tenant, c.project, c.item, "t024_fc_"+c.item); err != nil {
			t.Fatal(err)
		}
	}
	f.SeedRun(ot.TenantA, ot.ProjectA, "run_a", "running", 1)
	f.SeedRun(ot.TenantA, ot.ProjectA2, "run_a2", "running", 1)
	f.SeedRun(ot.TenantB, ot.ProjectB, "run_b", "running", 1)
	f.Commit(ot.TenantA, nil, ot.RunEvent(ot.TenantA, ot.ProjectA, "t024_fc_run_a", "run_a", 1))
	f.Commit(ot.TenantB, nil, ot.RunEvent(ot.TenantB, ot.ProjectB, "t024_fc_run_b", "run_b", 1))

	projectA := ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA)
	projectA2 := ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA2)
	projectB := ot.ProjectURL(srv.URL, ot.TenantB, ot.ProjectB)
	runA := ot.RunURL(srv.URL, ot.TenantA, ot.ProjectA, "run_a")

	// A genuine cursor and a genuine event cursor from the tenant-A project stream.
	code, _, body := ot.GetJSON(t, projectA, viewer, nil)
	if code != 200 {
		t.Fatalf("snapshot: %d %s", code, body)
	}
	snapCursor := jsonField(t, body, "cursor")
	s := ot.Connect(t, projectA, viewer, map[string]string{"Last-Event-ID": snapCursor})
	if _, err := f.TryCommit(ot.TenantA, nil, ot.WorkEvent(ot.TenantA, ot.ProjectA, "t024_fc_more", "wi_secret_a", 2)); err != nil {
		t.Fatal(err)
	}
	evFrame := s.NextN(t, 1, 3*time.Second)[0]
	s.Close()

	t.Run("a copied cursor cannot change tenant, project, run, endpoint or filters", func(t *testing.T) {
		for name, tc := range map[string]struct{ url, token string }{
			"another project of the same tenant":                  {projectA2, viewer},
			"another tenant's project, as that tenant's own user": {projectB, viewerB},
			"a run stream of the same project":                    {runA, viewer},
			"the same endpoint with a type filter":                {projectA + "?type=run.created", viewer},
			"the same endpoint with another filter set":           {projectA + "?type=work_item.created&type=run.created", viewer},
		} {
			for label, cursor := range map[string]string{"snapshot cursor": snapCursor, "event cursor": evFrame.ID} {
				st := ot.Connect(t, tc.url, tc.token, map[string]string{"Last-Event-ID": cursor})
				if st.Resp.StatusCode != 400 || ot.ErrorCode(st.Body) != "invalid_cursor" {
					t.Errorf("%s with a %s: %d %s", name, label, st.Resp.StatusCode, st.Body)
				}
				if strings.Contains(string(st.Body), "secret") {
					t.Errorf("%s leaked content: %s", name, st.Body)
				}
			}
		}
		// The run endpoint of another run cannot reuse a run cursor.
		_, _, rbody := ot.GetJSON(t, runA, viewer, nil)
		runCursor := jsonField(t, rbody, "cursor")
		if st := ot.Connect(t, ot.RunURL(srv.URL, ot.TenantA, ot.ProjectA2, "run_a2"), viewer, map[string]string{"Last-Event-ID": runCursor}); st.Resp.StatusCode != 400 || ot.ErrorCode(st.Body) != "invalid_cursor" {
			t.Errorf("a run cursor on another run: %d %s", st.Resp.StatusCode, st.Body)
		}
		// The genuine cursor still works for its own scope.
		if st := ot.Connect(t, projectA, viewer, map[string]string{"Last-Event-ID": evFrame.ID}); st.Resp.StatusCode != 200 {
			t.Errorf("the issuing scope refused its own cursor: %d %s", st.Resp.StatusCode, st.Body)
		}
	})

	t.Run("unauthorized scopes disclose nothing, with or without a cursor", func(t *testing.T) {
		missing := ot.ProjectURL(srv.URL, "t024_nowhere", "p024_nothing")
		// What a caller sees for a scope that does not exist is the baseline.
		cb, _, bb := ot.GetJSON(t, missing, viewer, nil)
		baseline := errorShape(t, cb, bb)
		if cb != 404 {
			t.Fatalf("baseline status %d", cb)
		}
		for name, tc := range map[string]struct{ url, token string }{
			"another tenant":                     {projectB, viewer},
			"another tenant, run stream":         {ot.RunURL(srv.URL, ot.TenantB, ot.ProjectB, "run_b"), viewer},
			"a project the user is not bound to": {projectA, string(f.projectOnly)},
			"a user with no membership":          {projectA, string(f.outsider)},
			"the other tenant's viewer":          {projectA, viewerB},
		} {
			for label, hdr := range map[string]map[string]string{
				"no cursor": nil, "own-scope cursor": {"Last-Event-ID": evFrame.ID}, "garbage cursor": {"Last-Event-ID": "x.y"},
			} {
				st := ot.Connect(t, tc.url, tc.token, hdr)
				if got := errorShape(t, st.Resp.StatusCode, st.Body); got != baseline {
					t.Errorf("%s (%s): response %q differs from the not-found baseline %q", name, label, got, baseline)
				}
			}
			if code, _, b := ot.GetJSON(t, tc.url, tc.token, nil); errorShape(t, code, b) != baseline {
				t.Errorf("%s (snapshot): %d %s", name, code, b)
			}
		}
	})

	t.Run("foreign resource references are indistinguishable from missing ones", func(t *testing.T) {
		cb, _, bb := ot.GetJSON(t, ot.RunURL(srv.URL, ot.TenantA, ot.ProjectA, "run_absent"), viewer, nil)
		baseline := errorShape(t, cb, bb)
		if cb != 404 {
			t.Fatalf("missing run: %d %s", cb, bb)
		}
		for name, url := range map[string]string{
			"a run of another project":             ot.RunURL(srv.URL, ot.TenantA, ot.ProjectA, "run_a2"),
			"another tenant's run in this project": ot.RunURL(srv.URL, ot.TenantA, ot.ProjectA, "run_b"),
		} {
			code, _, b := ot.GetJSON(t, url, viewer, nil)
			if got := errorShape(t, code, b); got != baseline {
				t.Errorf("%s (snapshot): %q != %q", name, got, baseline)
			}
			st := ot.Connect(t, url, viewer, nil)
			if got := errorShape(t, st.Resp.StatusCode, st.Body); got != baseline {
				t.Errorf("%s (stream): %q != %q", name, got, baseline)
			}
			st = ot.Connect(t, url, viewer, map[string]string{"Last-Event-ID": snapCursor})
			if st.Resp.StatusCode != 400 { // an authorized-scope mismatch, decided before the lookup
				t.Errorf("%s with a foreign cursor: %d", name, st.Resp.StatusCode)
			}
		}
		// A valid cursor of an existing run does not reveal a run that was never there.
		_, _, rb := ot.GetJSON(t, runA, viewer, nil)
		st := ot.Connect(t, ot.RunURL(srv.URL, ot.TenantA, ot.ProjectA, "run_absent"), viewer, map[string]string{"Last-Event-ID": jsonField(t, rb, "cursor")})
		if st.Resp.StatusCode != 400 {
			t.Errorf("a cursor of another run on an absent run: %d %s", st.Resp.StatusCode, st.Body)
		}
	})

	t.Run("a cursor is never a permission", func(t *testing.T) {
		// The administrator holds neither work nor run read: even a genuine
		// cursor of the very same scope opens nothing.
		st := ot.Connect(t, projectA, string(f.admin), map[string]string{"Last-Event-ID": evFrame.ID})
		if st.Resp.StatusCode != 403 || ot.ErrorCode(st.Body) != "forbidden" || strings.Contains(string(st.Body), "secret") {
			t.Fatalf("admin with a genuine cursor: %d %s", st.Resp.StatusCode, st.Body)
		}
		// Another user of the same scope may use the copied cursor only because they
		// are authorized themselves, and receive only what follows it.
		st = ot.Connect(t, projectA, string(f.otherViewer), map[string]string{"Last-Event-ID": snapCursor})
		fr := st.NextN(t, 1, 3*time.Second)
		if !strings.Contains(fr[0].Data, "t024_fc_more") {
			t.Fatalf("frame = %+v", fr[0])
		}
		// An unbound user cannot use it at all.
		st = ot.Connect(t, projectA, string(f.outsider), map[string]string{"Last-Event-ID": snapCursor})
		if st.Resp.StatusCode != 404 {
			t.Fatalf("outsider with a copied cursor: %d %s", st.Resp.StatusCode, st.Body)
		}
	})

	t.Run("no content from other scopes appears in a stream", func(t *testing.T) {
		st := ot.Connect(t, projectA, viewer, nil)
		snap := st.NextN(t, 1, 3*time.Second)[0]
		if strings.Contains(snap.Data, "secret_b") || strings.Contains(snap.Data, "secret_a2") || !strings.Contains(snap.Data, "wi_secret_a") {
			t.Fatalf("snapshot = %s", snap.Data)
		}
		for {
			fr, ok := st.Next(250 * time.Millisecond)
			if !ok {
				break
			}
			if strings.Contains(fr.Data, "secret_b") || strings.Contains(fr.Data, "t024_fc_run_b") || strings.Contains(fr.Data, "secret_a2") {
				t.Fatalf("a foreign event leaked: %+v", fr)
			}
		}
	})
}
