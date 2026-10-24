package runtime

import (
	"testing"
	"time"
)

type enum string

func TestStyleParam(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, tc := range map[string]struct {
		style   string
		explode bool
		loc     ParamLocation
		value   any
		want    string
	}{
		"path string":         {"simple", false, ParamLocationPath, "p inv/x", "p%20inv%2Fx"},
		"path enum":           {"simple", false, ParamLocationPath, enum("a"), "a"},
		"header":              {"simple", false, ParamLocationHeader, `"7"`, `"7"`},
		"query scalar":        {"form", true, ParamLocationQuery, "a b", "q=a+b"},
		"query integer":       {"form", true, ParamLocationQuery, 25, "q=25"},
		"query bool":          {"form", true, ParamLocationQuery, true, "q=true"},
		"query exploded list": {"form", true, ParamLocationQuery, []enum{"x", "y"}, "q=x&q=y"},
		"query joined list":   {"form", false, ParamLocationQuery, []string{"x", "y"}, "q=x,y"},
		"query escaped":       {"form", true, ParamLocationQuery, "a/b+c=", "q=a%2Fb%2Bc%3D"},
		"time":                {"form", true, ParamLocationQuery, ts, "q=2026-01-02T03%3A04%3A05Z"},
		"pointer":             {"form", true, ParamLocationQuery, func() *int { v := 7; return &v }(), "q=7"},
	} {
		got, err := StyleParamWithLocation(tc.style, tc.explode, "q", tc.loc, tc.value)
		if err != nil || got != tc.want {
			t.Errorf("%s: %q %v, want %q", name, got, err, tc.want)
		}
	}
	for name, v := range map[string]any{"nil pointer": (*int)(nil), "object": map[string]string{"a": "b"}, "bytes": []byte("x"), "untyped nil": nil} {
		if _, err := StyleParamWithLocation("form", true, "q", ParamLocationQuery, v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := StyleParamWithLocation("matrix", false, "q", ParamLocationPath, "x"); err == nil {
		t.Error("an unsupported style was accepted")
	}
}

func TestJSONMerge(t *testing.T) {
	got, err := JSONMerge([]byte(`{"a":1,"b":{"c":2,"d":3},"e":4}`), []byte(`{"b":{"c":9,"d":null},"f":5}`))
	if err != nil || string(got) != `{"a":1,"b":{"c":9},"e":4,"f":5}` {
		t.Fatalf("%s %v", got, err)
	}
	if got, err := JSONMerge(nil, []byte(`{"a":1}`)); err != nil || string(got) != `{"a":1}` {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := JSONMerge([]byte(`{`), []byte(`{}`)); err == nil {
		t.Fatal("malformed input accepted")
	}
}
