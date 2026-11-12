package identity

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// Compile-time pins: every credential port is implemented by one adapter.
var (
	_ token.MutationPort     = (*Adapter)(nil)
	_ token.CredentialReader = (*Adapter)(nil)
	_ token.TenantKeyReader  = (*Adapter)(nil)
)

// Mutate replaces or clears one principal's current nonce/expiry pair under its
// expected version. A service mutation may also carry the next enablement
// and/or role set so service administration commits with the pair instead of a
// second store call.
func (a *Adapter) Mutate(ctx context.Context, unit token.MutationUnit) (token.MutationResult, error) {
	if err := unit.Owner.Validate(); err != nil {
		return token.MutationResult{}, err
	}
	if _, err := id.ParsePrincipalID(string(unit.PrincipalID)); err != nil {
		return token.MutationResult{}, err
	}
	if !unit.Kind.Valid() {
		return token.MutationResult{}, fault.New(fault.InvalidInput, "credential mutation kind is not frozen")
	}
	if unit.ExpectedVersion == 0 {
		return token.MutationResult{}, fault.New(fault.InvalidInput, "credential mutation expected version is zero")
	}
	switch {
	case unit.ExecutionID != "" && unit.ServiceID != "":
		return token.MutationResult{}, fault.New(fault.InvalidInput, "credential mutation names both an execution and a service")
	case unit.ExecutionID != "":
		if _, err := id.ParseExecutionID(string(unit.ExecutionID)); err != nil {
			return token.MutationResult{}, err
		}
	case unit.ServiceID != "":
		if _, err := service.ParseServiceID(string(unit.ServiceID)); err != nil {
			return token.MutationResult{}, err
		}
	default:
		return token.MutationResult{}, fault.New(fault.InvalidInput, "credential mutation names neither an execution nor a service")
	}
	if unit.Kind == token.MutationRevoke {
		if unit.Credential.Present() {
			return token.MutationResult{}, fault.New(fault.InvalidInput, "a credential revocation must not carry a nonce/expiry pair")
		}
	} else if !unit.Credential.Present() {
		return token.MutationResult{}, fault.New(fault.InvalidInput, "a credential issue or rotation requires a complete nonce/expiry pair")
	}
	if unit.Service != nil {
		if unit.ServiceID == "" {
			return token.MutationResult{}, fault.New(fault.InvalidInput, "a service change requires a service target")
		}
		if err := unit.Service.Validate(); err != nil {
			return token.MutationResult{}, err
		}
	}

	table, target := "executions", string(unit.ExecutionID)
	if unit.ServiceID != "" {
		// The credential-lookup seam resolves a service credential's principal
		// as the service identity, so a service mutation's principal and
		// service identity must be the same value. A different principal could
		// never be recovered from the row.
		if unit.PrincipalID != id.PrincipalID(unit.ServiceID) {
			return token.MutationResult{}, fault.New(fault.InvalidInput, "a service credential mutation principal must be the service identity")
		}
		table, target = "services", string(unit.ServiceID)
	}
	values := map[string]any{
		"token_nonce":      nullableNonce(unit.Credential),
		"token_expires_at": nullableExpiry(unit.Credential),
	}
	disabled := false
	if unit.Service != nil {
		if unit.Service.State != nil {
			values["state"] = string(*unit.Service.State)
			disabled = *unit.Service.State == service.StateDisabled
		}
		if unit.Service.Roles != nil {
			roles, err := encodeRoles(unit.Service.Roles)
			if err != nil {
				return token.MutationResult{}, err
			}
			values["roles"] = roles
		}
	}
	// A disabled service can never keep a live credential pair, whatever the
	// unit kind: the clear commits with the disabled state.
	if disabled {
		values["token_nonce"] = nil
		values["token_expires_at"] = nil
	}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return token.MutationResult{}, err
	}
	// An execution credential names the execution row and its stored principal;
	// the principal column is the exact durable mapping of the unit's
	// PrincipalID, so a unit that names another principal is refused at commit.
	var expect map[string]any
	if table == "executions" {
		expect = map[string]any{"principal_id": string(unit.PrincipalID)}
	}
	locks = append(locks, store.Lock{
		Table:         table,
		IDs:           []string{target},
		ExpectVersion: unit.ExpectedVersion,
		Expect:        expect,
	})
	commit := store.Unit{
		Locks: locks,
		Writes: []store.Write{{
			Table:         table,
			ID:            target,
			ExpectVersion: unit.ExpectedVersion,
			Expect:        expect,
			Values:        values,
		}},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	result, err := a.executor.Execute(ctx, unit.Owner, commit)
	if err != nil {
		return token.MutationResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[token.MutationResult](result); ok {
		if err != nil {
			return token.MutationResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	state := token.StateCurrent
	if unit.Kind == token.MutationRevoke || disabled {
		state = token.StateAbsent
	}
	return token.MutationResult{State: state, Version: unit.ExpectedVersion + 1}, nil
}

// ReadCredential returns one principal's current committed credential pair and
// enablement at the authentication boundary.
func (a *Adapter) ReadCredential(ctx context.Context, request token.CredentialRequest) (token.CredentialFact, error) {
	if err := request.Validate(); err != nil {
		return token.CredentialFact{}, err
	}
	table, target := "executions", string(request.ExecutionID)
	if request.ServiceID != "" {
		table, target = "services", string(request.ServiceID)
	}
	row, err := a.executor.Read(ctx, request.Scope, table, target)
	if err != nil {
		return token.CredentialFact{}, err
	}
	state, err := rowString(row, "state")
	if err != nil {
		return token.CredentialFact{}, err
	}
	version, err := rowUint(row, "version")
	if err != nil {
		return token.CredentialFact{}, err
	}
	nonce, err := rowBytes(row, "token_nonce")
	if err != nil {
		return token.CredentialFact{}, err
	}
	expiry, err := rowNullableTime(row, "token_expires_at")
	if err != nil {
		return token.CredentialFact{}, err
	}
	credential := token.Credential{Nonce: nonce}
	if expiry != nil {
		credential.Expiry = *expiry
	}
	enabled := state == executionStateRunning
	var principal id.PrincipalID
	if table == "services" {
		enabled = state == serviceStateEnabled
		// A service credential's principal is the service identity, the
		// only principal identity the services row can carry.
		principal = id.PrincipalID(request.ServiceID)
	} else {
		stored, err := rowString(row, "principal_id")
		if err != nil {
			return token.CredentialFact{}, err
		}
		principal = id.PrincipalID(stored)
	}
	return token.CredentialFact{
		Scope:       request.Scope,
		PrincipalID: principal,
		ExecutionID: request.ExecutionID,
		ServiceID:   request.ServiceID,
		Enabled:     enabled,
		Version:     version,
		Credential:  credential,
	}, nil
}

// ReadTenantKey returns one tenant's current encrypted derivation key. The
// returned value carries only ciphertext and key-provider references, never
// plaintext key material.
func (a *Adapter) ReadTenantKey(ctx context.Context, request token.TenantKeyRequest) (token.TenantKey, error) {
	if err := request.Validate(); err != nil {
		return token.TenantKey{}, err
	}
	scope := tenancy.Scope{TenantID: request.TenantID}
	row, err := a.executor.Read(ctx, scope, "tenants", string(request.TenantID))
	if err != nil {
		return token.TenantKey{}, err
	}
	data, err := rowBytes(row, "derivation_key")
	if err != nil {
		return token.TenantKey{}, err
	}
	var key token.TenantKey
	if err := decodeJSON(data, &key); err != nil {
		return token.TenantKey{}, err
	}
	if key.TenantID != request.TenantID {
		return token.TenantKey{}, fault.New(fault.InternalFailure, "stored derivation key belongs to another tenant")
	}
	if err := key.Validate(); err != nil {
		return token.TenantKey{}, err
	}
	return key, nil
}

// nullableNonce renders the nonce column value. A revocation clears the pair,
// so an absent nonce is SQL NULL.
func nullableNonce(credential token.Credential) any {
	if !credential.Present() {
		return nil
	}
	return credential.Nonce
}

// nullableExpiry renders the expiry column value.
func nullableExpiry(credential token.Credential) any {
	if !credential.Present() {
		return nil
	}
	return credential.Expiry
}
