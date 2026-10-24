package domain

import "fmt"

// Scope identifies the tenant and project that own a record. Every runtime
// reference must be compared by scope; an identifier alone is never authority.
type Scope struct {
	TenantID  TenantID  `json:"tenantId"`
	ProjectID ProjectID `json:"projectId"`
}

func (s Scope) Validate() error {
	if err := s.TenantID.Validate(); err != nil {
		return err
	}
	return s.ProjectID.Validate()
}

func (s Scope) String() string { return fmt.Sprintf("%s/%s", s.TenantID, s.ProjectID) }
