package app

// Acceptance of the incremental business-loop assembly (T-075):
// business_loop_wiring. Everything runs over deterministic adapters and no
// service (`make test-m3`): constructor and worker selection, the production
// construction guard, authority at every route, event routing to the
// notification consumer and the current-authority behavior of review links. It
// verifies the wiring, not the real joins: actual transactions, notification
// delivery and replay over PostgreSQL, JetStream and a receiver, and the real
// gate as a link target, are T-044's business_loop_assembly,
// business_loop_gate_links and business_loop_event_replay (WM-01/WM-05/WM-12,
// AC-08/AC-09/AC-12, DQ-06/DQ-10, SD-11). Browser-agent QA (T-069) and the
// Supabase blueprint (T-047) are not exercised, and a mapping fixture is not
// exploratory QA.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	gatesmemory "github.com/optimiware/kodama/internal/gates/gatestest/memory"
	gateshttp "github.com/optimiware/kodama/internal/gates/http"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	chatopshttp "github.com/optimiware/kodama/internal/integrations/chatops/http"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/metrics"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
	dependencieshttp "github.com/optimiware/kodama/internal/work/dependencies/http"
	dependenciespg "github.com/optimiware/kodama/internal/work/dependencies/postgres"
)

// isBusinessLoopOperation reports whether an aggregate operation key ("METHOD
// /path") belongs to the business-loop routes of T-075.
func isBusinessLoopOperation(key string) bool {
	return strings.Contains(key, "/chatops-") || strings.Contains(key, "/dependencies")
}

// blInputs rebuilds the slice inputs of the dev composition for another
// environment or with a member changed.
func blInputs(t *testing.T, d *devSlice, env Env) BusinessLoopInputs {
	t.Helper()
	cursors, err := httpapi.NewCursorCodec(bytes32('c'))
	if err != nil {
		t.Fatal(err)
	}
	return BusinessLoopInputs{
		Env: env, Cursors: cursors, Dependencies: d.BusinessLoop.Dependencies, Flow: d.BusinessLoop.Flow, QA: d.BusinessLoop.QA,
		ChatOps: &ChatOpsParts{Service: d.BL.Service, Worker: d.BL.Worker, Consumer: chatops.ConsumerSpec(), Durable: chatopsDurable},
	}
}

// publishAndPump publishes the committed outbox events and delivers them to the
// consumers, as the publisher and the broker consumers do in the process.
func (d *devSlice) publishAndPump() {
	d.t.Helper()
	ctx := context.Background()
	pub, err := events.NewPublisher(d.Tenants, d.Events, d.Broker, events.PublisherConfig{
		Lease: time.Minute, PublishTimeout: time.Second, BatchSize: 50, Interval: time.Second, Retry: d.Tuning.Events.Retry,
	})
	if err != nil {
		d.t.Fatal(err)
	}
	if _, err := pub.RunOnce(ctx); err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.Broker.Pump(ctx); err != nil {
		d.t.Fatal(err)
	}
}

// createItem creates a ready-able story as the engineer.
func (d *devSlice) createItem(title string) client.WorkItem {
	d.t.Helper()
	spec := "A business-loop item."
	criteria := []client.AcceptanceCriterion{{Id: "ac_1", Text: "the checks pass"}}
	r, err := d.api(devEngineerToken).CreateWorkItemWithResponse(context.Background(), string(devTenant), string(devProject),
		client.CreateWorkItem{Kind: "story", Title: title, Spec: &spec, AcceptanceCriteria: &criteria})
	if err != nil || r.JSON201 == nil {
		d.t.Fatalf("create work item: %v %s", err, r.Body)
	}
	return *r.JSON201
}

func ifMatchVersion(v int64) client.IfMatch {
	return client.IfMatch(`"` + strconv.FormatInt(v, 10) + `"`)
}

// itemVersion reads the current version of the dependent as the dependency
// routes compare it (the work item's ETag).
func (d *devSlice) itemVersion(id string) int64 {
	d.t.Helper()
	r, err := d.api(devEngineerToken).ListWorkItemDependenciesWithResponse(context.Background(), string(devTenant), string(devProject), id, &client.ListWorkItemDependenciesParams{})
	if err != nil || r.JSON200 == nil {
		d.t.Fatalf("list dependencies: %v %d %s", err, r.StatusCode(), r.Body)
	}
	etag := r.HTTPResponse.Header.Get("ETag")
	v, perr := strconv.ParseInt(strings.Trim(etag, `"`), 10, 64)
	if perr != nil {
		d.t.Fatalf("ETag %q: %v", etag, perr)
	}
	return v
}

func (d *devSlice) addDependency(token, dependent, prerequisite string) *client.AddWorkItemDependencyResponse {
	d.t.Helper()
	r, err := d.api(token).AddWorkItemDependencyWithResponse(context.Background(), string(devTenant), string(devProject), dependent,
		&client.AddWorkItemDependencyParams{IfMatch: ifMatchVersion(d.itemVersion(dependent))}, client.AddDependency{DependencyId: prerequisite})
	if err != nil {
		d.t.Fatal(err)
	}
	return r
}

// TestBusinessLoopWiring (business_loop_wiring): the dependency graph, the flow
// summaries, the QA mapping and ChatOps, joined to the M2 composition over
// deterministic adapters.
func TestBusinessLoopWiring(t *testing.T) {
	d := newDevSlice(t)
	ctx := context.Background()
	tenant, project := string(devTenant), string(devProject)

	t.Run("the installation statement is strict and selects nothing by omission", func(t *testing.T) {
		if (BusinessLoopConfig{}).Configured() {
			t.Fatal("the zero configuration selects something")
		}
		write := func(body string) string {
			p := filepath.Join(t.TempDir(), "business-loop.json")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}
		const good = `{"qa":{"mappingPrincipal":"u_qa_mapper","reportOutput":"qa-report","deploymentInput":"deployment"},
			"chatops":{"origin":"https://kodama.example.test","hosts":["*.example.test"],"ports":[443,8443],
			"privateEndpoints":[{"tenant":"t_acme","project":"p_inv","address":"10.1.2.3:8443"}]}}`
		cfg, err := LoadBusinessLoop(write(good))
		if err != nil || !cfg.Configured() || cfg.QA == nil || cfg.ChatOps == nil {
			t.Fatalf("a valid statement: %+v %v", cfg, err)
		}
		if err := cfg.Validate(Production, DefaultTuning()); err != nil {
			t.Fatalf("a valid statement under production: %v", err)
		}
		for name, body := range map[string]string{
			"an unknown member":  `{"chatops":{"origin":"https://kodama.example.test","allowAll":true}}`,
			"trailing content":   good + ` {}`,
			"a non-object":       `[]`,
			"a misspelled block": `{"chatop":{"origin":"https://kodama.example.test"}}`,
		} {
			if _, err := LoadBusinessLoop(write(body)); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		bad := map[string]BusinessLoopConfig{
			"an http origin":          {ChatOps: &ChatOpsConfig{Origin: "http://kodama.example.test"}},
			"an origin with a path":   {ChatOps: &ChatOpsConfig{Origin: "https://kodama.example.test/api"}},
			"no origin":               {ChatOps: &ChatOpsConfig{}},
			"a private range":         {ChatOps: &ChatOpsConfig{Origin: devOrigin, PrivateEndpoints: []PrivateEndpointConfig{{Tenant: "t_acme", Address: "10.0.0.0/8"}}}},
			"a link-local endpoint":   {ChatOps: &ChatOpsConfig{Origin: devOrigin, PrivateEndpoints: []PrivateEndpointConfig{{Tenant: "t_acme", Address: "169.254.169.254:80"}}}},
			"a QA mapping, no names":  {QA: &QAMappingConfig{MappingPrincipal: "u_qa_mapper"}},
			"a QA mapping, no mapper": {QA: &QAMappingConfig{ReportOutput: "r", DeploymentInput: "d"}},
		}
		for name, c := range bad {
			if err := c.Validate(Development, DefaultTuning()); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		loopback := BusinessLoopConfig{ChatOps: &ChatOpsConfig{Origin: devOrigin, PrivateEndpoints: []PrivateEndpointConfig{{Tenant: "t_acme", Address: "127.0.0.1:8443"}}}}
		if err := loopback.Validate(Development, DefaultTuning()); err != nil {
			t.Errorf("a loopback receiver is a development fixture: %v", err)
		}
		var ae *AdapterError
		if err := loopback.Validate(Production, DefaultTuning()); !errors.As(err, &ae) {
			t.Errorf("production accepted a loopback receiver: %v", err)
		}
		slow := DefaultTuning()
		slow.ChatOps.AttemptTimeout = slow.ChatOps.Lease
		if err := cfg.Validate(Development, slow); err == nil {
			t.Error("a send that outlives its claim lease was accepted")
		}
		// The whole core configuration rejects it at startup, not at the first pass.
		core := CoreConfig{Env: Development, BusinessLoop: BusinessLoopConfig{ChatOps: &ChatOpsConfig{Origin: "http://x"}}, Tuning: DefaultTuning()}
		if err := core.Validate(); err == nil || !strings.Contains(err.Error(), "origin") {
			t.Errorf("the core configuration did not report the origin: %v", err)
		}
	})

	t.Run("the slice needs what it joins and applies the environment's construction guard", func(t *testing.T) {
		if _, err := AssembleBusinessLoop(blInputs(t, d, Env("staging"))); !errors.Is(err, ErrEnvUnknown) {
			t.Fatalf("an unknown environment assembled: %v", err)
		}
		for name, mutate := range map[string]func(*BusinessLoopInputs){
			"the dependency service": func(in *BusinessLoopInputs) { in.Dependencies = nil },
			"the flow service":       func(in *BusinessLoopInputs) { in.Flow = nil },
			"the cursor codec":       func(in *BusinessLoopInputs) { in.Cursors = nil },
			"the ChatOps service":    func(in *BusinessLoopInputs) { in.ChatOps.Service = nil },
			"the ChatOps worker":     func(in *BusinessLoopInputs) { in.ChatOps.Worker = nil },
			"the ChatOps consumer":   func(in *BusinessLoopInputs) { in.ChatOps.Consumer = events.ConsumerSpec{}; in.ChatOps.Durable = "" },
		} {
			in := blInputs(t, d, Development)
			mutate(&in)
			if _, err := AssembleBusinessLoop(in); err == nil {
				t.Errorf("assembled without %s", name)
			} else if !strings.Contains(err.Error(), "required") && !errors.Is(err, ErrEnvUnknown) {
				t.Errorf("without %s: %v", name, err)
			}
		}
		// ChatOps is selected as a whole or not at all, and the rest still assembles.
		in := blInputs(t, d, Development)
		in.ChatOps = nil
		s, err := AssembleBusinessLoop(in)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Workers) != 0 || len(s.Consumers) != 0 || s.ChatOps != nil || slices.Contains(s.Routes, "chatops") || !slices.Contains(s.Disabled, "chatops") {
			t.Errorf("an unselected ChatOps left a worker, consumer or route behind: %+v", s)
		}
		if !slices.Contains(s.Routes, "work-dependencies") || s.QA == nil || s.Flow == nil {
			t.Errorf("the dependency routes and services are always selected: %+v", s)
		}

		// Production refuses every development or test adapter of the composition,
		// wherever it is in the list, and accepts the same slice without them.
		prod := blInputs(t, d, Production)
		prod.Adapters = []any{d.BL.Deps}
		if _, err := AssembleBusinessLoop(prod); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production accepted the dependency double: %v", err)
		}
		for name, a := range map[string]any{"chatops store": d.BL.Chat, "sender": d.BL.Sender, "flow reader": d.BL.Flow, "qa ports": d.BL.QAPorts} {
			prod.Adapters = []any{a}
			if _, err := AssembleBusinessLoop(prod); !errors.Is(err, ErrDevelopmentAdapter) {
				t.Errorf("production accepted the %s double: %v", name, err)
			}
		}
		prod.Adapters = nil
		if _, err := AssembleBusinessLoop(prod); err != nil {
			t.Fatalf("production refused the slice over unflagged services: %v", err)
		}
		if _, err := buildDevSlice(t, Production); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production composed the development business loop: %v", err)
		}
	})

	t.Run("production selects the transaction-bound dependency graph, not the explicit none", func(t *testing.T) {
		guards := dependencyGuards(nil)
		if guards.Dependencies == nil {
			t.Fatal("the production work store has no dependency guard")
		}
		if _, none := guards.Dependencies.(work.NoDependencyGraph); none {
			t.Fatal("the production work store selected the M1 explicit none")
		}
		if _, ok := guards.Dependencies.(dependenciespg.DependencyGuard); !ok {
			t.Fatalf("the production guard is %T, not the transaction-bound dependency guard", guards.Dependencies)
		}
		// A guard with no transaction fails closed: an unknown answer is never admission.
		if err := guards.Dependencies.DependenciesSatisfied(ctx, devScope, "wi_probe"); !errors.Is(err, work.ErrUnavailable) {
			t.Fatalf("an unbound guard answered %v", err)
		}
		src, err := os.ReadFile("core.go")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "NoDependencyGraph") {
			t.Error("the production composition still names the explicit none dependency selection")
		}
		for _, want := range []string{"buildBusinessLoopPG(", "dependencyGuards(tx)", "loopBindings", "loop.Consumers", "loop.Workers", "DisabledCapabilitiesM3("} {
			if !strings.Contains(string(src), want) {
				t.Errorf("core.go does not join %s", want)
			}
		}
		if _, ok := any(txWorkPermissions(nil)).(work.PermissionChecker); !ok {
			t.Error("the dependency and QA stores are not bound to the identity transaction check")
		}
	})

	t.Run("routes, rules and recorded capabilities", func(t *testing.T) {
		for _, group := range []string{"work-dependencies", "chatops"} {
			if !slices.Contains(d.Routes.Registered, group) {
				t.Errorf("route group %q is not registered: %v", group, d.Routes.Registered)
			}
		}
		for _, name := range []string{"flow-summary-list", "exploratory-qa", "chatops-authenticated-destinations", "chatops-restore-fence"} {
			if !slices.Contains(d.Routes.Disabled, name) {
				t.Errorf("%q is not wired and must be recorded: %v", name, d.Routes.Disabled)
			}
		}
		if slices.Contains(d.Routes.Disabled, "chatops") {
			t.Errorf("chatops is wired and must not be recorded as disabled: %v", d.Routes.Disabled)
		}
		rules := coreRules()
		for action, perm := range map[httpapi.Action]string{
			dependencieshttp.ActionList: "work_item:read", dependencieshttp.ActionAdd: "work_item:edit_spec", dependencieshttp.ActionRemove: "work_item:edit_spec",
		} {
			r, ok := rules[action]
			if !ok || len(r.Any) != 1 || string(r.Any[0]) != perm {
				t.Errorf("action %s: rule %+v, want %s", action, r, perm)
			}
		}
		for action, want := range chatopshttp.Rules() {
			if got, ok := rules[action]; !ok || !slices.Equal(got.Any, want.Any) || got.Level != want.Level {
				t.Errorf("action %s: rule %+v, want the module's %+v", action, got, want)
			}
		}
		// Neither the mapping nor the flow summaries add a route of their own.
		for _, route := range chatopshttp.Routes() {
			if strings.Contains(route.Path, "qa") || strings.Contains(route.Path, "approve") {
				t.Errorf("route %s grants authority", route.Path)
			}
		}
		for _, banned := range []string{"/qa", "/findings", "/flow", "/metrics", "/approve"} {
			status, _ := d.probe(http.MethodGet, "/api/v1/tenants/"+tenant+"/projects/"+project+banned, devAdminToken)
			if status != http.StatusNotFound {
				t.Errorf("GET %s answered %d", banned, status)
			}
		}
		// A workload worker is registered once and starts and stops with its group.
		names := map[string]bool{}
		for _, w := range d.BusinessLoop.Workers {
			if w.Name == "" || w.Run == nil {
				t.Errorf("worker %+v is incomplete", w)
			}
			names[w.Name] = true
		}
		if !names["chatops-delivery"] || len(d.BusinessLoop.Workers) != 1 {
			t.Errorf("workers: %v", names)
		}
		if len(d.BusinessLoop.Consumers) != 1 || d.BusinessLoop.Consumers[0].Spec.Name != chatops.ConsumerName || d.BusinessLoop.Consumers[0].Durable != chatopsDurable {
			t.Errorf("consumers: %+v", d.BusinessLoop.Consumers)
		}
	})

	t.Run("dependency routes run under the identity authority and admission follows the links", func(t *testing.T) {
		a, b := d.createItem("depends-a"), d.createItem("depends-b")
		for _, id := range []string{a.Id, b.Id} {
			if r := d.transition(d.api(devEngineerToken), id, "ready"); r.StatusCode() != http.StatusOK {
				t.Fatalf("ready: %d %s", r.StatusCode(), r.Body)
			}
		}
		// Only a caller who may edit the specification changes the graph; a viewer
		// reads, a caller without a binding learns nothing, nobody anonymous does either.
		if r := d.addDependency(devViewerToken, a.Id, b.Id); r.StatusCode() != http.StatusForbidden {
			t.Fatalf("a viewer added a dependency: %d %s", r.StatusCode(), r.Body)
		}
		if r := d.addDependency(devOtherToken, a.Id, b.Id); r.StatusCode() != http.StatusNotFound && r.StatusCode() != http.StatusForbidden {
			t.Fatalf("a caller without a binding: %d %s", r.StatusCode(), r.Body)
		}
		if status, code := d.probe(http.MethodPost, "/api/v1/tenants/"+tenant+"/projects/"+project+"/work-items/"+a.Id+"/dependencies", ""); status != http.StatusUnauthorized || code != "invalid_credential" {
			t.Fatalf("anonymous: %d %q", status, code)
		}
		if got, err := d.api(devViewerToken).ListWorkItemDependenciesWithResponse(ctx, tenant, project, a.Id, &client.ListWorkItemDependenciesParams{}); err != nil || got.StatusCode() != http.StatusOK {
			t.Fatalf("a viewer could not read the links: %v %d %s", err, got.StatusCode(), got.Body)
		}

		r := d.addDependency(devEngineerToken, a.Id, b.Id)
		if r.StatusCode() != http.StatusCreated && r.StatusCode() != http.StatusOK {
			t.Fatalf("add: %d %s", r.StatusCode(), r.Body)
		}
		if again := d.addDependency(devEngineerToken, a.Id, b.Id); again.JSON200 == nil || again.JSON200.Changed {
			t.Fatalf("adding a link twice must be a no-op: %d %s", again.StatusCode(), again.Body)
		}
		if cyc := d.addDependency(devEngineerToken, b.Id, a.Id); cyc.StatusCode() != http.StatusUnprocessableEntity {
			t.Fatalf("a cycle was accepted: %d %s", cyc.StatusCode(), cyc.Body)
		}

		// The same links gate ready -> active: the prerequisite is not done.
		blocked := d.transition(d.api(devEngineerToken), a.Id, "active")
		if blocked.StatusCode() != http.StatusUnprocessableEntity || !strings.Contains(string(blocked.Body), "prerequisite") {
			t.Fatalf("admission ignored the link: %d %s", blocked.StatusCode(), blocked.Body)
		}
		// Deleting the link is the explicit way to drop the prerequisite.
		rm, err := d.api(devEngineerToken).RemoveWorkItemDependencyWithResponse(ctx, tenant, project, a.Id, b.Id,
			&client.RemoveWorkItemDependencyParams{IfMatch: ifMatchVersion(d.itemVersion(a.Id))})
		if err != nil || (rm.StatusCode() != http.StatusOK && rm.StatusCode() != http.StatusNoContent) {
			t.Fatalf("remove: %v %d %s", err, rm.StatusCode(), rm.Body)
		}
		if ok := d.transition(d.api(devEngineerToken), a.Id, "active"); ok.StatusCode() != http.StatusOK {
			t.Fatalf("admission after the link was deleted: %d %s", ok.StatusCode(), ok.Body)
		}
	})

	t.Run("flow summaries authorize before they read", func(t *testing.T) {
		item := d.createItem("flow")
		d.BL.Flow.known[domain.WorkItemID(item.Id)] = true
		w, err := d.BusinessLoop.Flow.Window(time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond), time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond))
		if err != nil {
			t.Fatal(err)
		}
		ids := []domain.WorkItemID{domain.WorkItemID(item.Id)}
		before := d.BL.Flow.Calls
		for _, who := range []string{devOther, ""} {
			if _, err := d.BusinessLoop.Flow.Summarize(ctx, devScope, who, ids, w); err == nil {
				t.Errorf("principal %q was served a summary", who)
			}
		}
		if d.BL.Flow.Calls != before {
			t.Fatal("a denied caller reached the summary reader")
		}
		got, err := d.BusinessLoop.Flow.Summarize(ctx, devScope, devViewer, ids, w)
		if err != nil || len(got) != 1 || got[0].ItemID != domain.WorkItemID(item.Id) {
			t.Fatalf("a viewer's summary: %+v %v", got, err)
		}
		if _, err := d.BusinessLoop.Flow.Summarize(ctx, devScope, devViewer, ids, metrics.Window{}); err == nil {
			t.Fatal("an empty window was accepted")
		}
	})

	t.Run("the QA mapping is reachable by the control plane only and never files without trusted facts", func(t *testing.T) {
		if d.BusinessLoop.QA == nil {
			t.Fatal("the QA mapping is not selected")
		}
		before := d.BL.QATouched.Load()
		for name, actor := range map[string]qa.Actor{
			"a person":    {Type: qa.ActorUser, ID: devAdmin},
			"an operator": {Type: qa.ActorOperator, ID: "op"},
			"a workload":  {Type: qa.ActorWorkload, ID: "att_probe"},
			"nobody":      {},
		} {
			_, err := d.BusinessLoop.QA.Record(ctx, qa.RecordCommand{Scope: devScope, Attempt: "att_probe", Caller: qa.Caller{Actor: actor, RequestID: "req_t075"}})
			if !errors.Is(err, qa.ErrDenied) {
				t.Errorf("%s was not denied: %v", name, err)
			}
		}
		if d.BL.QATouched.Load() != before {
			t.Fatal("a refused caller reached the execution, deployment, artifact or work modules")
		}
		// The control plane's call stops at the attempt: nothing is mapped without it.
		_, err := d.BusinessLoop.QA.Record(ctx, qa.RecordCommand{Scope: devScope, Attempt: "att_unknown", Caller: qa.Caller{Actor: qa.Actor{Type: qa.ActorSystem, ID: "reconciler"}, RequestID: "req_t075"}})
		if !errors.Is(err, qa.ErrNotFound) {
			t.Fatalf("an unknown attempt: %v", err)
		}
		if d.BL.QATouched.Load() == before {
			t.Fatal("the control plane's call did not read the attempt's trusted facts")
		}
	})

	t.Run("ChatOps destinations are administered under project:settings and the installation policy", func(t *testing.T) {
		const hook = "https://" + devHookHost + "/kodama"
		create := func(token string, body client.DestinationCreate) *client.CreateChatopsDestinationResponse {
			r, err := d.api(token).CreateChatopsDestinationWithResponse(ctx, tenant, project, body)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		selected := []client.ChatopsEventType{client.ChatopsEventTypeWorkItemTransitioned}
		if r := create(devEngineerToken, client.DestinationCreate{Name: "eng", Endpoint: hook, Events: selected}); r.StatusCode() != http.StatusForbidden {
			t.Fatalf("an engineer configured a destination: %d %s", r.StatusCode(), r.Body)
		}
		if r := create(devViewerToken, client.DestinationCreate{Name: "viewer", Endpoint: hook, Events: selected}); r.StatusCode() != http.StatusForbidden {
			t.Fatalf("a viewer configured a destination: %d %s", r.StatusCode(), r.Body)
		}
		if r := create(devOtherToken, client.DestinationCreate{Name: "other", Endpoint: hook, Events: selected}); r.StatusCode() != http.StatusNotFound && r.StatusCode() != http.StatusForbidden {
			t.Fatalf("a caller without a binding: %d %s", r.StatusCode(), r.Body)
		}
		if r := create(devAdminToken, client.DestinationCreate{Name: "elsewhere", Endpoint: "https://evil.example.invalid/x", Events: selected}); r.StatusCode() != http.StatusUnprocessableEntity {
			t.Fatalf("a host outside the installation policy: %d %s", r.StatusCode(), r.Body)
		}
		if r := create(devAdminToken, client.DestinationCreate{Name: "http", Endpoint: "http://" + devHookHost + "/x", Events: selected}); r.StatusCode() == http.StatusCreated {
			t.Fatalf("a plain-HTTP destination was accepted: %s", r.Body)
		}
		cred := "secret://installation/hook-token"
		if r := create(devAdminToken, client.DestinationCreate{Name: "authed", Endpoint: hook, Events: selected, Credential: &cred}); r.StatusCode() == http.StatusCreated {
			t.Fatalf("an authenticated destination was accepted although no credential source is selected: %s", r.Body)
		}
		created := create(devAdminToken, client.DestinationCreate{Name: "ops", Endpoint: hook, Events: selected})
		if created.JSON201 == nil {
			t.Fatalf("the administrator's destination: %d %s", created.StatusCode(), created.Body)
		}
		if list, err := d.api(devViewerToken).ListChatopsDeliveriesWithResponse(ctx, tenant, project, &client.ListChatopsDeliveriesParams{}); err != nil || list.StatusCode() != http.StatusOK {
			t.Fatalf("a viewer could not inspect deliveries (project:read): %v %d %s", err, list.StatusCode(), list.Body)
		}
		if list, err := d.api(devViewerToken).ListChatopsDestinationsWithResponse(ctx, tenant, project, &client.ListChatopsDestinationsParams{}); err != nil || list.StatusCode() != http.StatusForbidden {
			t.Fatalf("destination configuration is for settings administrators: %v %d %s", err, list.StatusCode(), list.Body)
		}
	})

	t.Run("events route to the notification consumer exactly once and only when selected", func(t *testing.T) {
		// A second project's work and the first project's unselected events must not
		// notify this project's destination; its selected ones must, once.
		hook := "https://" + devHookHost + "/route"
		dest, err := d.api(devAdminToken).CreateChatopsDestinationWithResponse(ctx, tenant, project, client.DestinationCreate{
			Name: "routing", Endpoint: hook, Events: []client.ChatopsEventType{client.ChatopsEventTypeWorkItemTransitioned, client.ChatopsEventTypeGateRequested},
		})
		if err != nil || dest.JSON201 == nil {
			t.Fatalf("destination: %v %s", err, dest.Body)
		}
		d.publishAndPump() // drain the earlier subtests' events: none of them predates this destination's intents
		baseline := len(d.BL.Chat.Deliveries())

		item := d.createItem("routed")
		d.publishAndPump()
		if n := len(d.BL.Chat.Deliveries()); n != baseline {
			t.Fatalf("a created event (not forwarded) produced %d deliveries", n-baseline)
		}
		if r := d.transition(d.api(devEngineerToken), item.Id, "ready"); r.StatusCode() != http.StatusOK {
			t.Fatalf("ready: %d %s", r.StatusCode(), r.Body)
		}
		d.publishAndPump()
		var mine []chatops.Delivery
		for _, del := range d.BL.Chat.Deliveries() {
			if del.DestinationID == chatops.DestinationID(dest.JSON201.Id) {
				mine = append(mine, del)
			}
		}
		var transitioned []chatops.Delivery
		for _, del := range mine {
			if del.EventType == eventsv1.TypeWorkItemTransitioned && strings.Contains(string(del.Payload), item.Id) {
				transitioned = append(transitioned, del)
			}
		}
		if len(transitioned) != 1 {
			t.Fatalf("the transition produced %d deliveries for the destination: %+v", len(transitioned), mine)
		}
		del := transitioned[0]
		var payload chatops.Payload
		if err := json.Unmarshal(del.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		wantURL := devOrigin + "/api/v1/tenants/" + tenant + "/projects/" + project + "/work-items/" + item.Id
		if payload.URL != wantURL || payload.NotificationID != string(del.ID) || payload.TenantID != tenant || payload.ProjectID != project {
			t.Fatalf("payload %+v, want the current-record link %s", payload, wantURL)
		}

		// The same event again (a broker redelivery) is absorbed by the inbox.
		handled, total := d.BL.Chat.Handled, len(d.BL.Chat.Deliveries())
		if !d.Broker.Redeliver(del.EventID) {
			t.Fatalf("event %s is not in the broker log", del.EventID)
		}
		if _, err := d.Broker.Pump(ctx); err != nil {
			t.Fatal(err)
		}
		if d.BL.Chat.Handled != handled || len(d.BL.Chat.Deliveries()) != total {
			t.Fatalf("a redelivered event created another intent (handled %d -> %d)", handled, d.BL.Chat.Handled)
		}

		// A gate request of the project is routed with the gate's own link; an
		// event of another project of the tenant is not.
		appendFixture := func(id, project string, fixture string) {
			t.Helper()
			raw, err := os.ReadFile(filepath.Join("..", "integrations", "chatops", "testdata", fixture))
			if err != nil {
				t.Fatal(err)
			}
			env, err := eventsv1.Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			env.ID, env.TenantID, env.ProjectID = id, devTenant, domain.ProjectID(project)
			if err := d.DB.Update(ctx, devTenant, func(tx memdb.Tx) error { _, err := d.Events.Append(tx, env); return err }); err != nil {
				t.Fatalf("append %s: %v", id, err)
			}
		}
		appendFixture("evt_t075_gate", project, "gate_requested.event.json")
		appendFixture("evt_t075_foreign", "p_other", "gate_requested.event.json")
		d.publishAndPump()
		var gate []chatops.Delivery
		for _, del := range d.BL.Chat.Deliveries() {
			if del.EventType == eventsv1.TypeGateRequested {
				gate = append(gate, del)
			}
		}
		if len(gate) != 1 || gate[0].EventID != "evt_t075_gate" {
			t.Fatalf("the gate request produced %+v", gate)
		}
		if !strings.Contains(string(gate[0].Payload), devOrigin+"/api/v1/tenants/"+tenant+"/projects/"+project+"/gates/gate_1042") {
			t.Fatalf("the gate notification does not link the current gate record: %s", gate[0].Payload)
		}
		if strings.Contains(string(gate[0].Payload), "internalNote") || strings.Contains(string(gate[0].Payload), "free-form") {
			t.Fatalf("producer free-form data was forwarded: %s", gate[0].Payload)
		}

		// A run event is not selected by this destination.
		runsBefore := len(d.BL.Chat.Deliveries())
		appendFixture("evt_t075_run", project, "deployment_state_changed.event.json")
		d.publishAndPump()
		if len(d.BL.Chat.Deliveries()) != runsBefore {
			t.Fatal("an event type the destination did not select was notified")
		}

		// The worker sends each durable intent once, with the stable key and the
		// exact stored payload, and a delivered record is final.
		d.BL.Sender.mu.Lock()
		d.BL.Sender.Requests = nil
		d.BL.Sender.mu.Unlock()
		res, err := d.BL.Worker.RunOnce(ctx)
		if err != nil || res.Delivered < 2 {
			t.Fatalf("worker pass: %+v %v", res, err)
		}
		sent := d.BL.Sender.Sent()
		seen := map[string]bool{}
		for _, r := range sent {
			if r.Scope != devScope || r.IdempotencyKey != string(r.DeliveryID) || r.Destination.Endpoint != hook && r.Destination.Endpoint != "https://"+devHookHost+"/kodama" {
				t.Errorf("request %+v", r)
			}
			if seen[r.IdempotencyKey] {
				t.Errorf("delivery %s was sent twice", r.IdempotencyKey)
			}
			seen[r.IdempotencyKey] = true
		}
		again, err := d.BL.Worker.RunOnce(ctx)
		if err != nil || again.Claimed != 0 || len(d.BL.Sender.Sent()) != len(sent) {
			t.Fatalf("a delivered notification was sent again: %+v %v", again, err)
		}
	})

	t.Run("a receiver outage is visible, never rolls business state back and replays by authorized request", func(t *testing.T) {
		hook := "https://" + devHookHost + "/outage"
		dest, err := d.api(devAdminToken).CreateChatopsDestinationWithResponse(ctx, tenant, project, client.DestinationCreate{
			Name: "outage", Endpoint: hook, Events: []client.ChatopsEventType{client.ChatopsEventTypeWorkItemTransitioned},
		})
		if err != nil || dest.JSON201 == nil {
			t.Fatalf("destination: %v %s", err, dest.Body)
		}
		item := d.createItem("outage")
		reject := chatops.Outcome{Class: chatops.ClassRejected, HTTPStatus: 400}
		d.BL.Sender.Refuse(&reject)
		if r := d.transition(d.api(devEngineerToken), item.Id, "ready"); r.StatusCode() != http.StatusOK {
			t.Fatalf("the review of the business state must not depend on the receiver: %d %s", r.StatusCode(), r.Body)
		}
		d.publishAndPump()
		if _, err := d.BL.Worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		list, err := d.api(devViewerToken).ListChatopsDeliveriesWithResponse(ctx, tenant, project, &client.ListChatopsDeliveriesParams{State: ptr(client.DeliveryState("failed"))})
		if err != nil || list.JSON200 == nil || len(list.JSON200.Items) == 0 {
			t.Fatalf("a failed delivery must be visible: %v %d %s", err, list.StatusCode(), list.Body)
		}
		failed := list.JSON200.Items[0]
		// The work item stays ready: the notification failed, the business state did not.
		if got, _ := d.api(devEngineerToken).GetWorkItemWithResponse(ctx, tenant, project, item.Id); got.JSON200 == nil || string(got.JSON200.State) != "ready" {
			t.Fatalf("the business state followed the outage: %s", got.Body)
		}
		reason := "receiver fixed"
		replay := func(token string) *client.ReplayChatopsDeliveryResponse {
			r, err := d.api(token).ReplayChatopsDeliveryWithResponse(ctx, tenant, project, failed.Id,
				&client.ReplayChatopsDeliveryParams{IfMatch: ifMatchVersion(int64(failed.Version))}, client.ReplayRequest{Reason: reason})
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		if r := replay(devViewerToken); r.StatusCode() != http.StatusForbidden {
			t.Fatalf("a viewer replayed a delivery: %d %s", r.StatusCode(), r.Body)
		}
		if r := replay(devAdminToken); r.StatusCode() != http.StatusOK && r.StatusCode() != http.StatusAccepted {
			t.Fatalf("the administrator's replay: %d %s", r.StatusCode(), r.Body)
		}
		d.BL.Sender.Refuse(nil) // the receiver is back
		before := len(d.BL.Sender.Sent())
		if res, err := d.BL.Worker.RunOnce(ctx); err != nil || res.Delivered == 0 {
			t.Fatalf("replayed pass: %+v %v", res, err)
		}
		var resent []chatops.SendRequest
		for _, r := range d.BL.Sender.Sent()[before:] {
			if r.DeliveryID == chatops.DeliveryID(failed.Id) {
				resent = append(resent, r)
			}
		}
		if len(resent) != 1 || resent[0].IdempotencyKey != failed.Id {
			t.Fatalf("a replay must send the failed notification once and keep its key: %d requests for %s", len(resent), failed.Id)
		}
	})

	t.Run("the worker stops with its group and fails deliberately", func(t *testing.T) {
		g := newWorkerGroup(context.Background(), nil)
		for _, w := range d.BusinessLoop.Workers {
			g.Go(w.Name, w.Run)
		}
		time.Sleep(30 * time.Millisecond)
		stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := g.Stop(stop); err != nil {
			t.Fatalf("the workers did not stop: %v", err)
		}
		if err := g.Healthy(); err != nil {
			t.Fatalf("a worker failed during a clean stop: %v", err)
		}
	})
}

// TestBusinessLoopGateLinks: a review link carries no authority. Following it
// asks the existing gate route to authorize the follower against the record as it
// is now, so forwarding it changes nothing, a revoked reviewer is refused and a
// GET never decides. The gate is the memory fixture of the gate module; the real
// gate, its subject applicability and the notification join are T-044.
func TestBusinessLoopGateLinks(t *testing.T) {
	ctx := context.Background()
	h := gatesmemory.NewHarness(t)
	h.E.SetTenants(gatesmemory.ScopeA.TenantID)
	c := h.NewCase(t, false)
	gate, _, err := h.E.Svc.RequestForStep(ctx, c.Scope, c.Run, c.Step)
	if err != nil {
		t.Fatalf("request the gate: %v", err)
	}

	gh, err := gateshttp.NewHandler(gateshttp.Config{Service: h.E.Svc, Decider: dvDecider{}, Cursors: fxCursors(t)})
	if err != nil {
		t.Fatal(err)
	}
	router, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: dvAuthn{}, Authorizer: fxAuthz{}, RateLimiter: httpapi.Unlimited(), Prefix: APIPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if err := gh.RegisterRoutes(router); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()

	// The link the planner puts in the notification of a gate request.
	links, err := chatops.NewLinkBuilder(devOrigin)
	if err != nil {
		t.Fatal(err)
	}
	link, err := links.URL(c.Scope, "gate", string(gate.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(link, "?#@") || !strings.HasPrefix(link, devOrigin+"/api/v1/") {
		t.Fatalf("the link carries more than the record's address: %s", link)
	}
	local := srv.URL + strings.TrimPrefix(link, devOrigin)

	follow := func(method, token string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, local, strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, string(b)
	}
	if status, _ := follow(http.MethodGet, gatesmemory.Reviewer); status != http.StatusOK {
		t.Fatalf("the reviewer following the link: %d", status)
	}
	// Forwarding the URL grants nothing: anonymous, unbound and non-reviewer
	// followers are answered by the route's own authorization.
	if status, _ := follow(http.MethodGet, ""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous follower: %d", status)
	}
	if status, _ := follow(http.MethodGet, gatesmemory.Stranger); status == http.StatusOK {
		t.Fatalf("a forwarded link opened the gate for a stranger")
	}
	if status, _ := follow(http.MethodGet, gatesmemory.Foreign); status == http.StatusOK {
		t.Fatalf("a forwarded link opened the gate across tenants")
	}
	// Opening or posting to the link decides nothing.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if status, _ := follow(method, gatesmemory.Reviewer); status == http.StatusOK || status == http.StatusCreated || status == http.StatusNoContent {
			t.Fatalf("%s on the link answered %d", method, status)
		}
	}
	if g, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step); err != nil || g.Version != gate.Version {
		t.Fatalf("following the link changed the gate: %v %+v", err, g)
	}
	// The same URL follows current authority: revoking the reviewer closes it.
	h.Revoke(t, gatesmemory.Reviewer)
	if status, _ := follow(http.MethodGet, gatesmemory.Reviewer); status == http.StatusOK {
		t.Fatal("the link still opened for a reviewer whose binding was revoked")
	}
}

// TestBusinessLoopContract: the business-loop operations are published and
// generated with the aggregate, additive over the M2 contract, with the flow
// summary options and the outbound payload left out of the client.
func TestBusinessLoopContract(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ops := loadOperations(t, filepath.Join(root, "api", "openapi.yaml"))
	var got []string
	for _, op := range ops {
		if isBusinessLoopOperation(op.Method + " " + op.Path) {
			got = append(got, op.Method+" "+op.Path)
		}
	}
	slices.Sort(got)
	prefix := "/tenants/{tenantId}/projects/{projectId}"
	want := []string{
		"DELETE " + prefix + "/work-items/{id}/dependencies/{dependencyId}",
		"GET " + prefix + "/chatops-deliveries", "GET " + prefix + "/chatops-deliveries/{id}",
		"GET " + prefix + "/chatops-destinations", "GET " + prefix + "/chatops-destinations/{id}",
		"GET " + prefix + "/work-items/{id}/dependencies",
		"PATCH " + prefix + "/chatops-destinations/{id}",
		"POST " + prefix + "/chatops-deliveries/{id}/replay", "POST " + prefix + "/chatops-destinations",
		"POST " + prefix + "/work-items/{id}/dependencies",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("business-loop operations\n got %v\nwant %v", got, want)
	}
	// Every published operation is also routed by the server: the route inventory
	// of the modules is exactly what the aggregate publishes.
	var routed []string
	for _, r := range chatopshttp.Routes() {
		routed = append(routed, r.Method+" "+r.Path)
	}
	for _, r := range []struct{ m, p string }{
		{http.MethodGet, "/work-items/{id}/dependencies"}, {http.MethodPost, "/work-items/{id}/dependencies"},
		{http.MethodDelete, "/work-items/{id}/dependencies/{dependencyId}"},
	} {
		routed = append(routed, r.m+" "+prefix+r.p)
	}
	for i, r := range routed {
		if !strings.Contains(r, prefix) {
			routed[i] = strings.Replace(r, " ", " "+prefix, 1)
		}
	}
	slices.Sort(routed)
	if !slices.Equal(routed, want) {
		t.Fatalf("the modules route\n got %v\nwant %v", routed, want)
	}
	if strings.Contains(string(raw), "includeFlowSummary") || strings.Contains(string(raw), "WebhookPayload") {
		t.Error("the aggregate publishes options or payloads no public route serves")
	}
	// The public client is generated from the aggregate, byte for byte.
	assertClientReproducible(t)
	// Every payload the dependency package emits validates against the document.
	if _, err := dependencies.NewLink(devScope, "wi_a", "wi_a"); !errors.Is(err, dependencies.ErrGraph) {
		t.Errorf("a self-link was not a graph error: %v", err)
	}
}

// The disabled-capability record follows what the business-loop slice wires:
// ChatOps leaves the list when it is selected, the slice's own refusals join it,
// and a composition without the slice reports exactly what it did before.
func TestBusinessLoopDisabledCapabilities(t *testing.T) {
	slice := &ExecutionSlice{Disabled: []string{"runtime-secret-resolution", "gates", "agents", "delivery", "cleanup-runs", "git-webhooks", "chatops"}}
	none := DisabledCapabilitiesM3(slice, &DeliverySlice{}, M2Slices{}, nil)
	if !slices.Contains(none, "chatops") || slices.Contains(none, "flow-summary-list") {
		t.Fatalf("without the slice: %v", none)
	}
	unselected := DisabledCapabilitiesM3(slice, &DeliverySlice{}, M2Slices{}, &BusinessLoopSlice{Disabled: []string{capFlowList, capChatOps, capQA}})
	if !slices.Contains(unselected, "chatops") || !slices.Contains(unselected, "flow-summary-list") || !slices.Contains(unselected, "exploratory-qa") {
		t.Fatalf("ChatOps not selected: %v", unselected)
	}
	if n := len(slices.DeleteFunc(slices.Clone(unselected), func(s string) bool { return s != "chatops" })); n != 1 {
		t.Fatalf("chatops is listed %d times: %v", n, unselected)
	}
	wired := DisabledCapabilitiesM3(slice, &DeliverySlice{}, M2Slices{}, &BusinessLoopSlice{ChatOps: &chatops.Service{}, Disabled: []string{capFlowList, capChatCreds, capQA}})
	if slices.Contains(wired, "chatops") || !slices.Contains(wired, "chatops-authenticated-destinations") || !slices.Contains(wired, "flow-summary-list") {
		t.Fatalf("ChatOps selected: %v", wired)
	}
}

// The core configuration validates the business-loop statement with the rest of
// the startup settings and applies the environment's guards, so a bad statement
// stops startup instead of the first worker pass.
func TestBusinessLoopCoreConfig(t *testing.T) {
	cfg := validConfig()
	cfg.BusinessLoop = BusinessLoopConfig{
		QA:      &QAMappingConfig{MappingPrincipal: devMapper, ReportOutput: "qa-report", DeploymentInput: "deployment"},
		ChatOps: &ChatOpsConfig{Origin: devOrigin, Hosts: []string{"*.example.test"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a complete statement: %v", err)
	}
	bad := cfg
	bad.BusinessLoop.QA = &QAMappingConfig{MappingPrincipal: devMapper}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "QA mapping") {
		t.Fatalf("an incomplete QA mapping: %v", err)
	}
	bad = cfg
	bad.Tuning.ChatOps.Retry = chatops.RetryPolicy{}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "retry policy") {
		t.Fatalf("an unusable retry policy: %v", err)
	}
	// Without a ChatOps statement the worker tuning is not consulted at all.
	bad = cfg
	bad.BusinessLoop.ChatOps = nil
	bad.Tuning.ChatOps = ChatOpsTuning{}
	if err := bad.Validate(); err != nil {
		t.Fatalf("an unselected ChatOps must not need tuning: %v", err)
	}
}
