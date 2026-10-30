package eventsv1

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaURL = "https://kodama.invalid/contracts/events/v1/schema.json"

type fixtureCase struct {
	Name        string         `json:"name"`
	Valid       bool           `json:"valid"`
	SchemaValid *bool          `json:"schemaValid"`
	Error       string         `json:"error"`
	Envelope    map[string]any `json:"envelope"`
	Raw         *string        `json:"raw"`
}

func (c fixtureCase) bytes(t *testing.T) []byte {
	t.Helper()
	if c.Raw != nil {
		return []byte(*c.Raw)
	}
	out, err := json.Marshal(c.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func loadSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	data, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, instance); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return s
}

func schemaVerdict(t *testing.T, s *jsonschema.Schema, raw []byte) bool {
	t.Helper()
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("instance: %v", err)
	}
	return s.Validate(instance) == nil
}

func errorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnsupportedVersion):
		return "unsupported_version"
	case errors.Is(err, ErrUnsupportedType):
		return "unsupported_type"
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case errors.Is(err, ErrMalformed):
		return "malformed"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	}
	return "?"
}

// TestEventContractFixtures: all catalog types agree between Go validation and
// the JSON Schema; optional additions stay compatible; invalid versions,
// payloads, actors, scopes, duplicate keys and oversize input fail.
func TestEventContractFixtures(t *testing.T) {
	schema := loadSchema(t)
	var cases []fixtureCase
	data, err := os.ReadFile("fixtures/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			raw := c.bytes(t)
			env, err := Decode(raw)
			if c.Valid {
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				covered[env.Type] = true
				// A valid envelope round-trips to a stable canonical form.
				enc, err := Encode(env)
				if err != nil {
					t.Fatalf("Encode: %v", err)
				}
				again, err := Decode(enc)
				if err != nil {
					t.Fatalf("round trip: %v", err)
				}
				d1, _ := Digest(env)
				d2, _ := Digest(again)
				if d1 != d2 {
					t.Fatalf("digest changed across round trip")
				}
				if _, err := DecodeData(env); err != nil {
					t.Fatalf("DecodeData: %v", err)
				}
			} else if got := errorKind(err); got != c.Error {
				t.Fatalf("error kind = %q (%v), want %q", got, err, c.Error)
			}
			if c.SchemaValid != nil {
				if got := schemaVerdict(t, schema, raw); got != *c.SchemaValid {
					t.Fatalf("schema verdict = %v, want %v", got, *c.SchemaValid)
				}
			}
			// A document the Go contract accepts must also satisfy the schema.
			if err == nil && !schemaVerdict(t, schema, raw) {
				t.Fatalf("Go accepts a payload the schema rejects")
			}
		})
	}
	for _, typ := range Types() {
		if !covered[typ] {
			t.Errorf("no valid fixture for %s", typ)
		}
	}
	if len(Types()) != 20 {
		t.Errorf("catalog and Git service events list 20 types, contract has %d", len(Types()))
	}
}

func TestSchemaPatternsMatchGo(t *testing.T) {
	data, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for name, pattern := range Patterns {
		if doc.Defs[name].Pattern != pattern {
			t.Errorf("pattern %s differs: schema %q, Go %q", name, doc.Defs[name].Pattern, pattern)
		}
	}
}

func TestSizeAndDepthLimits(t *testing.T) {
	base := `{"id":"evt_1","schemaVersion":1,"type":"work_item.created","occurredAt":"2026-10-19T10:05:00Z","tenantId":"t_acme","projectId":"p_1","resource":{"type":"work_item","id":"wi_1","version":1},"actor":{"type":"user","id":"u"},"correlationId":"r","causationId":null,"data":{"kind":"story","state":"draft"%s}}`
	if _, err := Decode([]byte(strings.Replace(base, "%s", "", 1))); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	big := `,"note":"` + strings.Repeat("x", MaxEnvelopeBytes) + `"`
	if _, err := Decode([]byte(strings.Replace(base, "%s", big, 1))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized envelope: %v", err)
	}
	bigData := `,"note":"` + strings.Repeat("x", MaxDataBytes) + `"`
	if _, err := Decode([]byte(strings.Replace(base, "%s", bigData, 1))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized data: %v", err)
	}
}

func TestCanonicalDigestIgnoresMemberOrder(t *testing.T) {
	a := `{"id":"evt_1","schemaVersion":1,"type":"work_item.created","occurredAt":"2026-10-19T10:05:00Z","tenantId":"t_acme","projectId":"p_1","resource":{"type":"work_item","id":"wi_1","version":1},"actor":{"type":"user","id":"u"},"correlationId":"r","causationId":null,"data":{"kind":"story","state":"draft"}}`
	b := `{"data":{"state":"draft","kind":"story"},"causationId":null,"correlationId":"r","actor":{"id":"u","type":"user"},"resource":{"version":1,"id":"wi_1","type":"work_item"},"projectId":"p_1","tenantId":"t_acme","occurredAt":"2026-10-19T10:05:00Z","type":"work_item.created","schemaVersion":1,"id":"evt_1"}`
	ea, err := Decode([]byte(a))
	if err != nil {
		t.Fatal(err)
	}
	eb, err := Decode([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	da, _ := Digest(ea)
	db, _ := Digest(eb)
	if da != db {
		t.Fatalf("digests differ: %s %s", da, db)
	}
	eb.Resource.Version = 2
	dc, _ := Digest(eb)
	if dc == da {
		t.Fatal("changed content produced the same digest")
	}
}

// TestExactMemberNames: members are matched to fields by their exact wire
// names. A differently-cased member is an ignorable addition, so it can neither
// override the real member (in either order) nor stand in for a missing
// required one, in the envelope, its nested objects and the typed payloads.
func TestExactMemberNames(t *testing.T) {
	obj := func(real, alias string, before bool) string {
		switch {
		case alias == "":
			return "{" + real + "}"
		case before:
			return "{" + alias + "," + real + "}"
		}
		return "{" + real + "," + alias + "}"
	}
	build := func(before bool, top, resource, actor, data string) string {
		return obj(`"id":"evt_a","schemaVersion":1,"type":"work_item.transitioned","occurredAt":"2026-10-19T10:05:00Z","tenantId":"t_acme","projectId":"p_1",`+
			`"resource":`+obj(`"type":"work_item","id":"wi_1","version":1`, resource, before)+`,"actor":`+obj(`"type":"user","id":"u"`, actor, before)+
			`,"correlationId":"r","causationId":null,"data":`+obj(`"from":"ready","to":"active","transitionId":"tr_1"`, data, before), top, before)
	}
	clean := build(false, "", "", "", "")

	t.Run("aliases never override the real members", func(t *testing.T) {
		for _, before := range []bool{false, true} {
			raw := build(before,
				`"ID":"evt_b","TenantID":"t_evil","ProjectId":"p_evil","TYPE":"run.created","CausationID":"evt_x","SchemaVersion":2`,
				`"ID":"wi_evil","Version":9,"Type":"run"`, `"Type":"service","ID":"evil"`,
				`"From":"closed","TO":"closed","TransitionId":"tr_evil"`)
			env, err := Decode([]byte(raw))
			if err != nil {
				t.Fatalf("before=%v: %v", before, err)
			}
			if env.ID != "evt_a" || env.TenantID != "t_acme" || env.ProjectID != "p_1" || env.Type != TypeWorkItemTransitioned || env.CausationID != nil || env.SchemaVersion != SchemaVersion {
				t.Fatalf("before=%v: envelope members overridden: %+v", before, env)
			}
			if env.Resource != (Resource{Type: "work_item", ID: "wi_1", Version: 1}) || env.Actor != (Actor{Type: ActorUser, ID: "u"}) {
				t.Fatalf("before=%v: nested members overridden: %+v %+v", before, env.Resource, env.Actor)
			}
			typed, err := DecodeData(env)
			if err != nil {
				t.Fatal(err)
			}
			if got := *typed.(*WorkItemTransitioned); got != (WorkItemTransitioned{From: "ready", To: "active", TransitionID: "tr_1"}) {
				t.Fatalf("before=%v: payload overridden: %+v", before, got)
			}
		}
	})

	t.Run("aliases never stand in for required members", func(t *testing.T) {
		for name, raw := range map[string]string{
			"id":            strings.Replace(clean, `"id":"evt_a"`, `"ID":"evt_a"`, 1),
			"tenantId":      strings.Replace(clean, `"tenantId"`, `"TenantId"`, 1),
			"resource.id":   strings.Replace(clean, `"id":"wi_1"`, `"ID":"wi_1"`, 1),
			"actor.type":    strings.Replace(clean, `"type":"user"`, `"Type":"user"`, 1),
			"data.to":       strings.Replace(clean, `"to":"active"`, `"To":"active"`, 1),
			"data.from":     strings.Replace(clean, `"from":"ready"`, `"FROM":"ready"`, 1),
			"correlationId": strings.Replace(clean, `"correlationId"`, `"CorrelationID"`, 1),
		} {
			if env, err := Decode([]byte(raw)); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: wrong-case member accepted: %+v %v", name, env, err)
			}
		}
	})
}

// TestMemberPresenceAgreesWithSchema mutates every payload member of every
// example (explicitly null, false, empty, or removed) and requires Go and the
// schema to reach the same verdict, so no optional-but-non-null or mutually
// exclusive member rule can drift between the two executable contracts.
func TestMemberPresenceAgreesWithSchema(t *testing.T) {
	schema := loadSchema(t)
	data, err := os.ReadFile("fixtures/examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var examples []map[string]any
	if err := json.Unmarshal(data, &examples); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(d map[string]any, k string){
		"null":      func(d map[string]any, k string) { d[k] = nil },
		"false":     func(d map[string]any, k string) { d[k] = false },
		"removed":   func(d map[string]any, k string) { delete(d, k) },
		"empty":     func(d map[string]any, k string) { d[k] = "" },
		"emptyList": func(d map[string]any, k string) { d[k] = []any{} },
		"both checkpoint origins": func(d map[string]any, k string) {
			d["publishingAttemptId"], d["humanSynchronization"] = "att_9", true
		},
	}
	checked := 0
	for _, ex := range examples {
		typ := ex["type"].(string)
		members := ex["data"].(map[string]any)
		for key := range members {
			for name, mutate := range mutations {
				clone := map[string]any{}
				for k, v := range ex {
					clone[k] = v
				}
				d := map[string]any{}
				for k, v := range members {
					d[k] = v
				}
				mutate(d, key)
				clone["data"] = d
				raw, err := json.Marshal(clone)
				if err != nil {
					t.Fatal(err)
				}
				_, derr := Decode(raw)
				if got, want := derr == nil, schemaVerdict(t, schema, raw); got != want {
					t.Errorf("%s with %s %s: Go accepts=%v (%v), schema accepts=%v", typ, key, name, got, derr, want)
				}
				checked++
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d mutations checked", checked)
	}
}
