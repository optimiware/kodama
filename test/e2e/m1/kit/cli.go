package kit

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/cli/auth"
	"github.com/optimiware/kodama/internal/cli/root"
)

// CLI is the built command-line client (`app.Main`, the process entry point)
// pointed at the running composition with a persona's real OIDC access token.
type CLI struct {
	s     *Stack
	dir   string
	store *auth.FileStore
	key   string
	env   map[string]string
	world *World
}

// NewCLI prepares a client with isolated contexts and credentials.
func (w *World) NewCLI() *CLI {
	t := w.S.T
	t.Helper()
	dir := t.TempDir()
	credDir := filepath.Join(dir, "credentials")
	if err := os.Mkdir(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := &CLI{s: w.S, dir: dir, store: auth.NewFileStore(credDir), world: w}
	c.key = root.Context{Factory: w.S.Base()}.CredentialKey()
	c.env = map[string]string{
		"KODAMA_CONTEXT_FILE": filepath.Join(dir, "contexts.json"), "KODAMA_CREDENTIAL_DIR": credDir, "KODAMA_ALLOW_INSECURE_LOOPBACK": "true",
	}
	return c
}

// As signs in as the persona (an empty persona signs out).
func (c *CLI) As(persona string) {
	t := c.s.T
	t.Helper()
	if persona == "" {
		if err := c.store.Delete(context.Background(), c.key); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := c.store.Save(context.Background(), c.key, auth.Credential{
		AccessToken: c.s.Token(persona), TokenType: "Bearer", ObtainedAt: time.Now(), Expiry: time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
}

// Run runs one command in JSON output mode against tenant A's project and
// returns the exit code and both streams.
func (c *CLI) Run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	full := append(append([]string{}, args...), "--factory", c.s.Base(), "--tenant", TenantA, "--project", c.world.Project, "--credential-store", "file", "--non-interactive", "--output", "json")
	code := app.Main(context.Background(), full, func(k string) string { return c.env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

// WriteFile writes a file under the client's directory.
func (c *CLI) WriteFile(t testing.TB, name, content string) string {
	t.Helper()
	p := filepath.Join(c.dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
