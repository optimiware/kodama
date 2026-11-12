package factory

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// TestSanitizeDiagnosticRedactsSecretPayloads is the regression guard for the
// failure-dump credential leak: a kubectl Secret rendered as YAML must never
// reach the test log with its base64 payload or the decoded token.
func TestSanitizeDiagnosticRedactsSecretPayloads(t *testing.T) {
	token := "s3cr3t-execution-token"
	encoded := base64.StdEncoding.EncodeToString([]byte(token))
	if strings.Contains(token, encoded) {
		t.Fatalf("test fixture token %q already contains its encoding %q", token, encoded)
	}

	secret := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: workload-credential
  labels:
    kodama.dev/execution: exec-1
data:
  KODAMA_TOKEN: %s
stringData:
  KODAMA_TOKEN: %s
`, encoded, token)
	output := SanitizeDiagnostic(secret, token)
	for _, forbidden := range []string{encoded, token, "KODAMA_TOKEN: " + encoded, "KODAMA_TOKEN: " + token} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("sanitized Secret still contains %q:\n%s", forbidden, output)
		}
	}
	for _, required := range []string{"workload-credential", "kodama.dev/execution", "exec-1"} {
		if !strings.Contains(output, required) {
			t.Fatalf("sanitized Secret lost the diagnostic metadata %q:\n%s", required, output)
		}
	}

	// A kubectl list wraps each Secret in a List item; the payload must be
	// scrubbed there too.
	list := fmt.Sprintf(`apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: Secret
  metadata:
    name: workload-credential
  data:
    KODAMA_TOKEN: %s
`, encoded)
	listOutput := SanitizeDiagnostic(list)
	for _, forbidden := range []string{encoded, token} {
		if strings.Contains(listOutput, forbidden) {
			t.Fatalf("sanitized Secret List still contains %q:\n%s", forbidden, listOutput)
		}
	}

	// Plain log text has no Secret object to scrub, so a caller-supplied value
	// is removed in both its raw and base64 forms.
	logText := "credential:" + token + "\nencoded:" + encoded + "\n"
	logOutput := SanitizeDiagnostic(logText, token)
	for _, forbidden := range []string{token, encoded} {
		if strings.Contains(logOutput, forbidden) {
			t.Fatalf("sanitized log still contains %q:\n%s", forbidden, logOutput)
		}
	}
	if !strings.Contains(logOutput, "credential:"+DiagnosticRedacted) {
		t.Fatalf("sanitized log lost its redaction marker:\n%s", logOutput)
	}

	// Text with no Secret payload is returned byte-for-byte.
	pod := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: workload\n"
	if got := SanitizeDiagnostic(pod); got != pod {
		t.Fatalf("non-Secret document was reformatted:\nwant:\n%s\ngot:\n%s", pod, got)
	}

	// Invalid YAML still falls back to dropping a data block rather than
	// returning it.
	broken := "kind: Secret\n\tdata:\n\t  KODAMA_TOKEN: " + encoded + "\n"
	brokenOutput := SanitizeDiagnostic(broken)
	if strings.Contains(brokenOutput, encoded) {
		t.Fatalf("fallback sanitizer returned a Secret payload:\n%s", brokenOutput)
	}
}

// TestDiagnosticKubectlGetArgsNeverRequestsSecretPayloads proves the diagnostic
// kubectl argument builder cannot ask for Secret data: a Secret kind is rendered
// as a name list, while other kinds keep the YAML detail.
func TestDiagnosticKubectlGetArgsNeverRequestsSecretPayloads(t *testing.T) {
	secretArgs := DiagnosticKubectlGetArgs("secrets", "-l", "kodama.dev/execution=exec-1")
	joined := strings.Join(secretArgs, " ")
	if strings.Contains(joined, "yaml") || strings.Contains(joined, "json") {
		t.Fatalf("Secret diagnostic args request a payload-bearing output: %q", joined)
	}
	if !strings.Contains(joined, "-o name") {
		t.Fatalf("Secret diagnostic args = %q; want a name list", joined)
	}
	if got := secretArgs[0]; got != "get" {
		t.Fatalf("Secret diagnostic args start with %q; want get", got)
	}

	for _, resource := range []string{"jobs", "pods", "configmaps", "persistentvolumeclaim"} {
		args := DiagnosticKubectlGetArgs(resource, "-l", "kodama.dev/execution=exec-1")
		if !strings.Contains(strings.Join(args, " "), "-o yaml") {
			t.Fatalf("%s diagnostic args = %v; want YAML", resource, args)
		}
	}

	// An explicit caller output is preserved rather than duplicated.
	explicit := DiagnosticKubectlGetArgs("pods", "-o", "wide")
	if strings.Count(strings.Join(explicit, " "), "-o") != 1 {
		t.Fatalf("explicit output was duplicated: %v", explicit)
	}
}
