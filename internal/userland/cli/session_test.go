package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// TestFileSessionStoreRoundTrip pins human-session persistence: JSON shape,
// mode 0600, a fresh read on every Load and no token in errors.
func TestFileSessionStoreRoundTrip(t *testing.T) {
	filesystem := newFakeFS()
	path := "/home/engineer/.kodama/session.json"
	store := cli.FileSessionStore{Path: path, Files: filesystem, Now: testNow}
	credential := humanCredential()

	if err := store.Save(context.Background(), credential); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, ok := filesystem.replaces[path]
	if !ok {
		t.Fatal("Save did not replace the session file")
	}
	if _, wrote := filesystem.writes[path]; wrote {
		t.Fatal("Save persisted the session through WriteFile")
	}
	if filesystem.modes[path] != 0o600 {
		t.Fatalf("session file mode = %o; want 600", filesystem.modes[path])
	}
	var file struct {
		Kind      string    `json:"kind"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("session file is not JSON: %v", err)
	}
	if file.Kind != string(client.KindSession) || file.Token != credential.Token || !file.ExpiresAt.Equal(credential.ExpiresAt) {
		t.Fatalf("session file = %+v; want the human credential", file)
	}

	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Kind != client.KindSession || loaded.Token != credential.Token || !loaded.ExpiresAt.Equal(credential.ExpiresAt) {
		t.Fatalf("loaded credential = %+v", loaded)
	}

	// Load reads the file again rather than caching the saved value.
	rotated := credential
	rotated.Token = "kodex.human-1.rotated"
	rotatedData, err := json.Marshal(map[string]any{
		"kind":       string(client.KindSession),
		"token":      rotated.Token,
		"expires_at": rotated.ExpiresAt,
	})
	if err != nil {
		t.Fatalf("marshal rotated session: %v", err)
	}
	filesystem.files[path] = rotatedData
	loaded, err = store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load(rotated): %v", err)
	}
	if loaded.Token != rotated.Token {
		t.Fatalf("Load cached the token: %q; want %q", loaded.Token, rotated.Token)
	}
}

// TestFileSessionStoreSaveRefusals pins that login persistence refuses
// non-human, missing, expired and unwritable sessions.
func TestFileSessionStoreSaveRefusals(t *testing.T) {
	path := "/home/engineer/.kodama/session.json"
	expired := humanCredential()
	expired.ExpiresAt = testNow().Add(-time.Second)
	cases := []struct {
		name       string
		credential client.Credential
		replaceErr error
	}{
		{name: "execution kind", credential: client.Credential{Kind: client.KindExecution, Token: "kodex.exec-1.opaque"}},
		{name: "service kind", credential: client.Credential{Kind: client.KindService, Token: "kodex.service-1.sig", ExpiresAt: testNow().Add(time.Hour)}},
		{name: "unknown expiry", credential: client.Credential{Kind: client.KindSession, Token: "token"}},
		{name: "empty token", credential: client.Credential{Kind: client.KindSession, Token: "", ExpiresAt: testNow().Add(time.Hour)}},
		{name: "oversized token", credential: client.Credential{Kind: client.KindSession, Token: strings.Repeat("x", api.MaxCredentialBytes+1), ExpiresAt: testNow().Add(time.Hour)}},
		{name: "expired", credential: expired},
		{name: "replace error", credential: humanCredential(), replaceErr: fs.ErrPermission},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			filesystem := newFakeFS()
			if testCase.replaceErr != nil {
				filesystem.replaceErr[path] = testCase.replaceErr
			}
			store := cli.FileSessionStore{Path: path, Files: filesystem, Now: testNow}
			if err := store.Save(context.Background(), testCase.credential); err == nil {
				t.Fatal("Save accepted an unusable session")
			}
			if _, replaced := filesystem.replaces[path]; replaced {
				t.Fatal("Save replaced the session file for an unusable session")
			}
			if _, wrote := filesystem.writes[path]; wrote {
				t.Fatal("Save wrote the session file for an unusable session")
			}
		})
	}
}

// TestFileSessionStoreSaveCreatesMissingParents pins the first-login case: the
// session store creates missing config parents 0700 without changing an
// existing directory, and the session file lands 0600.
func TestFileSessionStoreSaveCreatesMissingParents(t *testing.T) {
	filesystem := newFakeFS()
	filesystem.dirs["/home"] = 0o755
	path := "/home/engineer/.config/kodama/session.json"
	store := cli.FileSessionStore{Path: path, Files: filesystem, Now: testNow}

	if err := store.Save(context.Background(), humanCredential()); err != nil {
		t.Fatalf("Save(first login): %v", err)
	}
	for _, dir := range []string{"/home/engineer/.config", "/home/engineer/.config/kodama"} {
		mode, ok := filesystem.dirs[dir]
		if !ok {
			t.Errorf("Save did not create parent %s", dir)
			continue
		}
		if mode != 0o700 {
			t.Errorf("created parent %s mode = %o; want 700", dir, mode)
		}
	}
	if mode := filesystem.dirs["/home"]; mode != 0o755 {
		t.Errorf("existing parent mode = %o; want 755 unchanged", mode)
	}
	if mode := filesystem.modes[path]; mode != 0o600 {
		t.Errorf("session file mode = %o; want 600", mode)
	}
	for _, mode := range filesystem.tempModes {
		if mode != 0o600 {
			t.Errorf("temporary file mode = %o; want 600", mode)
		}
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load(first login): %v", err)
	}
	if loaded.Token != humanCredential().Token {
		t.Fatalf("persisted token = %q", loaded.Token)
	}
}

// TestFileSessionStoreSaveReplacesExistingFile pins that a pre-existing
// world-readable session file is replaced: the old bytes and the old mode are
// both gone.
func TestFileSessionStoreSaveReplacesExistingFile(t *testing.T) {
	filesystem := newFakeFS()
	path := "/home/engineer/.kodama/session.json"
	filesystem.files[path] = []byte(`{"kind":"session","token":"kodex.human-1.old","expires_at":"2026-02-03T05:05:06Z"}`)
	filesystem.modes[path] = 0o644
	store := cli.FileSessionStore{Path: path, Files: filesystem, Now: testNow}

	if err := store.Save(context.Background(), humanCredential()); err != nil {
		t.Fatalf("Save(rotate): %v", err)
	}
	if mode := filesystem.modes[path]; mode != 0o600 {
		t.Fatalf("replaced session file mode = %o; want 600", mode)
	}
	if bytes.Contains(filesystem.files[path], []byte("kodex.human-1.old")) {
		t.Fatalf("replaced session file kept the old token: %q", filesystem.files[path])
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load(rotate): %v", err)
	}
	if loaded.Token != humanCredential().Token {
		t.Fatalf("persisted token = %q", loaded.Token)
	}
}

// TestFileSessionStoreSaveReplaceFailure pins that a failed replace keeps the
// previous bytes and mode, reports the stable dependency class and leaks no
// token material.
func TestFileSessionStoreSaveReplaceFailure(t *testing.T) {
	filesystem := newFakeFS()
	path := "/home/engineer/.kodama/session.json"
	previous := []byte(`{"kind":"session","token":"kodex.human-1.previous","expires_at":"2026-02-03T05:05:06Z"}`)
	filesystem.files[path] = append([]byte(nil), previous...)
	filesystem.modes[path] = 0o644
	filesystem.replaceErr[path] = fs.ErrPermission
	store := cli.FileSessionStore{Path: path, Files: filesystem, Now: testNow}

	err := store.Save(context.Background(), humanCredential())
	if err == nil {
		t.Fatal("Save accepted a failed replace")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("class = %v; want dependency unavailable", class)
	}
	if strings.Contains(err.Error(), humanCredential().Token) {
		t.Fatalf("Save error leaked the token: %q", err.Error())
	}
	if got := filesystem.files[path]; string(got) != string(previous) {
		t.Fatalf("failed replace changed the session file: %q", got)
	}
	if mode := filesystem.modes[path]; mode != 0o644 {
		t.Fatalf("failed replace changed the session file mode: %o", mode)
	}
	if _, replaced := filesystem.replaces[path]; replaced {
		t.Fatal("failed replace was recorded as successful")
	}
}

// TestFileSessionStoreLoadRefusals pins that every unusable persisted session
// refuses with the stable unauthenticated class and no credential material.
func TestFileSessionStoreLoadRefusals(t *testing.T) {
	path := "/home/engineer/.kodama/session.json"
	cases := []struct {
		name    string
		content string
		missing bool
	}{
		{name: "missing", missing: true},
		{name: "malformed", content: "not json"},
		{name: "non-object", content: "[]"},
		{name: "execution kind", content: `{"kind":"execution","token":"kodex.exec-1.opaque","expires_at":"2026-02-03T05:05:06Z"}`},
		{name: "service kind", content: `{"kind":"service","token":"kodex.service-1.sig","expires_at":"2026-02-03T05:05:06Z"}`},
		{name: "empty token", content: `{"kind":"session","token":"","expires_at":"2026-02-03T05:05:06Z"}`},
		{name: "oversized token", content: `{"kind":"session","token":"` + strings.Repeat("x", api.MaxCredentialBytes+1) + `","expires_at":"2026-02-03T05:05:06Z"}`},
		{name: "zero expiry", content: `{"kind":"session","token":"token","expires_at":"0001-01-01T00:00:00Z"}`},
		{name: "expired", content: `{"kind":"session","token":"token","expires_at":"2026-02-03T04:05:05Z"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			filesystem := newFakeFS()
			if !testCase.missing {
				filesystem.files[path] = []byte(testCase.content)
			}
			store := cli.FileSessionStore{Path: path, Files: filesystem, Now: testNow}
			credential, err := store.Load(context.Background())
			if err == nil {
				t.Fatalf("Load accepted %q", testCase.content)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
				t.Fatalf("class = %v; want unauthenticated", class)
			}
			if credential.Token != "" {
				t.Fatalf("Load returned token material on refusal: %+v", credential)
			}
		})
	}
}

// TestRunLoginPersistsSessionFile pins the login command writing the durable
// session file and preserving it when refresh fails.
func TestRunLoginPersistsSessionFile(t *testing.T) {
	filesystem := newFakeFS()
	path := "/home/engineer/.kodama/session.json"
	store := cli.FileSessionStore{Path: path, Files: filesystem, Now: testNow}
	source := &scriptedSource{credentials: []client.Credential{humanCredential()}}
	ports, out, errOut := testPorts(&scriptedTransport{})
	ports.Files = filesystem
	ports.Session = store
	ports.Credential = source
	config := testConfig()
	config.CredentialFile = path

	if code := cli.Run(context.Background(), config, []string{"login"}, ports); code != cli.ExitSuccess {
		t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
	}
	if _, replaced := filesystem.replaces[path]; !replaced {
		t.Fatal("login did not persist the session file")
	}
	if strings.Contains(out.String(), humanCredential().Token) {
		t.Fatalf("stdout leaked the token: %q", out.String())
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after login: %v", err)
	}
	if loaded.Token != humanCredential().Token {
		t.Fatalf("persisted token = %q", loaded.Token)
	}
	before := append([]byte(nil), filesystem.replaces[path]...)

	// A refresh failure leaves the previous session untouched.
	failed := &scriptedSource{err: fault.New(fault.Unauthenticated, "refresh failed")}
	ports.Credential = failed
	if code := cli.Run(context.Background(), config, []string{"login"}, ports); code != cli.ExitFailure {
		t.Fatalf("refresh exit code = %d; want %d", code, cli.ExitFailure)
	}
	after := filesystem.replaces[path]
	if string(after) != string(before) {
		t.Fatal("failed refresh rewrote the persisted session")
	}
}
