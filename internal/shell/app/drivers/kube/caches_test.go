package kube

import (
	"encoding/json"
	"strings"
	"testing"

	runtimekube "github.com/optimiware/kodema/internal/shell/runtime/kube"
)

func TestBuildJobManifestRendersCacheSubPathAndEmptyDir(t *testing.T) {
	job := w31Job()
	job.Spec.Volumes = []runtimekube.Volume{
		{Name: "workspace", MountPath: "/workspace", ClaimName: "ws-claim"},
		{Name: "cache-shared", MountPath: "/kodema/cache/go-mod", ClaimName: "shared-cache", SubPath: "0123456789abcdef0123456789abcdef"},
		{Name: "cache-local", MountPath: "/kodema/cache/npm", EmptyDir: true},
	}
	manifest, err := buildJobManifest(job)
	if err != nil {
		t.Fatalf("buildJobManifest: %v", err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, `"mountPath":"/kodema/cache/go-mod","subPath":"0123456789abcdef0123456789abcdef"`) {
		t.Fatalf("missing cache subPath in rendered manifest: %s", text)
	}
	if !strings.Contains(text, `"mountPath":"/kodema/cache/npm"`) {
		t.Fatalf("missing emptyDir cache mount in rendered manifest: %s", text)
	}
}

func TestBuildJobManifestRejectsUnsafeVolumeSubPath(t *testing.T) {
	badSubPaths := []string{
		"/etc/passwd",
		"../escape",
		"caches/../escape",
		".",
		"caches/.",
		"caches//double",
		"caches/trailing/",
		`caches\backslash`,
		" leading-space",
		"   ",
	}
	for _, subPath := range badSubPaths {
		t.Run(subPath, func(t *testing.T) {
			job := w31Job()
			job.Spec.Volumes = []runtimekube.Volume{
				{Name: "cache-bad", MountPath: "/kodema/cache/go-mod", ClaimName: "shared-cache", SubPath: subPath},
			}
			if _, err := buildJobManifest(job); err == nil {
				t.Fatalf("buildJobManifest accepted unsafe SubPath %q; want invalid_input", subPath)
			}
		})
	}
}
