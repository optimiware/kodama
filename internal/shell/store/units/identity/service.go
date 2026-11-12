package identity

import (
	"context"

	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
)

// Compile-time pins: service administration and the service read seam are
// implemented by one adapter.
var (
	_ service.AdminPort     = (*Adapter)(nil)
	_ service.ServiceReader = (*Adapter)(nil)
)

// Create commits one service row with no credential pair. Credential issuance
// stays token.MutationUnit.
func (a *Adapter) Create(ctx context.Context, unit service.CreateUnit) (service.CreateResult, error) {
	if err := unit.Validate(); err != nil {
		return service.CreateResult{}, err
	}
	roles, err := encodeRoles(unit.Service.Roles)
	if err != nil {
		return service.CreateResult{}, err
	}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return service.CreateResult{}, err
	}
	commit := store.Unit{
		Locks: locks,
		Writes: []store.Write{{
			Table:  "services",
			ID:     string(unit.Service.ID),
			Insert: true,
			Values: map[string]any{
				"name":             string(unit.Service.ID),
				"state":            string(unit.Service.State),
				"roles":            roles,
				"token_nonce":      nil,
				"token_expires_at": nil,
				"version":          int64(unit.Service.Version),
				"created_at":       unit.At,
			},
		}},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	result, err := a.executor.Execute(ctx, unit.Owner, commit)
	if err != nil {
		return service.CreateResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[service.CreateResult](result); ok {
		if err != nil {
			return service.CreateResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return service.CreateResult{ServiceID: unit.Service.ID, Version: unit.Service.Version}, nil
}

// Update replaces one service's roles under its expected version and expected
// enablement. It never changes enablement or the credential pair.
func (a *Adapter) Update(ctx context.Context, unit service.UpdateUnit) (service.UpdateResult, error) {
	if err := unit.Validate(); err != nil {
		return service.UpdateResult{}, err
	}
	roles, err := encodeRoles(unit.Service.Roles)
	if err != nil {
		return service.UpdateResult{}, err
	}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return service.UpdateResult{}, err
	}
	locks = append(locks, store.Lock{
		Table:         "services",
		IDs:           []string{string(unit.ServiceID)},
		ExpectVersion: unit.ExpectedVersion,
		Expect:        map[string]any{"state": string(unit.ExpectedState)},
	})
	commit := store.Unit{
		Locks: locks,
		Writes: []store.Write{{
			Table:         "services",
			ID:            string(unit.ServiceID),
			ExpectVersion: unit.ExpectedVersion,
			Expect:        map[string]any{"state": string(unit.ExpectedState)},
			Values:        map[string]any{"roles": roles},
		}},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	result, err := a.executor.Execute(ctx, unit.Owner, commit)
	if err != nil {
		return service.UpdateResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[service.UpdateResult](result); ok {
		if err != nil {
			return service.UpdateResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return service.UpdateResult{Version: unit.Service.Version}, nil
}

// SetStatus commits one enablement change. A disablement carries the mandatory
// RevokeAll obligation and clears the service's one current nonce/expiry pair
// in the same commit, guarded by the expected service version.
func (a *Adapter) SetStatus(ctx context.Context, unit service.StatusUnit) (service.StatusResult, error) {
	if err := unit.Validate(); err != nil {
		return service.StatusResult{}, err
	}
	current, err := a.executor.Read(ctx, unit.Owner, "services", string(unit.ServiceID))
	if err != nil {
		return service.StatusResult{}, err
	}
	nonce, err := rowBytes(current, "token_nonce")
	if err != nil {
		return service.StatusResult{}, err
	}
	revoked := 0
	values := map[string]any{"state": string(unit.State)}
	if unit.State == service.StateDisabled {
		if len(nonce) > 0 {
			revoked = 1
		}
		values["token_nonce"] = nil
		values["token_expires_at"] = nil
	}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return service.StatusResult{}, err
	}
	locks = append(locks, store.Lock{
		Table:         "services",
		IDs:           []string{string(unit.ServiceID)},
		ExpectVersion: unit.ExpectedVersion,
	})
	commit := store.Unit{
		Locks: locks,
		Writes: []store.Write{{
			Table:         "services",
			ID:            string(unit.ServiceID),
			ExpectVersion: unit.ExpectedVersion,
			Values:        values,
		}},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	result, err := a.executor.Execute(ctx, unit.Owner, commit)
	if err != nil {
		return service.StatusResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[service.StatusResult](result); ok {
		if err != nil {
			return service.StatusResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return service.StatusResult{
		State:              unit.State,
		Version:            unit.ExpectedVersion + 1,
		RevokedCredentials: revoked,
	}, nil
}

// ReadService returns one service account's current committed state, roles,
// scope, version and credential metadata.
func (a *Adapter) ReadService(ctx context.Context, request service.ServiceReadRequest) (service.Service, error) {
	if err := request.Scope.Validate(); err != nil {
		return service.Service{}, err
	}
	if _, err := service.ParseServiceID(string(request.ServiceID)); err != nil {
		return service.Service{}, err
	}
	row, err := a.executor.Read(ctx, request.Scope, "services", string(request.ServiceID))
	if err != nil {
		return service.Service{}, err
	}
	return decodeService(row)
}

// decodeService rebuilds one service value from its committed row.
func decodeService(row store.Row) (service.Service, error) {
	scope, err := decodeScope(row)
	if err != nil {
		return service.Service{}, err
	}
	identity, err := rowString(row, "id")
	if err != nil {
		return service.Service{}, err
	}
	state, err := rowString(row, "state")
	if err != nil {
		return service.Service{}, err
	}
	roles, err := decodeRolesFromRow(row)
	if err != nil {
		return service.Service{}, err
	}
	version, err := rowUint(row, "version")
	if err != nil {
		return service.Service{}, err
	}
	nonce, err := rowBytes(row, "token_nonce")
	if err != nil {
		return service.Service{}, err
	}
	expiry, err := rowNullableTime(row, "token_expires_at")
	if err != nil {
		return service.Service{}, err
	}
	return service.Service{
		ID:          service.ServiceID(identity),
		Scope:       scope,
		State:       service.State(state),
		Roles:       roles,
		Version:     version,
		TokenNonce:  nonce,
		TokenExpiry: expiry,
	}, nil
}
