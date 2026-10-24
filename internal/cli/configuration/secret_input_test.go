package configuration_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

// binarySecret is multi-line, contains NUL, invalid UTF-8, CRLF and a trailing
// newline: bytes that a line reader, a string conversion or a trim would alter.
var binarySecret = []byte("line one\r\nline two\n\x00\xff\xfe-binary\x80\n")

// tree returns every file under dir so persisted CLI state can be searched.
func tree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		out[p] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertNoSecret(t *testing.T, where string, haystack []byte, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && bytes.Contains(haystack, []byte(s)) {
			t.Errorf("%s contains secret material %q", where, s)
		}
	}
}

// Test_secret_stdin_no_echo: redirected binary input reaches only the base64
// request value; neither plaintext nor its encoding appears in either output
// stream, in a diagnostic, or in any file the CLI persisted, whether the
// request succeeded, was rejected, or ended with an unknown outcome.
func Test_secret_stdin_no_echo(t *testing.T) {
	wire := base64.StdEncoding.EncodeToString(binarySecret)
	needles := []string{string(binarySecret), wire, "line one", "bGluZSBvbmU"}

	type outcome struct {
		name   string
		handle http.HandlerFunc
		code   int
	}
	for _, oc := range []outcome{
		{"created", func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 201, secretMeta(1), map[string]string{"ETag": `"1"`})
		}, 0},
		{"rejected", func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, 409, "name_conflict", "the name is already used", "req_s1", nil)
		}, 4},
		{"unavailable", func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, 503, "unavailable", "the key provider is unavailable", "req_s2", map[string]string{"Retry-After": "5"})
		}, 1},
		{"lost response", func(w http.ResponseWriter, r *http.Request) {
			c, _, _ := w.(http.Hijacker).Hijack()
			_ = c.Close()
		}, 1},
		{"gateway page", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(502)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		}, 1},
	} {
		for _, format := range []string{"human", "json"} {
			t.Run(oc.name+"/"+format, func(t *testing.T) {
				srv := clienttest.NewServer(t, oc.handle)
				dir := t.TempDir()
				r := run(t, srv, bytes.NewReader(binarySecret), dir, "secret", "create", "erp-read-token", "--kind", "generic",
					"--value-file", "-", "--idempotency-key", "secret-create-01", "--output", format)
				if int(r.code) != oc.code {
					t.Fatalf("exit %d, want %d: %s", r.code, oc.code, r.err)
				}
				reqs := srv.Requests()
				if len(reqs) != 1 {
					t.Fatalf("%d requests; a secret request is never retried", len(reqs))
				}
				// The request carries exactly the standard padded base64 of the bytes.
				var body struct {
					Name, Kind, Value string
				}
				if err := json.Unmarshal(reqs[0].Body, &body); err != nil || body.Value != wire || body.Kind != "generic" || body.Name != "erp-read-token" {
					t.Fatalf("request body %q (%v)", reqs[0].Body, err)
				}
				if got, _ := base64.StdEncoding.DecodeString(body.Value); !bytes.Equal(got, binarySecret) {
					t.Fatal("the wire value does not decode to the exact input bytes")
				}
				if reqs[0].Header.Get("Idempotency-Key") != "secret-create-01" || reqs[0].Path != base+"/secrets" {
					t.Fatalf("request %+v", reqs[0])
				}
				assertNoSecret(t, "stdout", []byte(r.out), needles...)
				assertNoSecret(t, "stderr", []byte(r.err), needles...)
				for p, b := range tree(t, dir) {
					assertNoSecret(t, p, b, needles...)
				}
				if oc.code != 0 && r.out != "" {
					t.Errorf("a failed command wrote to stdout: %q", r.out)
				}
				if oc.name == "lost response" || oc.name == "gateway page" {
					if !strings.Contains(r.err, "idempotency-key") || !strings.Contains(r.err, "utcome") {
						t.Errorf("no replay guidance for an uncertain outcome: %s", r.err)
					}
				}
			})
		}
	}
}

// A protected file and redirected stdin produce the identical request, and the
// CLI never writes the secret back to any file of its own.
func Test_secret_file_source_and_rotation(t *testing.T) {
	wire := base64.StdEncoding.EncodeToString(binarySecret)
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 201, map[string]any{
			"secret": secretMeta(3),
			"version": map[string]any{"secretId": "sec_1", "version": 2, "algorithm": "aes-256-gcm", "keyGeneration": 1,
				"createdAt": "2026-10-19T10:00:00Z", "createdBy": "usr_1", "revokedAt": nil},
		}, nil)
	}))
	f := writeSecretFile(t, binarySecret, 0o600)
	dir := t.TempDir()
	r := run(t, srv, strings.NewReader(""), dir, "secret", "rotate", "sec_1", "--if-version", "2", "--value-file", f, "--idempotency-key", "rot-1", "--output", "json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	req := srv.Requests()[0]
	if req.Method != "POST" || req.Path != base+"/secrets/sec_1/versions" || req.Header.Get("If-Match") != `"2"` || req.Header.Get("Idempotency-Key") != "rot-1" {
		t.Fatalf("request %+v", req)
	}
	if !strings.Contains(string(req.Body), wire) {
		t.Fatalf("body %s", req.Body)
	}
	assertNoSecret(t, "stdout", []byte(r.out), string(binarySecret), wire)
	if !strings.Contains(r.out, `"keyGeneration":1`) || strings.Contains(r.out, "value") && strings.Contains(r.out, wire) {
		t.Fatalf("output %s", r.out)
	}
	h := run(t, srv, strings.NewReader(""), dir, "secret", "rotate", "sec_1", "--if-version", "2", "--value-file", f, "--idempotency-key", "rot-1")
	if h.code != 0 || !strings.Contains(h.out, "added value version 2") {
		t.Fatalf("human: exit %d out %q", h.code, h.out)
	}
	for p, b := range tree(t, dir) {
		assertNoSecret(t, p, b, string(binarySecret), wire)
	}
}
