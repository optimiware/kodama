package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/api"
	"github.com/optimiware/kodama/internal/shell/store"
)

func journalSeq(n int64) *int64 { return &n }

func payloadOf(t *testing.T, ev store.MemoryEvent) (out struct {
	ReceiveID   string `json:"receiveId"`
	OriginRunID any    `json:"originRunId"`
	Actor       struct{ Type, ID string }
	Updates     []struct{ Ref, Old, New, Kind string }
}) {
	t.Helper()
	if err := json.Unmarshal(ev.Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The outcome of a receive is recorded once, attributed through its grant, with
// the kind of each update, and the acknowledged sequence never moves down.
func TestGitControlOutcomeIsRecordedOnce(t *testing.T) {
	e := newEnv(t)
	repo, repoID := e.seeded("backend", nil)
	grant, _ := e.mustCredential("tok-eng", repo, false)
	feature := "refs/heads/feature/x"

	receive := gitv1.NewReceiveID(grant)
	out := gitv1.OutcomeRequest{ReceiveID: receive, Applied: []gitv1.RefUpdate{update(feature, z0, c2), update("refs/heads/main", c1, c3), update("refs/heads/old", c1, c2)}, JournalSequence: journalSeq(3)}
	e.workers.ancestor = true
	for i := 0; i < 3; i++ {
		resp, err := e.control().Outcome(t.Context(), tenantID, receive, out)
		if err != nil || resp.Status != gitv1.PushApplied || resp.PushID != receive {
			t.Fatalf("outcome %d = %+v, %v", i, resp, err)
		}
	}
	forReceive := func(id string) (out []store.MemoryEvent) {
		for _, ev := range e.mem.EventsOfType("git.refs_updated") {
			if ev.ResourceID == id {
				out = append(out, ev)
			}
		}
		return out
	}
	events := forReceive(receive)
	if len(events) != 1 {
		t.Fatalf("git.refs_updated rows for the receive = %d", len(events))
	}
	ev := events[0]
	if ev.ID != store.GitReceiveEventID(repoID, receive) || ev.ResourceType != "git_push" || ev.ResourceID != receive || ev.ProjectID != projectID || ev.ActorType != "user" || ev.ActorID != "u_eng" {
		t.Fatalf("event = %+v", ev)
	}
	p := payloadOf(t, ev)
	if p.OriginRunID != nil || p.Actor.Type != "user" || len(p.Updates) != 3 || p.Updates[0].Kind != "create" || p.Updates[1].Kind != "fast_forward" {
		t.Fatalf("payload = %s", ev.Payload)
	}
	if strings.Contains(string(ev.Payload), "gitc_") {
		t.Fatal("a credential reached the event")
	}
	got, _ := e.mem.GetRepository(t.Context(), tenantID, repoID)
	if got.JournalAck != 3 {
		t.Fatalf("journal_ack = %d", got.JournalAck)
	}
	// A late report with a lower sequence never lowers the acknowledgement.
	low := gitv1.OutcomeRequest{ReceiveID: receive, Applied: out.Applied, JournalSequence: journalSeq(1)}
	if _, err := e.control().Outcome(t.Context(), tenantID, receive, low); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.mem.GetRepository(t.Context(), tenantID, repoID); got.JournalAck != 3 || len(forReceive(receive)) != 1 {
		t.Fatalf("after a lower report: ack %d, events %d", got.JournalAck, len(forReceive(receive)))
	}

	t.Run("kinds come from the worker", func(t *testing.T) {
		for name, c := range map[string]struct {
			ancestor bool
			err      error
			want     string
		}{
			"fast-forward":    {true, nil, "fast_forward"},
			"rewrite":         {false, nil, "rewrite"},
			"old commit gone": {false, &gitv1.ContractError{Code: gitv1.CodeCommitNotFound, Message: "gone"}, "rewrite"},
		} {
			e.workers.ancestor, e.workers.ancestorErr = c.ancestor, c.err
			r := gitv1.NewReceiveID(grant)
			if _, err := e.control().Outcome(t.Context(), tenantID, r, gitv1.OutcomeRequest{ReceiveID: r, Applied: []gitv1.RefUpdate{update(feature, c2, c3)}}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			evs := e.mem.EventsOfType("git.refs_updated")
			if k := payloadOf(t, evs[len(evs)-1]).Updates[0].Kind; k != c.want {
				t.Errorf("%s: kind %s", name, k)
			}
		}
		// A worker that cannot answer is unavailable and records nothing: the report repeats.
		e.workers.ancestorErr = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "down"}
		before := len(e.mem.EventsOfType("git.refs_updated"))
		r := gitv1.NewReceiveID(grant)
		_, err := e.control().Outcome(t.Context(), tenantID, r, gitv1.OutcomeRequest{ReceiveID: r, Applied: []gitv1.RefUpdate{update(feature, c2, c3)}})
		if contractCode(err) != domain.CodeUnavailable || len(e.mem.EventsOfType("git.refs_updated")) != before {
			t.Fatalf("worker down: %v", err)
		}
		e.workers.ancestorErr = nil
	})

	t.Run("a run's push carries its origin", func(t *testing.T) {
		e.mem.SetRun(runInfo("run_7", "build", "tsk_7", repoID))
		issued, err := e.git.IssueRunCredential(t.Context(), tenantID, projectID, "run_7", repoID)
		if err != nil {
			t.Fatal(err)
		}
		r := gitv1.NewReceiveID(issued.GrantID)
		if _, err := e.control().Outcome(t.Context(), tenantID, r, gitv1.OutcomeRequest{ReceiveID: r, Applied: []gitv1.RefUpdate{update("refs/heads/kodama/runs/run_7/x", z0, c2)}}); err != nil {
			t.Fatal(err)
		}
		evs := e.mem.EventsOfType("git.refs_updated")
		ev := evs[len(evs)-1]
		p := payloadOf(t, ev)
		if ev.ActorType != "workload" || p.OriginRunID != "run_7" || p.Actor.ID != "run_7" {
			t.Fatalf("run event = %s (actor %s)", ev.Payload, ev.ActorType)
		}
	})

	t.Run("nothing applied records the acknowledgement only", func(t *testing.T) {
		before := len(e.mem.EventsOfType("git.refs_updated"))
		r := gitv1.NewReceiveID(grant)
		resp, err := e.control().Outcome(t.Context(), tenantID, r, gitv1.OutcomeRequest{ReceiveID: r, Applied: []gitv1.RefUpdate{}, JournalSequence: journalSeq(9)})
		if err != nil || resp.Status != gitv1.PushRefused {
			t.Fatalf("empty outcome = %+v, %v", resp, err)
		}
		if got, _ := e.mem.GetRepository(t.Context(), tenantID, repoID); got.JournalAck != 9 || len(e.mem.EventsOfType("git.refs_updated")) != before {
			t.Fatalf("ack %d, events %d", got.JournalAck, len(e.mem.EventsOfType("git.refs_updated")))
		}
	})

	t.Run("a receive that names no known grant is refused", func(t *testing.T) {
		for name, r := range map[string]string{"no grant": "rcv_00000000000000000000000000000000", "unknown grant": gitv1.NewReceiveID("gg_nope")} {
			_, err := e.control().Outcome(t.Context(), tenantID, r, gitv1.OutcomeRequest{ReceiveID: r, Applied: []gitv1.RefUpdate{update(feature, z0, c2)}})
			if code := contractCode(err); code != domain.CodeInvalidRequest && code != domain.CodeNotFound {
				t.Errorf("%s: %v", name, err)
			}
		}
		// Another tenant's worker cannot record an outcome for this tenant's grant.
		if _, err := e.control().Outcome(t.Context(), otherID, receive, out); contractCode(err) != domain.CodeNotFound {
			t.Fatalf("foreign tenant: %v", err)
		}
		if _, err := e.control().Outcome(t.Context(), tenantID, "rcv_other", out); contractCode(err) != domain.CodeInvalidRequest {
			t.Fatalf("a push ID that is not the receive: %v", err)
		}
	})
}

// The worker's internal routes carry a tenant-bound identity: refused without
// one, on another tenant's path, or when no verifier is configured.
func TestGitInternalRoutesRequireTheTenantsWorkerIdentity(t *testing.T) {
	e := newEnv(t)
	repo := e.createRepo("tok-eng", "p_erp", "backend", nil)
	now := e.clock()
	identity := func(tenant string) string {
		tok, err := e.signer.SignWorkerIdentity(t.Context(), gitv1.WorkerIdentity{Audience: gitv1.ControlAudience, TenantID: tenant,
			WorkerInstance: "wi_1", IssuedAt: gitv1.Timestamp(now.Format(time.RFC3339Nano)), ExpiresAt: gitv1.Timestamp(now.Add(30 * time.Minute).Format(time.RFC3339Nano))})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	base := "/internal/git/v1/tenants/t_acme"

	r := e.do(http.MethodGet, base+"/assignments", identity("t_acme"), nil)
	if r.status != http.StatusOK {
		t.Fatalf("assignments = %d %s", r.status, r.raw)
	}
	items := r.body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["kind"] != "reconcile" || items[0].(map[string]any)["journalAck"] != float64(0) {
		t.Fatalf("assignments = %s", r.raw)
	}
	for name, c := range map[string]struct {
		path, token string
		want        int
	}{
		"no identity":         {base + "/assignments", "", http.StatusUnauthorized},
		"garbage identity":    {base + "/assignments", "x.y.z", http.StatusUnauthorized},
		"a user token":        {base + "/assignments", "tok-eng", http.StatusUnauthorized},
		"another tenant path": {"/internal/git/v1/tenants/t_other/assignments", identity("t_acme"), http.StatusForbidden},
		"unknown route":       {base + "/repositories", identity("t_acme"), http.StatusNotFound},
	} {
		if r := e.do(http.MethodGet, c.path, c.token, nil); r.status != c.want {
			t.Errorf("%s: %d %s, want %d", name, r.status, r.raw, c.want)
		}
	}

	// The initialization confirmation and a refused push authorization over HTTP.
	if r := e.do(http.MethodPost, base+"/repositories/"+str(repo["id"])+"/initialized", identity("t_acme"),
		gitv1.InitializedRequest{RepositoryID: str(repo["id"]), DefaultBranch: "main", Empty: true}); r.status != http.StatusOK || r.body["status"] != "ready" {
		t.Fatalf("initialized = %d %s", r.status, r.raw)
	}
	bad := gitv1.PushAuthorizeRequest{RepositoryID: str(repo["id"]), GrantID: "gg_nope", ReceiveID: "rcv_x", Updates: []gitv1.RefUpdate{create("refs/heads/x", c1)},
		PushOptions: []string{}, GitFacts: factsFor(true, create("refs/heads/x", c1))}
	if r := e.do(http.MethodPost, base+"/pushes", identity("t_acme"), bad); r.status != http.StatusBadRequest && r.status != http.StatusNotFound {
		t.Fatalf("authorize of an unknown grant = %d %s", r.status, r.raw)
	}
	if r := e.do(http.MethodPost, base+"/pushes", identity("t_acme"), `{"repositoryId":"r"}`); r.status != http.StatusBadRequest {
		t.Fatalf("malformed authorize = %d", r.status)
	}

	// Without a configured verifier every internal call is refused.
	closed, err := api.NewGit(api.GitConfig{Store: e.mem, Auth: tokens, Workers: e.workers, Signer: e.signer, Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(closed.Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+base+"/assignments", nil)
	req.Header.Set("Authorization", "Bearer "+identity("t_acme"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no verifier configured: %d", resp.StatusCode)
	}
}
