package app

// The pure bootstrap commit-unit shape. One unit carries the new tenant (with
// its encrypted derivation key), the initial ADMIN-only tenant-wide membership,
// one immutable audit row and one config.changed publication intent. The unit
// has no receipt and no locks: the tenant's unique insert is the one-shot guard.

import (
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// buildBootstrapAdminUnit builds the single atomic bootstrap unit. Identities
// are allocated in the pinned order: the membership row id first, then the audit
// event id, then the config.changed event id.
func buildBootstrapAdminUnit(
	config BootstrapAdminConfig,
	principal id.PrincipalID,
	key token.TenantKey,
	at time.Time,
	ids BootstrapAdminIDs,
) (store.Unit, BootstrapAdminResult, error) {
	if ids == nil {
		return store.Unit{}, BootstrapAdminResult{}, fault.New(fault.InvalidInput, "bootstrap id source is missing")
	}
	membershipID, err := ids.NewRowID()
	if err != nil {
		return store.Unit{}, BootstrapAdminResult{}, fault.Wrap(fault.InternalFailure, "bootstrap membership identity could not be allocated", err)
	}
	auditID, err := ids.NewEventID()
	if err != nil {
		return store.Unit{}, BootstrapAdminResult{}, fault.Wrap(fault.InternalFailure, "bootstrap audit identity could not be allocated", err)
	}
	eventID, err := ids.NewEventID()
	if err != nil {
		return store.Unit{}, BootstrapAdminResult{}, fault.Wrap(fault.InternalFailure, "bootstrap event identity could not be allocated", err)
	}
	encodedKey, err := json.Marshal(key)
	if err != nil {
		return store.Unit{}, BootstrapAdminResult{}, fault.Wrap(fault.InternalFailure, "bootstrap tenant derivation key could not be encoded", err)
	}
	roles, err := json.Marshal([]tenancy.Role{tenancy.RoleAdmin})
	if err != nil {
		return store.Unit{}, BootstrapAdminResult{}, fault.Wrap(fault.InternalFailure, "bootstrap membership roles could not be encoded", err)
	}
	scope := tenancy.Scope{TenantID: config.TenantID}
	unit := store.Unit{
		Writes: []store.Write{
			{
				Table:  "tenants",
				ID:     string(config.TenantID),
				Insert: true,
				Values: map[string]any{
					"name":           config.TenantName,
					"version":        uint64(1),
					"derivation_key": encodedKey,
					"created_at":     at,
				},
			},
			{
				Table:  "memberships",
				ID:     membershipID,
				Insert: true,
				Values: map[string]any{
					"principal_id": string(principal),
					"roles":        roles,
					"version":      uint64(1),
					"created_at":   at,
				},
			},
		},
		Audit: []event.Audit{{
			ID:        auditID,
			Scope:     scope,
			Actor:     principal,
			Operation: BootstrapAdminOperation,
			At:        at,
		}},
		Events: []event.Envelope{{
			ID:             eventID,
			Type:           event.TypeConfigChanged,
			Source:         event.Source{Kind: event.SourceShell, PrincipalID: principal},
			At:             at,
			Scope:          scope,
			SubjectID:      string(config.TenantID),
			SubjectVersion: 1,
		}},
	}
	result := BootstrapAdminResult{
		TenantID:     config.TenantID,
		PrincipalID:  principal,
		MembershipID: membershipID,
		AuditID:      auditID,
		EventID:      eventID,
	}
	return unit, result, nil
}
