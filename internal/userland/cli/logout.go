package cli

import (
	"context"
	"errors"
	"io/fs"

	"github.com/optimiware/kodama/internal/fault"
)

// Logout revokes the session's refresh token at the recorded RFC 7009
// revocation endpoint and always deletes the local session file. A revocation
// failure is reported as a dependency failure after the file is deleted, and
// the token never appears in the error. Logging out without a session is a
// success. The delete and the ambiguous-revocation cleanup do not depend on the
// caller's context staying alive.
func Logout(ctx context.Context, deps LogoutDeps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if deps.Store == nil {
		return fault.New(fault.InternalFailure, "no session store is configured")
	}
	if deps.Lock == nil {
		return fault.New(fault.InternalFailure, "no session lock is configured")
	}
	release, err := deps.Lock.Acquire(ctx)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "the session lock could not be acquired", err)
	}
	if release != nil {
		defer func() {
			_ = release()
		}()
	}
	record, err := deps.Store.LoadRecord(ctx)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fault.Wrap(fault.DependencyUnavailable, "the session could not be loaded", err)
	}
	var bindingErr error
	var revokeErr error
	if record.RefreshToken != "" && record.RevocationEndpoint != "" && deps.OIDC != nil {
		// Never send a token minted for another issuer/client/audience to the
		// recorded endpoint.
		if bindingErr = checkSessionBinding(record, deps.APIURL, deps.Issuer, deps.ClientID, deps.Audience); bindingErr == nil {
			revokeErr = deps.OIDC.Revoke(ctx, RevocationRequest{
				Endpoint:      record.RevocationEndpoint,
				Token:         record.RefreshToken,
				ClientID:      record.ClientID,
				TokenTypeHint: "refresh_token",
			})
		}
	}
	deleteErr := deleteSessionRecord(ctx, deps.Store)
	if deleteErr != nil && errors.Is(deleteErr, fs.ErrNotExist) {
		deleteErr = nil
	}
	switch {
	case bindingErr != nil && deleteErr != nil:
		return fault.Wrap(fault.DependencyUnavailable, "the session is bound to another identity provider and its file could not be deleted", errors.Join(bindingErr, deleteErr))
	case bindingErr != nil:
		return bindingErr
	case revokeErr != nil && deleteErr != nil:
		return fault.Wrap(fault.DependencyUnavailable, "the refresh token could not be revoked and the session file could not be deleted", revokeErr)
	case revokeErr != nil:
		return fault.Wrap(fault.DependencyUnavailable, "the refresh token could not be revoked", revokeErr)
	case deleteErr != nil:
		return fault.Wrap(fault.DependencyUnavailable, "the session file could not be deleted", deleteErr)
	}
	return nil
}
