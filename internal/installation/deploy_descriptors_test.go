package installation

// Cross-provider descriptor checks: every OpenTofu root must emit the
// production TLS mode Preflight requires. The scw root is pinned in
// deploy_scw_test.go; this file covers all three providers together.

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDeployDescriptorsEmitVerifyFull pins the three provider descriptors.
// Production Preflight refuses a PostgreSQL DSN that carries
// sslmode=verify-ca, so gcp and aws must match scw.
func TestDeployDescriptorsEmitVerifyFull(t *testing.T) {
	for _, cloud := range []string{"scw", "gcp", "aws"} {
		path := filepath.Join(repoRoot(t), "deploy", cloud, "outputs.tf")
		body := readText(t, path)
		if !regexp.MustCompile(`(?m)^[ \t]*ssl_mode[ \t]*=[ \t]*"verify-full"[ \t]*$`).MatchString(body) {
			t.Errorf("deploy/%s/outputs.tf does not emit ssl_mode = \"verify-full\"", cloud)
		}
		if strings.Contains(body, `"verify-ca"`) {
			t.Errorf("deploy/%s/outputs.tf still emits verify-ca", cloud)
		}
	}
}
