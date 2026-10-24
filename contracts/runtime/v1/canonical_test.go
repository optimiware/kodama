package runtimev1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type vector struct {
	Name      string `json:"name"`
	Input     string `json:"input"`
	Canonical string `json:"canonical"`
	SHA256    string `json:"sha256"`
	Error     bool   `json:"error"`
}

func readFixture(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile("fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// The expected bytes and hashes were derived independently (Python json +
// hashlib), so these tests do not merely replay the implementation.
func TestCanonicalVectors(t *testing.T) {
	var vectors []vector
	readFixture(t, "canonical.json", &vectors)
	if len(vectors) < 25 {
		t.Fatal("canonical vectors missing")
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			got, err := Canonicalize([]byte(v.Input))
			if v.Error {
				if err == nil {
					t.Fatalf("accepted %q as %q", v.Input, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != v.Canonical {
				t.Fatalf("canonical\n got %s\nwant %s", got, v.Canonical)
			}
			sum := sha256.Sum256(got)
			if "sha256:"+hex.EncodeToString(sum[:]) != v.SHA256 {
				t.Fatal("hash differs from the independently derived vector")
			}
			again, err := Canonicalize(got)
			if err != nil || string(again) != string(got) {
				t.Fatal("canonical form is not a fixed point")
			}
		})
	}
	// Null and omitted must never hash alike.
	byName := map[string]string{}
	for _, v := range vectors {
		byName[v.Name] = v.SHA256
	}
	if byName["null_is_not_omitted_present"] == byName["null_is_not_omitted_absent"] {
		t.Fatal("null and omitted hash identically")
	}
}

func TestResultHashIgnoresFormattingButNotContent(t *testing.T) {
	var cases []fixtureCase
	readFixture(t, "cases.json", &cases)
	var base *ResultRequest
	for _, c := range cases {
		if c.Name == "result_completed_reader" {
			r, err := DecodeRequest[ResultRequest](c.Payload)
			if err != nil {
				t.Fatal(err)
			}
			base = &r
		}
	}
	if base == nil {
		t.Fatal("fixture missing")
	}
	hash, err := ResultHash(base.Result)
	if err != nil || hash != base.ResultHash {
		t.Fatalf("fixture hash %s, computed %s (%v)", base.ResultHash, hash, err)
	}
	reordered := `{"diagnosticArtifactIds":["art_log"], "outputs":{"report":"art_tests"}, "sourceCommit":null,"question":null,"reason":null,"exitCode":0,"outcome":"completed"}`
	got, err := DecodeRequest[Result]([]byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := ResultHash(got); h != hash {
		t.Fatal("key order or whitespace changed the hash")
	}
	for name, mutate := range map[string]func(*Result){
		"output":      func(r *Result) { r.Outputs = map[string]Value{"report": StringValue("art_other")} },
		"commit":      func(r *Result) { c := GitCommit(strings.Repeat("2", 40)); r.SourceCommit = &c },
		"diagnostics": func(r *Result) { r.DiagnosticArtifactIDs = nil },
		"exit": func(r *Result) {
			n := int64(3)
			r.ExitCode = &n
			r.Outcome = OutcomeFailed
			reason := "exit_nonzero"
			r.Reason = &reason
		},
	} {
		r := base.Result
		mutate(&r)
		if h, err := ResultHash(r); err == nil && h == hash {
			t.Fatalf("%s change did not change the hash", name)
		}
	}
	if _, err := ResultHash(Result{}); err == nil {
		t.Fatal("hashed an invalid result")
	}
}
