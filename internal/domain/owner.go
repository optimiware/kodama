package domain

// Owner identifies a tenant resource, optionally within a project. An empty
// ProjectID means tenant ownership, not permission to reach every project.
// Scope remains the required-project value used by project-only operations.
type Owner struct {
	TenantID  TenantID  `json:"tenantId"`
	ProjectID ProjectID `json:"projectId,omitempty"`
}

func (o Owner) Validate() error {
	if err := o.TenantID.Validate(); err != nil {
		return err
	}
	if o.ProjectID != "" {
		return o.ProjectID.Validate()
	}
	return nil
}

// AvailableTo permits a same-project or explicitly shared tenant resource.
// It is a scope check; use cases must still authorize the requested operation.
func (o Owner) AvailableTo(s Scope) bool {
	return o.Validate() == nil && s.Validate() == nil && o.TenantID == s.TenantID &&
		(o.ProjectID == "" || o.ProjectID == s.ProjectID)
}
