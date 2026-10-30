package app

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	gitjwt "github.com/optimiware/kodama/internal/gitauth/jwt"
	"github.com/optimiware/kodama/internal/shell/serve"
)

func gitMain(ctx context.Context, env map[string]string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Main(ctx, args, func(k string) string { return env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// Test_unit_kernel_git_serve_command: `kodama git serve` and `kodama git-hook`
// are registered in the binary, validate their configuration before listening,
// hold no platform credential, authenticate every request and shut down
// gracefully; the existing commands are not shadowed.
func Test_unit_kernel_git_serve_command(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	signer, err := serve.NewGitSigner("development", "", "")
	if err != nil {
		t.Fatal(err)
	}
	keys := signer.ForwardingPublicKeys()
	pubPEM, err := gitjwt.MarshalPublicKeyPEM(keys.Current)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pubFile := write("forwarding.pem", string(pubPEM))
	identityFile := write("identity", "worker-identity-token\n")
	tokenFile := write("control-token", "control-secret\n")
	dataRoot := filepath.Join(dir, "data")

	t.Run("registered, and nothing is shadowed", func(t *testing.T) {
		if code, out, _ := gitMain(t.Context(), nil, "--help"); code != 0 || !strings.Contains(out, "git        Run a tenant's native Git worker") || !strings.Contains(out, "git-hook") {
			t.Fatalf("usage lists the commands: %d\n%s", code, out)
		}
		if code, out, _ := gitMain(t.Context(), nil, "server", "--help"); code != 0 || !strings.Contains(out, "-assembly") {
			t.Fatalf("server --help = %d\n%s", code, out)
		}
		if code, _, _ := gitMain(t.Context(), nil, "git"); code != 2 {
			t.Fatalf("kodama git without a subcommand = %d", code)
		}
		if code, out, _ := gitMain(t.Context(), nil, "git", "--help"); code != 0 || !strings.Contains(out, "git serve") {
			t.Fatalf("git --help = %d", code)
		}
		if code, _, errOut := gitMain(t.Context(), nil, "git", "clone"); code != 2 || !strings.Contains(errOut, `unknown subcommand "clone"`) {
			t.Fatalf("an unknown subcommand = %d %s", code, errOut)
		}
		if code, _, _ := gitMain(t.Context(), nil, "frobnicate"); code != 2 {
			t.Fatalf("an unknown command = %d", code)
		}
		if !isCLIGroup("pipeline") || isCLIGroup("git") {
			t.Fatal("the CLI groups changed")
		}
	})

	t.Run("hooks refuse outside a worker", func(t *testing.T) {
		if code, _, errOut := gitMain(t.Context(), nil, "git-hook", "pre-receive"); code != 1 || !strings.Contains(errOut, "Git worker") {
			t.Fatalf("pre-receive outside a worker = %d %q", code, errOut)
		}
		if code, _, errOut := gitMain(t.Context(), nil, "git-hook", "reference-transaction", "prepared"); code != 1 || !strings.Contains(errOut, "Git worker") {
			t.Fatalf("reference-transaction outside a worker = %d %q", code, errOut)
		}
		if code, _, _ := gitMain(t.Context(), nil, "git-hook", "update"); code != 2 {
			t.Fatalf("an unknown hook = %d", code)
		}
		if code, _, _ := gitMain(t.Context(), nil, "git-hook"); code != 2 {
			t.Fatalf("git-hook without a name = %d", code)
		}
	})

	good := func(extra ...string) []string {
		return append([]string{"git", "serve", "--tenant", "t_acme", "--data-root", dataRoot, "--control-url", "https://kodama.example.invalid",
			"--identity-file", identityFile, "--forwarding-keys-file", pubFile, "--control-token-file", tokenFile, "--poll-interval", "0s"}, extra...)
	}

	t.Run("configuration is validated before anything listens", func(t *testing.T) {
		for name, c := range map[string]struct {
			args []string
			env  map[string]string
			want string
		}{
			"nothing":              {[]string{"git", "serve"}, nil, "--tenant is required"},
			"no control url":       {[]string{"git", "serve", "--tenant", "t_acme", "--data-root", dataRoot}, nil, "--control-url is required"},
			"positional":           {good("extra"), nil, "flags only"},
			"unknown flag":         {good("--platform-database-url", "x"), nil, "flag provided but not defined"},
			"bad tenant":           {append(good(), "--tenant", "not a tenant!"), nil, ""},
			"relative data root":   {append(good(), "--data-root", "relative/data"), nil, "absolute"},
			"half a TLS pair":      {good("--tls-cert", "c.pem"), nil, "given together"},
			"bad poll interval":    {good("--poll-interval", "2h"), nil, "poll-interval"},
			"missing keys file":    {append(good(), "--forwarding-keys-file", filepath.Join(dir, "none.pem")), nil, "forwarding keys"},
			"private key refused":  {append(good(), "--forwarding-keys-file", write("private.pem", "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n-----END EC PRIVATE KEY-----\n")), nil, "PUBLIC KEY"},
			"empty token":          {append(good(), "--control-token-file", write("empty", "\n")), nil, "empty"},
			"plain http scheme ok": {nil, nil, "SKIP"},
		} {
			if c.want == "SKIP" {
				continue
			}
			code, _, errOut := gitMain(t.Context(), c.env, c.args...)
			if code != 2 || (c.want != "" && !strings.Contains(errOut, c.want)) {
				t.Errorf("%s: exit %d, stderr %q (want 2 containing %q)", name, code, errOut, c.want)
			}
		}
		if _, err := os.Stat(dataRoot); err == nil {
			t.Fatal("a refused configuration touched the data root")
		}
	})

	t.Run("serves, authenticates and stops", func(t *testing.T) {
		addr := freeAddr(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan int, 1)
		var errOut bytes.Buffer
		go func() {
			// Part of the configuration comes from the environment, as in a Pod.
			env := map[string]string{"KODAMA_GIT_LISTEN": addr, "KODAMA_GIT_TENANT_ID": "t_acme"}
			args := good()[:0]
			for i := 0; i < len(good()); i++ {
				if good()[i] == "--tenant" {
					i++
					continue
				}
				args = append(args, good()[i])
			}
			var out bytes.Buffer
			done <- Main(ctx, args, func(k string) string { return env[k] }, &out, &errOut)
		}()
		base := "http://" + addr
		var up bool
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if resp, err := http.Get(base + "/healthz"); err == nil {
				resp.Body.Close()
				up = resp.StatusCode == http.StatusOK
				break
			}
		}
		if !up {
			cancel()
			t.Fatalf("the worker did not come up: %s", errOut.String())
		}
		do := func(method, path string, headers map[string]string) int {
			req, _ := http.NewRequest(method, base+path, nil)
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		refs := "/repo_1.git/info/refs?service=git-upload-pack"
		if got := do(http.MethodGet, refs, nil); got != http.StatusUnauthorized {
			t.Errorf("a Git request without an assertion = %d", got)
		}
		if got := do(http.MethodGet, refs, map[string]string{"Kodama-Forwarding-Assertion": "x.y.z"}); got != http.StatusUnauthorized {
			t.Errorf("a Git request with a garbage assertion = %d", got)
		}
		foreign, err := serve.NewGitSigner("development", "", "")
		if err != nil {
			t.Fatal(err)
		}
		assert := func(s *gitjwt.Signer, tenant string) string {
			now := time.Now().UTC()
			tok, err := s.SignForwarding(t.Context(), gitv1.ForwardingAssertion{Audience: gitv1.WorkerAudience(tenant), TenantID: tenant, RepositoryID: "repo_1", GrantID: "gg_1",
				IssuedAt: gitv1.Timestamp(now.Add(-time.Second).Format(time.RFC3339Nano)), ExpiresAt: gitv1.Timestamp(now.Add(time.Minute).Format(time.RFC3339Nano)), RequestID: "req_1"})
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}
		if got := do(http.MethodGet, refs, map[string]string{"Kodama-Forwarding-Assertion": assert(foreign, "t_acme")}); got != http.StatusUnauthorized {
			t.Errorf("an assertion signed by another key = %d", got)
		}
		if got := do(http.MethodGet, refs, map[string]string{"Kodama-Forwarding-Assertion": assert(signer, "t_acme")}); got != http.StatusNotFound {
			t.Errorf("a valid assertion for a repository that does not exist = %d", got)
		}
		if got := do(http.MethodGet, refs, map[string]string{"Kodama-Forwarding-Assertion": assert(signer, "t_other")}); got != http.StatusForbidden {
			t.Errorf("another tenant's assertion = %d", got)
		}
		rpc := "/internal/worker/v1/repositories/repo_1/refs/list"
		if got := do(http.MethodPost, rpc, nil); got != http.StatusUnauthorized {
			t.Errorf("an RPC without the control credential = %d", got)
		}
		if got := do(http.MethodPost, rpc, map[string]string{"Authorization": "Bearer wrong"}); got != http.StatusUnauthorized {
			t.Errorf("an RPC with a wrong control credential = %d", got)
		}
		if got := do(http.MethodGet, "/repo_1.git/HEAD", map[string]string{"Kodama-Forwarding-Assertion": assert(signer, "t_acme")}); got != http.StatusNotFound {
			t.Errorf("dumb HTTP = %d", got)
		}
		if st, err := os.Stat(filepath.Join(dataRoot, "repositories")); err != nil || !st.IsDir() {
			t.Errorf("the data root was not prepared: %v", err)
		}
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("exit %d after shutdown: %s", code, errOut.String())
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the worker did not stop")
		}
	})
}
