package configuration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/configuration"
	"github.com/optimiware/kodama/internal/cli/root"
)

const (
	tenant  = "t_acme"
	project = "p_inventory"
	base    = "/api/v1/tenants/" + tenant + "/projects/" + project
)

type tokens struct{}

func (tokens) TokenSource(root.EffectiveContext) (client.AccessTokenSource, error) {
	return clienttest.Tokens("tok-1"), nil
}

type result struct {
	code root.ExitCode
	out  string
	err  string
}

// run executes the real runner with both configuration contributions. The
// context store lives in dir so a test can assert nothing sensitive persisted.
func run(t *testing.T, srv *clienttest.Server, stdin io.Reader, dir string, args ...string) result {
	t.Helper()
	store := root.NewFileContextStore(filepath.Join(dir, "kodama", "contexts.json"))
	var out, errb bytes.Buffer
	r, err := root.NewRunner(root.RunnerConfig{
		Streams: root.Streams{In: stdin, Out: &out, Err: &errb}, Contexts: store, Tokens: tokens{}, AllowInsecureLoopback: true,
	}, configuration.NewVariableCommands(), configuration.NewSecretCommands())
	if err != nil {
		t.Fatal(err)
	}
	full := append([]string{}, args[:2]...)
	if srv != nil {
		full = append(full, "--factory", srv.URL)
	}
	full = append(full, "--tenant", tenant, "--project", project)
	full = append(full, args[2:]...)
	code := r.Run(context.Background(), full)
	return result{code, out.String(), errb.String()}
}

func writeSecretFile(t *testing.T, content []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("not JSON: %v: %q", err, s)
	}
	return m
}

func secretMeta(version int) map[string]any {
	return map[string]any{
		"id": "sec_1", "name": "erp-read-token", "kind": "generic", "ref": "secret://project/erp-read-token",
		"currentVersion": 1, "version": version, "createdAt": "2026-10-19T10:00:00Z", "updatedAt": "2026-10-19T10:00:00Z", "deletedAt": nil,
	}
}

func variableDoc(name, typ string, value any, valueVersion, version int) map[string]any {
	return map[string]any{
		"id": "var_1", "name": name, "type": typ, "value": value, "valueVersion": valueVersion, "description": "",
		"version": version, "createdAt": "2026-10-19T10:00:00Z", "updatedAt": "2026-10-19T10:00:00Z", "deletedAt": nil,
	}
}
