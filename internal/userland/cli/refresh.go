package cli

import (
	"context"
	"errors"
	"io/fs"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// Refresh rotates one session under the session lock. The lock covers reload,
// the token call and the durable replace, so two processes never rotate the
// same refresh token twice. A session comfortably before expiry is returned
// untouched. An ambiguous rotation never replays the old refresh token: the
// session is deleted and the caller must log in again.
func Refresh(ctx context.Context, deps RefreshDeps) (SessionRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if deps.Store == nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "no session store is configured")
	}
	if deps.Lock == nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "no session lock is configured")
	}
	if deps.OIDC == nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "no OIDC client is configured")
	}
	clock := deps.Clock
	if clock == nil {
		clock = systemClock{}
	}
	release, err := deps.Lock.Acquire(ctx)
	if err != nil {
		return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the session lock could not be acquired", err)
	}
	if release != nil {
		defer func() {
			_ = release()
		}()
	}
	record, err := deps.Store.LoadRecord(ctx)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return SessionRecord{}, reauthenticationRequired("the session must be established again")
		}
		return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the session could not be loaded", err)
	}
	if record.Legacy() || record.RefreshToken == "" {
		return SessionRecord{}, reauthenticationRequired("the session cannot be refreshed")
	}
	if err := checkSessionBinding(record, deps.APIURL, deps.Issuer, deps.ClientID, deps.Audience); err != nil {
		return SessionRecord{}, err
	}
	if !record.NeedsRefresh(clock.Now()) {
		return record, nil
	}
	response, err := deps.OIDC.Token(ctx, TokenRequest{
		Endpoint:     record.TokenEndpoint,
		GrantType:    "refresh_token",
		RefreshToken: record.RefreshToken,
		ClientID:     record.ClientID,
	})
	if err != nil {
		// The request may have reached the server: the old refresh token is no
		// longer safe to replay, and the cleanup must run even if the caller's
		// context just died.
		if cleanupErr := deleteSessionRecord(ctx, deps.Store); cleanupErr != nil {
			return SessionRecord{}, fault.Wrap(fault.Unauthenticated,
				"the session must be established again and its stale file could not be removed",
				errors.Join(ErrReauthenticationRequired, cleanupErr))
		}
		return SessionRecord{}, reauthenticationRequired("the session must be established again")
	}
	if response.AccessToken == "" || response.ExpiresIn <= 0 {
		if cleanupErr := deleteSessionRecord(ctx, deps.Store); cleanupErr != nil {
			return SessionRecord{}, fault.Wrap(fault.Unauthenticated,
				"the session must be established again and its stale file could not be removed",
				errors.Join(ErrReauthenticationRequired, cleanupErr))
		}
		return SessionRecord{}, reauthenticationRequired("the session must be established again")
	}
	updated := record
	updated.Token = response.AccessToken
	if response.RefreshToken != "" {
		updated.RefreshToken = response.RefreshToken
	}
	updated.ExpiresAt = clock.Now().Add(response.ExpiresIn)
	// The rotated record must be persisted even if the caller's context died
	// during the token call, and the stale refresh token must never remain
	// replayable when the write fails.
	if err := persistSessionRecord(ctx, deps.Store, updated); err != nil {
		if cleanupErr := deleteSessionRecord(ctx, deps.Store); cleanupErr != nil {
			return SessionRecord{}, fault.Wrap(fault.Unauthenticated,
				"the rotated session could not be persisted and the stale session could not be removed",
				errors.Join(ErrReauthenticationRequired, cleanupErr))
		}
		return SessionRecord{}, reauthenticationRequired("the rotated session could not be persisted")
	}
	return updated, nil
}

// checkSessionBinding refuses a record minted for another issuer, public
// client, audience or API URL before any token or revocation call. The
// configured expectations must be complete; a missing expectation is a wiring
// failure rather than a reason to skip the check.
func checkSessionBinding(record SessionRecord, apiURL, issuer, clientID, audience string) error {
	if apiURL == "" || issuer == "" || clientID == "" || audience == "" {
		return fault.New(fault.InvalidInput, "the expected session binding is incomplete")
	}
	document := api.DiscoveryDocument{
		Issuer:   issuer,
		ClientID: clientID,
		Audience: audience,
		APIURL:   apiURL,
	}
	if err := record.BoundTo(document, apiURL); err != nil {
		return reauthenticationRequired("the session is bound to another identity provider")
	}
	return nil
}
