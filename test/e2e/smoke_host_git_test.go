//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSmokeHostGitAskpassScoped proves the host Git askpass answers only a
// credential prompt for the configured served origin and returns nothing for
// another origin or port. It runs the exact script the host helper installs.
func TestSmokeHostGitAskpassScoped(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	root := t.TempDir()
	askpass := filepath.Join(root, "askpass.sh")
	if err := os.WriteFile(askpass, []byte(hostGitAskpass), 0o700); err != nil {
		t.Fatalf("write the askpass script: %v", err)
	}
	tokenFile := filepath.Join(root, "token")
	const token = "kodex.hermetic.token"
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	run := func(origin, prompt string) (string, error) {
		command := exec.Command("/bin/sh", askpass, prompt)
		command.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"W49_GIT_ORIGIN=" + origin,
			"W49_GIT_TOKEN_FILE=" + tokenFile,
		}
		output, err := command.CombinedOutput()
		return string(output), err
	}
	origin := "https://git.example.test:18445"

	username, err := run(origin, "Username for 'https://git.example.test:18445': ")
	if err != nil || username != "kodama\n" {
		t.Fatalf("matching username prompt = %q, err=%v; want kodama", username, err)
	}
	password, err := run(origin, "Password for 'https://kodama@git.example.test:18445': ")
	if err != nil || password != token {
		t.Fatalf("matching password prompt returned %d bytes, err=%v; want the token", len(password), err)
	}
	unnamedPassword, err := run(origin, "Password for 'https://git.example.test:18445': ")
	if err != nil || unnamedPassword != token {
		t.Fatalf("matching unnamed password prompt returned %d bytes, err=%v; want the token", len(unnamedPassword), err)
	}
	for name, prompt := range map[string]string{
		"another host": "Password for 'https://evil.example.test:18445': ",
		"another port": "Password for 'https://git.example.test:18446': ",
		"plain text":   "some other prompt",
	} {
		answer, err := run(origin, prompt)
		if err != nil {
			t.Fatalf("%s prompt: %v", name, err)
		}
		if answer != "" {
			t.Fatalf("%s prompt answered %d bytes; want no answer", name, len(answer))
		}
	}
	// A prompt that only shares the URL prefix must not match: the origin is
	// matched as one value, not as a prefix of a longer host.
	answer, err := run(origin, "Password for 'https://git.example.test:184450': ")
	if err != nil {
		t.Fatalf("longer-port prompt: %v", err)
	}
	if answer != "" {
		t.Fatalf("longer-port prompt answered %d bytes; want no answer", len(answer))
	}
}

// TestSmokeHostGitAuthenticationRefused proves the host Git error classifier names
// credential expiry for authentication/401 shapes and leaves a policy refusal
// or transport failure as an ordinary error.
func TestSmokeHostGitAuthenticationRefused(t *testing.T) {
	for _, output := range []string{
		"fatal: Authentication failed for 'https://localhost:18445/smoke/run/'",
		"fatal: could not read Username for 'https://localhost:18445': terminal prompts disabled",
		"remote: HTTP 401",
		"HTTP/1.1 401 Unauthorized",
	} {
		if !hostGitAuthenticationRefused([]byte(output)) {
			t.Fatalf("credential failure %q was not classified as an authentication refusal", output)
		}
	}
	for _, output := range []string{
		"remote: kodama git: update rejected",
		"fatal: unable to access 'https://localhost:18445/': TLS handshake timeout",
		"fatal: couldn't find remote ref refs/heads/work/x",
	} {
		if hostGitAuthenticationRefused([]byte(output)) {
			t.Fatalf("non-credential failure %q was classified as credential expiry", strings.TrimSpace(output))
		}
	}
}
