package api

// Tenant, project and membership administration wire vocabulary.
//
// Each request DTO is the user-supplied projection of the Shell tenancy
// command: actor, caller key and receipt content stay at the transport edge and
// are never part of the public input. Each result DTO is the committed
// observation the public edge returns.

// TenantCreateRequest is the tenant.create input.
type TenantCreateRequest struct {
	TenantID string `json:"tenant_id" schema:"id"`
	Name     string `json:"name"`
}

// Validate checks the bounded tenant identity and non-empty name.
func (request TenantCreateRequest) Validate() error {
	if !validID(request.TenantID) {
		return invalid("tenant_id is invalid")
	}
	if request.Name == "" {
		return invalid("tenant name is empty")
	}
	return nil
}

// TenantCreateResult is the tenant.create result.
type TenantCreateResult struct {
	TenantID string `json:"tenant_id"`
	Version  uint64 `json:"version" schema:"version"`
}

// Validate checks the tenant identity and committed version.
func (result TenantCreateResult) Validate() error {
	if !validID(result.TenantID) {
		return invalid("tenant_id is invalid")
	}
	if result.Version == 0 {
		return invalid("tenant version must be positive")
	}
	return nil
}

// TenantRotateKeyRequest is the tenant.rotate_key input. The tenant is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type TenantRotateKeyRequest struct {
	TenantID string `json:"tenant_id,omitempty" schema:"id"`
	Name     string `json:"name"`
}

// Validate checks the optional tenant identity and non-empty tenant name.
func (request TenantRotateKeyRequest) Validate() error {
	if request.TenantID != "" && !validID(request.TenantID) {
		return invalid("tenant_id is invalid")
	}
	if request.Name == "" {
		return invalid("tenant name is empty")
	}
	return nil
}

// TenantRotateKeyResult is the tenant.rotate_key result.
type TenantRotateKeyResult struct {
	TenantVersion uint64 `json:"tenant_version" schema:"version"`
	KeyVersion    uint64 `json:"key_version" schema:"version"`
}

// Validate checks that both rotated versions are positive.
func (result TenantRotateKeyResult) Validate() error {
	if result.TenantVersion == 0 || result.KeyVersion == 0 {
		return invalid("tenant and key versions must be positive")
	}
	return nil
}

// ProjectCreateRequest is the project.create input. The tenant is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type ProjectCreateRequest struct {
	TenantID  string `json:"tenant_id,omitempty" schema:"id"`
	ProjectID string `json:"project_id" schema:"id"`
	Name      string `json:"name"`
}

// Validate checks the optional tenant identity, bounded project identity and
// non-empty name.
func (request ProjectCreateRequest) Validate() error {
	if request.TenantID != "" && !validID(request.TenantID) {
		return invalid("tenant_id is invalid")
	}
	if !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Name == "" {
		return invalid("project name is empty")
	}
	return nil
}

// ProjectCreateResult is the project.create result.
type ProjectCreateResult struct {
	ProjectID string `json:"project_id"`
	Version   uint64 `json:"version" schema:"version"`
}

// Validate checks the project identity and committed version.
func (result ProjectCreateResult) Validate() error {
	if !validID(result.ProjectID) {
		return invalid("project_id is invalid")
	}
	if result.Version == 0 {
		return invalid("project version must be positive")
	}
	return nil
}

// ProjectUpdateRequest is the project.update input. The project is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type ProjectUpdateRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	Name      string `json:"name"`
}

// Validate checks the optional project identity and non-empty project name.
func (request ProjectUpdateRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Name == "" {
		return invalid("project name is empty")
	}
	return nil
}

// ProjectUpdateResult is the project.update result.
type ProjectUpdateResult struct {
	Version uint64 `json:"version" schema:"version"`
}

// Validate checks the committed version.
func (result ProjectUpdateResult) Validate() error {
	if result.Version == 0 {
		return invalid("project version must be positive")
	}
	return nil
}

// MembershipGrantRequest is the membership.grant input. It carries the complete
// replacement role set; the project and target principal are carried explicitly
// for MCP callers and are optional for REST callers whose route supplies them.
type MembershipGrantRequest struct {
	ProjectID   string   `json:"project_id,omitempty" schema:"id"`
	PrincipalID string   `json:"principal_id,omitempty" schema:"id"`
	Roles       []string `json:"roles"`
}

// Validate checks the optional project/principal identities and the non-empty
// complete human role set.
func (request MembershipGrantRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.PrincipalID != "" && !validID(request.PrincipalID) {
		return invalid("principal_id is invalid")
	}
	if request.Roles == nil {
		return invalid("membership roles are missing")
	}
	for _, role := range request.Roles {
		if !validHumanRole(role) {
			return invalid("unknown membership role %q", role)
		}
	}
	return nil
}

// MembershipGrantResult is the membership.grant result.
type MembershipGrantResult struct {
	Version uint64 `json:"version" schema:"version"`
}

// Validate checks the committed version.
func (result MembershipGrantResult) Validate() error {
	if result.Version == 0 {
		return invalid("membership version must be positive")
	}
	return nil
}

// MembershipRevokeRequest is the membership.revoke input. The project and
// principal are carried explicitly for MCP callers and are optional for REST
// callers whose route supplies them.
type MembershipRevokeRequest struct {
	ProjectID   string `json:"project_id,omitempty" schema:"id"`
	PrincipalID string `json:"principal_id,omitempty" schema:"id"`
}

// Validate checks the optional project and principal identities.
func (request MembershipRevokeRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.PrincipalID != "" && !validID(request.PrincipalID) {
		return invalid("principal_id is invalid")
	}
	return nil
}

// MembershipRevokeResult is the membership.revoke result.
type MembershipRevokeResult struct {
	Version uint64 `json:"version" schema:"version"`
}

// Validate checks the committed version.
func (result MembershipRevokeResult) Validate() error {
	if result.Version == 0 {
		return invalid("membership version must be positive")
	}
	return nil
}

// MembershipListRequest is the membership.list input. The project is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type MembershipListRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	Page      *Page  `json:"page,omitempty"`
}

// Validate checks the optional project identity and page bounds.
func (request MembershipListRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Page != nil {
		return request.Page.Validate()
	}
	return nil
}

// MembershipView is one membership observation.
type MembershipView struct {
	PrincipalID string   `json:"principal_id"`
	Roles       []string `json:"roles"`
	Version     uint64   `json:"version" schema:"version"`
}

// Validate checks the principal identity, complete human role set and version.
func (view MembershipView) Validate() error {
	if !validID(view.PrincipalID) {
		return invalid("membership principal_id is invalid")
	}
	if view.Roles == nil {
		return invalid("membership roles are missing")
	}
	for _, role := range view.Roles {
		if !validHumanRole(role) {
			return invalid("unknown membership role %q", role)
		}
	}
	if view.Version == 0 {
		return invalid("membership version must be positive")
	}
	return nil
}

// MembershipListResult is the membership.list result.
type MembershipListResult struct {
	Memberships []MembershipView `json:"memberships"`
	NextCursor  string           `json:"next_cursor,omitempty"`
}

// Validate checks every membership and the optional cursor bound.
func (result MembershipListResult) Validate() error {
	for _, membership := range result.Memberships {
		if err := membership.Validate(); err != nil {
			return err
		}
	}
	if len(result.NextCursor) > MaxCursorBytes {
		return invalid("next_cursor is %d bytes; at most %d are allowed", len(result.NextCursor), MaxCursorBytes)
	}
	return nil
}

// PrincipalWhoamiRequest is the principal.whoami input.
type PrincipalWhoamiRequest struct{}

// Validate accepts the empty principal.whoami request.
func (request PrincipalWhoamiRequest) Validate() error {
	return nil
}

// PrincipalMembershipView is one project membership observation in
// principal.whoami.
type PrincipalMembershipView struct {
	ProjectID string   `json:"project_id"`
	Roles     []string `json:"roles"`
}

// Validate checks the project identity and role set.
func (view PrincipalMembershipView) Validate() error {
	if !validID(view.ProjectID) {
		return invalid("membership project_id is invalid")
	}
	if view.Roles == nil {
		return invalid("membership roles are missing")
	}
	for _, role := range view.Roles {
		if !validHumanRole(role) {
			return invalid("unknown membership role %q", role)
		}
	}
	return nil
}

// whoamiPrincipalKinds is the principal.whoami kind vocabulary. It accepts
// workload (the container-facing term) as well as execution for compatibility.
var whoamiPrincipalKinds = []string{"human", "service", "workload", "execution"}

func validWhoamiPrincipalKind(kind string) bool {
	for _, candidate := range whoamiPrincipalKinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

// PrincipalWhoamiResult is the principal.whoami result.
type PrincipalWhoamiResult struct {
	PrincipalID string                    `json:"principal_id"`
	Kind        string                    `json:"kind"`
	TenantID    string                    `json:"tenant_id"`
	ProjectID   string                    `json:"project_id,omitempty"`
	Roles       []string                  `json:"roles"`
	Memberships []PrincipalMembershipView `json:"memberships,omitempty"`
}

// Validate checks the authenticated identity, scope, roles and visible project
// memberships.
func (result PrincipalWhoamiResult) Validate() error {
	if !validID(result.PrincipalID) {
		return invalid("principal_id is invalid")
	}
	if !validWhoamiPrincipalKind(result.Kind) {
		return invalid("principal kind %q is unknown", result.Kind)
	}
	if !validID(result.TenantID) {
		return invalid("tenant_id is invalid")
	}
	if result.ProjectID != "" && !validID(result.ProjectID) {
		return invalid("project_id is invalid")
	}
	if result.Roles == nil {
		return invalid("principal roles are missing")
	}
	for _, role := range result.Roles {
		if !validHumanRole(role) {
			return invalid("unknown principal role %q", role)
		}
	}
	for _, membership := range result.Memberships {
		if err := membership.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ProjectListRequest is the project.list input. The tenant is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type ProjectListRequest struct {
	TenantID string `json:"tenant_id,omitempty" schema:"id"`
	Page     *Page  `json:"page,omitempty"`
}

// Validate checks the optional tenant identity and page bounds.
func (request ProjectListRequest) Validate() error {
	if request.TenantID != "" && !validID(request.TenantID) {
		return invalid("tenant_id is invalid")
	}
	if request.Page != nil {
		return request.Page.Validate()
	}
	return nil
}

// ProjectView is one project observation in project.list.
type ProjectView struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Version   uint64 `json:"version" schema:"version"`
}

// Validate checks the project identity, non-empty name and version.
func (view ProjectView) Validate() error {
	if !validID(view.ProjectID) {
		return invalid("project_id is invalid")
	}
	if view.Name == "" {
		return invalid("project name is empty")
	}
	if view.Version == 0 {
		return invalid("project version must be positive")
	}
	return nil
}

// ProjectListResult is the project.list result.
type ProjectListResult struct {
	Projects   []ProjectView `json:"projects"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// Validate checks every project observation and the optional cursor bound.
func (result ProjectListResult) Validate() error {
	for _, project := range result.Projects {
		if err := project.Validate(); err != nil {
			return err
		}
	}
	if len(result.NextCursor) > MaxCursorBytes {
		return invalid("next_cursor is %d bytes; at most %d are allowed", len(result.NextCursor), MaxCursorBytes)
	}
	return nil
}
