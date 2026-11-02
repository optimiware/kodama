package domain

import "testing"

func TestOwnerAvailability(t *testing.T) {
	scope := Scope{TenantID: "a", ProjectID: "p"}
	for _, tc := range []struct {
		owner     Owner
		available bool
	}{
		{Owner{TenantID: "a"}, true}, {Owner{TenantID: "a", ProjectID: "p"}, true},
		{Owner{TenantID: "a", ProjectID: "other"}, false}, {Owner{TenantID: "b"}, false}, {Owner{}, false},
	} {
		if got := tc.owner.AvailableTo(scope); got != tc.available {
			t.Fatalf("%+v = %v", tc.owner, got)
		}
	}
}
