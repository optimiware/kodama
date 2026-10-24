package environment_test

import (
	"encoding/json"
	"testing"
)

// canon re-encodes a JSON document with sorted keys for order-free comparison.
func canon(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	out, _ := json.Marshal(v)
	return string(out)
}
