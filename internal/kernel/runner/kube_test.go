package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

func TestKubeStepJobIsRestrictedAndDeterministic(t *testing.T) {
	k := &Kube{cfg: KubeConfig{
		Tenants:  map[string]KubeTenant{"t_acme": {Namespace: "kodama-tenant-acme", ServiceAccount: "kodama-execution"}},
		Storage:  KubeStorage{Class: "scw-bssd", Size: "10Gi"},
		Identity: KubeIdentity{RunAsUser: 65532, RunAsGroup: 65532, PullPolicy: "IfNotPresent"},
		Checkout: KubeCheckout{Image: "registry.invalid/kodama@sha256:" + strings.Repeat("a", 64)},
	}}
	spec := Spec{Scope: domain.Scope{TenantID: "t_acme", ProjectID: "p_one"}, RunID: "run_1", StepKey: "build",
		Image: "registry.invalid/step@sha256:" + strings.Repeat("b", 64), Commands: []string{"echo ok"}, Timeout: time.Minute}
	target := k.cfg.Tenants["t_acme"]
	job := k.stepJob(target, stepName(spec.Scope, spec.RunID, spec.StepKey), runName(spec.Scope, spec.RunID), "", "", spec)
	if job.Metadata.Name != stepName(spec.Scope, spec.RunID, spec.StepKey) {
		t.Fatalf("job name = %q", job.Metadata.Name)
	}
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != "kodama-execution" || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatalf("service account/token = %q/%v", pod.ServiceAccountName, pod.AutomountServiceAccountToken)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Fatal("pod is not non-root")
	}
	c := pod.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || *c.SecurityContext.Privileged ||
		c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation ||
		c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) != 1 {
		t.Fatal("step container is not the restricted baseline")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 60 {
		t.Fatalf("deadline = %v", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.Template.Spec.Containers[0].Command[0] != "/bin/sh" || job.Spec.Template.Spec.Containers[0].WorkingDir != "/workspace" {
		t.Fatal("the step does not run through a shell in /workspace")
	}
}

func TestKubeNameStaysAValidObjectName(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_one"}
	if got := runName(scope, "run_1"); got != "kodama-run-t_acme-run_1" {
		t.Fatalf("runName = %q", got)
	}
	if got := stepName(scope, "run_1", "a/very long step name with spaces"); strings.ContainsAny(got, "/ ") || len(got) > 63 {
		t.Fatalf("stepName = %q", got)
	}
}

func TestKubeConfigRefusesMissingInstallationInput(t *testing.T) {
	if _, err := NewKube(KubeConfig{}); err == nil {
		t.Fatal("an empty Kubernetes configuration was accepted")
	}
}
