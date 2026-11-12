// Tests for the session-backed client credential source: it reloads the
// version 2 record on every attempt, refreshes only inside the leeway under the
// session lock, never returns an expired token and maps legacy or unusable
// sessions to a stable login-required unauthenticated error.
package cli_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/cli"
)

func sourceFor(store *commandStore, lock *commandLock, oidc *commandOIDC) cli.SessionCredentialSource {
	return cli.SessionCredentialSource{
		Store:  store,
		Lock:   lock,
		OIDC:   oidc,
		APIURL: commandAPI,
		Clock:  commandClock{now: testNow()},
	}
}

func TestSessionCredentialSourceCurrentSession(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(10 * time.Minute))}
	lock := &commandLock{}
	oidc := &commandOIDC{document: commandDocument()}
	credential, err := sourceFor(store, lock, oidc).Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if credential.Kind != client.KindSession || credential.Token != "old-access-canary" {
		t.Fatalf("credential = %+v", credential)
	}
	if !credential.ExpiresAt.Equal(testNow().Add(10 * time.Minute)) {
		t.Fatalf("expiry = %s", credential.ExpiresAt)
	}
	if len(oidc.tokenRequests()) != 0 {
		t.Fatal("a current session triggered a refresh")
	}
	if acquires, _ := lock.counts(); acquires != 0 {
		t.Fatalf("a current session acquired the session lock %d times", acquires)
	}
}

func TestSessionCredentialSourceRefreshesNearExpiry(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(30 * time.Second))}
	lock := &commandLock{}
	oidc := &commandOIDC{document: commandDocument()}
	oidc.tokenFn = func(cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{AccessToken: "fresh-access-canary", RefreshToken: "fresh-refresh-canary", ExpiresIn: time.Hour}, nil
	}
	credential, err := sourceFor(store, lock, oidc).Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if credential.Token != "fresh-access-canary" {
		t.Fatalf("credential = %+v", credential)
	}
	tokens := oidc.tokenRequests()
	if len(tokens) != 1 || tokens[0].GrantType != "refresh_token" ||
		tokens[0].Endpoint != commandIssuer+"/oauth/token" {
		t.Fatalf("token requests = %+v", tokens)
	}
	if discovered := oidc.discovered(); len(discovered) != 1 || discovered[0] != commandAPI+api.DiscoveryPath {
		t.Fatalf("discoveries = %v; want exactly the configured API discovery", discovered)
	}
	record, present := store.current()
	if !present || record.Token != "fresh-access-canary" {
		t.Fatalf("rotated record = %+v", record)
	}
}

func TestSessionCredentialSourceConcurrentRefreshUsesOneTokenCall(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(30 * time.Second))}
	lock := &commandLock{}
	oidc := &commandOIDC{document: commandDocument()}
	oidc.tokenFn = func(cli.TokenRequest) (cli.TokenResponse, error) {
		time.Sleep(20 * time.Millisecond)
		return cli.TokenResponse{AccessToken: "fresh-access-canary", RefreshToken: "fresh-refresh-canary", ExpiresIn: time.Hour}, nil
	}
	source := sourceFor(store, lock, oidc)
	start := make(chan struct{})
	type result struct {
		credential client.Credential
		err        error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			<-start
			credential, err := source.Credential(context.Background())
			results <- result{credential: credential, err: err}
		})
	}
	close(start)
	wait.Wait()
	close(results)
	for outcome := range results {
		if outcome.err != nil {
			t.Fatalf("Credential: %v", outcome.err)
		}
		if outcome.credential.Token != "fresh-access-canary" {
			t.Fatalf("credential = %+v", outcome.credential)
		}
	}
	if got := len(oidc.tokenRequests()); got != 1 {
		t.Fatalf("token calls = %d; want exactly 1", got)
	}
	if acquires, releases := lock.counts(); acquires != 2 || releases != 2 {
		t.Fatalf("lock acquires/releases = %d/%d; want 2/2", acquires, releases)
	}
}

func TestSessionCredentialSourceLoginRequired(t *testing.T) {
	cases := []struct {
		name  string
		store *commandStore
	}{
		{name: "missing session", store: &commandStore{}},
		{
			name: "legacy session",
			store: &commandStore{present: true, record: cli.SessionRecord{
				Version:   1,
				Kind:      "session",
				Token:     "legacy-access-canary",
				ExpiresAt: testNow().Add(time.Hour),
			}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			oidc := &commandOIDC{document: commandDocument()}
			credential, err := sourceFor(testCase.store, &commandLock{}, oidc).Credential(context.Background())
			if err == nil {
				t.Fatalf("Credential accepted %s: %+v", testCase.name, credential)
			}
			if class, _ := fault.ClassOf(err); class != fault.Unauthenticated {
				t.Fatalf("class = %q; want unauthenticated", class)
			}
			if !errors.Is(err, cli.ErrReauthenticationRequired) {
				t.Fatalf("error does not carry the reauthentication sentinel: %v", err)
			}
			if !strings.Contains(err.Error(), "login required") {
				t.Fatalf("error = %q; want a login-required message", err)
			}
			if credential.Token != "" {
				t.Fatalf("a refused source returned a token: %+v", credential)
			}
			if len(oidc.tokenRequests()) != 0 {
				t.Fatal("a legacy session triggered a refresh")
			}
		})
	}
}

func TestSessionCredentialSourceNeverReturnsExpiredToken(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(-time.Minute))}
	oidc := &commandOIDC{document: commandDocument()}
	oidc.tokenFn = func(cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{}, &cli.OAuthError{Code: "invalid_grant", Description: "expired"}
	}
	credential, err := sourceFor(store, &commandLock{}, oidc).Credential(context.Background())
	if err == nil {
		t.Fatalf("an expired session returned a credential: %+v", credential)
	}
	if class, _ := fault.ClassOf(err); class != fault.Unauthenticated {
		t.Fatalf("class = %q; want unauthenticated", class)
	}
	if len(oidc.tokenRequests()) != 1 {
		t.Fatalf("token calls = %d; want 1", len(oidc.tokenRequests()))
	}
	if _, present := store.current(); present {
		t.Fatal("an unrefreshable session was retained")
	}

	// A provider response with no usable lifetime is also a refusal, never a
	// replay of the stored token.
	shortStore := &commandStore{present: true, record: commandRecord(testNow().Add(-time.Minute))}
	shortOIDC := &commandOIDC{document: commandDocument()}
	shortOIDC.tokenFn = func(cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{AccessToken: "short-canary"}, nil
	}
	credential, err = sourceFor(shortStore, &commandLock{}, shortOIDC).Credential(context.Background())
	if err == nil || credential.Token != "" {
		t.Fatalf("a lifetime-less rotation returned %+v", credential)
	}
}

func TestSessionCredentialSourceRefusesForeignBinding(t *testing.T) {
	for _, remaining := range []time.Duration{30 * time.Second, 10 * time.Minute} {
		t.Run(remaining.String(), func(t *testing.T) {
			store := &commandStore{present: true, record: commandRecord(testNow().Add(remaining))}
			oidc := &commandOIDC{document: commandDocument()}
			source := sourceFor(store, &commandLock{}, oidc)
			source.Issuer = "https://other.example"
			if _, err := source.Credential(context.Background()); err == nil {
				t.Fatal("a foreign issuer binding was accepted")
			}
			if len(oidc.tokenRequests()) != 0 {
				t.Fatal("a foreign binding sent a refresh token")
			}

			foreignServer := commandRecord(testNow().Add(remaining))
			foreignServer.APIURL = "https://other-server.example"
			foreignStore := &commandStore{present: true, record: foreignServer}
			if _, err := sourceFor(foreignStore, &commandLock{}, oidc).Credential(context.Background()); err == nil {
				t.Fatal("a session minted for another API URL was accepted")
			}
		})
	}
}

func TestSessionCredentialSourceRefusesChangedBinding(t *testing.T) {
	for _, remaining := range []time.Duration{30 * time.Second, 10 * time.Minute} {
		t.Run(remaining.String(), func(t *testing.T) {
			store := &commandStore{present: true, record: commandRecord(testNow().Add(remaining))}
			oidc := &commandOIDC{document: changedBindingDocument()}
			credential, err := sourceFor(store, &commandLock{}, oidc).Credential(context.Background())
			if err == nil {
				t.Fatalf("a changed provider binding returned %+v", credential)
			}
			if class, _ := fault.ClassOf(err); class != fault.Unauthenticated {
				t.Fatalf("class = %q; want unauthenticated", class)
			}
			if !errors.Is(err, cli.ErrReauthenticationRequired) || !strings.Contains(err.Error(), "login required") {
				t.Fatalf("error = %v; want login required", err)
			}
			if len(oidc.discovered()) != 1 {
				t.Fatalf("discoveries = %d; want 1", len(oidc.discovered()))
			}
			if len(oidc.tokenRequests()) != 0 {
				t.Fatal("a changed provider binding sent the refresh token")
			}
			if record, present := store.current(); !present || record.Token != "old-access-canary" {
				t.Fatalf("a refused binding check mutated the record: %+v", record)
			}
			if strings.Contains(err.Error(), refreshCanary) || strings.Contains(err.Error(), accessCanary) {
				t.Fatalf("refusal rendered a token: %v", err)
			}
		})
	}
}

func TestSessionCredentialSourceRefusesDiscoveryFailure(t *testing.T) {
	for _, remaining := range []time.Duration{30 * time.Second, 10 * time.Minute} {
		t.Run(remaining.String(), func(t *testing.T) {
			store := &commandStore{present: true, record: commandRecord(testNow().Add(remaining))}
			oidc := &commandOIDC{document: commandDocument(), discoveryErr: errors.New("discovery down")}
			_, err := sourceFor(store, &commandLock{}, oidc).Credential(context.Background())
			if err == nil {
				t.Fatal("a failed discovery returned a credential")
			}
			if class, _ := fault.ClassOf(err); class != fault.Unauthenticated {
				t.Fatalf("class = %q; want unauthenticated", class)
			}
			if len(oidc.tokenRequests()) != 0 {
				t.Fatal("a failed discovery sent the refresh token")
			}
		})
	}
}

func TestSessionCredentialSourceRefusesAPIURLMismatch(t *testing.T) {
	for _, remaining := range []time.Duration{30 * time.Second, 10 * time.Minute} {
		t.Run(remaining.String(), func(t *testing.T) {
			store := &commandStore{present: true, record: commandRecord(testNow().Add(remaining))}
			oidc := &commandOIDC{document: otherAPIDocument()}
			_, err := sourceFor(store, &commandLock{}, oidc).Credential(context.Background())
			if err == nil {
				t.Fatal("a discovery bound to another API URL returned a credential")
			}
			if len(oidc.tokenRequests()) != 0 {
				t.Fatal("a discovery bound to another API URL sent the refresh token")
			}
		})
	}
}

func TestSessionCredentialSourceRefusesMissingTrustInputs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*cli.SessionCredentialSource, *commandOIDC)
	}{
		{
			name: "nil OIDC client",
			mutate: func(source *cli.SessionCredentialSource, _ *commandOIDC) {
				source.OIDC = nil
			},
		},
		{
			name: "empty API URL",
			mutate: func(source *cli.SessionCredentialSource, _ *commandOIDC) {
				source.APIURL = ""
			},
		},
	}
	for _, remaining := range []time.Duration{30 * time.Second, 10 * time.Minute} {
		for _, testCase := range cases {
			t.Run(testCase.name+"/"+remaining.String(), func(t *testing.T) {
				store := &commandStore{present: true, record: commandRecord(testNow().Add(remaining))}
				oidc := &commandOIDC{document: commandDocument()}
				source := sourceFor(store, &commandLock{}, oidc)
				testCase.mutate(&source, oidc)
				if _, err := source.Credential(context.Background()); err == nil {
					t.Fatalf("%s returned a credential", testCase.name)
				}
				if len(oidc.tokenRequests()) != 0 {
					t.Fatalf("%s sent the refresh token", testCase.name)
				}
			})
		}
	}
}

func TestSessionCredentialSourceSurfacesDependencyFailure(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(30 * time.Second))}
	lock := &commandLock{acquireErr: errors.New("lock unavailable")}
	oidc := &commandOIDC{document: commandDocument()}
	_, err := sourceFor(store, lock, oidc).Credential(context.Background())
	if err == nil {
		t.Fatal("a lock failure returned a credential")
	}
	if class, _ := fault.ClassOf(err); class != fault.DependencyUnavailable {
		t.Fatalf("class = %q; want dependency_unavailable", class)
	}
}

// stringsContains avoids importing strings in every assertion helper.
