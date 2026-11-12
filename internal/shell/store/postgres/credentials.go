// Pre-authentication credential lookup over the kodama_auth role.
//
// The lookup answers exactly one exact-kind/exact-id question and returns a
// candidate scope that is never authority. The role has exact-column SELECT on
// the two credential-bearing tables and an exact_row RLS policy on the bound id,
// so it cannot list, range-scan or mutate a row.
package postgres

import (
	"context"
	"database/sql"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
)

// Credentials implements store.CredentialLookup over the auth-role pool.
type Credentials struct {
	db DB
}

// NewCredentialLookup builds the read-only pre-authentication lookup over the
// kodama_auth pool.
func NewCredentialLookup(db DB) *Credentials {
	return &Credentials{db: db}
}

var _ store.CredentialLookup = (*Credentials)(nil)

// establishCredentialSQL sets the transaction-local exact-row identity the auth
// role's exact_row policy reads. The third set_config argument is true, so the
// value resets when the lookup transaction completes.
const establishCredentialSQL = `SELECT set_config('` + scopeCredentialSetting + `', $1, true)`

// uniformCredentialRefusal is the single indistinguishable refusal for a missing
// row and a row of a different kind.
func uniformCredentialRefusal() error {
	return fault.New(fault.Unauthenticated, "credential candidate is not available")
}

// LookupCredential returns the candidate scope of exactly one credential. A
// missing row and a row of a different kind return the same unauthenticated
// error, so the lookup cannot enumerate credentials. It never returns token
// material and never evaluates a grant.
func (credentials *Credentials) LookupCredential(ctx context.Context, kind store.CredentialKind, credentialID string) (store.CandidateScope, error) {
	if err := ctx.Err(); err != nil {
		return store.CandidateScope{}, err
	}
	if !kind.Valid() {
		return store.CandidateScope{}, faultInvalid("credential kind is not frozen")
	}
	if !validIdentity(credentialID) {
		return store.CandidateScope{}, uniformCredentialRefusal()
	}
	tx, err := beginUnscoped(ctx, credentials.db, true)
	if err != nil {
		return store.CandidateScope{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, store.Statement{SQL: establishCredentialSQL, Args: []any{credentialID}}); err != nil {
		return store.CandidateScope{}, err
	}
	var candidate store.CandidateScope
	switch kind {
	case store.CredentialExecution:
		candidate, err = lookupExecutionCredential(ctx, tx, credentialID)
	case store.CredentialService:
		candidate, err = lookupServiceCredential(ctx, tx, credentialID)
	}
	if err != nil {
		return store.CandidateScope{}, err
	}
	if err := candidate.Validate(); err != nil {
		return store.CandidateScope{}, fault.Wrap(fault.InternalFailure, "durable credential row has an invalid scope", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CandidateScope{}, err
	}
	return candidate, nil
}

func lookupExecutionCredential(ctx context.Context, tx store.Transaction, credentialID string) (store.CandidateScope, error) {
	rows, err := tx.Query(ctx, store.Query{
		SQL:  `SELECT id, tenant_id, project_id, principal_id FROM executions WHERE id = $1`,
		Args: []any{credentialID},
	})
	if err != nil {
		return store.CandidateScope{}, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return store.CandidateScope{}, err
		}
		return store.CandidateScope{}, uniformCredentialRefusal()
	}
	var identity, tenant, project, principal string
	if err := rows.Scan(&identity, &tenant, &project, &principal); err != nil {
		return store.CandidateScope{}, fault.Wrap(fault.InternalFailure, "read execution credential", err)
	}
	if err := rows.Err(); err != nil {
		return store.CandidateScope{}, err
	}
	return store.CandidateScope{
		TenantID:    id.TenantID(tenant),
		ProjectID:   id.ProjectID(project),
		PrincipalID: id.PrincipalID(principal),
	}, nil
}

func lookupServiceCredential(ctx context.Context, tx store.Transaction, credentialID string) (store.CandidateScope, error) {
	rows, err := tx.Query(ctx, store.Query{
		SQL:  `SELECT id, tenant_id, project_id FROM services WHERE id = $1`,
		Args: []any{credentialID},
	})
	if err != nil {
		return store.CandidateScope{}, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return store.CandidateScope{}, err
		}
		return store.CandidateScope{}, uniformCredentialRefusal()
	}
	var identity, tenant string
	var project sql.NullString
	if err := rows.Scan(&identity, &tenant, &project); err != nil {
		return store.CandidateScope{}, fault.Wrap(fault.InternalFailure, "read service credential", err)
	}
	if err := rows.Err(); err != nil {
		return store.CandidateScope{}, err
	}
	candidate := store.CandidateScope{
		TenantID:    id.TenantID(tenant),
		PrincipalID: id.PrincipalID(credentialID),
	}
	if project.Valid {
		candidate.ProjectID = id.ProjectID(project.String)
	}
	return candidate, nil
}
