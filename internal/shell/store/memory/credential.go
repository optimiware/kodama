package memory

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
)

var _ store.CredentialLookup = (*Store)(nil)

// LookupCredential answers exactly one exact-kind/exact-id pre-authentication
// question and returns a candidate scope. The result is never authority: the
// caller still verifies the MAC, current nonce/expiry and current principal
// state inside the candidate scope before evaluating any grant.
//
// A missing row and a row of another kind are indistinguishable: both return
// fault.Unauthenticated with the same message, so the lookup cannot enumerate
// tenants, projects, executions or services. The lookup never returns token
// material and never lists rows.
func (s *Store) LookupCredential(ctx context.Context, kind store.CredentialKind, credentialID string) (store.CandidateScope, error) {
	if err := ctx.Err(); err != nil {
		return store.CandidateScope{}, err
	}
	if !kind.Valid() {
		return store.CandidateScope{}, faultInvalid("credential kind is not frozen")
	}
	if !validIdentity(credentialID) {
		return store.CandidateScope{}, uniformCredentialRefusal()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case store.CredentialExecution:
		values, ok := s.state.row("executions", credentialID)
		if !ok {
			return store.CandidateScope{}, uniformCredentialRefusal()
		}
		return candidateFromRow(values, credentialPrincipal(values, ""))
	case store.CredentialService:
		values, ok := s.state.row("services", credentialID)
		if !ok {
			return store.CandidateScope{}, uniformCredentialRefusal()
		}
		return candidateFromRow(values, id.PrincipalID(credentialID))
	default:
		return store.CandidateScope{}, faultInvalid(fmt.Sprintf("credential kind %q is not frozen", kind))
	}
}

// uniformCredentialRefusal is the one indistinguishable refusal for a missing
// credential and a credential presented under the wrong kind.
func uniformCredentialRefusal() error {
	return fault.New(fault.Unauthenticated, "credential candidate is not available")
}

// candidateFromRow builds the bootstrap scope hint from one credential row. The
// principal is the row's stored principal for an execution and the service
// identity for a service credential.
func candidateFromRow(values map[string]any, principal id.PrincipalID) (store.CandidateScope, error) {
	tenant, _ := values["tenant_id"].(string)
	project, _ := values["project_id"].(string)
	candidate := store.CandidateScope{
		TenantID:    id.TenantID(tenant),
		ProjectID:   id.ProjectID(project),
		PrincipalID: principal,
	}
	if err := candidate.Validate(); err != nil {
		return store.CandidateScope{}, faultInternal("the credential row has no usable candidate scope")
	}
	return candidate, nil
}

// credentialPrincipal reads a row's principal column, falling back to the
// supplied identity when the column is absent.
func credentialPrincipal(values map[string]any, fallback id.PrincipalID) id.PrincipalID {
	if principal, ok := values["principal_id"].(string); ok && principal != "" {
		return id.PrincipalID(principal)
	}
	return fallback
}
