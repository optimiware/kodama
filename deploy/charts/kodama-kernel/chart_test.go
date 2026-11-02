package kodama_kernel_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func render(t *testing.T, extra ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("KODAMA_REQUIRE_HELM") == "1" {
			t.Fatal("helm is required")
		}
		t.Skip("helm is not installed")
	}
	args := []string{"template", "kodama", ".", "--namespace", "kodama-system"}
	args = append(args, extra...)
	cmd := exec.Command("helm", args...)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return string(out)
}

func Test_kernel_chart_renders_the_kernel_assembly(t *testing.T) {
	out := render(t)
	for _, want := range []string{
		`"--assembly"`, `"kernel"`, `name: KODAMA_ENV`, `value: "production"`,
		`name: KODAMA_EXECUTOR`, `value: "kubernetes"`,
		`name: KODAMA_GIT_HOSTING`, `value: "embedded"`,
		`name: KODAMA_GIT_EMBEDDED_ACK`, `value: "stage-only"`,
		`path: /readyz`, `kube.json`, `kms.json`,
		`name: kodama-tenant-example`, `pod-security.kubernetes.io/enforce: restricted`,
		`kind: Role`, `kind: RoleBinding`, `kind: ClusterRoleBinding`,
		`persistentVolumeClaim:`, `mountPath: /var/lib/kodama`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered chart has no %q", want)
		}
	}
	if strings.Contains(out, `"--assembly", "core"`) {
		t.Fatal("the kernel chart renders the legacy core assembly")
	}
}

func Test_kernel_chart_rejects_hosted_authority_and_bad_kms(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	cmd := exec.Command("helm", "template", "kodama", ".", "--namespace", "kodama-system",
		"--set", "image.digest=sha256:short")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("an unpinned image rendered:\n%s", out)
	}
}
