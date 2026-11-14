package principal

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

func TestForOIDCValidationAndDeterminism(t *testing.T) {
	got, err := ForOIDC("https://auth.example.com", "github:octocat")
	if err != nil {
		t.Fatalf("ForOIDC: %v", err)
	}
	if !strings.HasPrefix(string(got), "oidc-") || len(got) != len("oidc-")+32 {
		t.Fatalf("ForOIDC = %q; want oidc-<32 hex chars>", got)
	}
	if _, err := id.ParsePrincipalID(string(got)); err != nil {
		t.Fatalf("derived id %q is not a valid PrincipalID: %v", got, err)
	}

	again, err := ForOIDC("https://auth.example.com", "github:octocat")
	if err != nil || again != got {
		t.Fatalf("ForOIDC repeat = %q, %v; want %q", again, err, got)
	}

	left, err := ForOIDC("ab", "c")
	if err != nil {
		t.Fatalf("ForOIDC ab/c: %v", err)
	}
	right, err := ForOIDC("a", "bc")
	if err != nil {
		t.Fatalf("ForOIDC a/bc: %v", err)
	}
	if left == right {
		t.Fatal("ForOIDC collided across issuer/subject boundary")
	}

	for _, tc := range []struct {
		issuer  string
		subject string
	}{
		{"", "sub"},
		{"https://auth.example.com", ""},
	} {
		if _, err := ForOIDC(tc.issuer, tc.subject); err == nil {
			t.Fatalf("ForOIDC(%q, %q) succeeded; want invalid_input", tc.issuer, tc.subject)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("ForOIDC(%q, %q) class = %v; want %v", tc.issuer, tc.subject, class, fault.InvalidInput)
		}
	}
}

func TestSubjectHelpers(t *testing.T) {
	gh, err := SubjectForGitHub("  Octo-Cat99 ")
	if err != nil {
		t.Fatalf("SubjectForGitHub: %v", err)
	}
	if gh != "github:octo-cat99" {
		t.Fatalf("SubjectForGitHub = %q; want github:octo-cat99", gh)
	}
	for _, bad := range []string{
		"",
		"   ",
		"-leading",
		"trailing-",
		"@octocat",
		"octo_cat",
		"octo/cat",
		"octo:cat",
		"octo cat",
		strings.Repeat("a", 40),
	} {
		if _, err := SubjectForGitHub(bad); err == nil {
			t.Errorf("SubjectForGitHub(%q) succeeded; want error", bad)
		}
	}

	em, err := SubjectForEmail("  Alice.Reviewer+1@Example.COM ")
	if err != nil {
		t.Fatalf("SubjectForEmail: %v", err)
	}
	if em != "email:alice.reviewer+1@example.com" {
		t.Fatalf("SubjectForEmail = %q; want email:alice.reviewer+1@example.com", em)
	}
	for _, bad := range []string{
		"",
		"   ",
		"plain",
		"@example.com",
		"alice@",
		"alice@b@c.com",
		"alice @example.com",
		"alice\n@example.com",
		".alice@example.com",
		"alice@example.com.",
	} {
		if _, err := SubjectForEmail(bad); err == nil {
			t.Errorf("SubjectForEmail(%q) succeeded; want error", bad)
		}
	}

	local, err := SubjectForLocal("  reviewer_1-a ")
	if err != nil {
		t.Fatalf("SubjectForLocal: %v", err)
	}
	if local != "reviewer_1-a" {
		t.Fatalf("SubjectForLocal = %q; want reviewer_1-a", local)
	}
	for _, bad := range []string{
		"",
		"   ",
		".",
		"..",
		"github:octocat",
		"email:alice@example.com",
		"bad space",
		"bad/slash",
		strings.Repeat("x", id.MaxIDLength+1),
	} {
		if _, err := SubjectForLocal(bad); err == nil {
			t.Errorf("SubjectForLocal(%q) succeeded; want error", bad)
		}
	}
}
