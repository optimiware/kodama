package m1_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

// TestM1ClientRoundtrip (m1_client_roundtrip) drives T-027's generated public
// client against the actual server (real PostgreSQL, JetStream, S3, Git, OIDC and
// the Podman runtime): run submission and status, an identical-key replay, a
// conflicting key and body, and the stable decoding of every error the run
// routes document. AC-01, AC-02, AC-10, AC-12 (AC-A01, AC-A02).
func TestM1ClientRoundtrip(t *testing.T) {
	s := kit.New(t, kit.Options{})
	s.Start()
	s.WaitReady()
	w := s.Seed(true)
	ctx := t.Context()
	api := s.API("engineer")

	item := w.WorkItem("Client roundtrip", "# client")
	w.Provision(item.Id)
	def := w.PodmanDefinition("client", kit.Step{Script: `printf '{"outcome":"completed","outputs":{"v":"ok"}}' > "$KODAMA_RESULTS_FILE"`, Outputs: []string{"v"}})
	other := w.PodmanDefinition("client-other", kit.Step{Script: `true`})
	req := client.CreateRunRequest{WorkItemId: item.Id, DefinitionId: def.Id, ContentRevision: int64(item.ContentRevision), SourceCommit: s.Repo.Initial}
	submit := func(persona, key string, req client.CreateRunRequest) *client.CreateRunResponse {
		c := s.API(persona)
		if persona == "" {
			c = s.APIWith(func() string { return "" })
		}
		r, err := c.CreateRunWithResponse(ctx, kit.TenantA, w.Project, &client.CreateRunParams{IdempotencyKey: key}, req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// ---- submission ----
	created := submit("engineer", "client-key", req)
	if created.StatusCode() != http.StatusCreated || created.JSON201 == nil {
		t.Fatalf("submit: HTTP %s: %s", created.Status(), created.Body)
	}
	run := *created.JSON201
	if created.HTTPResponse.Header.Get("Idempotent-Replayed") != "" || created.HTTPResponse.Header.Get("ETag") == "" || created.HTTPResponse.Header.Get("Location") == "" {
		t.Fatalf("submission headers %v", created.HTTPResponse.Header)
	}
	if run.Id == "" || run.Purpose != client.RunPurpose("work") || run.WorkItemId == nil || *run.WorkItemId != item.Id || run.DefinitionId != def.Id {
		t.Fatalf("submitted run %+v", run)
	}

	// ---- identical-key replay: the original answer, no second run ----
	replay := submit("engineer", "client-key", req)
	if replay.StatusCode() != http.StatusCreated || replay.JSON201 == nil || replay.JSON201.Id != run.Id || replay.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: HTTP %s: %s", replay.Status(), replay.Body)
	}
	if n := s.OwnerCount(`SELECT count(*) FROM runs WHERE work_item_id = $1`, item.Id); n != 1 {
		t.Fatalf("%d runs after a replayed submission", n)
	}

	// ---- conflicting key and body: a stable, typed error ----
	changed := req
	changed.DefinitionId = other.Id
	conflict := submit("engineer", "client-key", changed)
	if conflict.StatusCode() != http.StatusConflict || conflict.JSON409 == nil || conflict.JSON409.Error.Code != "idempotency_conflict" || conflict.JSON409.Error.RequestId == "" {
		t.Fatalf("conflicting body under a used key: HTTP %s: %s", conflict.Status(), conflict.Body)
	}
	if conflict.JSON409.Error.RequestId != conflict.HTTPResponse.Header.Get("X-Request-ID") {
		t.Fatalf("the error's request ID %q is not the response header's %q", conflict.JSON409.Error.RequestId, conflict.HTTPResponse.Header.Get("X-Request-ID"))
	}

	// ---- status: the run is observed to completion through the client ----
	var seen []client.RunStatus
	kit.WaitFor(t, "the run to succeed", 3*time.Minute, func() bool {
		got, err := api.GetRunWithResponse(ctx, kit.TenantA, w.Project, run.Id)
		if err != nil || got.StatusCode() != http.StatusOK {
			return false
		}
		if n := len(seen); n == 0 || seen[n-1] != got.JSON200.Status {
			seen = append(seen, got.JSON200.Status)
		}
		return got.JSON200.Status == client.RunStatusSucceeded
	})
	if seen[0] != client.RunStatusQueued && seen[0] != client.RunStatusRunning {
		t.Fatalf("a new run was observed as %v", seen)
	}
	steps, err := api.ListRunStepsWithResponse(ctx, kit.TenantA, w.Project, run.Id, &client.ListRunStepsParams{})
	if err != nil || steps.StatusCode() != http.StatusOK || len(steps.JSON200.Items) != 1 || steps.JSON200.Items[0].Outputs == nil {
		t.Fatalf("steps: %v %s", err, steps.Body)
	}
	if v, err := (*steps.JSON200.Items[0].Outputs)["v"].AsRunsScalar0(); err != nil || v != "ok" {
		t.Fatalf("output %q (%v)", v, err)
	}
	// After the run ended the key still answers with the original admission.
	if again := submit("engineer", "client-key", req); again.StatusCode() != http.StatusCreated || again.JSON201.Id != run.Id || again.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay after completion: HTTP %s: %s", again.Status(), again.Body)
	}
	if n := s.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 1 {
		t.Fatalf("%d attempts", n)
	}

	// ---- stable error decoding across the documented failures ----
	envelope := func(what string, status int, code string, body []byte, e *client.ErrorEnvelope, header http.Header) {
		t.Helper()
		if e == nil {
			t.Errorf("%s: HTTP %d did not decode into the typed envelope: %s", what, status, body)
			return
		}
		if e.Error.Code != code || e.Error.Message == "" || e.Error.RequestId == "" || e.Error.Details == nil || header.Get("X-Request-ID") != e.Error.RequestId {
			t.Errorf("%s: %+v (want %s, request %s)", what, e.Error, code, header.Get("X-Request-ID"))
		}
	}
	fresh := func(tag string) client.CreateRunRequest { r := req; r.WorkItemId = item.Id; _ = tag; return r }

	r401 := submit("", "client-401", fresh("401"))
	envelope("no credential", r401.StatusCode(), "invalid_credential", r401.Body, r401.JSON401, r401.HTTPResponse.Header)
	if r401.StatusCode() != http.StatusUnauthorized {
		t.Errorf("no credential: %d", r401.StatusCode())
	}
	r403 := submit("viewer", "client-403", fresh("403"))
	envelope("a viewer submits", r403.StatusCode(), "forbidden", r403.Body, r403.JSON403, r403.HTTPResponse.Header)
	if r403.StatusCode() != http.StatusForbidden {
		t.Errorf("viewer: %d", r403.StatusCode())
	}
	r404 := submit("outsider", "client-404", fresh("404"))
	envelope("an unbound caller", r404.StatusCode(), "not_found", r404.Body, r404.JSON404, r404.HTTPResponse.Header)
	if r404.StatusCode() != http.StatusNotFound {
		t.Errorf("outsider: %d", r404.StatusCode())
	}
	missing, err := api.GetRunWithResponse(ctx, kit.TenantA, w.Project, "run_t026_missing")
	if err != nil || missing.StatusCode() != http.StatusNotFound {
		t.Fatalf("a missing run: %v %d", err, missing.StatusCode())
	}
	envelope("a missing run", missing.StatusCode(), "not_found", missing.Body, missing.JSON404, missing.HTTPResponse.Header)
	bad := fresh("422")
	bad.DefinitionId = "pd_t026_missing"
	r422 := submit("engineer", "client-422", bad)
	if r422.StatusCode() != http.StatusUnprocessableEntity && r422.StatusCode() != http.StatusNotFound {
		t.Errorf("an unknown definition: %d %s", r422.StatusCode(), r422.Body)
	}
	stale := fresh("409")
	stale.ContentRevision = int64(item.ContentRevision) + 7
	r409 := submit("engineer", "client-stale", stale)
	envelope("a stale content revision", r409.StatusCode(), "run_conflict", r409.Body, r409.JSON409, r409.HTTPResponse.Header)
	if r409.StatusCode() != http.StatusConflict {
		t.Errorf("stale revision: %d", r409.StatusCode())
	}
	if resp, body := s.Raw("POST", "/api/v1/tenants/"+kit.TenantA+"/projects/"+w.Project+"/runs", s.Token("engineer"), kit.JSONBody(t, req), map[string]string{"Content-Type": "application/json"}); resp.StatusCode != http.StatusBadRequest || kit.ErrorCode(body) != "invalid_request" {
		t.Errorf("a submission without an idempotency key: %d %s", resp.StatusCode, body)
	}
	// Cancelling a finished run changes nothing and decodes as a run.
	if c := w.CancelRun(run.Id); c.StatusCode() != http.StatusAccepted || c.JSON202.Status != client.RunStatusSucceeded {
		t.Errorf("cancel of a finished run: %d %s", c.StatusCode(), c.Body)
	}
}
