package chatopshttp_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	chatopshttp "github.com/optimiware/kodama/internal/integrations/chatops/http"
)

func strictDecode(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func ifMatch(v string) map[string]string { return map[string]string{"If-Match": v} }

// TestHTTPContract: OpenAPI examples, generated DTOs, ETags, cursor binding,
// replay reason and version requirements, and nondisclosing errors. No external
// service: the use-case port is a controlled double that proves the transport only.
func TestHTTPContract(t *testing.T) {
	sp := loadSpec(t)

	t.Run("the exported route inventory is exactly the documented operations", func(t *testing.T) {
		documented := map[string]bool{}
		for _, o := range sp.operations() {
			documented[o.method+" "+o.fullPath()] = true
		}
		inventory := map[string]bool{}
		actions := map[httpapi.Action]bool{}
		for _, r := range chatopshttp.Routes() {
			inventory[r.Method+" /api/v1"+r.Path] = true
			if actions[r.Action] {
				t.Errorf("action %s is shared by two routes", r.Action)
			}
			actions[r.Action] = true
		}
		for k := range documented {
			if !inventory[k] {
				t.Errorf("documented but not routed: %s", k)
			}
		}
		for k := range inventory {
			if !documented[k] {
				t.Errorf("routed but not documented: %s", k)
			}
		}
		// Every route has a rule, no rule is dangling and each opens at project level.
		rules := chatopshttp.Rules()
		for a := range actions {
			r, ok := rules[a]
			if !ok || len(r.Any) == 0 {
				t.Errorf("action %s has no rule", a)
			}
		}
		for a := range rules {
			if !actions[a] {
				t.Errorf("rule %s names no route", a)
			}
		}
		// The adapter-local mapping to existing permissions: no new role or permission.
		want := map[httpapi.Action]identity.Permission{
			chatopshttp.ActionDestinationList: identity.PermProjectSettings, chatopshttp.ActionDestinationCreate: identity.PermProjectSettings,
			chatopshttp.ActionDestinationRead: identity.PermProjectSettings, chatopshttp.ActionDestinationUpdate: identity.PermProjectSettings,
			chatopshttp.ActionDeliveryReplay: identity.PermProjectSettings,
			chatopshttp.ActionDeliveryList:   identity.PermProjectRead, chatopshttp.ActionDeliveryRead: identity.PermProjectRead,
		}
		for a, p := range want {
			if r := rules[a]; len(r.Any) != 1 || r.Any[0] != p {
				t.Errorf("%s opens with %v, want only %s", a, r.Any, p)
			}
		}
	})

	t.Run("every operation is authenticated by the explicit bearer scheme", func(t *testing.T) {
		ops := sp.operations()
		if len(ops) != 7 {
			t.Fatalf("operations = %d, want the seven management/read/replay operations", len(ops))
		}
		for _, o := range ops {
			sec := sp.effectiveSecurity(o)
			if len(sec) != 1 {
				t.Errorf("%s: effective security = %v, want exactly bearerAuth", o.id, sec)
				continue
			}
			scopes, ok := sec[0]["bearerAuth"].([]any)
			if len(sec[0]) != 1 || !ok || len(scopes) != 0 {
				t.Errorf("%s: effective security = %v, want exactly bearerAuth", o.id, sec)
			}
		}
		schemes := sp.raw["components"].(map[string]any)["securitySchemes"].(map[string]any)
		if _, ok := schemes["bearerAuth"]; !ok {
			t.Error("the bearerAuth scheme is not declared")
		}
	})

	t.Run("every documented operation has the documented error statuses", func(t *testing.T) {
		for _, o := range sp.operations() {
			for _, status := range []string{"401", "403", "404", "503"} {
				if _, ok := o.raw["responses"].(map[string]any)[status]; !ok && !(status == "403" && false) {
					t.Errorf("%s does not document %s", o.id, status)
				}
			}
		}
	})

	t.Run("the published examples validate and decode through the generated DTOs", func(t *testing.T) {
		for example, schema := range map[string]string{"DestinationCreate": "DestinationCreate", "Destination": "Destination", "DeliveryDetail": "DeliveryDetail",
			"ReplayRequest": "ReplayRequest", "WebhookPayload": "WebhookPayload"} {
			raw := sp.example(t, example)
			if err := sp.validate(t, schema, raw); err != nil {
				t.Errorf("example %s: %v", example, err)
			}
		}
		var dc chatopshttp.DestinationCreate
		var d chatopshttp.Destination
		var dd chatopshttp.DeliveryDetail
		var rr chatopshttp.ReplayRequest
		var wp chatopshttp.WebhookPayload
		for name, target := range map[string]any{"DestinationCreate": &dc, "Destination": &d, "DeliveryDetail": &dd, "ReplayRequest": &rr, "WebhookPayload": &wp} {
			if err := strictDecode(sp.example(t, name), target); err != nil {
				t.Errorf("example %s does not decode into its DTO: %v", name, err)
			}
		}
		if dd.State != "delivered" || len(dd.AttemptHistory) != 2 || wp.SchemaVersion != 1 {
			t.Errorf("decoded examples = %+v %+v", dd, wp)
		}
	})

	t.Run("the outbound payload fixtures and projections satisfy the documented schema", func(t *testing.T) {
		for _, name := range []string{"gate_requested", "deployment_state_changed"} {
			raw, err := os.ReadFile("../testdata/" + name + ".payload.json")
			if err != nil {
				t.Fatal(err)
			}
			if err := sp.validate(t, "WebhookPayload", raw); err != nil {
				t.Errorf("%s fixture: %v", name, err)
			}
		}
		links, _ := chatops.NewLinkBuilder("https://api.t041.example")
		for _, ev := range []string{"gate_requested", "deployment_state_changed"} {
			raw, _ := os.ReadFile("../testdata/" + ev + ".event.json")
			env, err := eventsv1.Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			p, err := chatops.Project(env, "cdl_x", links)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := p.Encode()
			if err := sp.validate(t, "WebhookPayload", b); err != nil {
				t.Errorf("%s projection: %v", ev, err)
			}
		}
	})

	svc := &fakeService{dest: destination(), view: detail(), dests: []chatops.Destination{destination()}}
	dels := make([]chatops.Delivery, 5)
	for i := range dels {
		dels[i] = failedDelivery()
		dels[i].ID = chatops.DeliveryID(fmt.Sprintf("cdl_t041_%d", 9-i))
		dels[i].CreatedAt = t0.Add(-time.Duration(i) * time.Minute)
	}
	svc.dels = dels
	srv := newServer(t, svc)
	base := projectPath(scopeA)

	validateError := func(t *testing.T, r response, status int, code string) {
		t.Helper()
		if r.Status != status {
			t.Fatalf("status = %d, want %d: %s", r.Status, status, r.Body)
		}
		var env struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(r.Body, &env); err != nil {
			t.Fatal(err)
		}
		if env.Error.Code != code {
			t.Fatalf("code = %q, want %q: %s", env.Error.Code, code, r.Body)
		}
		v, err := jsonUnmarshal(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := sp.errorEnvelope(t).Validate(v); err != nil {
			t.Fatalf("error body does not satisfy the shared envelope: %v: %s", err, r.Body)
		}
	}

	t.Run("destinations: create, read and list carry quoted-version ETags and validate against the schema", func(t *testing.T) {
		body := `{"name":"review-channel","endpoint":"https://hooks.t041.example/secret-path","events":["gate.requested","gate.decided"],"credential":"cred/ops"}`
		r := srv.do(t, http.MethodPost, base+"/chatops-destinations", "admin", nil, body)
		if r.Status != http.StatusCreated || r.Header.Get("ETag") != `"3"` || !strings.HasSuffix(r.Header.Get("Location"), "/chatops-destinations/cdst_t041_1") {
			t.Fatalf("create = %d etag=%q location=%q %s", r.Status, r.Header.Get("ETag"), r.Header.Get("Location"), r.Body)
		}
		if err := sp.validate(t, "Destination", r.Body); err != nil {
			t.Fatal(err)
		}
		// Defaults and input mapping: enabled defaults to true; the principal is the caller.
		if !svc.input.Enabled || svc.input.Credential != "cred/ops" || svc.caller.Actor.ID != "admin" || svc.caller.Actor.Type != chatops.ActorUser || svc.scope != scopeA {
			t.Fatalf("input=%+v caller=%+v scope=%v", svc.input, svc.caller, svc.scope)
		}
		r = srv.do(t, http.MethodPost, base+"/chatops-destinations", "admin", nil, `{"name":"x","endpoint":"https://hooks.t041.example/h","events":["gate.requested"],"enabled":false}`)
		if r.Status != http.StatusCreated || svc.input.Enabled {
			t.Fatalf("explicit enabled=false: %d %+v", r.Status, svc.input)
		}
		r = srv.do(t, http.MethodGet, base+"/chatops-destinations/cdst_t041_1", "admin", nil, "")
		if r.Status != http.StatusOK || r.Header.Get("ETag") != `"3"` {
			t.Fatalf("get = %d %q", r.Status, r.Header.Get("ETag"))
		}
		if err := sp.validate(t, "Destination", r.Body); err != nil {
			t.Fatal(err)
		}
		r = srv.do(t, http.MethodGet, base+"/chatops-destinations", "admin", nil, "")
		if r.Status != http.StatusOK {
			t.Fatalf("list = %d %s", r.Status, r.Body)
		}
		if err := sp.validate(t, "DestinationPage", r.Body); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("destination edits require a single strong If-Match and a non-empty strict body", func(t *testing.T) {
		const edit = `{"enabled":false}`
		path := base + "/chatops-destinations/cdst_t041_1"
		validateError(t, srv.do(t, http.MethodPatch, path, "admin", nil, edit), http.StatusPreconditionRequired, "version_required")
		for _, bad := range []string{`W/"3"`, `*`, `"3", "4"`, `3`, `"03"`, `"0"`} {
			if r := srv.do(t, http.MethodPatch, path, "admin", ifMatch(bad), edit); r.Status != http.StatusBadRequest {
				t.Errorf("If-Match %q = %d, want 400", bad, r.Status)
			}
		}
		for name, body := range map[string]string{"empty edit": `{}`, "unknown member": `{"enabled":true,"id":"cdst_other"}`, "null member": `{"credential":null}`,
			"duplicate member": `{"enabled":true,"enabled":false}`, "not an object": `[]`, "wrong type": `{"enabled":"yes"}`, "trailing data": `{"enabled":true}{}`,
			"unknown event type": `{"events":["usage.recorded"]}`, "empty events": `{"events":[]}`} {
			r := srv.do(t, http.MethodPatch, path, "admin", ifMatch(`"3"`), body)
			if r.Status != http.StatusBadRequest {
				t.Errorf("%s = %d %s, want 400", name, r.Status, r.Body)
				continue
			}
			validateError(t, r, http.StatusBadRequest, "invalid_request")
		}
		svc.err = nil
		r := srv.do(t, http.MethodPatch, path, "admin", ifMatch(`"3"`), `{"enabled":false,"credential":""}`)
		if r.Status != http.StatusOK || svc.expected != 3 || svc.edit.Enabled == nil || *svc.edit.Enabled || svc.edit.Credential == nil || *svc.edit.Credential != "" {
			t.Fatalf("edit = %d expected=%d edit=%+v", r.Status, svc.expected, svc.edit)
		}
		svc.err = chatops.Fail(chatops.ErrVersionConflict, nil, "the destination changed")
		validateError(t, srv.do(t, http.MethodPatch, path, "admin", ifMatch(`"2"`), edit), http.StatusPreconditionFailed, "version_conflict")
		svc.err = nil
	})

	t.Run("deliveries: sanitized status and attempts, never the endpoint, credential, payload or receiver text", func(t *testing.T) {
		r := srv.do(t, http.MethodGet, base+"/chatops-deliveries/cdl_t041_1", "viewer", nil, "")
		if r.Status != http.StatusOK || r.Header.Get("ETag") != `"9"` {
			t.Fatalf("get = %d %q %s", r.Status, r.Header.Get("ETag"), r.Body)
		}
		if err := sp.validate(t, "DeliveryDetail", r.Body); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"hooks.t041.example", "secret-path", "cred/ops", "payload must not be exposed", "api.t041.example", "diagnostic"} {
			if strings.Contains(string(r.Body), secret) {
				t.Fatalf("the response leaks %q: %s", secret, r.Body)
			}
		}
		var d chatopshttp.DeliveryDetail
		if err := strictDecode(r.Body, &d); err != nil {
			t.Fatal(err)
		}
		if d.State != "failed" || d.FailedReason == nil || *d.FailedReason != "exhausted" || d.LastFailure == nil || d.LastFailure.Class != "receiver_error" ||
			d.LastFailure.HttpStatus == nil || *d.LastFailure.HttpStatus != 503 || d.NextAttemptAt != nil || len(d.AttemptHistory) != 3 {
			t.Fatalf("detail = %+v", d)
		}
		if first := d.AttemptHistory[0]; first.Class == nil || *first.Class != "timeout" || !first.Uncertain || first.FinishedAt == nil {
			t.Fatalf("first attempt = %+v", first)
		}
		if last := d.AttemptHistory[2]; last.Result != "in_flight" || last.FinishedAt != nil {
			t.Fatalf("an unfinished attempt must be in_flight: %+v", last)
		}
		// A pending delivery shows when it will be retried.
		pending := failedDelivery()
		pending.State, pending.FailedReason, pending.NextAttemptAt = chatops.StatePending, "", t0.Add(time.Hour)
		svc.view.Delivery = pending
		r = srv.do(t, http.MethodGet, base+"/chatops-deliveries/cdl_t041_1", "viewer", nil, "")
		var p chatopshttp.DeliveryDetail
		if err := strictDecode(r.Body, &p); err != nil || p.NextAttemptAt == nil || !p.NextAttemptAt.Equal(t0.Add(time.Hour)) || p.FailedReason != nil {
			t.Fatalf("pending detail = %+v %v", p, err)
		}
		svc.view = detail()
	})

	t.Run("a legal attempt history beyond 1,000 entries is complete and valid", func(t *testing.T) {
		const n = 1500
		view := detail()
		view.Attempts = make([]chatops.Attempt, n)
		for i := range view.Attempts {
			at := t0.Add(time.Duration(i) * time.Minute)
			view.Attempts[i] = chatops.Attempt{Number: i + 1, ClaimToken: int64(i + 1), StartedAt: at, FinishedAt: at.Add(time.Second),
				Result: chatops.ResultRetry, Class: chatops.ClassTimeout, Uncertain: true}
		}
		view.Delivery.Attempts, view.Delivery.CycleAttempts = n, n
		svc.view = view
		defer func() { svc.view = detail() }()
		r := srv.do(t, http.MethodGet, base+"/chatops-deliveries/cdl_t041_1", "viewer", nil, "")
		if r.Status != http.StatusOK {
			t.Fatalf("get = %d %s", r.Status, r.Body)
		}
		if err := sp.validate(t, "DeliveryDetail", r.Body); err != nil {
			t.Fatalf("a %d-entry history is not valid: %v", n, err)
		}
		var d chatopshttp.DeliveryDetail
		if err := strictDecode(r.Body, &d); err != nil || len(d.AttemptHistory) != n || d.AttemptHistory[n-1].Number != n {
			t.Fatalf("history was truncated or reordered: %d %v", len(d.AttemptHistory), err)
		}
		// The schema bound is the persisted lifetime attempt limit, not smaller.
		var doc map[string]any
		if err := json.Unmarshal(sp.example(t, "DeliveryDetail"), &doc); err != nil {
			t.Fatal(err)
		}
		entry := doc["attemptHistory"].([]any)[0]
		for count, wantValid := range map[int]bool{100000: true, 100001: false} {
			hist := make([]any, count)
			for i := range hist {
				hist[i] = entry
			}
			doc["attemptHistory"] = hist
			raw, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := sp.validate(t, "DeliveryDetail", raw); (err == nil) != wantValid {
				t.Errorf("history of %d: valid=%v, want %v (%v)", count, err == nil, wantValid, err)
			}
		}
	})

	t.Run("replay needs If-Match, a reason and an authorized caller; the caller is the principal", func(t *testing.T) {
		path := base + "/chatops-deliveries/cdl_t041_1/replay"
		const ok = `{"reason":"receiver repaired"}`
		validateError(t, srv.do(t, http.MethodPost, path, "admin", nil, ok), http.StatusPreconditionRequired, "version_required")
		for name, body := range map[string]string{"no body": ``, "empty object": `{}`, "empty reason": `{"reason":""}`, "blank reason": `{"reason":"   "}`,
			"oversized reason": `{"reason":"` + strings.Repeat("r", chatops.MaxReasonBytes+1) + `"}`, "unknown member": `{"reason":"x","actor":"someone-else"}`,
			"null reason": `{"reason":null}`, "duplicate": `{"reason":"a","reason":"b"}`} {
			r := srv.do(t, http.MethodPost, path, "admin", ifMatch(`"9"`), body)
			if r.Status != http.StatusBadRequest && r.Status != http.StatusUnsupportedMediaType {
				t.Errorf("%s = %d %s, want a client error", name, r.Status, r.Body)
			}
		}
		r := srv.do(t, http.MethodPost, path, "admin", ifMatch(`"9"`), ok)
		if r.Status != http.StatusOK || svc.expected != 9 || svc.reason != "receiver repaired" || svc.caller.Actor.ID != "admin" || r.Header.Get("ETag") != `"9"` {
			t.Fatalf("replay = %d expected=%d reason=%q caller=%+v %s", r.Status, svc.expected, svc.reason, svc.caller, r.Body)
		}
		if err := sp.validate(t, "Delivery", r.Body); err != nil {
			t.Fatal(err)
		}
		// The viewer lacks project:settings at the router; the use case is never reached.
		n := len(svc.called())
		validateError(t, srv.do(t, http.MethodPost, path, "viewer", ifMatch(`"9"`), ok), http.StatusForbidden, "forbidden")
		validateError(t, srv.do(t, http.MethodPost, path, "mallory", ifMatch(`"9"`), ok), http.StatusNotFound, "not_found")
		validateError(t, srv.do(t, http.MethodPost, path, "", ifMatch(`"9"`), ok), http.StatusUnauthorized, "invalid_credential")
		if len(svc.called()) != n {
			t.Fatal("an unauthorized request reached the use case")
		}
		// Use-case refusals map to documented codes.
		for err, want := range map[error][2]any{
			chatops.Fail(chatops.ErrInvalidTransition, nil, "x"): {http.StatusUnprocessableEntity, "invalid_transition"},
			chatops.Fail(chatops.ErrVersionConflict, nil, "x"):   {http.StatusPreconditionFailed, "version_conflict"},
			chatops.Fail(chatops.ErrNotFound, nil, "x"):          {http.StatusNotFound, "not_found"},
			chatops.Fail(chatops.ErrDenied, nil, "x"):            {http.StatusForbidden, "forbidden"},
			chatops.Fail(chatops.ErrInvalid, nil, "x"):           {http.StatusBadRequest, "invalid_request"},
		} {
			svc.err = err
			validateError(t, srv.do(t, http.MethodPost, path, "admin", ifMatch(`"9"`), ok), want[0].(int), want[1].(string))
		}
		svc.err = nil
	})

	t.Run("module errors are classified and never disclose their cause", func(t *testing.T) {
		path := base + "/chatops-destinations"
		const body = `{"name":"a","endpoint":"https://hooks.t041.example/h","events":["gate.requested"]}`
		secret := "dial tcp 10.9.8.7:5432: password=hunter2 endpoint https://hooks.t041.example/secret-path cred/ops"
		for err, want := range map[error][2]any{
			chatops.Fail(chatops.ErrNameTaken, errors.New(secret), "x"):             {http.StatusConflict, "name_taken"},
			chatops.Fail(chatops.ErrConflict, errors.New(secret), "x"):              {http.StatusConflict, "conflict"},
			chatops.Fail(chatops.ErrLimit, errors.New(secret), "x"):                 {http.StatusUnprocessableEntity, "destination_limit"},
			chatops.Fail(chatops.ErrPolicy, errors.New(secret), "x"):                {http.StatusUnprocessableEntity, "destination_refused"},
			chatops.Fail(chatops.ErrCredentialUnavailable, errors.New(secret), "x"): {http.StatusUnprocessableEntity, "credential_unavailable"},
			chatops.Fail(chatops.ErrNotConfigured, errors.New(secret), "x"):         {http.StatusNotImplemented, "not_configured"},
			chatops.Fail(chatops.ErrUnavailable, errors.New(secret), "x"):           {http.StatusServiceUnavailable, "unavailable"},
			errors.New(secret): {http.StatusServiceUnavailable, "unavailable"},
		} {
			svc.err = err
			r := srv.do(t, http.MethodPost, path, "admin", nil, body)
			validateError(t, r, want[0].(int), want[1].(string))
			for _, leaked := range []string{"10.9.8.7", "hunter2", "secret-path", "cred/ops", "dial tcp"} {
				if strings.Contains(string(r.Body), leaked) {
					t.Errorf("%s: the error body leaks %q: %s", want[1], leaked, r.Body)
				}
			}
		}
		svc.err = nil
		// A hidden and a missing resource are the same answer.
		svc.err = chatops.Fail(chatops.ErrNotFound, errors.New("it exists in another tenant"), "not found")
		a := srv.do(t, http.MethodGet, base+"/chatops-deliveries/cdl_foreign", "viewer", nil, "")
		svc.err = chatops.Fail(chatops.ErrNotFound, nil, "not found")
		b := srv.do(t, http.MethodGet, base+"/chatops-deliveries/cdl_missing", "viewer", nil, "")
		strip := regexp.MustCompile(`req_[0-9a-f]{32}`)
		if a.Status != b.Status || strip.ReplaceAllString(string(a.Body), "") != strip.ReplaceAllString(string(b.Body), "") {
			t.Fatalf("hidden and missing differ:\n%s\n%s", a.Body, b.Body)
		}
		svc.err = nil
	})

	t.Run("cursors are bound to the request that issued them", func(t *testing.T) {
		list := base + "/chatops-deliveries"
		first := srv.do(t, http.MethodGet, list+"?limit=2", "admin", nil, "")
		var page chatopshttp.DeliveryPage
		if first.Status != http.StatusOK || strictDecode(first.Body, &page) != nil || len(page.Items) != 2 || page.NextCursor == nil {
			t.Fatalf("first page = %d %s", first.Status, first.Body)
		}
		if err := sp.validate(t, "DeliveryPage", first.Body); err != nil {
			t.Fatal(err)
		}
		cursor := *page.NextCursor
		// The cursor continues exactly where the page ended, newest first.
		second := srv.do(t, http.MethodGet, list+"?limit=2&cursor="+cursor, "admin", nil, "")
		var page2 chatopshttp.DeliveryPage
		if second.Status != http.StatusOK || strictDecode(second.Body, &page2) != nil || len(page2.Items) != 2 || page2.Items[0].Id == page.Items[1].Id {
			t.Fatalf("second page = %d %s", second.Status, second.Body)
		}
		if string(svc.after.ID) != string(page.Items[1].Id) {
			t.Fatalf("the position after = %v", svc.after)
		}
		other := "/tenants/t041_acme/projects/p041_billing"
		for _, path := range map[string]string{
			"another project":             other + "/chatops-deliveries?limit=2&cursor=" + cursor,
			"another tenant":              "/tenants/t041_globex/projects/p041_ops/chatops-deliveries?limit=2&cursor=" + cursor,
			"a filter was added":          list + "?limit=2&state=failed&cursor=" + cursor,
			"another endpoint":            base + "/chatops-destinations?limit=2&cursor=" + cursor,
			"a tampered token":            list + "?limit=2&cursor=" + cursor[:len(cursor)-2] + "AA",
			"a forged position":           list + "?limit=2&cursor=" + forge(t, srv, "cdl_x"),
			"a signed malformed position": list + "?limit=2&cursor=" + signedFor(t, srv, `{"t":"nope","id":"x y"}`),
			"not a cursor":                list + "?cursor=abc",
		} {
			r := srv.do(t, http.MethodGet, path, "admin", nil, "")
			validateError(t, r, http.StatusBadRequest, "invalid_cursor")
		}
		// Query validation: unknown parameters and out-of-range limits are rejected, never clamped.
		for _, q := range []string{"?limit=0", "?limit=201", "?limit=abc", "?limit=1&limit=2", "?bogus=1", "?state=a&state=b", "?state="} {
			if r := srv.do(t, http.MethodGet, list+q, "admin", nil, ""); r.Status != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400", q, r.Status)
			}
		}
		// Filters reach the use case as given.
		srv.do(t, http.MethodGet, list+"?state=failed&destinationId=cdst_t041_1", "admin", nil, "")
		if svc.filter.State != chatops.StateFailed || svc.filter.Destination != "cdst_t041_1" {
			t.Fatalf("filter = %+v", svc.filter)
		}
		// Destination cursors page the same way.
		svc.dests = nil
		for i := 0; i < 3; i++ {
			d := destination()
			d.ID = chatops.DestinationID(fmt.Sprintf("cdst_t041_%d", i+1))
			svc.dests = append(svc.dests, d)
		}
		r := srv.do(t, http.MethodGet, base+"/chatops-destinations?limit=2", "admin", nil, "")
		var dp chatopshttp.DestinationPage
		if strictDecode(r.Body, &dp) != nil || len(dp.Items) != 2 || dp.NextCursor == nil {
			t.Fatalf("destination page = %s", r.Body)
		}
		r = srv.do(t, http.MethodGet, base+"/chatops-destinations?limit=2&cursor="+*dp.NextCursor, "admin", nil, "")
		if strictDecode(r.Body, &dp) != nil || len(dp.Items) != 1 || dp.NextCursor != nil || svc.afterD != "cdst_t041_2" {
			t.Fatalf("second destination page = %s after=%s", r.Body, svc.afterD)
		}
		if !bytes.Contains(r.Body, []byte(`"nextCursor":null`)) {
			t.Fatalf("an exhausted list must carry a JSON null cursor: %s", r.Body)
		}
	})

	t.Run("there is no approval, capability-link or receiver route", func(t *testing.T) {
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, base + "/chatops-deliveries/cdl_t041_1/approve"},
			{http.MethodPost, base + "/gates/gate_1/decisions"},
			{http.MethodGet, base + "/chatops-deliveries/cdl_t041_1/link"},
			{http.MethodPost, base + "/chatops-webhook"},
			{http.MethodDelete, base + "/chatops-destinations/cdst_t041_1"},
			{http.MethodPut, base + "/chatops-destinations/cdst_t041_1"},
			{http.MethodPatch, base + "/chatops-deliveries/cdl_t041_1"},
		} {
			if r := srv.do(t, tc.method, tc.path, "admin", ifMatch(`"1"`), `{}`); r.Status != http.StatusNotFound && r.Status != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d", tc.method, tc.path, r.Status)
			}
		}
	})
}

func jsonUnmarshal(b []byte) (any, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return v, dec.Decode(&v)
}

// signedFor signs a position under the real scope of the deliveries listing, to
// prove a validly signed but malformed position is still refused.
func signedFor(t *testing.T, srv *server, position string) string {
	t.Helper()
	scope := httpapi.CursorScope{TenantID: scopeA.TenantID, ProjectID: scopeA.ProjectID, Endpoint: "GET /api/v1/tenants/{tenantId}/projects/{projectId}/chatops-deliveries",
		PathValues: []string{string(scopeA.TenantID), string(scopeA.ProjectID)}, Filters: map[string][]string{}, Sort: "created_at desc,id desc"}
	tok, err := srv.codec.Encode(scope, []byte(position))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// forge builds a validly signed token for another scope's position, to prove a
// well-formed but foreign cursor is refused.
func forge(t *testing.T, srv *server, id string) string {
	t.Helper()
	scope := httpapi.CursorScope{TenantID: "t041_other", ProjectID: "p041_other", Endpoint: "GET /x", Sort: "id"}
	tok, err := srv.codec.Encode(scope, []byte(`{"t":"2026-10-19T12:00:00Z","id":"`+id+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
