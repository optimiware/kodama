package exhttp_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The OpenAPI document must describe every route and every error code the handler
// can emit. This is a drift check, not a schema validator.
func TestOpenAPIDescribesTheImplementedInterface(t *testing.T) {
	raw, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	for _, want := range []string{
		"/healthz:", "/readyz:", "/version:",
		"/api/v1/tenants/{tenantId}/exceptions:", "/api/v1/tenants/{tenantId}/exceptions/{id}:",
		"/api/v1/tenants/{tenantId}/exceptions/{id}/history:", "/api/v1/tenants/{tenantId}/exceptions/{id}/resolution:",
		"operationId: createException", "operationId: listExceptions", "operationId: getException", "operationId: resolveException",
		"Idempotency-Key", "If-Match", "'202'", "'412'", "'428'",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("openapi.yaml does not mention %q", want)
		}
	}
	// Every machine code the handler emits is in the documented enum.
	src, _ := os.ReadFile("errors.go")
	codes := regexp.MustCompile(`problem\{http\.\w+, "([a-z_]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(codes) < 10 {
		t.Fatalf("found only %d error codes in errors.go", len(codes))
	}
	for _, c := range codes {
		if !strings.Contains(spec, c[1]) {
			t.Errorf("error code %q is not documented", c[1])
		}
	}
}
