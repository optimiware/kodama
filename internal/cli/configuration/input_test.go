package configuration_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/configuration"
)

func neverCalled(t *testing.T) *clienttest.Server {
	return clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached the server: %s %s", r.Method, r.URL.Path)
	}))
}

// Test_configuration_input_safety: unsafe or malformed input is refused before
// any request is made and without echoing it.
func Test_configuration_input_safety(t *testing.T) {
	srv := neverCalled(t)
	dir := t.TempDir()
	key := []string{"--idempotency-key", "k1", "--kind", "generic"}
	create := func(valueFile string, stdin *os.File) result {
		args := append([]string{"secret", "create", "erp-token", "--value-file", valueFile}, key...)
		if stdin == nil {
			return run(t, srv, strings.NewReader(""), dir, args...)
		}
		return run(t, srv, stdin, dir, args...)
	}

	t.Run("group and world readable file", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o666} {
			if r := create(writeSecretFile(t, []byte("super-secret"), mode), nil); r.code != 2 || r.out != "" || !strings.Contains(r.err, "chmod 600") {
				t.Errorf("mode %o: exit %d err %q", mode, r.code, r.err)
			}
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := writeSecretFile(t, []byte("super-secret"), 0o600)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skip("symlinks unavailable")
		}
		if r := create(link, nil); r.code != 2 || !strings.Contains(r.err, "not a regular file") {
			t.Errorf("exit %d err %q", r.code, r.err)
		}
	})
	t.Run("directory and missing", func(t *testing.T) {
		if r := create(t.TempDir(), nil); r.code != 2 {
			t.Errorf("directory: exit %d", r.code)
		}
		if r := create(filepath.Join(t.TempDir(), "absent"), nil); r.code != 2 || !strings.Contains(r.err, "no such file") {
			t.Errorf("missing: exit %d err %q", r.code, r.err)
		}
	})
	t.Run("empty and oversized", func(t *testing.T) {
		if r := create(writeSecretFile(t, nil, 0o600), nil); r.code != 2 || !strings.Contains(r.err, "empty") {
			t.Errorf("empty: exit %d err %q", r.code, r.err)
		}
		big := bytes.Repeat([]byte{'x'}, configuration.MaxSecretBytes+1)
		if r := create(writeSecretFile(t, big, 0o600), nil); r.code != 2 || !strings.Contains(r.err, "larger than") {
			t.Errorf("oversized: exit %d err %q", r.code, r.err)
		}
	})
	t.Run("terminal or device standard input", func(t *testing.T) {
		dev, err := os.Open(os.DevNull)
		if err != nil {
			t.Skip(err)
		}
		defer dev.Close()
		if r := create("-", dev); r.code != 2 || !strings.Contains(r.err, "terminal or device") {
			t.Errorf("exit %d err %q", r.code, r.err)
		}
	})
	t.Run("plaintext as an argument or environment is not accepted", func(t *testing.T) {
		t.Setenv("KODAMA_SECRET_VALUE", "from-env-secret")
		for _, args := range [][]string{
			{"secret", "create", "erp-token", "--kind", "generic", "--idempotency-key", "k1", "--value", "plaintext-secret-arg"},
			{"secret", "create", "erp-token", "--kind", "generic", "--idempotency-key", "k1", "--value=plaintext-secret-arg"},
			{"secret", "create", "erp-token", "--kind", "generic", "--idempotency-key", "k1"},
		} {
			r := run(t, srv, strings.NewReader("from-stdin-must-not-be-read"), dir, args...)
			if r.code != 2 || strings.Contains(r.err+r.out, "plaintext-secret-arg") || strings.Contains(r.err+r.out, "from-env-secret") {
				t.Errorf("%v: exit %d err %q", args, r.code, r.err)
			}
		}
	})
	t.Run("missing required headers and kinds", func(t *testing.T) {
		f := writeSecretFile(t, []byte("super-secret"), 0o600)
		for name, args := range map[string][]string{
			"no key":         {"secret", "create", "erp-token", "--kind", "generic", "--value-file", f},
			"empty key":      {"secret", "create", "erp-token", "--kind", "generic", "--value-file", f, "--idempotency-key", ""},
			"invalid key":    {"secret", "create", "erp-token", "--kind", "generic", "--value-file", f, "--idempotency-key", "bad key!"},
			"no kind":        {"secret", "create", "erp-token", "--value-file", f, "--idempotency-key", "k1"},
			"bad kind":       {"secret", "create", "erp-token", "--kind", "password", "--value-file", f, "--idempotency-key", "k1"},
			"rotate no ver":  {"secret", "rotate", "sec_1", "--value-file", f, "--idempotency-key", "k1"},
			"rotate no key":  {"secret", "rotate", "sec_1", "--if-version", "1", "--value-file", f},
			"rotate no file": {"secret", "rotate", "sec_1", "--if-version", "1", "--idempotency-key", "k1"},
			"revoke no ver":  {"secret", "revoke", "sec_1"},
			"revoke zero":    {"secret", "revoke", "sec_1", "--version", "0"},
			"delete no ver":  {"secret", "delete", "sec_1"},
		} {
			if r := run(t, srv, strings.NewReader(""), dir, args...); r.code != 2 || r.out != "" {
				t.Errorf("%s: exit %d out %q err %q", name, r.code, r.out, r.err)
			}
		}
	})
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}

func Test_configuration_secret_size_bounds_are_inclusive(t *testing.T) {
	var got []byte
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 201, secretMeta(1), nil)
	}))
	for _, n := range []int{1, configuration.MaxSecretBytes} {
		got = bytes.Repeat([]byte{0xff}, n)
		r := run(t, srv, bytes.NewReader(got), t.TempDir(), "secret", "create", "erp-token", "--kind", "generic", "--idempotency-key", "k1", "--value-file", "-")
		if r.code != 0 {
			t.Fatalf("%d bytes: exit %d %s", n, r.code, r.err)
		}
	}
	if len(srv.Requests()) != 2 {
		t.Fatalf("requests %d", len(srv.Requests()))
	}
}

func Test_configuration_variable_scalar_input(t *testing.T) {
	srv := neverCalled(t)
	for name, args := range map[string][]string{
		"both":          {"variable", "create", "limit", "--value", "1", "--value-json", "1", "--idempotency-key", "k"},
		"neither":       {"variable", "create", "limit", "--idempotency-key", "k"},
		"no key":        {"variable", "create", "limit", "--value", "1"},
		"float":         {"variable", "create", "limit", "--value-json", "1.5", "--idempotency-key", "k"},
		"exponent":      {"variable", "create", "limit", "--value-json", "1e3", "--idempotency-key", "k"},
		"null":          {"variable", "create", "limit", "--value-json", "null", "--idempotency-key", "k"},
		"array":         {"variable", "create", "limit", "--value-json", "[1]", "--idempotency-key", "k"},
		"object":        {"variable", "create", "limit", "--value-json", "{}", "--idempotency-key", "k"},
		"trailing":      {"variable", "create", "limit", "--value-json", "1 2", "--idempotency-key", "k"},
		"bare word":     {"variable", "create", "limit", "--value-json", "yes", "--idempotency-key", "k"},
		"leading zero":  {"variable", "create", "limit", "--value-json", "007", "--idempotency-key", "k"},
		"update empty":  {"variable", "update", "var_1", "--if-version", "1"},
		"update no ver": {"variable", "update", "var_1", "--value", "x"},
		"delete no ver": {"variable", "delete", "var_1"},
	} {
		if r := run(t, srv, strings.NewReader(""), t.TempDir(), args...); r.code != 2 || r.out != "" {
			t.Errorf("%s: exit %d out %q err %q", name, r.code, r.out, r.err)
		}
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}
