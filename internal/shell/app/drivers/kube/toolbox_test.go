package kube

import (
	"testing"

	runtimekube "github.com/optimiware/kodema/internal/shell/runtime/kube"
)

func TestToolboxInitHasOnlyItsCopyDestination(t *testing.T) {
	job := w31Job()
	job.Spec.ToolboxImage = job.Spec.Image
	job.Spec.Env = []runtimekube.EnvVar{{Name: "KODEMA_EXECUTION", Value: "exec-a"}}
	job.Spec.Volumes = []runtimekube.Volume{
		{Name: "credential", MountPath: "/kodema/secrets", SecretName: "credential", ReadOnly: true},
		{Name: "workspace", MountPath: "/workspace", ClaimName: "workspace"},
		{Name: "toolbox", MountPath: "/kodema/bin", EmptyDir: true, ReadOnly: true},
	}
	manifest, err := buildJobManifest(job)
	if err != nil {
		t.Fatal(err)
	}
	pod := manifest.Spec.Template.Spec
	if len(pod.InitContainers) != 1 || pod.AutomountServiceAccountToken {
		t.Fatalf("init containers=%d service-account token=%v", len(pod.InitContainers), pod.AutomountServiceAccountToken)
	}
	copy := pod.InitContainers[0]
	want := volumeMountBody{Name: "toolbox", MountPath: "/kodema/bin"}
	if len(copy.Env) != 0 || len(copy.VolumeMounts) != 1 || copy.VolumeMounts[0] != want {
		t.Fatalf("installer received workload context: env=%v mounts=%v", copy.Env, copy.VolumeMounts)
	}
	if copy.SecurityContext == nil || !copy.SecurityContext.RunAsNonRoot || copy.SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("toolbox copy requires no root or privilege escalation")
	}
}
