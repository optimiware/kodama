package factory

// Unit tests for the test-owned race barriers. These are the harness primitives
// the real-stack races rely on: the canonical membership advisory key must
// reproduce the store's own encoding byte for byte, and the stop gate must block
// exactly the armed call and release on the test's signal (or context).

import (
	"context"
	"testing"
	"time"
)

func TestMembershipKeyTextMatchesAbsenceKeyEncoding(t *testing.T) {
	cases := []struct {
		name string
		key  MembershipKey
		want string
	}{
		{
			name: "tenant-wide row has a NULL project segment",
			key:  MembershipKey{TenantID: "tenant-i04", PrincipalID: "engineer-i04"},
			want: "t11:memberships;s10:tenant-i04;n;s12:engineer-i04;",
		},
		{
			name: "project row carries the project segment",
			key:  MembershipKey{TenantID: "tenant-i04", ProjectID: "project-i04", PrincipalID: "engineer-i04"},
			want: "t11:memberships;s10:tenant-i04;s11:project-i04;s12:engineer-i04;",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := membershipKeyText(test.key)
			if err != nil {
				t.Fatalf("membershipKeyText: %v", err)
			}
			if got != test.want {
				t.Fatalf("membershipKeyText = %q; want %q", got, test.want)
			}
		})
	}
}

func TestMembershipKeyTextRefusesIncompleteKey(t *testing.T) {
	for _, key := range []MembershipKey{
		{PrincipalID: "principal-1"},
		{TenantID: "tenant-1"},
		{TenantID: "tenant-1", PrincipalID: "  "},
	} {
		if _, err := membershipKeyText(key); err == nil {
			t.Fatalf("membershipKeyText(%+v) accepted an incomplete key", key)
		}
	}
}

func TestStopGateGatesExactlyOneArmedCall(t *testing.T) {
	gate := NewStopGate()
	gate.Arm()
	done := make(chan error, 1)
	go func() { done <- gate.wait(context.Background()) }()

	select {
	case <-gate.Reached():
	case <-time.After(2 * time.Second):
		t.Fatal("the gated call never reported that it was reached")
	}
	select {
	case err := <-done:
		t.Fatalf("gated call returned before Release: %v", err)
	default:
	}

	gate.Release()
	if err := <-done; err != nil {
		t.Fatalf("gated call after Release: %v", err)
	}
	// Only the armed call is gated: a later call passes through immediately.
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("second call was gated: %v", err)
	}
	// Release is idempotent and harmless before a later Arm.
	gate.Release()
	gate.Arm()
	gate.Release()
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("armed call after a pre-release was gated: %v", err)
	}
}

func TestStopGateContextCancellationReleases(t *testing.T) {
	gate := NewStopGate()
	gate.Arm()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gate.wait(ctx) }()
	select {
	case <-gate.Reached():
	case <-time.After(2 * time.Second):
		t.Fatal("the gated call never reported that it was reached")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("gated call ignored context cancellation")
	}
}
