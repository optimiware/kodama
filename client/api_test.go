// Credential tests for package client: the unknown-expiry rule for mounted
// execution credentials (Credential.Validate, Credential.Usable and Bearer)
// and FileCredentialSource, which reads the mounted token file again on every
// call and refuses with the unauthenticated class. The HTTPTransport tests are
// in http_test.go and inline_start_test.go.
//
// fixedNow and fakeSource below are shared by every test file in the package.
package client_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
)

// fixedNow is the authoritative test clock.
func fixedNow() time.Time {
	return time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
}

// fakeSource is a scripted CredentialSource.
type fakeSource struct {
	mu          sync.Mutex
	credentials []client.Credential
	err         error
	calls       int
}

func (source *fakeSource) Credential(context.Context) (client.Credential, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	if source.err != nil {
		return client.Credential{}, source.err
	}
	if len(source.credentials) == 0 {
		return client.Credential{}, fault.New(fault.Unauthenticated, "no credential")
	}
	index := source.calls - 1
	if index >= len(source.credentials) {
		index = len(source.credentials) - 1
	}
	return source.credentials[index], nil
}

func (source *fakeSource) callCount() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls
}

// TestCredentialUnknownExpiryOnlyForExecution pins the unknown-expiry decision:
// an opaque mounted execution credential may omit its expiry, but a human
// session or service credential may not, and an explicit expiry still wins.
func TestCredentialUnknownExpiryOnlyForExecution(t *testing.T) {
	now := fixedNow()

	unknown := client.Credential{Kind: client.KindExecution, Token: "kodex.exec-1.opaque"}
	if err := unknown.Validate(); err != nil {
		t.Fatalf("unknown-expiry execution credential refused: %v", err)
	}
	if !unknown.Usable(now) {
		t.Fatal("unknown-expiry execution credential is not usable")
	}
	if strings.Contains(unknown.String(), unknown.Token) {
		t.Fatal("unknown-expiry credential String() leaked the token")
	}

	for _, kind := range []client.CredentialKind{client.KindSession, client.KindService} {
		credential := client.Credential{Kind: kind, Token: "token"}
		if err := credential.Validate(); err == nil {
			t.Errorf("%s credential with unknown expiry was accepted", kind)
		}
		if credential.Usable(now) {
			t.Errorf("%s credential with unknown expiry is usable", kind)
		}
	}

	boundaries := []struct {
		name      string
		expiresAt time.Time
		usable    bool
	}{
		{"before", now.Add(-time.Nanosecond), false},
		{"equal", now, false},
		{"after", now.Add(time.Nanosecond), true},
	}
	for _, boundary := range boundaries {
		credential := client.Credential{Kind: client.KindExecution, Token: "token", ExpiresAt: boundary.expiresAt}
		if err := credential.Validate(); err != nil {
			t.Errorf("%s: explicit expiry refused: %v", boundary.name, err)
			continue
		}
		if got := credential.Usable(now); got != boundary.usable {
			t.Errorf("%s: Usable(now) = %v; want %v", boundary.name, got, boundary.usable)
		}
	}
}

// TestBearerUnknownExpiryExecution pins that the unknown-expiry rule reaches
// Bearer: a mounted execution token authenticates, while a session with no
// expiry still refuses.
func TestBearerUnknownExpiryExecution(t *testing.T) {
	now := fixedNow()
	execution := &fakeSource{credentials: []client.Credential{
		{Kind: client.KindExecution, Token: "kodex.exec-1.opaque"},
	}}
	header, err := client.Bearer(context.Background(), execution, now)
	if err != nil {
		t.Fatalf("Bearer(unknown-expiry execution): %v", err)
	}
	if header != "Bearer kodex.exec-1.opaque" {
		t.Fatalf("Bearer header = %q", header)
	}

	session := &fakeSource{credentials: []client.Credential{
		{Kind: client.KindSession, Token: "human-token"},
	}}
	if header, err := client.Bearer(context.Background(), session, now); err == nil {
		t.Fatalf("Bearer accepted a session credential with unknown expiry: %q", header)
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
		t.Fatalf("unknown-expiry session class = %v; want unauthenticated", class)
	}
}

// TestFileCredentialSourceReadsFreshEachCall pins that a mounted execution
// token file is read again for every authentication and never caches.
func TestFileCredentialSourceReadsFreshEachCall(t *testing.T) {
	contents := []string{"first-token\n", " second-token \r\n"}
	var paths []string
	calls := 0
	source := client.FileCredentialSource{
		Path: "/run/kodama/token",
		ReadFile: func(path string) ([]byte, error) {
			paths = append(paths, path)
			content := contents[calls]
			calls++
			return []byte(content), nil
		},
	}

	first, err := source.Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential(first): %v", err)
	}
	second, err := source.Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential(second): %v", err)
	}
	if first.Token != "first-token" || second.Token != "second-token" {
		t.Fatalf("tokens = %q, %q; want trimmed fresh reads", first.Token, second.Token)
	}
	if first.Kind != client.KindExecution || second.Kind != client.KindExecution {
		t.Fatalf("kinds = %q, %q; want execution", first.Kind, second.Kind)
	}
	if !first.ExpiresAt.IsZero() || !second.ExpiresAt.IsZero() {
		t.Fatal("mounted execution credential fabricated an expiry")
	}
	if !first.Usable(fixedNow()) {
		t.Fatal("mounted execution credential is not usable")
	}
	if len(paths) != 2 || paths[0] != "/run/kodama/token" || paths[1] != "/run/kodama/token" {
		t.Fatalf("read paths = %v", paths)
	}
}

// TestFileCredentialSourceRefusals pins the mounted-file refusals. A source
// failure is always the stable unauthenticated class, never a private cause.
func TestFileCredentialSourceRefusals(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		content string
		readErr error
	}{
		{name: "read error", path: "/run/kodama/token", readErr: fault.New(fault.DependencyUnavailable, "read failed")},
		{name: "empty", path: "/run/kodama/token", content: ""},
		{name: "whitespace", path: "/run/kodama/token", content: " \r\n\t"},
		{name: "oversized", path: "/run/kodama/token", content: strings.Repeat("x", api.MaxCredentialBytes+1)},
		{name: "empty path", path: "", content: "token"},
	}
	for _, testCase := range cases {
		source := client.FileCredentialSource{
			Path: testCase.path,
			ReadFile: func(string) ([]byte, error) {
				if testCase.readErr != nil {
					return nil, testCase.readErr
				}
				return []byte(testCase.content), nil
			},
		}
		credential, err := source.Credential(context.Background())
		if err == nil {
			t.Errorf("%s: Credential accepted %+v", testCase.name, credential)
			continue
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Errorf("%s: class = %v; want unauthenticated", testCase.name, class)
		}
		if !credential.ExpiresAt.IsZero() || credential.Token != "" {
			t.Errorf("%s: refusal carried credential material %+v", testCase.name, credential)
		}
	}

	source := client.FileCredentialSource{Path: "/run/kodama/token", ReadFile: func(string) ([]byte, error) {
		return []byte("token"), nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Credential(ctx); err == nil {
		t.Fatal("Credential accepted a canceled context")
	}
}
