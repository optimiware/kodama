package eventsv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
)

const (
	testFingerprint = "sha256:abababababababababababababababababababababababababababababababab"
	testTenant      = "t_acme"
	testProject     = "p_inventory"
	testRun         = "run_01"
)

// rrEnvelope builds a run_resource.state_changed envelope as a generic tree so
// a test can corrupt any member; its resource ID is the contract's derived ID.
func rrEnvelope(t *testing.T, kind, key, from, to string, uidBound bool) map[string]any {
	t.Helper()
	id, err := RunResourceID(testTenant, testProject, testRun, key)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": "evt_rr_1", "schemaVersion": 1, "type": TypeRunResourceStateChanged,
		"occurredAt": "2026-10-19T10:05:00Z", "tenantId": testTenant, "projectId": testProject,
		"resource":      map[string]any{"type": "run_resource", "id": id, "version": 3},
		"actor":         map[string]any{"type": "service", "id": "svc_execution"},
		"correlationId": "req_01", "causationId": nil,
		"data": map[string]any{
			"runId": testRun, "kind": kind, "key": key, "from": from, "to": to,
			"reason": "service_ready", "specFingerprint": testFingerprint, "uidBound": uidBound,
		},
	}
}

func rrData(e map[string]any) map[string]any { return e["data"].(map[string]any) }

func marshalTree(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Test_unit_run_resource_event_contract is the T-119 acceptance: the
// run_resource.state_changed producer contract, validated by the typed Go
// contract and the JSON Schema together, with the frozen resource identity.
func Test_unit_run_resource_event_contract(t *testing.T) {
	schema := loadSchema(t)

	t.Run("resource ID is frozen: golden vectors, framing and domain validity", func(t *testing.T) {
		// Vectors are computed independently of the implementation (Python
		// hashlib over hand-assembled preimages).
		for _, g := range []struct{ tenant, project, run, key, want string }{
			{"t", "p", "r", "network", "rr-9596d9a3f9241178c6bc8f54b452b65f82e54faf"},
			{"t_acme", "p_inventory", "run_01", "network", "rr-b6db60ce0677ae6f334067ebe0214c4795279151"},
			{"t_acme", "p_inventory", "run_01", "service.db", "rr-4f2ffd415817e8593fc1f5f29f2e4c8e48ceaa32"},
		} {
			got, err := RunResourceID(domain.TenantID(g.tenant), domain.ProjectID(g.project), domain.RunID(g.run), g.key)
			if err != nil || got != g.want {
				t.Errorf("RunResourceID(%q,%q,%q,%q) = %q, %v; want %q", g.tenant, g.project, g.run, g.key, got, err, g.want)
			}
			if err := domain.RunID(got).Validate(); err != nil {
				t.Errorf("ID %q violates the ID domain: %v", got, err)
			}
			if len(got) != 43 {
				t.Errorf("ID %q has length %d, want 43", got, len(got))
			}
		}
		// The preimage is the 4-byte big-endian byte length then the bytes of
		// each of tenant, project, run and key, spelled out by hand.
		pre, _ := hex.DecodeString("000000017400000001700000000172000000076e6574776f726b")
		sum := sha256.Sum256(pre)
		if got, _ := RunResourceID("t", "p", "r", "network"); got != "rr-"+hex.EncodeToString(sum[:])[:40] {
			t.Errorf("framing differs from the documented preimage: %s", got)
		}
		// The raw RUN_ID/KEY and every unsafe input are refused, never hashed.
		for name, in := range map[string][4]string{
			"empty tenant":   {"", testProject, testRun, "network"},
			"slash in run":   {testTenant, testProject, "run/1", "network"},
			"slash in proj":  {testTenant, "p/1", testRun, "network"},
			"wildcard":       {"t_*", testProject, testRun, "network"},
			"unknown key":    {testTenant, testProject, testRun, "volume"},
			"service no nam": {testTenant, testProject, testRun, "service."},
			"upper service":  {testTenant, testProject, testRun, "service.DB"},
			"long run":       {testTenant, testProject, strings.Repeat("r", 129), "network"},
		} {
			if id, err := RunResourceID(domain.TenantID(in[0]), domain.ProjectID(in[1]), domain.RunID(in[2]), in[3]); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: accepted %q (%v)", name, id, err)
			}
		}
	})

	t.Run("separator concatenation aliases but the framed identity does not", func(t *testing.T) {
		join := func(parts ...string) string {
			s := sha256.Sum256([]byte(strings.Join(parts, "_")))
			return "rr-" + hex.EncodeToString(s[:])[:40]
		}
		// (t_a, p) and (t, a_p) are different valid scopes, yet an underscore
		// join cannot tell them apart.
		if join("t_a", "p", testRun, "network") != join("t", "a_p", testRun, "network") {
			t.Fatal("counterexample is not a collision under underscore framing")
		}
		a, err := RunResourceID("t_a", "p", testRun, "network")
		if err != nil {
			t.Fatal(err)
		}
		b, err := RunResourceID("t", "a_p", testRun, "network")
		if err != nil {
			t.Fatal(err)
		}
		if a == b || a != "rr-3663490fc4e1bf2c0f3faf91b34b72faf2ee1a3e" || b != "rr-13c688e63e9c74df3a118b68637202e3be2de8ec" {
			t.Fatalf("framed IDs alias or drifted: %s %s", a, b)
		}
		// Run and key of different resources never alias.
		c1, _ := RunResourceID(testTenant, testProject, "run_x", "service.db")
		c2, _ := RunResourceID(testTenant, testProject, "run_x_service", "network")
		if c1 == c2 {
			t.Fatal("run/key boundary aliases")
		}
	})

	t.Run("every kind and phase pair is accepted by Go and schema", func(t *testing.T) {
		keys := map[string][]string{
			RunResourceKindNetwork:     {"network"},
			RunResourceKindWorkspace:   {"workspace"},
			RunResourceKindPreparation: {"preparation"},
			RunResourceKindService:     {"service.db", "service.a", "service.web-1", "service.a" + strings.Repeat("b", 29)},
		}
		froms := append([]string{RunResourcePhaseNone}, runResourcePhase...)
		n := 0
		for kind, ks := range keys {
			for _, key := range ks {
				for _, from := range froms {
					for _, to := range runResourcePhase {
						for _, uid := range []bool{false, true} {
							raw := marshalTree(t, rrEnvelope(t, kind, key, from, to, uid))
							_, err := Decode(raw)
							want := from != to && (to != RunResourcePhaseReady || uid)
							if (err == nil) != want {
								t.Errorf("%s/%s %s->%s uidBound=%v: accepted=%v (%v), want %v", kind, key, from, to, uid, err == nil, err, want)
							}
							if got := schemaVerdict(t, schema, raw); got != want {
								t.Errorf("%s/%s %s->%s uidBound=%v: schema accepts=%v, want %v", kind, key, from, to, uid, got, want)
							}
							n++
						}
					}
				}
			}
		}
		// 7 from-phases x 6 to-phases x 2 uidBound values x 7 keys.
		if want := 7 * 6 * 2 * 7; n != want {
			t.Fatalf("checked %d combinations, want %d", n, want)
		}
	})

	t.Run("invalid members are rejected by Go and schema alike", func(t *testing.T) {
		type mutation struct {
			name   string
			change func(e map[string]any)
			// goOnly: the rule (derived resource ID) cannot be expressed in JSON
			// Schema, which then accepts.
			goOnly bool
		}
		longRef := strings.Repeat("r", 129)
		d := func(k string, v any) func(map[string]any) { return func(e map[string]any) { rrData(e)[k] = v } }
		drop := func(k string) func(map[string]any) { return func(e map[string]any) { delete(rrData(e), k) } }
		muts := []mutation{
			{"absent project", func(e map[string]any) { delete(e, "projectId") }, false},
			{"empty project", func(e map[string]any) { e["projectId"] = "" }, false},
			{"wildcard project", func(e map[string]any) { e["projectId"] = "p_*" }, false},
			{"foreign project", func(e map[string]any) { e["projectId"] = "p_other" }, true},
			{"foreign tenant", func(e map[string]any) { e["tenantId"] = "t_other" }, true},
			{"wrong resource type", func(e map[string]any) { e["resource"].(map[string]any)["type"] = "run" }, false},
			{"raw RUN_ID/KEY resource id", func(e map[string]any) { e["resource"].(map[string]any)["id"] = testRun + "/network" }, false},
			{"resource id of another key", func(e map[string]any) {
				id, _ := RunResourceID(testTenant, testProject, testRun, "workspace")
				e["resource"].(map[string]any)["id"] = id
			}, true},
			{"resource id of another run", d("runId", "run_02"), true},
			{"kind unknown", d("kind", "volume"), false},
			{"kind empty", d("kind", ""), false},
			{"kind null", d("kind", nil), false},
			{"kind missing", drop("kind"), false},
			{"kind/key mismatch network", func(e map[string]any) { rrData(e)["kind"], rrData(e)["key"] = "network", "workspace" }, false},
			{"kind workspace with service key", func(e map[string]any) { rrData(e)["kind"], rrData(e)["key"] = "workspace", "service.db" }, false},
			{"kind service with fixed key", func(e map[string]any) { rrData(e)["kind"], rrData(e)["key"] = "service", "network" }, false},
			{"key uppercase service", d("key", "service.DB"), false},
			{"key without service name", d("key", "service."), false},
			{"key service name with underscore", d("key", "service.a_b"), false},
			{"key service name too long", d("key", "service.a"+strings.Repeat("b", 30)), false},
			{"key with path", d("key", "service.db/../x"), false},
			{"key missing", drop("key"), false},
			{"runId malformed", d("runId", "run/01"), false},
			{"runId too long", d("runId", longRef), false},
			{"runId missing", drop("runId"), false},
			{"from unknown", d("from", "running"), false},
			{"from empty", d("from", ""), false},
			{"from missing", drop("from"), false},
			{"to none", d("to", "none"), false},
			{"to unknown", d("to", "gone"), false},
			{"to uppercase", d("to", "Ready"), false},
			{"to missing", drop("to"), false},
			{"same phase", d("from", "ready"), false},
			{"reason missing", drop("reason"), false},
			{"reason null", d("reason", nil), false},
			{"reason empty", d("reason", ""), false},
			{"reason prose", d("reason", "service did not become ready"), false},
			{"reason carries a secret", d("reason", "token=abc"), false},
			{"reason uppercase", d("reason", "Service_Lost"), false},
			{"reason leading digit", d("reason", "1_lost"), false},
			{"reason too long", d("reason", "a"+strings.Repeat("b", 64)), false},
			{"reason control character", d("reason", "lost\n"), false},
			{"fingerprint missing", drop("specFingerprint"), false},
			{"fingerprint null", d("specFingerprint", nil), false},
			{"fingerprint not sha256", d("specFingerprint", "md5:abababababababababababababababab"), false},
			{"fingerprint uppercase", d("specFingerprint", "sha256:"+strings.Repeat("AB", 32)), false},
			{"fingerprint short", d("specFingerprint", "sha256:abab"), false},
			{"fingerprint bare hex", d("specFingerprint", strings.Repeat("ab", 32)), false},
			{"uidBound missing", drop("uidBound"), false},
			{"uidBound null", d("uidBound", nil), false},
			{"uidBound string", d("uidBound", "true"), false},
			{"uidBound number", d("uidBound", 1), false},
			{"uidBound false when ready", d("uidBound", false), false},
			{"data is empty", func(e map[string]any) { e["data"] = map[string]any{} }, false},
		}
		for _, m := range muts {
			e := rrEnvelope(t, "service", "service.db", "pending", "ready", true)
			m.change(e)
			raw := marshalTree(t, e)
			if env, err := Decode(raw); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: accepted %+v (%v)", m.name, env.Resource, err)
			}
			if got := schemaVerdict(t, schema, raw); got != m.goOnly {
				t.Errorf("%s: schema accepts=%v, want %v", m.name, got, m.goOnly)
			}
		}
	})

	t.Run("a false uidBound is explicit presence, not an omitted default", func(t *testing.T) {
		ok := rrEnvelope(t, "workspace", "workspace", "none", "pending", false)
		env, err := Decode(marshalTree(t, ok))
		if err != nil {
			t.Fatal(err)
		}
		typed, err := DecodeData(env)
		if err != nil {
			t.Fatal(err)
		}
		if got := typed.(*RunResourceStateChanged); got.UIDBound {
			t.Fatalf("uidBound = true, want false")
		}
		delete(rrData(ok), "uidBound")
		if _, err := Decode(marshalTree(t, ok)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("omitted uidBound accepted: %v", err)
		}
		// Case aliases cannot stand in for the member.
		rrData(ok)["UIDBound"] = false
		if _, err := Decode(marshalTree(t, ok)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("aliased uidBound accepted: %v", err)
		}
	})

	t.Run("typed projection has exactly the eight safe members", func(t *testing.T) {
		want := []string{"from", "key", "kind", "reason", "runId", "specFingerprint", "to", "uidBound"}
		typ := reflect.TypeOf(RunResourceStateChanged{})
		var names []string
		for i := 0; i < typ.NumField(); i++ {
			names = append(names, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
			if strings.Contains(typ.Field(i).Tag.Get("json"), "omitempty") {
				t.Errorf("field %s is optional, every member is required", typ.Field(i).Name)
			}
		}
		sort.Strings(names)
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("payload members = %v, want %v", names, want)
		}
		// Unsafe additions arriving on the wire never reach the typed
		// projection a consumer reads; they are ignored additions.
		e := rrEnvelope(t, "service", "service.db", "pending", "ready", true)
		for k, v := range map[string]any{"providerUid": "uid-123", "endpoint": "http://10.0.0.1:5432", "logs": "password=x", "secret": "s3cr3t", "content": "scratch"} {
			rrData(e)[k] = v
		}
		env, err := Decode(marshalTree(t, e))
		if err != nil {
			t.Fatal(err)
		}
		typed, err := DecodeData(env)
		if err != nil {
			t.Fatal(err)
		}
		out := string(marshalTree(t, typed))
		for _, leak := range []string{"uid-123", "10.0.0.1", "password", "s3cr3t", "scratch"} {
			if strings.Contains(out, leak) {
				t.Errorf("typed projection leaks %q: %s", leak, out)
			}
		}
		var members map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &members); err != nil || len(members) != 8 {
			t.Fatalf("projection members = %d (%v): %s", len(members), err, out)
		}
		if got := *typed.(*RunResourceStateChanged); got != (RunResourceStateChanged{
			RunID: testRun, Kind: "service", Key: "service.db", From: "pending", To: "ready", Reason: "service_ready", SpecFingerprint: testFingerprint, UIDBound: true,
		}) {
			t.Fatalf("projection = %+v", got)
		}
	})

	t.Run("registration, round trip and digest stability", func(t *testing.T) {
		spec, ok := Spec(TypeRunResourceStateChanged)
		if !ok || spec.ResourceType != "run_resource" || !spec.ProjectRequired {
			t.Fatalf("spec = %+v, %v", spec, ok)
		}
		raw := marshalTree(t, rrEnvelope(t, "service", "service.db", "pending", "ready", true))
		env, err := Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		enc, err := Encode(env)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Decode(enc)
		if err != nil {
			t.Fatal(err)
		}
		d1, _ := Digest(env)
		d2, _ := Digest(again)
		if d1 != d2 {
			t.Fatal("digest changed across round trip")
		}
		// Encode refuses an envelope whose ID is not the derived one.
		env.Resource.ID = testRun + "_service.db"
		if _, err := Encode(env); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Encode accepted a non-derived resource ID: %v", err)
		}
	})

	t.Run("existing event types and fixtures are unchanged and still compatible", func(t *testing.T) {
		// Digest of every pre-existing example, pinned before this addition.
		pinned := map[string]string{
			"work_item.created":            "sha256:8e64e3b31366c99238250646e1705a4cc6632d20c635f62a82ed767307add081",
			"work_item.updated":            "sha256:9bf2c1b72d7c45807f1b116ee5f787f9d358fbed5148407846a7e7747928a849",
			"work_item.transitioned":       "sha256:cf1a361b63c9c28b8744065343d065fe4914f2d51dcbfc049a602a7948be42b0",
			"workspace.state_changed":      "sha256:ec527067ed81a2b6e2a09e7086a54cf81ed1633e57b2a0e5c1df7558d44494d4",
			"workspace.checkpoint_created": "sha256:0512a89c0be80c11a308e12b32757d7d09b221fc4c4f193aded0f20fc054d11c",
			"run.created":                  "sha256:94dfd34f7fbc366a29a7682dc9ec0b8fa87f26e7cbcab87ebb3fcfed7b7f9d8f",
			"run.state_changed":            "sha256:cbb8c3f6d0c04c1cf73ef4805d14488f09fce5c43816a57b947662260229cd5b",
			"step_attempt.state_changed":   "sha256:fd64c36825360a6e958d53214396f6d56e30cde244f2df12c8111d353435b6e4",
			"gate.requested":               "sha256:b929de3198f4fba929e91d62704bc05d8e377ce5a313c04732f792f3a3066a2c",
			"gate.decided":                 "sha256:6865cf81a8f9d1c33d68bbe01b6da496f676f60c408ab709bedc9ce84b6b30d0",
			"gate.invalidated":             "sha256:12e3e7e3a791a42377b39955357ee93cb4138cb9bb98f506eed9189bb410c913",
			"deployment.state_changed":     "sha256:c7e70c7edd87133b894c237fa0f257990c3978708cd99a09944f58bc0535a4d9",
			"membership.changed":           "sha256:87302a79b9ffb6a2c98c5b6fa920e0de5104b349e7288cd788df81cf1ab82477",
			"project.settings_changed":     "sha256:e6fc2ec64027e1d9c28984664c8a647636c3c256384cfb3a6ba4b3d5b6d2c670",
			"configuration.changed":        "sha256:2101ced6ccc8426b50444b31447ca84f6bf2e9bf6805e2dfe6d80fa2fdec7b89",
			"usage.recorded":               "sha256:15f8c37f145dedf6a168e8f5e4330358033e08330089f5f376b89acff7f93a5a",
		}
		data, err := os.ReadFile("fixtures/examples.json")
		if err != nil {
			t.Fatal(err)
		}
		var examples []json.RawMessage
		if err := json.Unmarshal(data, &examples); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, raw := range examples {
			env, err := Decode(raw)
			if err != nil {
				t.Fatalf("example: %v", err)
			}
			seen[env.Type] = true
			if !schemaVerdict(t, schema, raw) {
				t.Errorf("%s example rejected by schema", env.Type)
			}
			if want, ok := pinned[env.Type]; ok {
				if got, _ := Digest(env); got != want {
					t.Errorf("%s digest changed: %s", env.Type, got)
				}
			}
		}
		for typ := range pinned {
			if !seen[typ] {
				t.Errorf("no pre-existing example for %s", typ)
			}
		}
		if !seen[TypeRunResourceStateChanged] {
			t.Error("no run_resource.state_changed example")
		}
		// Every older type remains supported with its aggregate binding.
		for typ := range pinned {
			if _, ok := Spec(typ); !ok {
				t.Errorf("%s no longer supported", typ)
			}
		}
		// The schema still rejects a type outside the catalog and version 2.
		bad := rrEnvelope(t, "network", "network", "none", "pending", false)
		bad["type"] = "run_resource.unknown"
		if _, err := Decode(marshalTree(t, bad)); !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("unknown sibling type: %v", err)
		}
		if schemaVerdict(t, schema, marshalTree(t, bad)) {
			t.Error("schema accepts an unlisted sibling type")
		}
		bad = rrEnvelope(t, "network", "network", "none", "pending", false)
		bad["schemaVersion"] = 2
		if _, err := Decode(marshalTree(t, bad)); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("version 2: %v", err)
		}
	})

	t.Run("duplicate keys and oversize payload stay rejected", func(t *testing.T) {
		raw := string(marshalTree(t, rrEnvelope(t, "network", "network", "none", "pending", false)))
		dup := strings.Replace(raw, `"uidBound":false`, `"uidBound":false,"uidBound":true`, 1)
		if _, err := Decode([]byte(dup)); !errors.Is(err, ErrMalformed) {
			t.Errorf("duplicate uidBound: %v", err)
		}
		big := strings.Replace(raw, `"uidBound":false`, fmt.Sprintf(`"uidBound":false,"note":%q`, strings.Repeat("x", MaxDataBytes)), 1)
		if _, err := Decode(bytes.NewBufferString(big).Bytes()); !errors.Is(err, ErrTooLarge) {
			t.Errorf("oversize: %v", err)
		}
	})
}
