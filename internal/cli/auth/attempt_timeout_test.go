package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/root"
)

// Test_login_attempt_timeout proves LoginTimeout bounds the whole attempt: a
// callback and exchange that succeed do not stop a blocked keyring prompt
// during persistence from hanging the command.
func Test_login_attempt_timeout(t *testing.T) {
	p := newProvider(t)
	kr := newPromptKeyring(true) // every backend operation blocks like an unlock prompt
	defer close(kr.release)
	env := newLoginEnv(t, p, nil, func(c *ServiceConfig) {
		c.Keyring = NewKeyringStore(kr, "t017-attempt-timeout")
		c.LoginTimeout = 400 * time.Millisecond
	})
	env.svc.cfg.Browser = approving(p, func(redirect, state string) {
		callback(t, redirect, url.Values{"state": {state}, "code": {"code-t017"}})
	})

	done := make(chan error, 1)
	var res LoginResult
	go func() {
		var err error
		res, err = env.svc.Login(context.Background(), env.ec) // no caller deadline
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("login hung on credential persistence despite LoginTimeout")
	}
	if res != (LoginResult{}) {
		t.Fatalf("login reported success: %+v", res)
	}
	pr := root.Classify(err)
	if pr.Exit != root.ExitTimeout || pr.Code != "timeout" || pr.Details["outcomeUncertain"] != true {
		t.Fatalf("classified %+v", pr)
	}
	if n := p.exchanges.Load(); n != 1 {
		t.Fatalf("exchanges = %d", n)
	}
	if !portIsFree(env.port) {
		t.Fatal("loopback listener still open")
	}
}

// Test_cancelled_file_logout_keeps_credential drives `auth logout` through the
// runner with an already-cancelled context and a file-backed store with no
// contention: the credential must survive and only stderr may speak.
func Test_cancelled_file_logout_keeps_credential(t *testing.T) {
	p := newProvider(t)
	dir := filepath.Join(t.TempDir(), "creds")
	env := newLoginEnv(t, p, nil, func(c *ServiceConfig) { c.File = NewFileStore(dir) })
	env.ec.CredentialStore = root.StoreFile
	if err := env.svc.cfg.File.Save(context.Background(), env.ec.CredentialKey(), Credential{AccessToken: "tok", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
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
	args := []string{
		"auth", "logout", "--output", "json", "--credential-store", "file", "--factory", env.ec.Factory, "--issuer", a.Issuer,
		"--client-id", a.ClientID, "--authorization-endpoint", a.AuthorizationEndpoint, "--token-endpoint", a.TokenEndpoint,
		"--scopes", "openid,kodama", "--redirect-uri", a.RedirectURI,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code := runner.Run(ctx, args)
	var e struct{ Error struct{ Code string } }
	if code != root.ExitTimeout || out.Len() != 0 || json.Unmarshal(errb.Bytes(), &e) != nil || e.Error.Code != "interrupted" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, env.ec.CredentialKey()+".json")); err != nil {
		t.Fatalf("cancelled logout removed the credential: %v", err)
	}
	if _, err := env.svc.cfg.File.Load(context.Background(), env.ec.CredentialKey()); err != nil {
		t.Fatal(err)
	}
}

// Test_file_store_cancelled_before_commit proves the file adapter honours an
// already-cancelled context before any side effect.
func Test_file_store_cancelled_before_commit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds")
	s := NewFileStore(dir)
	bg := context.Background()
	key := "k-t017"
	if err := s.Save(bg, key, Credential{AccessToken: "old", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := s.Save(ctx, key, Credential{AccessToken: "new", TokenType: "Bearer"}); err == nil {
		t.Fatal("cancelled save succeeded")
	}
	if err := s.Delete(ctx, key); err == nil {
		t.Fatal("cancelled delete succeeded")
	}
	c, err := s.Load(bg, key)
	if err != nil || c.AccessToken != "old" {
		t.Fatalf("stored credential changed: %v %v", c, err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("directory has %d entries", len(ents))
	}

	// Cancellation arriving after the temporary file is written but before
	// the rename must leave the old credential and no temporary file.
	s.rename = func(string, string) error { t.Fatal("rename reached after cancellation"); return nil }
	late := &lateCancelCtx{Context: bg, okCalls: 1}
	if err := s.Save(late, key, Credential{AccessToken: "new", TokenType: "Bearer"}); err == nil {
		t.Fatal("late-cancelled save succeeded")
	}
	if c, err := s.Load(bg, key); err != nil || c.AccessToken != "old" {
		t.Fatalf("stored credential changed: %v %v", c, err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("temporary file left behind: %d entries", len(ents))
	}
}

// lateCancelCtx reports no error for the first okCalls calls to Err, then
// context.Canceled, simulating cancellation between two steps of an adapter.
type lateCancelCtx struct {
	context.Context
	okCalls int
}

func (c *lateCancelCtx) Err() error {
	if c.okCalls > 0 {
		c.okCalls--
		return nil
	}
	return context.Canceled
}

// Test_login_persist_timeout_reports_uncertainty drives `auth login` through the
// runner, in both output formats, with a keyring whose write blocks past the
// attempt timeout. The operator must be told the credential may still be saved,
// with login-specific guidance (not the resource-mutation text); releasing the
// backend afterwards shows the late save really happens.
func Test_login_persist_timeout_reports_uncertainty(t *testing.T) {
	for _, format := range []string{"json", "human"} {
		t.Run(format, func(t *testing.T) {
			p := newProvider(t)
			kr := newPromptKeyring(true)
			released := false
			release := func() {
				if !released {
					released = true
					close(kr.release)
				}
			}
			defer release()
			env := newLoginEnv(t, p, nil, func(c *ServiceConfig) {
				c.Keyring = NewKeyringStore(kr, "t017-persist-uncertain")
				c.LoginTimeout = 400 * time.Millisecond
			})
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
			args := []string{
				"auth", "login", "--output", format, "--factory", env.ec.Factory, "--issuer", a.Issuer,
				"--client-id", a.ClientID, "--authorization-endpoint", a.AuthorizationEndpoint, "--token-endpoint", a.TokenEndpoint,
				"--scopes", "openid,kodama", "--redirect-uri", a.RedirectURI,
			}
			done := make(chan root.ExitCode, 1)
			go func() { done <- runner.Run(context.Background(), args) }() // no caller deadline
			var code root.ExitCode
			select {
			case code = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("login hung on credential persistence")
			}
			if code != root.ExitTimeout || out.Len() != 0 || p.exchanges.Load() != 1 {
				t.Fatalf("exit %d exchanges %d stdout %q stderr %q", code, p.exchanges.Load(), out.String(), errb.String())
			}
			if strings.Contains(errb.String(), root.RecoveryGuidance) || strings.Contains(errb.String(), "idempotency") {
				t.Fatalf("resource-mutation recovery text leaked into a login failure: %q", errb.String())
			}
			if format == "json" {
				var e struct {
					Error struct {
						Code    string         `json:"code"`
						Details map[string]any `json:"details"`
					} `json:"error"`
				}
				if err := json.Unmarshal(errb.Bytes(), &e); err != nil {
					t.Fatalf("stderr is not one JSON envelope: %q", errb.String())
				}
				rec, _ := e.Error.Details["recovery"].(string)
				if e.Error.Code != "timeout" || e.Error.Details["outcomeUncertain"] != true ||
					!strings.Contains(rec, "kodama auth login") || !strings.Contains(rec, "kodama auth logout") || !strings.Contains(rec, "credential") {
					t.Fatalf("uncertainty lost in JSON: %q", errb.String())
				}
			} else {
				got := errb.String()
				if !strings.Contains(got, "outcome unknown: ") || !strings.Contains(got, "may still complete") ||
					!strings.Contains(got, "kodama auth login") || !strings.Contains(got, "kodama auth logout") {
					t.Fatalf("uncertainty lost in human output: %q", got)
				}
			}

			// The abandoned write is still blocked; once the backend lets go it
			// installs a usable credential, which is exactly what was reported.
			if kr.dataCount() != 0 {
				t.Fatal("credential saved before the backend released")
			}
			release()
			deadline := time.Now().Add(5 * time.Second)
			for kr.dataCount() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if kr.dataCount() != 1 {
				t.Fatal("the abandoned keyring write never completed; the test cannot show the late save")
			}
		})
	}
}
