package opsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// confirmReadAuthority re-evaluates the operation's declared current-authority
// checks inside scope when an AuthorityReader is configured.
func confirmReadAuthority(ctx context.Context, deps Dependencies, call ops.AuthorizedCall, scope tenancy.Scope) error {
	if deps.Authority == nil || len(call.Operation.Authority) == 0 {
		return nil
	}
	reads := make([]authorize.AuthorityRead, 0, len(call.Operation.Authority))
	for _, check := range call.Operation.Authority {
		reads = append(reads, authorize.AuthorityRead{
			Operation:   call.Operation.Name,
			Required:    call.Operation.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: call.Principal.ID,
		})
	}
	facts, err := deps.Authority.Read(ctx, reads)
	if err != nil {
		return err
	}
	return authorize.Confirm(reads, facts)
}

// requireScanner returns the configured store.Scanner or a fail-closed
// dependency refusal.
func requireScanner(deps Dependencies) (store.Scanner, error) {
	if deps.Scanner == nil {
		return nil, fault.New(fault.DependencyUnavailable, "scanner is not configured")
	}
	return deps.Scanner, nil
}

// pageOrDefault returns the caller's page or the empty default page.
func pageOrDefault(page *api.Page) api.Page {
	if page == nil {
		return api.Page{}
	}
	return *page
}

// nonNilStrings returns a non-nil string slice copy so JSON encoding always
// emits [] rather than null.
func nonNilStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string(nil), values...)
}

// whoamiKind maps the internal principal kind to the public principal.whoami
// kind string.
func whoamiKind(kind identity.PrincipalKind) string {
	switch kind {
	case identity.KindHuman:
		return "human"
	case identity.KindService:
		return "service"
	case identity.KindExecution:
		return "workload"
	default:
		return string(kind)
	}
}

// whoamiMembershipsAndRoles resolves the current role slice and visible project
// memberships within the caller's tenant for principal.whoami.
func whoamiMembershipsAndRoles(ctx context.Context, deps Dependencies, call ops.AuthorizedCall) ([]string, []api.PrincipalMembershipView, error) {
	roles := nonNilStrings(roleNames(call.Principal.Roles))
	if call.Principal.Kind != identity.KindHuman {
		return roles, nil, nil
	}
	memberships := []api.PrincipalMembershipView{}
	if deps.Scanner == nil {
		return roles, memberships, nil
	}
	if call.Principal.Scope.ProjectScoped() {
		projectRoles, found, err := scanPrincipalMembershipRoles(ctx, deps.Scanner, call.Principal.Scope, call.Principal.ID)
		if err != nil {
			return nil, nil, err
		}
		if found {
			memberships = append(memberships, api.PrincipalMembershipView{
				ProjectID: string(call.Principal.Scope.ProjectID),
				Roles:     nonNilStrings(projectRoles),
			})
		}
		return roles, memberships, nil
	}

	tenantScope := tenancy.Scope{TenantID: call.Principal.Scope.TenantID}
	tenantRoles, found, err := scanPrincipalMembershipRoles(ctx, deps.Scanner, tenantScope, call.Principal.ID)
	if err != nil {
		return nil, nil, err
	}
	if found {
		roles = nonNilStrings(tenantRoles)
	}

	var after string
	for {
		page, err := deps.Scanner.Query(ctx, tenantScope, store.ScanQuery{
			Table:   "projects",
			OrderBy: []store.Order{{Column: "id"}},
			After:   after,
			Limit:   store.MaxQueryLimit,
		})
		if err != nil {
			return nil, nil, err
		}
		for _, row := range page.Rows {
			projectID, err := scanRowString(row, "id")
			if err != nil {
				return nil, nil, err
			}
			projectScope := tenancy.Scope{
				TenantID:  call.Principal.Scope.TenantID,
				ProjectID: id.ProjectID(projectID),
			}
			projectRoles, found, err := scanPrincipalMembershipRoles(ctx, deps.Scanner, projectScope, call.Principal.ID)
			if err != nil {
				return nil, nil, err
			}
			if found {
				memberships = append(memberships, api.PrincipalMembershipView{
					ProjectID: projectID,
					Roles:     nonNilStrings(projectRoles),
				})
			}
		}
		if !page.HasMore {
			break
		}
		after = page.Next
	}
	return roles, memberships, nil
}

// scanPrincipalMembershipRoles reads the exact membership row of principalID in
// scope via store.Scanner.
func scanPrincipalMembershipRoles(ctx context.Context, scanner store.Scanner, scope tenancy.Scope, principalID id.PrincipalID) ([]string, bool, error) {
	page, err := scanner.Query(ctx, scope, store.ScanQuery{
		Table:   "memberships",
		Equals:  map[string]any{"principal_id": string(principalID)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   1,
	})
	if err != nil {
		return nil, false, err
	}
	if len(page.Rows) == 0 {
		return nil, false, nil
	}
	roles, err := scanDecodeRoles(page.Rows[0], false)
	if err != nil {
		return nil, false, err
	}
	return roles, true, nil
}

// bindingMatchesKind reports whether a resource binding matches an optional
// kind filter: either metadata["kind"] equals kind, the enforcing service ID
// equals kind, or any declared resource operation belongs to the kind prefix
// ("<kind>.").
func bindingMatchesKind(view api.ResourceBindingView, kind string) bool {
	if kind == "" {
		return true
	}
	if view.Metadata != nil && view.Metadata["kind"] == kind {
		return true
	}
	if view.EnforcingService == kind {
		return true
	}
	prefix := kind + "."
	for _, op := range view.Operations {
		if strings.HasPrefix(op, prefix) {
			return true
		}
	}
	return false
}

// decodeBindingRow decodes one resource_bindings row into api.ResourceBindingView.
func decodeBindingRow(row store.Row) (api.ResourceBindingView, error) {
	bindingID, err := scanRowString(row, "id")
	if err != nil {
		return api.ResourceBindingView{}, err
	}
	resourceID, err := scanRowString(row, "resource_id")
	if err != nil {
		return api.ResourceBindingView{}, err
	}
	serviceID, err := scanRowString(row, "service_id")
	if err != nil {
		return api.ResourceBindingView{}, err
	}
	origins, err := scanDecodeStringList(row, "origins")
	if err != nil {
		return api.ResourceBindingView{}, err
	}
	operations, err := scanDecodeStringList(row, "operations")
	if err != nil {
		return api.ResourceBindingView{}, err
	}
	reportable, err := scanDecodeStringList(row, "reportable_events")
	if err != nil {
		return api.ResourceBindingView{}, err
	}
	metadata, err := scanDecodeMetadata(row, "metadata")
	if err != nil {
		return api.ResourceBindingView{}, err
	}
	version, err := scanRowUint(row, "version")
	if err != nil {
		return api.ResourceBindingView{}, err
	}
	if origins == nil {
		origins = []string{}
	}
	if operations == nil {
		operations = []string{}
	}
	if reportable == nil {
		reportable = []string{}
	}
	return api.ResourceBindingView{
		BindingID:        bindingID,
		ResourceID:       resourceID,
		EnforcingService: serviceID,
		TrustedOrigins:   origins,
		Operations:       operations,
		ReportableEvents: reportable,
		Metadata:         metadata,
		Version:          version,
	}, nil
}

func scanRowString(row store.Row, name string) (string, error) {
	value, ok := row.Values[name]
	if !ok || value == nil {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	text, ok := value.(string)
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not text", row.Table, name))
	}
	return text, nil
}

func scanRowNullableString(row store.Row, name string) (string, error) {
	value, ok := row.Values[name]
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	if value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not text", row.Table, name))
	}
	return text, nil
}

func scanRowUint(row store.Row, name string) (uint64, error) {
	value, ok := row.Values[name]
	if !ok || value == nil {
		return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	switch typed := value.(type) {
	case int64:
		if typed < 0 {
			return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is negative", row.Table, name))
		}
		return uint64(typed), nil
	case int:
		if typed < 0 {
			return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is negative", row.Table, name))
		}
		return uint64(typed), nil
	case uint64:
		return typed, nil
	default:
		return 0, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not an integer", row.Table, name))
	}
}

func scanRowTime(row store.Row, name string) (time.Time, error) {
	value, ok := row.Values[name]
	if !ok || value == nil {
		return time.Time{}, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	moment, ok := value.(time.Time)
	if !ok {
		return time.Time{}, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not a timestamp", row.Table, name))
	}
	return moment, nil
}

func scanRowBytes(row store.Row, name string) ([]byte, error) {
	value, ok := row.Values[name]
	if !ok {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is missing", row.Table, name))
	}
	if value == nil {
		return nil, nil
	}
	switch typed := value.(type) {
	case []byte:
		return typed, nil
	case string:
		return []byte(typed), nil
	default:
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is not bytes", row.Table, name))
	}
}

func scanDecodeStringList(row store.Row, name string) ([]string, error) {
	data, err := scanRowBytes(row, name)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []string{}, nil
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fault.Wrap(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is invalid JSON", row.Table, name), err)
	}
	return values, nil
}

func scanDecodeMetadata(row store.Row, name string) (map[string]string, error) {
	data, err := scanRowBytes(row, name)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var metadata map[string]string
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fault.Wrap(fault.InternalFailure, fmt.Sprintf("stored row %s.%s is invalid JSON", row.Table, name), err)
	}
	if len(metadata) == 0 {
		return nil, nil
	}
	return metadata, nil
}

func scanDecodeRoles(row store.Row, serviceOnly bool) ([]string, error) {
	rawRoles, err := scanDecodeStringList(row, "roles")
	if err != nil {
		return nil, err
	}
	domainRoles := make([]tenancy.Role, 0, len(rawRoles))
	for _, raw := range rawRoles {
		domainRoles = append(domainRoles, tenancy.Role(raw))
	}
	if serviceOnly {
		domainRoles = tenancy.AssignableServiceRoles(domainRoles)
	}
	out := make([]string, 0, len(domainRoles))
	for _, role := range domainRoles {
		if role.Valid() {
			out = append(out, string(role))
		}
	}
	return out, nil
}
