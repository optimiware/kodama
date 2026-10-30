package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/secrets"
)

const (
	pubTenant  = domain.TenantID("t_pub")
	pubProject = domain.ProjectID("p_pub")
	pubRun     = domain.RunID("run_pub")
)

var pubScope = domain.Scope{TenantID: pubTenant, ProjectID: pubProject}

// pubRuns serves one aggregate (or an error) to the run_shared public routes.
type pubRuns struct {
	agg *execution.Aggregate
	err error
	// seq, when set, decides the n-th read (1-based): a run that changes between
	// reads.
	seq   func(n int) *execution.Aggregate
	reads *int
}

func (p pubRuns) Get(context.Context, domain.Scope, domain.RunID) (*execution.Aggregate, error) {
	if p.seq != nil {
		*p.reads++
		return p.seq(*p.reads), p.err
	}
	return p.agg, p.err
}

type pubResources struct {
	execution.RunResources
	content   string
	truncated bool
	err       error
	asked     []execution.ResourceHandle
}

func (p *pubResources) Logs(_ context.Context, h execution.ResourceHandle, _ execution.LogBound) (execution.ResourceLogs, error) {
	p.asked = append(p.asked, h)
	return execution.ResourceLogs{Content: p.content, Truncated: p.truncated}, p.err
}

type pubReader struct{ err error }

func (p pubReader) AuthorizeRead(context.Context, string, domain.Scope) error { return p.err }

// pubSecrets is a scan-capable version source serving fixed values.
type pubSecrets struct{ values map[string]string }

type pubPending struct{ v []byte }

func (p pubPending) Value() []byte                                      { return p.v }
func (pubPending) Finalize(context.Context, secrets.FinalizeInfo) error { return nil }
func (pubPending) Discard()                                             {}
func (s pubSecrets) pending(r secrets.VersionRequest) (secrets.PendingValue, error) {
	v, ok := s.values[string(r.SecretName)]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return pubPending{[]byte(v)}, nil
}
func (s pubSecrets) Open(_ context.Context, r secrets.VersionRequest) (secrets.PendingValue, error) {
	return s.pending(r)
}
func (s pubSecrets) OpenForScan(_ context.Context, r secrets.VersionRequest) (secrets.PendingValue, error) {
	return s.pending(r)
}

func pubAggregate(shared bool) *execution.Aggregate {
	at := func(t time.Time) *time.Time { return &t }
	a := &execution.Aggregate{Run: &execution.Run{Scope: pubScope, ID: pubRun, Status: execution.RunRunning, Version: 7}}
	if !shared {
		return a
	}
	deadline := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	a.Run.DeadlineAt = &deadline
	a.Run.Shared = &execution.RunShared{RunModel: execution.RunModelShared, Order: []string{"generate", "build"}, Access: map[string]execution.Access{"generate": execution.AccessWrite, "build": execution.AccessRead}}
	a.Resources = []*execution.RunResource{
		{Scope: pubScope, RunID: pubRun, Kind: execution.KindWorkspace, Key: "workspace", Phase: execution.PhaseReady, ReadyAt: at(deadline.Add(-time.Hour))},
		{Scope: pubScope, RunID: pubRun, Kind: execution.KindPreparation, Key: "preparation", Phase: execution.PhaseStopped, UID: "uid-prep", StoppedAt: at(deadline.Add(-30 * time.Minute)),
			Spec: execution.ResourceSpec{Scope: pubScope, RunID: pubRun, Kind: execution.KindPreparation, Key: "preparation"}},
		{Scope: pubScope, RunID: pubRun, Kind: execution.KindService, Key: "service.db", Phase: execution.PhaseFailed, Reason: execution.ReasonServiceLost, UID: "uid-db",
			Spec: execution.ResourceSpec{Scope: pubScope, RunID: pubRun, Kind: execution.KindService, Key: "service.db", Service: &execution.ServiceSpec{
				Name: "db", Secrets: []execution.SecretPin{{Ref: "secret://project/db-password", Version: 1, Name: "PW", Delivery: runtimev1.DeliveryEnv}}}}},
		{Scope: pubScope, RunID: pubRun, Kind: execution.KindNetwork, Key: "network", Phase: execution.PhaseReady},
	}
	return a
}

func newPub(t *testing.T, runs pubRuns, res *pubResources, reader pubReader, src pubSecrets) *SharedPublic {
	t.Helper()
	s := &SharedPublic{
		log: nil, runs: runs, resources: res, reader: reader, secrets: src,
		limits: LogsTuning{MaxLines: 1000, MaxBytes: 1 << 20, MaxDuration: time.Minute},
	}
	return s
}

func runDoc(extra string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"7"`)
		_, _ = w.Write([]byte(`{"id":"run_pub","status":"running","version":7` + extra + `}` + "\n"))
	})
}

// Test_unit_app_run_shared_public_view: the run read of a run_shared run gains
// runModel, deadlineAt, executionOrder, effectiveAccess and the safe resource
// observations from committed state, after the module's own authorized answer;
// every other answer (a refusal, an attempt_private run) passes through
// untouched, a run that cannot be read now is never answered without its
// resources, and only the exact run route is intercepted.
func Test_unit_app_run_shared_public_view(t *testing.T) {
	path := APIPrefix + "/tenants/t_pub/projects/p_pub/runs/run_pub"
	serve := func(s *SharedPublic, next http.Handler, method, p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.Wrap(next).ServeHTTP(rec, httptest.NewRequest(method, p, nil))
		return rec
	}

	s := newPub(t, pubRuns{agg: pubAggregate(true)}, &pubResources{}, pubReader{}, pubSecrets{})
	rec := serve(s, runDoc(""), "GET", path)
	if rec.Code != 200 || rec.Header().Get("ETag") != `"7"` {
		t.Fatalf("status %d etag %q", rec.Code, rec.Header().Get("ETag"))
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "run_pub" || got["version"] != float64(7) || got["runModel"] != "run_shared" || got["deadlineAt"] != "2026-10-30T16:00:00Z" {
		t.Fatalf("view: %v", got)
	}
	if order, _ := got["executionOrder"].([]any); len(order) != 2 || order[0] != "generate" {
		t.Fatalf("order: %v", got["executionOrder"])
	}
	if acc, _ := got["effectiveAccess"].(map[string]any); acc["generate"] != "write" || acc["build"] != "read" {
		t.Fatalf("access: %v", got["effectiveAccess"])
	}
	resources, _ := got["resources"].([]any)
	if len(resources) != 4 {
		t.Fatalf("resources: %v", got["resources"])
	}
	db := resources[2].(map[string]any)
	if db["key"] != "service.db" || db["phase"] != "failed" || db["reason"] != "service_lost" || db["readyAt"] != nil {
		t.Fatalf("service: %v", db)
	}
	// Nothing internal leaks into the projection.
	for _, forbidden := range []string{"uid", "UID", "fingerprint", "spec", "secret://", "uid-db"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("the view discloses %q: %s", forbidden, rec.Body)
		}
	}
	if _, present := got["cleanupReason"]; !present {
		t.Error("the cleanup reason member is absent")
	}

	// An attempt_private run is answered as the module answered it.
	plain := newPub(t, pubRuns{agg: pubAggregate(false)}, &pubResources{}, pubReader{}, pubSecrets{})
	if rec := serve(plain, runDoc(""), "GET", path); !strings.Contains(rec.Body.String(), `"version":7}`) || strings.Contains(rec.Body.String(), "runModel") {
		t.Fatalf("an attempt_private run was decorated: %s", rec.Body)
	}
	// A refusal passes through untouched (the module authorized, not this layer).
	refuse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { httpapi.WriteError(w, r, httpapi.NotFoundError()) })
	if rec := serve(s, refuse, "GET", path); rec.Code != 404 || strings.Contains(rec.Body.String(), "runModel") {
		t.Fatalf("a refusal was rewritten: %d %s", rec.Code, rec.Body)
	}
	// Only GET of the exact route is intercepted.
	hit := false
	spy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true; w.WriteHeader(204) })
	for _, p := range []string{path + "/steps", path + "/", APIPrefix + "/tenants/t_pub/projects/p_pub/runs", path + "/cancel", "/api/v1/other"} {
		hit = false
		if rec := serve(s, spy, "GET", p); !hit || rec.Code != 204 {
			t.Errorf("%s was intercepted", p)
		}
	}
	hit = false
	if rec := serve(s, spy, "POST", path); !hit || rec.Code != 204 {
		t.Error("a POST of the run was intercepted")
	}
	// A run that cannot be read now is not answered without its resources.
	down := newPub(t, pubRuns{err: errors.New("database down")}, &pubResources{}, pubReader{}, pubSecrets{})
	if rec := serve(down, runDoc(""), "GET", path); rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "database down") {
		t.Fatalf("an unreadable run: %d %s", rec.Code, rec.Body)
	}
	gone := newPub(t, pubRuns{err: execution.ErrNotFound}, &pubResources{}, pubReader{}, pubSecrets{})
	if rec := serve(gone, runDoc(""), "GET", path); rec.Code != http.StatusNotFound {
		t.Fatalf("a vanished run: %d", rec.Code)
	}
	// A run that moved between the module's answer and the committed read is read
	// again: status, version and ETag (the module's) and the resources and deadline
	// (this read) are of one version, never mixed.
	t.Run("a transition between the two reads", func(t *testing.T) {
		moved := pubAggregate(true)
		moved.Run.Version, moved.Run.Status = 8, execution.RunSucceeded
		moved.Resources[2].Phase = execution.PhaseStopped // the service stopped with the transition
		reads, answers := 0, 0
		runs := pubRuns{reads: &reads, seq: func(n int) *execution.Aggregate {
			if n == 1 {
				return moved // the run moved right after the module answered version 7
			}
			return moved
		}}
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			answers++
			v, st := 7, "running"
			if answers > 1 {
				v, st = 8, "succeeded"
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"`+strconv.Itoa(v)+`"`)
			_, _ = w.Write([]byte(`{"id":"run_pub","status":"` + st + `","version":` + strconv.Itoa(v) + "}\n"))
		})
		rec := serve(newPub(t, runs, &pubResources{}, pubReader{}, pubSecrets{}), next, "GET", path)
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		db := got["resources"].([]any)[2].(map[string]any)
		if got["version"] != float64(8) || got["status"] != "succeeded" || rec.Header().Get("ETag") != `"8"` || db["phase"] != "stopped" || answers != 2 {
			t.Fatalf("a mixed view: version %v status %v etag %q service %v after %d answers", got["version"], got["status"], rec.Header().Get("ETag"), db["phase"], answers)
		}
		// A run that keeps moving is not answered with a mixed view.
		answers = 0
		always := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			answers++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"run_pub","status":"running","version":` + strconv.Itoa(answers) + "}\n"))
		})
		if rec := serve(newPub(t, runs, &pubResources{}, pubReader{}, pubSecrets{}), always, "GET", path); rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "runModel") {
			t.Fatalf("a run that moves on every read: %d %s", rec.Code, rec.Body)
		}
		if answers != runReadAttempts {
			t.Fatalf("%d reads", answers)
		}
	})
	// The cleanup reason of a draining run is visible.
	held := pubAggregate(true)
	held.Resources[3].Phase = execution.PhaseUnknown
	held.Run.Status = execution.RunCancelled
	held.Run.Reason = execution.Reason("cancelled")
	members, _ := sharedMembers(held.SharedView())
	if _, ok := members["cleanupReason"]; !ok {
		t.Error("the cleanup reason member is absent")
	}
}

// Test_unit_app_run_shared_resource_logs: the log of a preparation or service is
// the bounded NDJSON of the attempt log with `resource` in the meta record;
// current authorization is asked first; a foreign or absent resource, and a run
// that is not run_shared, are indistinguishable (404); every value the run
// disclosed to any step or service is redacted, and an unreadable secret set
// refuses the read instead of serving it unredacted.
func Test_unit_app_run_shared_resource_logs(t *testing.T) {
	call := &httpapi.Call{Principal: httpapi.Principal{ID: "usr_1"}, Target: httpapi.Target{TenantID: pubTenant, ProjectID: pubProject}, RequestID: "req_pub"}
	get := func(s *SharedPublic, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", APIPrefix+"/tenants/t_pub/projects/p_pub/runs/run_pub/logs?"+query, nil)
		req.SetPathValue("id", string(pubRun))
		rec := httptest.NewRecorder()
		s.resourceLogs(rec, req, call)
		return rec
	}
	lines := func(rec *httptest.ResponseRecorder) []map[string]any {
		var out []map[string]any
		for _, l := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("not NDJSON: %q", rec.Body)
			}
			out = append(out, m)
		}
		return out
	}
	res := &pubResources{content: "starting\nlogin with hunter2 ok\nlistening\n"}
	s := newPub(t, pubRuns{agg: pubAggregate(true)}, res, pubReader{}, pubSecrets{values: map[string]string{"db-password": "hunter2"}})

	rec := get(s, "resource=service.db")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-ndjson" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	rs := lines(rec)
	meta, summary := rs[0], rs[len(rs)-1]
	if meta["type"] != "meta" || meta["resource"] != "service.db" || meta["runId"] != "run_pub" || meta["origin"] != "live" || meta["completeness"] != "unknown" ||
		summary["type"] != "summary" || summary["stop"] != "end" || summary["lines"] != float64(3) || summary["truncated"] != false {
		t.Fatalf("records: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "hunter2") || !strings.Contains(rec.Body.String(), logs.Marker) {
		t.Fatalf("a disclosed value reached the output: %s", rec.Body)
	}
	if len(res.asked) != 1 || res.asked[0].Key != "service.db" || res.asked[0].UID != "uid-db" {
		t.Fatalf("the port was asked for %+v", res.asked)
	}
	// The preparation has no secrets but the run's values are still removed.
	res.content = "cloning with hunter2\n"
	if rec := get(s, "resource=preparation"); strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("the preparation log carries a disclosed value: %s", rec.Body)
	}
	// Tail cuts the output and says so.
	res.content = "a\nb\nc\nd\n"
	rec = get(s, "resource=service.db&tail=2")
	rs = lines(rec)
	if rs[len(rs)-1]["stop"] != "line_limit" || rs[len(rs)-1]["truncated"] != true || len(rs) != 4 || rs[1]["text"] != "c" {
		t.Fatalf("tail: %s", rec.Body)
	}
	// A truncation by the adapter's byte bound is reported as such.
	res.content, res.truncated = "x\n", true
	if rs := lines(get(s, "resource=service.db")); rs[len(rs)-1]["stop"] != "byte_limit" {
		t.Fatalf("truncated: %v", rs)
	}
	res.truncated = false

	// A begin that commits while the backend is read discloses a value that is
	// already in the bytes: the disclosures are read after them, so it is redacted.
	// A run that cannot be reloaded after the bytes are read serves nothing.
	t.Run("a begin committed during the backend read", func(t *testing.T) {
		before := pubAggregate(true)
		before.Resources[2].Spec.Service.Secrets = nil
		after := pubAggregate(true)
		after.Resources[2].Spec.Service.Secrets = nil
		pc := kubetest.Context(t, kubetest.ContextSpec{Tenant: pubTenant, Project: pubProject, Run: pubRun, Step: "build", Attempt: "att_late", Image: kubetest.WorkloadImage})
		pc.Secrets = []runtimev1.SecretBinding{{Ref: "secret://project/late-token", Version: 1, Name: "LATE", Delivery: runtimev1.DeliveryEnv}}
		raw, err := json.Marshal(pc)
		if err != nil {
			t.Fatal(err)
		}
		after.Attempts = []*execution.Attempt{{ID: "att_late", StepID: "build", Context: raw}}
		after.Begins = []*execution.BeginRecord{{Scope: pubScope, RunID: pubRun, AttemptID: "att_late", StepID: "build"}}
		reads := 0
		runs := pubRuns{reads: &reads, seq: func(n int) *execution.Aggregate {
			if n == 1 {
				return before
			}
			return after
		}}
		res := &pubResources{content: "token=l4te-secret\n"}
		s := newPub(t, runs, res, pubReader{}, pubSecrets{values: map[string]string{"late-token": "l4te-secret"}})
		rec := get(s, "resource=service.db")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "l4te-secret") || !strings.Contains(rec.Body.String(), logs.Marker) {
			t.Fatalf("a value disclosed during the read reached the output: %d %s", rec.Code, rec.Body)
		}
		if reads != 2 {
			t.Fatalf("the run was read %d times: the disclosures were not reloaded after the bytes", reads)
		}
		// The reload fails: nothing is served.
		reads = 0
		fails := pubRuns{reads: &reads, seq: func(n int) *execution.Aggregate {
			if n == 1 {
				return before
			}
			return nil
		}}
		s = newPub(t, fails, &pubResources{content: "token=l4te-secret\n"}, pubReader{}, pubSecrets{values: map[string]string{"late-token": "l4te-secret"}})
		if rec := get(s, "resource=service.db"); rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "l4te-secret") {
			t.Fatalf("an unreloadable run was served: %d %s", rec.Code, rec.Body)
		}
	})

	// An unreadable secret set refuses the read.
	broken := newPub(t, pubRuns{agg: pubAggregate(true)}, &pubResources{content: "hunter2\n"}, pubReader{}, pubSecrets{values: map[string]string{}})
	if rec := get(broken, "resource=service.db"); rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("an unredactable log was served: %d %s", rec.Code, rec.Body)
	}
	// The resource log of an unbound resource is empty and says why.
	agg := pubAggregate(true)
	agg.Resources[2].UID = ""
	unbound := newPub(t, pubRuns{agg: agg}, &pubResources{content: "never read\n"}, pubReader{}, pubSecrets{values: map[string]string{"db-password": "x"}})
	rs = lines(get(unbound, "resource=service.db"))
	if rs[0]["reasons"].([]any)[0] != "workload_missing" || len(rs) != 2 {
		t.Fatalf("unbound: %v", rs)
	}

	// Authorization, hiding and validation.
	for name, tc := range map[string]struct {
		s      *SharedPublic
		query  string
		status int
	}{
		"no membership":         {newPub(t, pubRuns{agg: pubAggregate(true)}, res, pubReader{logs.ErrNotFound}, pubSecrets{}), "resource=preparation", 404},
		"no permission":         {newPub(t, pubRuns{agg: pubAggregate(true)}, res, pubReader{logs.ErrDenied}, pubSecrets{}), "resource=preparation", 403},
		"an authority outage":   {newPub(t, pubRuns{agg: pubAggregate(true)}, res, pubReader{errors.New("down")}, pubSecrets{}), "resource=preparation", 503},
		"a run that is private": {newPub(t, pubRuns{agg: pubAggregate(false)}, res, pubReader{}, pubSecrets{}), "resource=preparation", 404},
		"an absent run":         {newPub(t, pubRuns{err: execution.ErrNotFound}, res, pubReader{}, pubSecrets{}), "resource=preparation", 404},
		"an unknown service":    {s, "resource=service.cache", 404},
		"the workspace":         {s, "resource=workspace", 400},
		"a bad key":             {s, "resource=service.Bad", 400},
		"an attempt as well":    {s, "resource=preparation&attemptId=att_1", 400},
		"follow":                {s, "resource=preparation&follow=true", 400},
		"two resources":         {s, "resource=preparation&resource=service.db", 400},
		"a zero tail":           {s, "resource=preparation&tail=0", 400},
		"a huge tail":           {s, "resource=preparation&tail=100000", 400},
		"a non-numeric tail":    {s, "resource=preparation&tail=x", 400},
		"a run outage":          {newPub(t, pubRuns{err: errors.New("down")}, res, pubReader{}, pubSecrets{}), "resource=preparation", 503},
	} {
		if rec := get(tc.s, tc.query); rec.Code != tc.status {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	res.err = errors.New("kubernetes unreachable")
	if rec := get(s, "resource=service.db"); rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "kubernetes unreachable") {
		t.Fatalf("an adapter outage: %d %s", rec.Code, rec.Body)
	}

	// The constructor refuses a missing collaborator, and the wrapper sends a
	// `resource` log request to the dedicated router while leaving others alone.
	if _, err := NewSharedPublic(SharedPublicInputs{}); err == nil {
		t.Fatal("an empty composition was accepted")
	}
	if p, ok := publicRunPath(APIPrefix + "/tenants/t/projects/p/runs/r/logs"); !ok || len(p) != 7 {
		t.Fatalf("path: %v %v", p, ok)
	}
	for _, bad := range []string{"/api/v1/tenants//projects/p/runs/r", "/api/v1/tenants/t/projects/p/runs/r/extra/more", "/api/v2/tenants/t/projects/p/runs/r", "/api/v1/tenants/t/projects/p/run/r"} {
		if _, ok := publicRunPath(bad); ok {
			t.Errorf("%s parsed as a run path", bad)
		}
	}
}
