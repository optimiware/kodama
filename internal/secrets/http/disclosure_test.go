package secretshttp_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/httpapi"
)

// Test_no_plaintext_metadata is acceptance no_plaintext_metadata (VS-04,
// VS-A03): sentinel secret bytes, in every plausible encoding, appear in no
// management response, error, log, audit entry, outbox/observation event or
// idempotency record.
func Test_no_plaintext_metadata(t *testing.T) {
	e := newEnv(t)
	const (
		v1 = "SENTINEL-plaintext-one-9f3a77c1"
		v2 = "SENTINEL-plaintext-two-51be0d42"
		v3 = "SENTINEL-never-stored-3c9d11aa"
	)
	forms := func(v string) []string {
		raw := []byte(v)
		return []string{
			v,
			base64.StdEncoding.EncodeToString(raw), base64.RawStdEncoding.EncodeToString(raw),
			base64.URLEncoding.EncodeToString(raw), base64.RawURLEncoding.EncodeToString(raw),
			hex.EncodeToString(raw),
		}
	}
	var needles []string
	for _, v := range []string{v1, v2, v3} {
		needles = append(needles, forms(v)...)
	}
	// A distinctive core survives any base64 alignment of an embedded value.
	needles = append(needles, "plaintext-one", "plaintext-two", "never-stored")

	// Success paths.
	created := e.createSecret(t, "k-create", "erp-token", v1)
	if created.Code != 201 {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	id := secretID(t, created)
	spath := baseURL + "/secrets/" + id
	rot := e.call("POST", spath+"/versions", "alice", mustJSON(t, map[string]any{"value": b64(v2)}), "Idempotency-Key", "k-rot", "If-Match", `"1"`)
	if rot.Code != 201 {
		t.Fatalf("rotate: %d %s", rot.Code, rot.Body)
	}
	e.call("GET", spath, "alice", "")
	e.call("GET", spath+"/versions", "alice", "")
	e.call("GET", baseURL+"/secrets", "alice", "")
	e.call("POST", spath+"/versions/1/revoke", "alice", `{"reason":"rotated after exposure"}`)
	e.call("POST", baseURL+"/variables", "alice", `{"name":"region","value":"eu"}`, "Idempotency-Key", "k-var")
	// Replays and conflicts.
	e.call("POST", baseURL+"/secrets", "alice", mustJSON(t, map[string]any{"name": "erp-token", "kind": "generic", "value": b64(v1)}), "Idempotency-Key", "k-create")
	e.call("POST", baseURL+"/secrets", "alice", mustJSON(t, map[string]any{"name": "erp-token", "kind": "generic", "value": b64(v3)}), "Idempotency-Key", "k-create")

	// Error paths that carry the value in the request.
	bad := map[string]string{
		"bad kind":           mustJSON(t, map[string]any{"name": "a", "kind": "oauth", "value": b64(v3)}),
		"bad name":           mustJSON(t, map[string]any{"name": "A B", "kind": "generic", "value": b64(v3)}),
		"duplicate name":     mustJSON(t, map[string]any{"name": "erp-token", "kind": "generic", "value": b64(v3)}),
		"unknown field":      fmt.Sprintf(`{"name":"b","kind":"generic","value":%q,"extra":%q}`, b64(v3), v3),
		"truncated json":     fmt.Sprintf(`{"name":"b","kind":"generic","value":%q`, b64(v3)),
		"value in the name":  mustJSON(t, map[string]any{"name": v3, "kind": "generic", "value": b64(v3)}),
		"value in the kind":  mustJSON(t, map[string]any{"name": "c", "kind": v3, "value": b64(v3)}),
		"not base64":         fmt.Sprintf(`{"name":"d","kind":"generic","value":%q}`, v3+"***"),
		"wrong value type":   fmt.Sprintf(`{"name":"d","kind":"generic","value":{"x":%q}}`, v3),
		"oversized":          mustJSON(t, map[string]any{"name": "e", "kind": "generic", "value": b64(strings.Repeat(v3, 6000))}),
		"variable with it":   fmt.Sprintf(`{"name":%q,"value":%q}`, v3, v3),
		"variable bad type":  fmt.Sprintf(`{"name":"v","value":{"k":%q}}`, v3),
		"revoke with a body": fmt.Sprintf(`{"reason":%q,"x":1}`, v3),
	}
	for name, body := range bad {
		path := baseURL + "/secrets"
		extra := []string{"Idempotency-Key", "k-bad-" + strings.ReplaceAll(name, " ", "-")}
		switch {
		case strings.HasPrefix(name, "variable"):
			path = baseURL + "/variables"
		case strings.HasPrefix(name, "revoke"):
			path, extra = spath+"/versions/1/revoke", nil
		}
		res := e.call("POST", path, "alice", body, extra...)
		if res.Code < 400 {
			t.Errorf("%s: accepted (%d)", name, res.Code)
		}
	}
	// Stale rotations and wrong headers.
	e.call("POST", spath+"/versions", "alice", mustJSON(t, map[string]any{"value": b64(v3)}), "Idempotency-Key", "k-stale", "If-Match", `"99"`)
	e.call("POST", spath+"/versions", "alice", mustJSON(t, map[string]any{"value": b64(v3)}), "Idempotency-Key", "k-nomatch")
	e.call("POST", spath+"/versions", "alice", mustJSON(t, map[string]any{"value": b64(v3)}), "If-Match", `"2"`)
	e.call("POST", spath+"/versions", "bob", mustJSON(t, map[string]any{"value": b64(v3)}), "Idempotency-Key", "k-bob", "If-Match", `"2"`)
	e.call("POST", spath+"/versions", "alice", mustJSON(t, map[string]any{"value": b64(v3)}), "Idempotency-Key", "k-ct", "If-Match", `"2"`, "Content-Type", "text/plain")
	// Delete.
	e.call("DELETE", spath, "alice", "", "If-Match", `"3"`)
	e.call("GET", spath, "alice", "")

	// Nothing in the observable surface contains any form of any value.
	surfaces := map[string]string{
		"responses":               e.seen(),
		"logs":                    e.logs.String(),
		"persisted text/metadata": e.dumpAll(),
	}
	for name, text := range surfaces {
		if text == "" && name != "logs" {
			t.Fatalf("%s: nothing recorded; the check would be vacuous", name)
		}
		for _, n := range needles {
			if strings.Contains(text, n) {
				t.Errorf("%s contain the sentinel form %q", name, n)
			}
		}
	}
	// Ciphertext columns are opaque: no plaintext form either.
	rows, err := e.base.Migration.Query(context.Background(), `SELECT nonce || ciphertext FROM secret_versions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	versions := 0
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			t.Fatal(err)
		}
		versions++
		for _, n := range needles {
			if bytes.Contains(blob, []byte(n)) {
				t.Errorf("stored ciphertext contains %q", n)
			}
		}
	}
	if versions != 2 {
		t.Fatalf("expected 2 stored versions, found %d", versions)
	}

	// The recorded evidence is meaningful: the transcript holds metadata, the
	// idempotency table holds a metadata-only response, and the events exist.
	if !strings.Contains(e.seen(), "secret://project/erp-token") {
		t.Fatal("responses do not include the secret metadata")
	}
	if e.count(`SELECT count(*) FROM idempotency`) < 3 {
		t.Fatal("no idempotency records were written")
	}
	if e.events(tenantA) < 4 {
		t.Fatalf("only %d events", e.events(tenantA))
	}
	// The stored request fingerprint of a value-bearing request is keyed, not a
	// plain hash of a body containing the value; a variable request (no secret)
	// keeps the plain fingerprint, which proves the check can see the difference.
	plainHash := func(path, body string, headers ...string) []byte {
		req := httptest.NewRequest("POST", path, nil)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		h := httpapi.HashRequest(req, []byte(body))
		return h[:]
	}
	hashRows := func(h []byte) int { return e.count(`SELECT count(*) FROM idempotency WHERE request_hash = $1`, h) }
	createBody := mustJSON(t, map[string]any{"name": "erp-token", "kind": "generic", "value": b64(v1)})
	rotateBody := mustJSON(t, map[string]any{"value": b64(v2)})
	if n := hashRows(plainHash(baseURL+"/secrets", createBody)); n != 0 {
		t.Errorf("the create request's plain SHA-256 is stored (%d rows)", n)
	}
	if n := hashRows(plainHash(spath+"/versions", rotateBody, "If-Match", `"1"`)); n != 0 {
		t.Errorf("the rotation request's plain SHA-256 is stored (%d rows)", n)
	}
	if n := hashRows(plainHash(baseURL+"/variables", `{"name":"region","value":"eu"}`)); n != 1 {
		t.Errorf("control: the variable request's plain fingerprint was not found (%d rows)", n)
	}
	var idem string
	if err := e.base.Migration.QueryRow(context.Background(), `SELECT string_agg(convert_from(response_body, 'UTF8'), ' ') FROM idempotency`).Scan(&idem); err != nil || !strings.Contains(idem, `"currentVersion"`) {
		t.Fatalf("idempotency bodies: %v", err)
	}
}
