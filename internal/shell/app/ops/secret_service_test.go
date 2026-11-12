package opsapp_test

import (
	"bytes"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestSecretFamily drives the real secret manager: write, metadata read (both
// by declared secret name and by row SecretID) and delete. The written value
// never appears in any result.
func TestSecretFamily(t *testing.T) {
	h := newHarness(t)

	written, err := h.invoke(t, testToken, "secret.write", `{"value":"s3cr3t-value"}`, map[string]string{
		"project": testProject,
		"secret":  "DB_PASSWORD",
	})
	if err != nil {
		t.Fatalf("secret.write: %v", err)
	}
	var write api.SecretWriteResult
	decodeResult(t, written, &write)
	if write.SecretID == "" || write.Name != "DB_PASSWORD" || write.Version != 1 {
		t.Fatalf("write result = %+v", write)
	}
	if string(written.Body) == "" || bytes.Contains(written.Body, []byte("s3cr3t-value")) {
		t.Fatalf("secret.write result leaks the value: %s", written.Body)
	}

	byName, err := h.invoke(t, testToken, "secret.metadata.read", `{}`, map[string]string{
		"project": testProject,
		"secret":  "DB_PASSWORD",
	})
	if err != nil {
		t.Fatalf("secret.metadata.read by name: %v", err)
	}
	var readByName api.SecretMetadataReadResult
	decodeResult(t, byName, &readByName)
	if readByName.SecretID != write.SecretID || readByName.Name != "DB_PASSWORD" || readByName.Version != 1 {
		t.Fatalf("metadata by name result = %+v", readByName)
	}
	if bytes.Contains(byName.Body, []byte("s3cr3t-value")) {
		t.Fatalf("secret.metadata.read by name result leaks the value: %s", byName.Body)
	}

	metadata, err := h.invoke(t, testToken, "secret.metadata.read", `{}`, map[string]string{
		"project": testProject,
		"secret":  write.SecretID,
	})
	if err != nil {
		t.Fatalf("secret.metadata.read: %v", err)
	}
	var read api.SecretMetadataReadResult
	decodeResult(t, metadata, &read)
	if read != readByName {
		t.Fatalf("metadata by id = %+v; want %+v", read, readByName)
	}
	if bytes.Contains(metadata.Body, []byte("s3cr3t-value")) {
		t.Fatalf("secret.metadata.read result leaks the value: %s", metadata.Body)
	}

	if _, err := h.invoke(t, testToken, "secret.metadata.read", `{}`, map[string]string{
		"project": testProject,
		"secret":  id.SecretTokenName,
	}); err == nil {
		t.Fatal("secret.metadata.read accepted KODAMA_TOKEN")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("KODAMA_TOKEN metadata read class = %v (%v); want invalid_input", class, err)
	}

	deleted, err := h.invokeVersion(t, testToken, "secret.delete", `{}`, 1, map[string]string{
		"project": testProject,
		"secret":  "DB_PASSWORD",
	})
	if err != nil {
		t.Fatalf("secret.delete: %v", err)
	}
	var deletion api.SecretDeleteResult
	decodeResult(t, deleted, &deletion)
	if deletion.Version != 2 {
		t.Fatalf("delete version = %d; want 2", deletion.Version)
	}
}

// TestSecretMetadataReadConfirmsCurrentMembership proves secret.metadata.read
// confirms current CheckMembership authority at read time rather than trusting
// a stale principal snapshot.
func TestSecretMetadataReadConfirmsCurrentMembership(t *testing.T) {
	h := newHarness(t)
	if _, err := h.invoke(t, testToken, "secret.write", `{"value":"s3cr3t-value"}`, map[string]string{
		"project": testProject,
		"secret":  "DB_PASSWORD",
	}); err != nil {
		t.Fatalf("secret.write: %v", err)
	}

	h.principals.principals["token-revoked-viewer"] = identity.Principal{
		ID:      "viewer-revoked",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: testProject},
		Roles:   []tenancy.Role{tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	}
	h.buildRouter(t)

	_, err := h.invoke(t, "token-revoked-viewer", "secret.metadata.read", `{}`, map[string]string{
		"project": testProject,
		"secret":  "DB_PASSWORD",
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("revoked viewer secret.metadata.read = %v; want forbidden", err)
	}
}

// TestServiceCredentialFamily drives the real service administration and
// credential lifecycle services. Only issue and rotate carry a token.
func TestServiceCredentialFamily(t *testing.T) {
	h := newHarness(t)
	// The tenant derivation key must exist before a credential can be derived.
	if _, err := h.invoke(t, "token-admin", "tenant.create", `{"tenant_id":"tenant-a","name":"Tenant A"}`, nil); err != nil {
		t.Fatalf("tenant.create: %v", err)
	}

	created, err := h.invoke(t, testToken, "service.create", `{"service_id":"service-1","roles":["engineer"]}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("service.create: %v", err)
	}
	var service api.ServiceCreateResult
	decodeResult(t, created, &service)
	if service.ServiceID != "service-1" || service.Version != 1 {
		t.Fatalf("service.create result = %+v", service)
	}

	updated, err := h.invokeVersion(t, testToken, "service.update", `{"roles":["viewer"]}`, 1, map[string]string{"service": "service-1"})
	if err != nil {
		t.Fatalf("service.update: %v", err)
	}
	var update api.ServiceUpdateResult
	decodeResult(t, updated, &update)
	if update.Version != 2 {
		t.Fatalf("service.update version = %d; want 2", update.Version)
	}

	disabled, err := h.invokeVersion(t, testToken, "service.disable", `{}`, 2, map[string]string{"service": "service-1"})
	if err != nil {
		t.Fatalf("service.disable: %v", err)
	}
	var disabledResult api.ServiceDisableResult
	decodeResult(t, disabled, &disabledResult)
	if disabledResult.State != "disabled" || disabledResult.Version != 3 {
		t.Fatalf("service.disable result = %+v", disabledResult)
	}

	enabled, err := h.invokeVersion(t, testToken, "service.enable", `{}`, 3, map[string]string{"service": "service-1"})
	if err != nil {
		t.Fatalf("service.enable: %v", err)
	}
	var enabledResult api.ServiceEnableResult
	decodeResult(t, enabled, &enabledResult)
	if enabledResult.State != "enabled" || enabledResult.Version != 4 {
		t.Fatalf("service.enable result = %+v", enabledResult)
	}

	issued, err := h.invokeVersion(t, testToken, "service.credential.issue", `{"ttl_seconds":3600}`, 4, map[string]string{"service": "service-1"})
	if err != nil {
		t.Fatalf("service.credential.issue: %v", err)
	}
	var issue api.ServiceCredentialIssueResult
	decodeResult(t, issued, &issue)
	if issue.State != "current" || issue.Version != 5 || issue.Token == nil || *issue.Token == "" {
		t.Fatalf("issue result = %+v", issue)
	}

	rotated, err := h.invokeVersion(t, testToken, "service.credential.rotate", `{"ttl_seconds":3600}`, 5, map[string]string{"service": "service-1"})
	if err != nil {
		t.Fatalf("service.credential.rotate: %v", err)
	}
	var rotation api.ServiceCredentialRotateResult
	decodeResult(t, rotated, &rotation)
	if rotation.Version != 6 || rotation.Token == nil || *rotation.Token == "" {
		t.Fatalf("rotate result = %+v", rotation)
	}

	revoked, err := h.invokeVersion(t, testToken, "service.credential.revoke", `{}`, 6, map[string]string{"service": "service-1"})
	if err != nil {
		t.Fatalf("service.credential.revoke: %v", err)
	}
	var revocation api.ServiceCredentialRevokeResult
	decodeResult(t, revoked, &revocation)
	if revocation.Version != 7 || revocation.Invalidated == nil {
		t.Fatalf("revoke result = %+v", revocation)
	}
	if bytes.Contains(revoked.Body, []byte("\"token\"")) {
		t.Fatalf("credential revoke result carries a token: %s", revoked.Body)
	}
}
