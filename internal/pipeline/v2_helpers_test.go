package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/pipeline"
)

const fixturesV2 = "../../contracts/pipeline/v1alpha2/fixtures"

const (
	v2Digest1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	v2Digest2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// v2Base is a valid kodama/v1alpha2 document with two services, one edge and a
// step that reaches both; every invalid-document case mutates it by one path.
const v2Base = `{
 "apiVersion": "kodama/v1alpha2", "kind": "Pipeline", "metadata": {"name": "base"},
 "spec": {
  "timeoutSeconds": 1800,
  "inputs": {"region": {"type": "string", "default": "eu"}},
  "services": {
   "db": {"image": "registry.example.invalid/db@` + v2Digest1 + `", "ports": {"sql": {"port": 5432}, "admin": {"port": 5433}},
          "resources": {"cpuMillis": 500, "memoryMiB": 512, "ephemeralMiB": 256},
          "readiness": {"tcp": "sql", "timeoutSeconds": 60}},
   "cache": {"image": "registry.example.invalid/cache@` + v2Digest2 + `", "ports": {"kv": {"port": 6379}},
          "resources": {"cpuMillis": 100, "memoryMiB": 64, "ephemeralMiB": 64},
          "readiness": {"tcp": "kv", "timeoutSeconds": 30}}
  },
  "network": {"edges": [{"from": "cache", "to": "db", "ports": ["sql"]}]},
  "steps": [
   {"id": "test", "kind": "container", "image": "registry.example.invalid/tools@` + v2Digest1 + `",
    "command": ["/bin/sh", "-ec"], "args": ["true"], "timeoutSeconds": 600,
    "env": {"PGHOST": "${{ services.db.host }}", "PGPORT": "${{ services.db.ports.sql }}"}}
  ]
 }
}`

// mutate applies edits to v2Base and returns the JSON document. A path is
// dot-separated; numeric segments index arrays. A nil value deletes the key.
func mutate(t testing.TB, edits ...any) []byte {
	t.Helper()
	return mutateDoc(t, v2Base, edits...)
}

func mutateDoc(t testing.TB, base string, edits ...any) []byte {
	t.Helper()
	var root any
	if err := json.Unmarshal([]byte(base), &root); err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(edits); i += 2 {
		root = setPath(t, root, strings.Split(edits[i].(string), "."), edits[i+1])
	}
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func setPath(t testing.TB, node any, segs []string, val any) any {
	t.Helper()
	switch n := node.(type) {
	case map[string]any:
		if len(segs) == 1 {
			if val == nil {
				delete(n, segs[0])
			} else {
				n[segs[0]] = val
			}
			return n
		}
		if n[segs[0]] == nil {
			n[segs[0]] = map[string]any{}
		}
		n[segs[0]] = setPath(t, n[segs[0]], segs[1:], val)
		return n
	case []any:
		i, err := strconv.Atoi(segs[0])
		if err != nil {
			t.Fatalf("path segment %q is not an index", segs[0])
		}
		if len(segs) == 1 {
			n[i] = val
			return n
		}
		n[i] = setPath(t, n[i], segs[1:], val)
		return n
	}
	t.Fatalf("cannot descend into %T at %v", node, segs)
	return nil
}

// v2env is a validator environment with the kodama/v1alpha2 format enabled.
func newEnvV2(t testing.TB) *env {
	t.Helper()
	e := newEnv(t)
	e.refs.secrets["test-db-password"] = true
	e.refs.secrets["file-only"] = false
	v, err := pipeline.NewValidator(pipeline.Config{Capabilities: e.caps, Profiles: e.profiles, References: e.refs, EnableV1Alpha2: true})
	if err != nil {
		t.Fatal(err)
	}
	e.v = v
	return e
}

func (e *env) validateJSON(raw []byte) (pipeline.Validated, error) {
	return e.v.ValidateJSON(context.Background(), scope, actor, raw)
}

func (e *env) mustValidateJSON(t testing.TB, raw []byte) pipeline.Validated {
	t.Helper()
	v, err := e.validateJSON(raw)
	if err != nil {
		t.Fatalf("expected a valid document: %v\n%s", err, raw)
	}
	return v
}

func stageOf(err error) pipeline.Stage {
	var ve *pipeline.ValidationError
	if errors.As(err, &ve) {
		return ve.Stage
	}
	return ""
}
