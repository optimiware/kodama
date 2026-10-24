package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/root"
)

// Test_auth_commands drives `auth login` and `auth logout` through the real
// runner, flag parsing and JSON contract.
func Test_auth_commands(t *testing.T) {
	p := newProvider(t)
	env := newLoginEnv(t, p, nil)
	env.svc.cfg.Browser = approving(p, func(redirect, state string) {
		callback(t, redirect, url.Values{"state": {state}, "code": {"code-t017"}})
	})
	var out, errb bytes.Buffer
	runner, err := root.NewRunner(root.RunnerConfig{
		Streams:  root.Streams{Out: &out, Err: &errb},
		Contexts: root.NewFileContextStore(filepath.Join(t.TempDir(), "contexts.json")),
		Tokens:   env.svc,
	}, NewCommands(env.svc))
	if err != nil {
		t.Fatal(err)
	}
	a := env.ec.Auth
	flags := []string{
		"--output", "json", "--factory", env.ec.Factory, "--issuer", a.Issuer, "--client-id", a.ClientID,
		"--authorization-endpoint", a.AuthorizationEndpoint, "--token-endpoint", a.TokenEndpoint,
		"--scopes", "openid,kodama", "--redirect-uri", a.RedirectURI,
	}
	if code := runner.Run(context.Background(), append([]string{"auth", "login"}, flags...)); code != root.ExitOK {
		t.Fatalf("login exit %d: %s", code, errb.String())
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res["factory"] != env.ec.Factory {
		t.Fatalf("stdout = %q (%v)", out.String(), err)
	}
	if errb.Len() != 0 || strings.Contains(out.String(), "access-token") {
		t.Fatalf("stderr %q / stdout %q", errb.String(), out.String())
	}
	if env.store.count() != 1 {
		t.Fatal("credential not stored")
	}

	out.Reset()
	for i := 0; i < 2; i++ {
		if code := runner.Run(context.Background(), append([]string{"auth", "logout"}, flags...)); code != root.ExitOK {
			t.Fatalf("logout %d exit %d: %s", i, code, errb.String())
		}
	}
	if env.store.count() != 0 {
		t.Fatal("credential not removed")
	}

	// Noninteractive login is a usage failure on stderr only, with empty stdout.
	out.Reset()
	errb.Reset()
	code := runner.Run(context.Background(), append([]string{"auth", "login", "--non-interactive"}, flags...))
	if code != root.ExitUsage || out.Len() != 0 {
		t.Fatalf("exit %d stdout %q", code, out.String())
	}
	var env2 struct {
		Error struct{ Code, RequestID string }
	}
	if err := json.Unmarshal(errb.Bytes(), &env2); err != nil || env2.Error.Code != "interactive_required" || env2.Error.RequestID != "" {
		t.Fatalf("stderr = %q", errb.String())
	}
}
