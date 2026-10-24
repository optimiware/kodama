package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestIDSyntax(t *testing.T) {
	for _, ok := range []string{"t_acme", "wi_1042", "A", "0", "a-b_c", strings.Repeat("a", 128)} {
		if err := TenantID(ok).Validate(); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "_x", "-x", "a b", "a/b", "a.b", "t:1", "é", "a\n", strings.Repeat("a", 129)} {
		err := ArtifactID(bad).Validate()
		if !errors.Is(err, ErrInvalidValue) {
			t.Errorf("%q accepted or wrong error: %v", bad, err)
		}
	}
}

func TestScopeValidation(t *testing.T) {
	if err := (Scope{TenantID: "t_acme", ProjectID: "p_inventory"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []Scope{{ProjectID: "p"}, {TenantID: "t"}, {TenantID: "t/x", ProjectID: "p"}} {
		if s.Validate() == nil {
			t.Errorf("%+v accepted", s)
		}
	}
	if (Scope{"t_acme", "p_a"}) == (Scope{"t_acme", "p_b"}) {
		t.Fatal("scopes in different projects compare equal")
	}
}

func TestVersionBounds(t *testing.T) {
	for _, v := range []int64{1, 7, MaxSafeInteger} {
		if Version(v).Validate() != nil || ContentRevision(v).Validate() != nil {
			t.Errorf("%d rejected", v)
		}
	}
	for _, v := range []int64{0, -1, MaxSafeInteger + 1} {
		if Version(v).Validate() == nil || ContentRevision(v).Validate() == nil {
			t.Errorf("%d accepted", v)
		}
	}
}

func TestErrorString(t *testing.T) {
	if got := (&Error{Code: CodeNotFound, Message: "gone"}).Error(); got != "not_found: gone" {
		t.Fatal(got)
	}
}

func TestErrConflictMatchesWhenWrapped(t *testing.T) {
	if err := fmt.Errorf("put: %w", ErrConflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrapped ErrConflict does not match: %v", err)
	}
}
