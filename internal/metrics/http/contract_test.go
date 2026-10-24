package metricshttp_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/metrics"
	metricshttp "github.com/optimiware/kodama/internal/metrics/http"
)

const (
	summariesYAML = "../../../api/work-summaries.yaml"
	workYAML      = "../../../api/work.yaml"
)

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

type spec struct {
	raw map[string]any
	c   *jsonschema.Compiler
}

func loadYAML(t testing.TB, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// translate rewrites OpenAPI 3.0 `nullable: true` into JSON Schema (type unions,
// and allOf-wrapped references into anyOf with null).
func translate(n any) {
	switch v := n.(type) {
	case map[string]any:
		if nb, _ := v["nullable"].(bool); nb {
			if ty, ok := v["type"].(string); ok {
				v["type"] = []any{ty, "null"}
			} else if all, ok := v["allOf"].([]any); ok {
				delete(v, "allOf")
				v["anyOf"] = []any{map[string]any{"allOf": all}, map[string]any{"type": "null"}}
			}
		}
		delete(v, "nullable")
		for _, c := range v {
			translate(c)
		}
	case []any:
		for _, c := range v {
			translate(c)
		}
	}
}

func loadSpec(t testing.TB) *spec {
	t.Helper()
	raw := loadYAML(t, summariesYAML)
	orig := loadYAML(t, summariesYAML) // untranslated, for structure checks
	translate(raw)
	js, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("work-summaries.yaml", doc); err != nil {
		t.Fatal(err)
	}
	return &spec{raw: orig, c: c}
}

func (s *spec) validate(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile("work-summaries.yaml#/components/schemas/" + schema)
	if err != nil {
		t.Fatalf("schema %s: %v", schema, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, doc)
	}
	return sch.Validate(v)
}

func (s *spec) section(name string) map[string]any {
	return s.raw["components"].(map[string]any)[name].(map[string]any)
}

func strictDecode(doc []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Test_work_summary_contract: the fragment declares no route and only a
// contribution to the existing work list; its examples decode into the generated
// types and validate; real summaries map to valid documents; units, nullable
// members and open bounds are as documented; the option set is validated and
// bound canonically. No external services.
func Test_work_summary_contract(t *testing.T) {
	sp := loadSpec(t)

	t.Run("no_route_and_no_resource", func(t *testing.T) {
		if paths, _ := sp.raw["paths"].(map[string]any); len(paths) != 0 {
			t.Fatalf("the fragment registers routes: %v", paths)
		}
		for name := range sp.section("schemas") {
			if strings.HasSuffix(name, "Page") || strings.Contains(name, "Resource") {
				t.Errorf("schema %s looks like a resource or a page", name)
			}
		}
		// The existing list contract is neither edited nor required to know about us.
		work, err := os.ReadFile(workYAML)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(work), "flowSummary") || strings.Contains(string(work), "includeFlowSummary") {
			t.Error("api/work.yaml was edited by T-042")
		}
	})

	t.Run("parameters_are_optional_query_options", func(t *testing.T) {
		params := sp.section("parameters")
		want := map[string]string{"IncludeFlowSummary": "includeFlowSummary", "FlowFrom": "flowFrom", "FlowTo": "flowTo"}
		if len(params) != len(want) {
			t.Fatalf("parameters = %v", params)
		}
		for key, name := range want {
			p := params[key].(map[string]any)
			if p["name"] != name || p["in"] != "query" || p["required"] != false {
				t.Errorf("%s = %v", key, p)
			}
		}
		inc := params["IncludeFlowSummary"].(map[string]any)["schema"].(map[string]any)
		if inc["type"] != "boolean" || inc["default"] != false {
			t.Errorf("includeFlowSummary schema = %v", inc)
		}
		for _, k := range []string{"FlowFrom", "FlowTo"} {
			s := params[k].(map[string]any)["schema"].(map[string]any)
			if s["type"] != "string" || s["format"] != "date-time" {
				t.Errorf("%s schema = %v", k, s)
			}
		}
	})

	t.Run("the_contribution_adds_one_optional_member_only", func(t *testing.T) {
		c := sp.section("schemas")["FlowSummaryContribution"].(map[string]any)
		props := c["properties"].(map[string]any)
		if len(props) != 1 || props["flowSummary"] == nil || c["required"] != nil {
			t.Fatalf("contribution = %v", c)
		}
		for _, doc := range []string{`{}`, `{"flowSummary": ` + mustJSON(t, metricshttp.FromSummary(okSummary())) + `}`} {
			if err := sp.validate(t, "FlowSummaryContribution", []byte(doc)); err != nil {
				t.Errorf("%s: %v", doc, err)
			}
		}
		// Page shape words never appear: pagination stays the list's.
		for _, forbidden := range []string{"items", "nextCursor", "cursor", "limit"} {
			if _, ok := props[forbidden]; ok {
				t.Errorf("contribution declares %s", forbidden)
			}
		}
	})

	t.Run("examples_decode_into_generated_types_and_validate", func(t *testing.T) {
		examples := sp.section("examples")
		var names []string
		for n := range examples {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) < 4 {
			t.Fatalf("examples = %v", names)
		}
		statuses := map[string]bool{}
		for _, n := range names {
			body, _ := json.Marshal(examples[n].(map[string]any)["value"])
			var dto metricshttp.FlowSummary
			if err := strictDecode(body, &dto); err != nil {
				t.Errorf("%s: %v", n, err)
				continue
			}
			if err := sp.validate(t, "FlowSummary", body); err != nil {
				t.Errorf("%s: %v", n, err)
			}
			again, _ := json.Marshal(dto)
			if !jsonEqual(t, body, again) {
				t.Errorf("%s does not round-trip through the generated type:\n%s\n%s", n, body, again)
			}
			statuses[string(dto.Status)] = true
		}
		for _, s := range []string{"ok", "not_started", "invalid_history"} {
			if !statuses[s] {
				t.Errorf("no example with status %s", s)
			}
		}
	})

	t.Run("mapping_preserves_units_nullable_members_and_open_bounds", func(t *testing.T) {
		got := metricshttp.FromSummary(okSummary())
		if got.Durations.LeadMicros != 90*60*1_000_000 || got.Durations.ActiveMicros != 30*60*1_000_000 ||
			got.Durations.BlockedMicros != 1_000_001 || got.Durations.CompletedLeadMicros != 7 {
			t.Fatalf("durations = %+v", got.Durations)
		}
		if got.Counts.Reopens != 2 || got.Counts.BlockedIntervals != 3 || got.Counts.CompletedEpisodes != 1 || got.Counts.CancelledEpisodes != 4 {
			t.Fatalf("counts = %+v", got.Counts)
		}
		if got.Anomaly != nil || got.Censoring.StateAtEnd == nil || *got.Censoring.StateAtEnd != metricshttp.FlowStateActive ||
			!got.Censoring.OpenAtEnd || !got.Censoring.InFlightAtStart || got.ObservedVersion != 9 {
			t.Fatalf("censoring = %+v anomaly %v", got.Censoring, got.Anomaly)
		}
		if !got.Window.From.Equal(okSummary().Window.From) || got.Window.To.Location() != time.UTC {
			t.Fatalf("window = %+v", got.Window)
		}
		check := func(name string, s metrics.WorkSummary, wantAnomaly string, wantState bool) {
			t.Helper()
			dto := metricshttp.FromSummary(s)
			body := mustJSON(t, dto)
			if err := sp.validate(t, "FlowSummary", []byte(body)); err != nil {
				t.Errorf("%s: %v\n%s", name, err, body)
			}
			var m map[string]any
			_ = json.Unmarshal([]byte(body), &m)
			if (m["anomaly"] != nil) != (wantAnomaly != "") || (m["censoring"].(map[string]any)["stateAtEnd"] != nil) != wantState {
				t.Errorf("%s: nullable members wrong: %s", name, body)
			}
			if _, present := m["anomaly"]; !present {
				t.Errorf("%s: anomaly must be present as null, not omitted", name)
			}
		}
		check("ok", okSummary(), "", true)
		w := okSummary().Window
		check("not started", metrics.WorkSummary{Scope: okSummary().Scope, ItemID: "wi_1", Window: w, Status: metrics.StatusNotStarted}, "", false)
		check("invalid", metrics.WorkSummary{Scope: okSummary().Scope, ItemID: "wi_1", Window: w, Status: metrics.StatusInvalidHistory, Anomaly: "version_gap"}, "version_gap", false)
		for _, s := range []metrics.WorkSummary{okSummary()} {
			if err := s.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("contributions_are_keyed_by_item", func(t *testing.T) {
		a, b := okSummary(), okSummary()
		b.ItemID = "wi_2"
		got := metricshttp.Contributions([]metrics.WorkSummary{a, b})
		if len(got) != 2 || got["wi_1"].FlowSummary == nil || got["wi_2"].FlowSummary == nil || got["wi_3"].FlowSummary != nil {
			t.Fatalf("contributions = %+v", got)
		}
	})

	t.Run("options", func(t *testing.T) {
		yes, no := true, false
		from, to := now.Add(-time.Hour), now
		ok, err := metricshttp.ParseOptions(&yes, &from, &to, now)
		if err != nil || !ok.Include || !ok.Window.From.Equal(from) {
			t.Fatalf("valid options: %+v %v", ok, err)
		}
		for name, c := range map[string]struct {
			inc      *bool
			from, to *time.Time
		}{
			"summary without a window":   {&yes, nil, nil},
			"summary without an end":     {&yes, &from, nil},
			"summary without a start":    {&yes, nil, &to},
			"window without the option":  {nil, &from, &to},
			"window with the option off": {&no, &from, &to},
			"reversed window":            {&yes, &to, &from},
			"an end in the future":       {&yes, &from, ptr(now.Add(time.Second))},
		} {
			if _, err := metricshttp.ParseOptions(c.inc, c.from, c.to, now); !errors.Is(err, metrics.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// Off is the default and carries no window.
		for _, inc := range []*bool{nil, &no} {
			off, err := metricshttp.ParseOptions(inc, nil, nil, now)
			if err != nil || off.Include || off.Binding() != "flow=off" {
				t.Errorf("off: %+v %v", off, err)
			}
		}
		// The cursor binding distinguishes windows and the off state, and ignores
		// the zone the client used.
		other, _ := metricshttp.ParseOptions(&yes, &from, ptr(now.Add(-time.Microsecond)), now)
		est := time.FixedZone("est", -5*3600)
		sameInEST, _ := metricshttp.ParseOptions(&yes, ptr(from.In(est)), ptr(to.In(est)), now)
		if ok.Binding() == other.Binding() || ok.Binding() == "flow=off" || ok.Binding() != sameInEST.Binding() {
			t.Errorf("bindings: %q %q %q", ok.Binding(), other.Binding(), sameInEST.Binding())
		}
	})

	t.Run("the_generated_types_match_the_schema_fields", func(t *testing.T) {
		schemas := sp.section("schemas")
		for name, typ := range map[string]reflect.Type{
			"FlowSummary":   reflect.TypeOf(metricshttp.FlowSummary{}),
			"FlowDurations": reflect.TypeOf(metricshttp.FlowDurations{}),
			"FlowCounts":    reflect.TypeOf(metricshttp.FlowCounts{}),
			"FlowCensoring": reflect.TypeOf(metricshttp.FlowCensoring{}),
			"FlowWindow":    reflect.TypeOf(metricshttp.FlowWindow{}),
		} {
			props := schemas[name].(map[string]any)["properties"].(map[string]any)
			required := schemas[name].(map[string]any)["required"].([]any)
			if len(props) != typ.NumField() || len(required) != len(props) {
				t.Errorf("%s: %d properties, %d required, %d fields", name, len(props), len(required), typ.NumField())
			}
			for i := 0; i < typ.NumField(); i++ {
				tag, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
				if _, ok := props[tag]; !ok {
					t.Errorf("%s.%s is not a schema property", name, tag)
				}
			}
		}
	})
}

func ptr[T any](v T) *T { return &v }

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jsonEqual(t testing.TB, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

func okSummary() metrics.WorkSummary {
	return metrics.WorkSummary{
		Scope:  domain.Scope{TenantID: "t_a", ProjectID: "p_a"},
		ItemID: "wi_1",
		Window: metrics.Window{From: now.Add(-3 * time.Hour), To: now}, Status: metrics.StatusOK, ObservedVersion: 9,
		Lead: 90 * time.Minute, Active: 30 * time.Minute, Review: 10 * time.Minute, Blocked: time.Second + time.Microsecond,
		Reopened: time.Minute, CompletedLead: 7 * time.Microsecond,
		ReopenCount: 2, BlockedIntervals: 3, CompletedEpisodes: 1, CancelledEpisodes: 4,
		InFlightAtStart: true, OpenAtEnd: true, StateAtEnd: "active",
	}
}
