package factory

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Role-limited human fixture principals.
//
// The shared ProjectToken belongs to one human holding several roles, so it can
// never prove an assignment or a refusal that depends on exactly one role. These
// principals hold exactly one project role each and are registered as explicit
// development tokens plus public project memberships. EngineerPrincipal is
// project-scoped only: its tenant-wide membership row is genuinely absent, which
// is the absence pin the admission-versus-authority race holds.
const (
	ReviewerPrincipal = "reviewer-i04"
	OperatorPrincipal = "operator-i04"
	EngineerPrincipal = "engineer-i04"
	// AdmissionPrincipal is project-scoped but has no project membership: the
	// harness seeds it a tenant-wide engineer membership instead, so a
	// project-scoped admission pins its absent exact-project membership row. The
	// seed exists because the public API exposes only project-scoped
	// membership writes; the race itself uses the public project-scoped first
	// grant.
	AdmissionPrincipal = "admission-i04"
)

// LimitedPrincipal is one role-limited human fixture principal.
type LimitedPrincipal struct {
	ID        id.PrincipalID
	Token     string
	Role      tenancy.Role
	expiresAt time.Time
}

// newLimitedPrincipal mints one random bearer credential for a role-limited
// human. The token is generated per run with crypto/rand so no credential value
// is ever committed to source.
func newLimitedPrincipal(t *testing.T, principalID string, role tenancy.Role, expiresAt time.Time) LimitedPrincipal {
	t.Helper()
	return LimitedPrincipal{
		ID:        id.PrincipalID(principalID),
		Token:     randomToken(t),
		Role:      role,
		expiresAt: expiresAt,
	}
}

// DevToken renders the principal as an explicit development authenticator
// token. Roles are exactly the limited role so the grant stage can never rely on
// another role held by the same human.
func (principal LimitedPrincipal) DevToken() app.DevToken {
	return app.DevToken{
		Credential: principal.Token,
		Principal: identity.Principal{
			ID:      principal.ID,
			Kind:    identity.KindHuman,
			Scope:   tenancy.Scope{TenantID: id.TenantID(TenantID), ProjectID: id.ProjectID(ProjectID)},
			Roles:   []tenancy.Role{principal.Role},
			Enabled: true,
			Version: 1,
		},
		ExpiresAt: principal.expiresAt,
	}
}

// randomToken returns one fresh 256-bit bearer token. A failure to read secure
// randomness fails the test instead of falling back to a predictable value.
func randomToken(t *testing.T) string {
	t.Helper()
	material := make([]byte, 32)
	if _, err := rand.Read(material); err != nil {
		t.Fatalf("factory: generate principal token: %v", err)
	}
	return "fixture-i04-token-" + hex.EncodeToString(material)
}
