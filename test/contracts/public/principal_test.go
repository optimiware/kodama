package publiccontract

import (
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/userland/principal"
)

// TestUserlandPrincipalForOIDCMatchesShellOIDCDerivation pins that Userland's
// pure principal.ForOIDC helper produces the exact same PrincipalID and fault
// classification as Shell's oidc.PrincipalIDFor across normal, social, unicode,
// separator-boundary and invalid inputs.
func TestUserlandPrincipalForOIDCMatchesShellOIDCDerivation(t *testing.T) {
	cases := []struct {
		name    string
		issuer  string
		subject string
	}{
		{"https standard", "https://auth.kodama.example", "admin"},
		{"trailing slash issuer", "https://tenant.eu.auth0.com/", "auth0|123456"},
		{"loopback port", "http://127.0.0.1:8082", "reviewer"},
		{"ipv6 loopback", "http://[::1]:8082", "engineer"},
		{"github subject", "https://auth.kodama.example", "github:octocat"},
		{"email subject", "https://auth.kodama.example", "email:reviewer@example.com"},
		{"upstream oidc subject", "https://auth.kodama.example", "oidc:https://accounts.google.com:10987654321"},
		{"separator left", "ab", "c"},
		{"separator right", "a", "bc"},
		{"embedded nul in subject", "https://auth.kodama.example", "part1\x00part2"},
		{"unicode subject", "https://auth.kodama.example", "user-木の精-λ"},
		{"long subject", "https://auth.kodama.example", strings.Repeat("s", 4096)},
		{"whitespace preserved", " https://auth.kodama.example ", " sub "},
		{"empty issuer", "", "admin"},
		{"empty subject", "https://auth.kodama.example", ""},
		{"both empty", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotErr := principal.ForOIDC(tc.issuer, tc.subject)
			wantID, wantErr := oidc.PrincipalIDFor(tc.issuer, tc.subject)

			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("ForOIDC(%q, %q) err = %v; oidc.PrincipalIDFor err = %v", tc.issuer, tc.subject, gotErr, wantErr)
			}
			if wantErr != nil {
				gotClass, gotOK := fault.ClassOf(gotErr)
				wantClass, wantOK := fault.ClassOf(wantErr)
				if gotOK != wantOK || gotClass != wantClass {
					t.Fatalf("fault class = (%v, %v); want (%v, %v)", gotClass, gotOK, wantClass, wantOK)
				}
				return
			}
			if gotID != wantID {
				t.Fatalf("ForOIDC(%q, %q) = %q; want %q", tc.issuer, tc.subject, gotID, wantID)
			}
		})
	}

	for i := range 256 {
		issuer := fmt.Sprintf("https://issuer-%d.kodama.invalid:%d/path", i%17, 8000+i)
		var subject string
		switch i % 3 {
		case 0:
			var err error
			subject, err = principal.SubjectForGitHub(fmt.Sprintf("Dev-User-%d", i))
			if err != nil {
				t.Fatalf("SubjectForGitHub: %v", err)
			}
		case 1:
			var err error
			subject, err = principal.SubjectForEmail(fmt.Sprintf("User.%d@Example.ORG", i))
			if err != nil {
				t.Fatalf("SubjectForEmail: %v", err)
			}
		default:
			var err error
			subject, err = principal.SubjectForLocal(fmt.Sprintf("persona_%d", i))
			if err != nil {
				t.Fatalf("SubjectForLocal: %v", err)
			}
		}
		got, err := principal.ForOIDC(issuer, subject)
		if err != nil {
			t.Fatalf("ForOIDC(%q, %q): %v", issuer, subject, err)
		}
		want, err := oidc.PrincipalIDFor(issuer, subject)
		if err != nil {
			t.Fatalf("oidc.PrincipalIDFor(%q, %q): %v", issuer, subject, err)
		}
		if got != want {
			t.Fatalf("mismatch at %d: got %q, want %q", i, got, want)
		}
	}
}
