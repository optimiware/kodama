package platform_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestSecretLookupRoundTripsScopedEnvelope asserts the lookup returns the exact
// scoped metadata and retained envelope bytes.
func TestSecretLookupRoundTripsScopedEnvelope(t *testing.T) {
	s := newStore()
	seedSecret(t, s, testScope(), testSecretID)

	lookup, err := platform.NewSecretLookup(s)
	if err != nil {
		t.Fatalf("NewSecretLookup: %v", err)
	}
	stored, err := lookup.Lookup(context.Background(), testScope(), testSecretName)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if string(stored.Metadata.ID) != testSecretID {
		t.Errorf("secret id = %q, want %q", stored.Metadata.ID, testSecretID)
	}
	if stored.Metadata.Scope != testScope() {
		t.Errorf("secret scope = %#v, want %#v", stored.Metadata.Scope, testScope())
	}
	if stored.Metadata.Name != testSecretName {
		t.Errorf("secret name = %q, want %q", stored.Metadata.Name, testSecretName)
	}
	if stored.Metadata.Version != 4 {
		t.Errorf("secret version = %d, want 4", stored.Metadata.Version)
	}
	if !stored.Metadata.UpdatedAt.Equal(fixedNow.Add(time.Minute)) {
		t.Errorf("secret updated_at = %v", stored.Metadata.UpdatedAt)
	}
	if !stored.CreatedAt.Equal(fixedNow) {
		t.Errorf("secret created_at = %v", stored.CreatedAt)
	}
	if stored.Envelope.KeyRef != "key-1" || stored.Envelope.KeyVersion != 2 {
		t.Errorf("envelope key = %q/%d, want key-1/2", stored.Envelope.KeyRef, stored.Envelope.KeyVersion)
	}
	if !bytes.Equal(stored.Envelope.Ciphertext, []byte("ciphertext-bytes")) {
		t.Errorf("ciphertext = %q", stored.Envelope.Ciphertext)
	}
	if !bytes.Equal(stored.Envelope.Nonce, bytesOfLength(12, 9)) {
		t.Errorf("nonce did not round-trip")
	}
	if !bytes.Equal(stored.Envelope.WrappedKey, bytesOfLength(48, 7)) {
		t.Errorf("wrapped key did not round-trip")
	}
	if !bytes.Equal(stored.Envelope.WrapNonce, bytesOfLength(12, 3)) {
		t.Errorf("wrap nonce did not round-trip")
	}
}

// TestSecretLookupReadsTenantWideRow asserts a tenant-scoped lookup reads the
// tenant-wide row and a project lookup does not silently widen to it.
func TestSecretLookupReadsTenantWideRow(t *testing.T) {
	s := newStore()
	seedSecret(t, s, tenancy.Scope{TenantID: testTenant}, testSecretID)

	lookup, err := platform.NewSecretLookup(s)
	if err != nil {
		t.Fatalf("NewSecretLookup: %v", err)
	}
	stored, err := lookup.Lookup(context.Background(), tenancy.Scope{TenantID: testTenant}, testSecretName)
	if err != nil {
		t.Fatalf("tenant-wide Lookup: %v", err)
	}
	if stored.Metadata.Scope.ProjectScoped() {
		t.Errorf("tenant-wide secret scope = %#v, want no project", stored.Metadata.Scope)
	}
	_, err = lookup.Lookup(context.Background(), testScope(), testSecretName)
	requireClass(t, err, fault.NotFound)
}

// TestSecretLookupIsScopeIsolated asserts another scope cannot observe the row.
func TestSecretLookupIsScopeIsolated(t *testing.T) {
	s := newStore()
	seedSecret(t, s, testScope(), testSecretID)

	lookup, err := platform.NewSecretLookup(s)
	if err != nil {
		t.Fatalf("NewSecretLookup: %v", err)
	}
	_, err = lookup.Lookup(context.Background(), otherScope(), testSecretName)
	requireClass(t, err, fault.NotFound)
}

// TestSecretLookupRefusesInvalidName asserts the adapter refuses an unusable
// declared name before running a scan.
func TestSecretLookupRefusesInvalidName(t *testing.T) {
	s := newStore()
	lookup, err := platform.NewSecretLookup(s)
	if err != nil {
		t.Fatalf("NewSecretLookup: %v", err)
	}
	_, err = lookup.Lookup(context.Background(), testScope(), id.SecretName(id.SecretTokenName))
	requireClass(t, err, fault.InvalidInput)
}
