package statev1

import (
	"encoding/json"
	"testing"
)

func TestValueValidation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"object", `{"a":1}`, true},
		{"array", `[1,2]`, true},
		{"trailing", `{"a":1} {}`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		raw := json.RawMessage(tc.raw)
		if err := ValidateValue(raw); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v", tc.name, err)
		}
	}
}

func TestKeyValidation(t *testing.T) {
	for _, key := range []string{"resources/abc", "a/b-c_d", "a.b"} {
		if err := ValidateKey(key); err != nil {
			t.Errorf("%q: %v", key, err)
		}
	}
	for _, key := range []string{"", "/abs", "../x", "a//b", "a/", "a b"} {
		if err := ValidateKey(key); err == nil {
			t.Errorf("%q accepted", key)
		}
	}
}

func TestArtifactReferencesAreSorted(t *testing.T) {
	refs := []ArtifactReference{
		{ArtifactID: "b", Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{ArtifactID: "a", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	if err := ValidateArtifactReferences(refs); err == nil {
		t.Fatal("unsorted references accepted")
	}
}
