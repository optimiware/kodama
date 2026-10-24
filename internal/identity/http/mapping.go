package identityhttp

import (
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

func ts(t time.Time) time.Time { return t.UTC() }

func roleDTOs(rs []identity.Role) []Role {
	out := make([]Role, 0, len(rs))
	for _, r := range rs {
		out = append(out, Role(r))
	}
	return out
}

func meDTO(u identity.User) Me {
	return Me{Id: string(u.ID), Issuer: u.Issuer, Subject: u.Subject, CreatedAt: ts(u.CreatedAt)}
}

func tenantDTO(t identity.TenantSummary) Tenant {
	return Tenant{Id: string(t.ID), Key: t.Key, DisplayName: t.DisplayName, Roles: roleDTOs(t.Roles)}
}

func bindingDTO(b identity.Binding) Membership {
	m := Membership{Id: string(b.ID), UserId: string(b.User), Role: Role(b.Role), Version: int64(b.Version), CreatedAt: ts(b.CreatedAt)}
	if b.Project != "" {
		p := string(b.Project)
		m.ProjectId = &p
	}
	return m
}

func projectDTO(v identity.ProjectView) Project {
	p := v.Project
	settings := p.Settings
	if len(settings) == 0 {
		settings = json.RawMessage(`{}`)
	}
	return Project{
		Id: string(p.Scope.ProjectID), Key: p.Key, DisplayName: p.DisplayName, Settings: settings, ActiveWipLimit: p.ActiveWIPLimit,
		Version: int64(p.Version), CreatedAt: ts(p.CreatedAt), UpdatedAt: ts(p.UpdatedAt), CallerRoles: roleDTOs(v.CallerRoles),
	}
}

func rawSettings(s *json.RawMessage) json.RawMessage {
	if s == nil {
		return nil
	}
	return *s
}

func projectID(s string) domain.ProjectID { return domain.ProjectID(s) }
