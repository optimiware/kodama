// Refresh and rotation vectors.
//
// A rotating refresh token is only safe under one cross-process lock covering
// reload → refresh → ReplaceFile. An ambiguous refresh is never replayed with
// the old token: the session becomes re-authentication-required instead.
package publiccontract

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/cli"
)

func (fixture *loginFixture) refreshDeps() cli.RefreshDeps {
	return cli.RefreshDeps{
		APIURL:   fixture.discovery.APIURL,
		Issuer:   fixture.discovery.Issuer,
		ClientID: fixture.discovery.ClientID,
		Audience: fixture.discovery.Audience,
		OIDC:     fixture.oidc,
		Clock:    fixture.clock,
		Lock:     fixture.lock,
		Store:    fixture.store,
	}
}

// TestRefreshLeewayBoundary pins when a session is refreshed before expiry.
func TestRefreshLeewayBoundary(t *testing.T) {
	if cli.RefreshLeeway != time.Minute {
		t.Fatalf("RefreshLeeway = %s; want 1m", cli.RefreshLeeway)
	}
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	record := loginRecord(now)
	record.ExpiresAt = now.Add(cli.RefreshLeeway)
	if !record.NeedsRefresh(now) {
		t.Error("session expiring exactly at the leeway boundary does not need refresh")
	}
	record.ExpiresAt = now.Add(cli.RefreshLeeway + time.Nanosecond)
	if record.NeedsRefresh(now) {
		t.Error("session expiring beyond the leeway needs refresh")
	}
}

// TestRefreshRotation pins a successful rotation: the new access and refresh
// tokens are persisted under the lock and the old refresh token is sent once.
func TestRefreshRotation(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{AccessToken: "AT-NEW", RefreshToken: "RT-NEW", ExpiresIn: time.Hour}, nil
	}

	updated, err := cli.Refresh(context.Background(), fixture.refreshDeps())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	requests := fixture.oidc.issuedTokens()
	if len(requests) != 1 {
		t.Fatalf("token endpoint called %d times; want 1", len(requests))
	}
	request := requests[0]
	if request.GrantType != "refresh_token" || request.RefreshToken != "RT-CANARY" ||
		request.ClientID != fixture.discovery.ClientID {
		t.Errorf("refresh request = %+v", request)
	}
	if request.Endpoint != fixture.discovery.TokenEndpoint {
		t.Errorf("refresh endpoint = %q; want the recorded %q", request.Endpoint, fixture.discovery.TokenEndpoint)
	}
	if updated.Token != "AT-NEW" || updated.RefreshToken != "RT-NEW" {
		t.Errorf("rotated record tokens = %q/%q", updated.Token, updated.RefreshToken)
	}
	if !updated.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("rotated expiry = %s; want %s", updated.ExpiresAt, now.Add(time.Hour))
	}
	if updated.Issuer != fixture.discovery.Issuer || updated.APIURL != fixture.discovery.APIURL {
		t.Errorf("rotation lost the session binding: %+v", updated)
	}
	saved := fixture.store.saved()
	if len(saved) != 1 || saved[0].RefreshToken != "RT-NEW" {
		t.Fatalf("persisted rotations = %+v; want exactly the new refresh token", saved)
	}
	acquisitions, releases := fixture.lock.counts()
	if acquisitions != 1 || releases != 1 {
		t.Errorf("lock acquisitions/releases = %d/%d; want 1/1", acquisitions, releases)
	}
}

// TestRefreshConcurrentRotation pins that two processes refreshing the same
// session perform exactly one token rotation.
func TestRefreshConcurrentRotation(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	var callsMu sync.Mutex
	calls := 0
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		return cli.TokenResponse{AccessToken: "AT-NEW", RefreshToken: "RT-NEW", ExpiresIn: time.Hour}, nil
	}

	var wait sync.WaitGroup
	results := make([]cli.SessionRecord, 2)
	errs := make([]error, 2)
	for index := 0; index < 2; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index], errs[index] = cli.Refresh(context.Background(), fixture.refreshDeps())
		}()
	}
	wait.Wait()
	for index := range errs {
		if errs[index] != nil {
			t.Fatalf("concurrent Refresh[%d]: %v", index, errs[index])
		}
		if results[index].Token != "AT-NEW" || results[index].RefreshToken != "RT-NEW" {
			t.Errorf("concurrent result[%d] = %+v", index, results[index])
		}
	}
	callsMu.Lock()
	total := calls
	callsMu.Unlock()
	if total != 1 {
		t.Fatalf("token endpoint called %d times; want exactly one rotation", total)
	}
	record, present := fixture.store.current()
	if !present || record.RefreshToken != "RT-NEW" {
		t.Fatalf("stored record = %+v; want the single rotated token", record)
	}
}

// TestRefreshAmbiguousRequiresReauthentication pins that an uncertain rotation
// never replays the old refresh token: the session drops its refresh token and
// a later refresh refuses without another token call.
func TestRefreshAmbiguousRequiresReauthentication(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{}, errors.New("connection reset after request write")
	}

	_, err := cli.Refresh(context.Background(), fixture.refreshDeps())
	if err == nil {
		t.Fatal("Refresh accepted an ambiguous token endpoint failure")
	}
	if !errors.Is(err, cli.ErrReauthenticationRequired) {
		t.Errorf("ambiguous error = %v; want reauthentication required", err)
	}
	// The session may be deleted or kept as an unusable access-token-only
	// record; the one forbidden outcome is leaving the old refresh token.
	if record, present := fixture.store.current(); present && record.RefreshToken != "" {
		t.Errorf("ambiguous refresh kept refresh token %q; it must not be replayed", record.RefreshToken)
	}
	requests := fixture.oidc.issuedTokens()
	if len(requests) != 1 || requests[0].RefreshToken != "RT-CANARY" {
		t.Fatalf("first refresh requests = %+v", requests)
	}

	_, err = cli.Refresh(context.Background(), fixture.refreshDeps())
	if !errors.Is(err, cli.ErrReauthenticationRequired) {
		t.Errorf("second refresh error = %v; want reauthentication required", err)
	}
	if calls := len(fixture.oidc.issuedTokens()); calls != 1 {
		t.Errorf("second refresh replayed the old token: %d token calls", calls)
	}
}

// TestRefreshLegacyRefused pins that a legacy access-token-only session cannot
// be refreshed and requires a new login.
func TestRefreshLegacyRefused(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	record := loginRecord(now)
	record.Version = 1
	record.RefreshToken = ""
	record.Issuer = ""
	record.ClientID = ""
	record.Audience = ""
	record.APIURL = ""
	record.TokenEndpoint = ""
	record.RevocationEndpoint = ""
	fixture.store.record = record
	fixture.store.present = true
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		t.Fatal("legacy session reached the token endpoint")
		return cli.TokenResponse{}, nil
	}
	if _, err := cli.Refresh(context.Background(), fixture.refreshDeps()); !errors.Is(err, cli.ErrReauthenticationRequired) {
		t.Fatalf("legacy refresh error = %v; want reauthentication required", err)
	}
	if calls := len(fixture.oidc.issuedTokens()); calls != 0 {
		t.Errorf("legacy refresh called the token endpoint %d times", calls)
	}
}

// TestRefreshBindingMismatchRefused pins that a session minted for another API
// URL is never replayed against this one.
func TestRefreshBindingMismatchRefused(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	record := loginRecord(now)
	record.APIURL = "https://other.example.com"
	fixture.store.record = record
	fixture.store.present = true
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		t.Fatal("foreign session reached the token endpoint")
		return cli.TokenResponse{}, nil
	}
	if _, err := cli.Refresh(context.Background(), fixture.refreshDeps()); !errors.Is(err, cli.ErrReauthenticationRequired) {
		t.Fatalf("binding mismatch error = %v; want reauthentication required", err)
	}
}

// TestRefreshNotNeeded pins that a session comfortably before expiry is
// returned without contacting the token endpoint.
func TestRefreshNotNeeded(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	record := loginRecord(now)
	record.ExpiresAt = now.Add(2 * time.Hour)
	fixture.store.record = record
	fixture.store.present = true
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		t.Fatal("current session reached the token endpoint")
		return cli.TokenResponse{}, nil
	}
	updated, err := cli.Refresh(context.Background(), fixture.refreshDeps())
	if err != nil {
		t.Fatalf("Refresh(current): %v", err)
	}
	if updated.Token != record.Token || updated.RefreshToken != record.RefreshToken {
		t.Errorf("Refresh(current) changed the record: %+v", updated)
	}
	if len(fixture.store.saved()) != 0 {
		t.Error("Refresh(current) rewrote the session file")
	}
}

// TestRefreshChangedBindingRefused pins that a session bound to another issuer,
// client or audience is refused before any token call, whatever the token
// endpoint adapter might otherwise do.
func TestRefreshChangedBindingRefused(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	mutations := map[string]func(*cli.RefreshDeps){
		"issuer":   func(deps *cli.RefreshDeps) { deps.Issuer = "https://other.example.com" },
		"client":   func(deps *cli.RefreshDeps) { deps.ClientID = "other-client" },
		"audience": func(deps *cli.RefreshDeps) { deps.Audience = "other-api" },
	}
	for name, mutate := range mutations {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			fixture := newLoginFixture(now)
			fixture.store.record = loginRecord(now)
			fixture.store.present = true
			fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
				t.Fatal("a session with a changed binding reached the token endpoint")
				return cli.TokenResponse{}, nil
			}
			deps := fixture.refreshDeps()
			mutate(&deps)
			if _, err := cli.Refresh(context.Background(), deps); !errors.Is(err, cli.ErrReauthenticationRequired) {
				t.Fatalf("changed %s error = %v; want reauthentication required", name, err)
			}
			if calls := len(fixture.oidc.issuedTokens()); calls != 0 {
				t.Errorf("changed %s called the token endpoint %d times", name, calls)
			}
		})
	}
}

// TestRefreshAmbiguousCanceledContextDoesNotReplay pins the cleanup contract
// when the token call itself dies with the caller's context: the old refresh
// token must not remain in the store, and a later refresh must not replay it.
func TestRefreshAmbiguousCanceledContextDoesNotReplay(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	ctx, cancel := context.WithCancel(context.Background())
	fixture.oidc.tokenFn = func(callCtx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		cancel()
		return cli.TokenResponse{}, callCtx.Err()
	}

	_, err := cli.Refresh(ctx, fixture.refreshDeps())
	if err == nil {
		t.Fatal("Refresh accepted a canceled ambiguous token call")
	}
	if !errors.Is(err, cli.ErrReauthenticationRequired) {
		t.Errorf("canceled ambiguous error = %v; want reauthentication required", err)
	}
	// A dead caller context must not make the cleanup silently skip the file.
	if record, present := fixture.store.current(); present && record.RefreshToken != "" {
		t.Fatalf("canceled ambiguous refresh kept refresh token %q", record.RefreshToken)
	}
	if _, err := cli.Refresh(context.Background(), fixture.refreshDeps()); !errors.Is(err, cli.ErrReauthenticationRequired) {
		t.Errorf("second refresh error = %v; want reauthentication required", err)
	}
	if calls := len(fixture.oidc.issuedTokens()); calls != 1 {
		t.Errorf("second refresh replayed the old token: %d token calls", calls)
	}
}

// TestRefreshPersistFailureDropsOldToken pins that a rotated token which cannot
// be persisted never leaves the old refresh token replayable.
func TestRefreshPersistFailureDropsOldToken(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	fixture.store.saveErr = errors.New("disk full")
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{AccessToken: "AT-NEW", RefreshToken: "RT-NEW", ExpiresIn: time.Hour}, nil
	}
	_, err := cli.Refresh(context.Background(), fixture.refreshDeps())
	if !errors.Is(err, cli.ErrReauthenticationRequired) {
		t.Fatalf("persist failure error = %v; want reauthentication required", err)
	}
	if record, present := fixture.store.current(); present && record.RefreshToken != "" {
		t.Fatalf("persist failure kept the old refresh token %q", record.RefreshToken)
	}
	if _, err := cli.Refresh(context.Background(), fixture.refreshDeps()); !errors.Is(err, cli.ErrReauthenticationRequired) {
		t.Errorf("second refresh error = %v; want reauthentication required", err)
	}
	if calls := len(fixture.oidc.issuedTokens()); calls != 1 {
		t.Errorf("second refresh replayed the old token: %d token calls", calls)
	}
}

// TestRefreshPersistsAfterContextCanceled pins that a successful rotation is
// still persisted when the caller's context dies before the write.
func TestRefreshPersistsAfterContextCanceled(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	ctx, cancel := context.WithCancel(context.Background())
	fixture.oidc.tokenFn = func(callCtx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		cancel()
		return cli.TokenResponse{AccessToken: "AT-NEW", RefreshToken: "RT-NEW", ExpiresIn: time.Hour}, nil
	}
	updated, err := cli.Refresh(ctx, fixture.refreshDeps())
	if err != nil {
		t.Fatalf("Refresh after a completed rotation: %v", err)
	}
	if updated.Token != "AT-NEW" || updated.RefreshToken != "RT-NEW" {
		t.Errorf("rotated record = %+v", updated)
	}
	record, present := fixture.store.current()
	if !present || record.RefreshToken != "RT-NEW" {
		t.Fatalf("rotated session was not persisted: %+v (present=%v)", record, present)
	}
}

// TestRefreshCleanupFailureSurfaced pins that a failed removal of a stale
// session is reported instead of being silently ignored.
func TestRefreshCleanupFailureSurfaced(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	fixture.store.deleteErr = errors.New("permission denied")
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{}, errors.New("connection reset after request write")
	}
	if _, err := cli.Refresh(context.Background(), fixture.refreshDeps()); !errors.Is(err, cli.ErrReauthenticationRequired) {
		t.Fatalf("cleanup failure error = %v; want reauthentication required", err)
	}
	if calls := len(fixture.oidc.issuedTokens()); calls != 1 {
		t.Errorf("token endpoint called %d times; want 1", calls)
	}
}
