package api

// Service account and credential-lifecycle wire vocabulary.
//
// A service account never holds the admin role. A credential issue or rotate
// result is the only result that may carry a fresh one-time token; every other
// result is secret-free.

// ServiceCreateRequest is the service.create input. The project is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type ServiceCreateRequest struct {
	ProjectID string   `json:"project_id,omitempty" schema:"id"`
	ServiceID string   `json:"service_id" schema:"id"`
	Roles     []string `json:"roles"`
}

// Validate checks the optional project identity, bounded service identity and
// service-assignable roles.
func (request ServiceCreateRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if !validID(request.ServiceID) {
		return invalid("service_id is invalid")
	}
	return validateServiceRoles(request.Roles)
}

// ServiceCreateResult is the service.create result.
type ServiceCreateResult struct {
	ServiceID string `json:"service_id"`
	Version   uint64 `json:"version" schema:"version"`
}

// Validate checks the service identity and committed version.
func (result ServiceCreateResult) Validate() error {
	if !validID(result.ServiceID) {
		return invalid("service_id is invalid")
	}
	if result.Version == 0 {
		return invalid("service version must be positive")
	}
	return nil
}

// ServiceUpdateRequest is the service.update input. The service is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type ServiceUpdateRequest struct {
	ServiceID string   `json:"service_id,omitempty" schema:"id"`
	Roles     []string `json:"roles"`
}

// Validate checks the optional service identity and complete service-assignable
// role set.
func (request ServiceUpdateRequest) Validate() error {
	if request.ServiceID != "" && !validID(request.ServiceID) {
		return invalid("service_id is invalid")
	}
	return validateServiceRoles(request.Roles)
}

// ServiceUpdateResult is the service.update result.
type ServiceUpdateResult struct {
	Version uint64 `json:"version" schema:"version"`
}

// Validate checks the committed version.
func (result ServiceUpdateResult) Validate() error {
	if result.Version == 0 {
		return invalid("service version must be positive")
	}
	return nil
}

// ServiceEnableRequest is the service.enable input. The service is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type ServiceEnableRequest struct {
	ServiceID string `json:"service_id,omitempty" schema:"id"`
}

// Validate checks an optional bounded service identity.
func (request ServiceEnableRequest) Validate() error {
	if request.ServiceID != "" && !validID(request.ServiceID) {
		return invalid("service_id is invalid")
	}
	return nil
}

// ServiceEnableResult is the service.enable result.
type ServiceEnableResult struct {
	State              string `json:"state"`
	Version            uint64 `json:"version" schema:"version"`
	RevokedCredentials uint64 `json:"revoked_credentials"`
}

// Validate checks the enabled state and committed version.
func (result ServiceEnableResult) Validate() error {
	if result.State != serviceStateEnabled {
		return invalid("service enable state %q is not %q", result.State, serviceStateEnabled)
	}
	if result.Version == 0 {
		return invalid("service version must be positive")
	}
	return nil
}

// ServiceDisableRequest is the service.disable input. The service is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type ServiceDisableRequest struct {
	ServiceID string `json:"service_id,omitempty" schema:"id"`
}

// Validate checks an optional bounded service identity.
func (request ServiceDisableRequest) Validate() error {
	if request.ServiceID != "" && !validID(request.ServiceID) {
		return invalid("service_id is invalid")
	}
	return nil
}

// ServiceDisableResult is the service.disable result.
type ServiceDisableResult struct {
	State              string `json:"state"`
	Version            uint64 `json:"version" schema:"version"`
	RevokedCredentials uint64 `json:"revoked_credentials"`
}

// Validate checks the disabled state and committed version.
func (result ServiceDisableResult) Validate() error {
	if result.State != serviceStateDisabled {
		return invalid("service disable state %q is not %q", result.State, serviceStateDisabled)
	}
	if result.Version == 0 {
		return invalid("service version must be positive")
	}
	return nil
}

// ServiceCredentialIssueRequest is the service.credential.issue input. The
// service is carried explicitly for MCP callers and is optional for REST
// callers whose route supplies it.
type ServiceCredentialIssueRequest struct {
	ServiceID  string `json:"service_id,omitempty" schema:"id"`
	TTLSeconds int    `json:"ttl_seconds" schema:"ttl"`
}

// Validate checks the optional service identity and positive credential
// lifetime.
func (request ServiceCredentialIssueRequest) Validate() error {
	if request.ServiceID != "" && !validID(request.ServiceID) {
		return invalid("service_id is invalid")
	}
	if request.TTLSeconds <= 0 {
		return invalid("ttl_seconds must be positive")
	}
	return nil
}

// ServiceCredentialIssueResult is the service.credential.issue result. It is
// one of the only two results that may carry a fresh one-time token.
type ServiceCredentialIssueResult struct {
	State   string  `json:"state"`
	Version uint64  `json:"version" schema:"version"`
	Token   *string `json:"token,omitempty" schema:"token"`
}

// Validate checks the current state, committed version and optional token.
func (result ServiceCredentialIssueResult) Validate() error {
	if result.State != serviceStateCurrent {
		return invalid("credential issue state %q is not %q", result.State, serviceStateCurrent)
	}
	if result.Version == 0 {
		return invalid("service version must be positive")
	}
	return validateCredentialToken(result.Token)
}

// ServiceCredentialRotateRequest is the service.credential.rotate input. The
// service is carried explicitly for MCP callers and is optional for REST
// callers whose route supplies it.
type ServiceCredentialRotateRequest struct {
	ServiceID  string `json:"service_id,omitempty" schema:"id"`
	TTLSeconds int    `json:"ttl_seconds" schema:"ttl"`
}

// Validate checks the optional service identity and positive credential
// lifetime.
func (request ServiceCredentialRotateRequest) Validate() error {
	if request.ServiceID != "" && !validID(request.ServiceID) {
		return invalid("service_id is invalid")
	}
	if request.TTLSeconds <= 0 {
		return invalid("ttl_seconds must be positive")
	}
	return nil
}

// ServiceCredentialRotateResult is the service.credential.rotate result. It is
// one of the only two results that may carry a fresh one-time token.
type ServiceCredentialRotateResult struct {
	State   string  `json:"state"`
	Version uint64  `json:"version" schema:"version"`
	Token   *string `json:"token,omitempty" schema:"token"`
}

// Validate checks the current state, committed version and optional token.
func (result ServiceCredentialRotateResult) Validate() error {
	if result.State != serviceStateCurrent {
		return invalid("credential rotate state %q is not %q", result.State, serviceStateCurrent)
	}
	if result.Version == 0 {
		return invalid("service version must be positive")
	}
	return validateCredentialToken(result.Token)
}

// ServiceCredentialRevokeRequest is the service.credential.revoke input. The
// service is carried explicitly for MCP callers and is optional for REST
// callers whose route supplies it.
type ServiceCredentialRevokeRequest struct {
	ServiceID string `json:"service_id,omitempty" schema:"id"`
}

// Validate checks an optional bounded service identity.
func (request ServiceCredentialRevokeRequest) Validate() error {
	if request.ServiceID != "" && !validID(request.ServiceID) {
		return invalid("service_id is invalid")
	}
	return nil
}

// ServiceCredentialRevokeResult is the service.credential.revoke result.
type ServiceCredentialRevokeResult struct {
	Version     uint64 `json:"version" schema:"version"`
	Invalidated *bool  `json:"invalidated"`
}

// Validate checks the committed version and the required invalidation flag.
func (result ServiceCredentialRevokeResult) Validate() error {
	if result.Version == 0 {
		return invalid("service version must be positive")
	}
	if result.Invalidated == nil {
		return invalid("credential revocation invalidation flag is missing")
	}
	return nil
}

// ServiceListRequest is the service.list input. The project is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type ServiceListRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	Page      *Page  `json:"page,omitempty"`
}

// Validate checks the optional project identity and page bounds.
func (request ServiceListRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Page != nil {
		return request.Page.Validate()
	}
	return nil
}

// ServiceSummaryView is one credential-free service account observation in
// service.list.
type ServiceSummaryView struct {
	ServiceID string   `json:"service_id"`
	State     string   `json:"state"`
	Roles     []string `json:"roles"`
	Version   uint64   `json:"version" schema:"version"`
}

// Validate checks the service identity, enabled/disabled state, non-Admin role
// set and version.
func (view ServiceSummaryView) Validate() error {
	if !validID(view.ServiceID) {
		return invalid("service_id is invalid")
	}
	if view.State != serviceStateEnabled && view.State != serviceStateDisabled {
		return invalid("unknown service state %q", view.State)
	}
	if err := validateServiceRoles(view.Roles); err != nil {
		return err
	}
	if view.Version == 0 {
		return invalid("service version must be positive")
	}
	return nil
}

// ServiceListResult is the service.list result.
type ServiceListResult struct {
	Services   []ServiceSummaryView `json:"services"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

// Validate checks every service summary and the optional cursor bound.
func (result ServiceListResult) Validate() error {
	for _, service := range result.Services {
		if err := service.Validate(); err != nil {
			return err
		}
	}
	if len(result.NextCursor) > MaxCursorBytes {
		return invalid("next_cursor is %d bytes; at most %d are allowed", len(result.NextCursor), MaxCursorBytes)
	}
	return nil
}

// Service lifecycle state names.
const (
	serviceStateEnabled  = "enabled"
	serviceStateDisabled = "disabled"
	serviceStateCurrent  = "current"
)

// validateServiceRoles checks the complete service-assignable role set. The set
// must be present but may be empty (a service may hold no role); the admin role
// is never assignable to a service account.
func validateServiceRoles(roles []string) error {
	if roles == nil {
		return invalid("service roles are missing")
	}
	for _, role := range roles {
		if !validHumanRole(role) {
			return invalid("unknown service role %q", role)
		}
		if role == "admin" {
			return invalid("service role %q is not service-assignable", role)
		}
	}
	return nil
}

// validateCredentialToken checks an optional one-time credential token. A
// present token must be non-empty and bounded; an absent token is a replayed
// result and stays valid.
func validateCredentialToken(token *string) error {
	if token == nil {
		return nil
	}
	if *token == "" {
		return invalid("credential token is empty")
	}
	if len(*token) > MaxCredentialBytes {
		return invalid("credential token is %d bytes; at most %d are allowed", len(*token), MaxCredentialBytes)
	}
	return nil
}
