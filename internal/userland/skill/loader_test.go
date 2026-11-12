package skill_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/userland/skill"
)

func mustName(t *testing.T, value string) skill.Name {
	t.Helper()
	name, err := skill.ParseName(value)
	if err != nil {
		t.Fatalf("ParseName(%q): %v", value, err)
	}
	return name
}

func newLoader(t *testing.T, source *fakeSource) skill.Loader {
	t.Helper()
	loader, err := skill.NewLoader(source)
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	return loader
}

// TestLoaderRefusesNilSource pins construction: a loader needs an explicit
// source and reads no filesystem by itself.
func TestLoaderRefusesNilSource(t *testing.T) {
	if loader, err := skill.NewLoader(nil); err == nil {
		t.Fatalf("NewLoader(nil) = %+v; want refusal", loader)
	}
}

// TestLoaderListSortsShuffledEnumeration pins deterministic listing: two
// sources enumerating the same names in different orders (with a duplicate)
// produce the same sorted, de-duplicated list.
func TestLoaderListSortsShuffledEnumeration(t *testing.T) {
	first := newFakeSource()
	first.add(mustName(t, "beta"), "# beta\n")
	first.add(mustName(t, "alpha"), "# alpha\n")
	first.add(mustName(t, "gamma"), "# gamma\n")
	first.add(mustName(t, "alpha"), "# alpha again\n")

	second := newFakeSource()
	second.add(mustName(t, "gamma"), "# gamma\n")
	second.add(mustName(t, "alpha"), "# alpha\n")
	second.add(mustName(t, "beta"), "# beta\n")

	want := []skill.Name{mustName(t, "alpha"), mustName(t, "beta"), mustName(t, "gamma")}
	firstNames, err := newLoader(t, first).List(context.Background())
	if err != nil {
		t.Fatalf("List(first): %v", err)
	}
	secondNames, err := newLoader(t, second).List(context.Background())
	if err != nil {
		t.Fatalf("List(second): %v", err)
	}
	if !reflect.DeepEqual(firstNames, want) {
		t.Fatalf("List(first) = %v; want %v", firstNames, want)
	}
	if !reflect.DeepEqual(secondNames, want) {
		t.Fatalf("List(second) = %v; want %v", secondNames, want)
	}
	if !reflect.DeepEqual(firstNames, secondNames) {
		t.Fatalf("enumeration order changed the listing: %v vs %v", firstNames, secondNames)
	}
}

// TestLoaderListRefusesInvalidName pins that a source cannot smuggle a name
// that is not a bounded Markdown skill name into the listing.
func TestLoaderListRefusesInvalidName(t *testing.T) {
	source := newFakeSource()
	source.add(mustName(t, "valid"), "# valid\n")
	source.names = append(source.names, skill.Name("Not-Valid"))
	if names, err := newLoader(t, source).List(context.Background()); err == nil {
		t.Fatalf("List accepted an invalid name: %v", names)
	}
}

// TestLoaderListPropagatesSourceError pins that a source failure is not an
// empty success.
func TestLoaderListPropagatesSourceError(t *testing.T) {
	source := newFakeSource()
	source.listErr = errors.New("source unavailable")
	if names, err := newLoader(t, source).List(context.Background()); err == nil {
		t.Fatalf("List hid a source failure: %v", names)
	}
}

// TestLoaderLoadValidatesNameBeforeSource pins the confined boundary: a
// traversal, absolute or nested name refuses before the source is touched.
func TestLoaderLoadValidatesNameBeforeSource(t *testing.T) {
	source := newFakeSource()
	loader := newLoader(t, source)
	for _, name := range []skill.Name{"../escape", "..", "a/b", "/abs", "", "UPPER", "a.md", "a b"} {
		if loaded, err := loader.Load(context.Background(), name); err == nil {
			t.Errorf("Load(%q) = %+v; want refusal", name, loaded)
		}
	}
	if len(source.reads) != 0 {
		t.Fatalf("invalid names reached the source: %v", source.reads)
	}
}

// TestLoaderRefusesSecretKnowledge pins the refusal gate: supplied knowledge
// carrying a credential is refused instead of loaded.
func TestLoaderRefusesSecretKnowledge(t *testing.T) {
	source := newFakeSource()
	source.add(mustName(t, "leaky"), "Use this key:\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\n")
	source.add(mustName(t, "prefixed"), "Clone with GITHUB_TOKEN=ghp_leaked123\n")
	source.add(mustName(t, "clean"), "# Review\n\nRead the diff and list risks.\n")
	loader := newLoader(t, source)

	for _, name := range []string{"leaky", "prefixed"} {
		if loaded, err := loader.Load(context.Background(), mustName(t, name)); err == nil {
			t.Fatalf("Load(%s) = %+v; want refusal", name, loaded)
		}
	}
	loaded, err := loader.Load(context.Background(), mustName(t, "clean"))
	if err != nil {
		t.Fatalf("Load(clean): %v", err)
	}
	if loaded.Name != mustName(t, "clean") {
		t.Fatalf("Load(clean).Name = %q", loaded.Name)
	}
	if string(loaded.Body) != "# Review\n\nRead the diff and list risks.\n" {
		t.Fatalf("Load(clean).Body = %q", loaded.Body)
	}
}

// TestLoaderValidatesSourceResult pins that a malformed source result is
// refused rather than returned to the agent.
func TestLoaderValidatesSourceResult(t *testing.T) {
	requested := mustName(t, "review")
	cases := map[string]skill.Skill{
		"mismatched name": {Name: mustName(t, "other"), Path: "review.md", Body: []byte("# Review\n")},
		"invalid path":    {Name: requested, Path: "../review.md", Body: []byte("# Review\n")},
		"empty body":      {Name: requested, Path: "review.md"},
		"oversized body":  {Name: requested, Path: "review.md", Body: []byte(strings.Repeat("x", skill.MaxBodyBytes+1))},
	}
	for name, value := range cases {
		source := newFakeSource()
		source.skills[requested] = value
		if loaded, err := newLoader(t, source).Load(context.Background(), requested); err == nil {
			t.Errorf("%s: Load = %+v; want refusal", name, loaded)
		}
	}
}
