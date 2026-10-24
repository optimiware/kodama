package delivery_test

import (
	"encoding/json"
	"strings"
	"testing"

	"example.invalid/inventory/internal/delivery"
)

type obj = map[string]any

func renderPlan(t *testing.T) []obj {
	t.Helper()
	r := newRig(t)
	pl := delivery.Plan{
		Namespace: ns, ImageRepo: repo, ImageDigest: digest, Config: r.snapshot.Config, ConfigDigest: "sha256:" + strings.Repeat("d", 64),
		Annotations: map[string]string{delivery.AnnotationDeploymentID: "dep_1"},
		Secrets: map[string]string{"DATABASE_RUNTIME_URL": "postgres://runtime", "DATABASE_MIGRATION_URL": "postgres://migration",
			"CURSOR_KEY": "cursor-secret-value", "ERP_TOKEN": "erp-secret-value"},
	}
	raw, err := pl.Render()
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []obj `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func podSpec(o obj) obj {
	spec := o["spec"].(obj)
	if o["kind"] == "Job" {
		return spec["template"].(obj)["spec"].(obj)
	}
	return spec["template"].(obj)["spec"].(obj)
}

func mountedSecretFiles(t *testing.T, pod obj) (secret string, files []string, mode float64) {
	t.Helper()
	for _, v := range pod["volumes"].([]any) {
		vol := v.(obj)
		if sec, ok := vol["secret"].(obj); ok {
			secret, mode = sec["secretName"].(string), sec["defaultMode"].(float64)
			items, ok := sec["items"].([]any)
			if !ok || len(items) == 0 {
				t.Fatalf("a credential volume must project explicit items, never a whole Secret: %v", sec)
			}
			for _, it := range items {
				files = append(files, it.(obj)["path"].(string))
			}
		}
	}
	return
}

// TestManifestCredentialIsolation: each pod receives only its documented
// credential files, readable by the non-root process but not world-readable.
func TestManifestCredentialIsolation(t *testing.T) {
	items := renderPlan(t)
	want := map[string]struct {
		secret string
		files  []string
	}{
		"inventory-api":    {"inventory-runtime", []string{"database-url", "cursor-key"}},
		"inventory-worker": {"inventory-runtime", []string{"database-url", "erp-token"}},
	}
	seen := 0
	for _, o := range items {
		kind, name := o["kind"].(string), o["metadata"].(obj)["name"].(string)
		if kind != "Deployment" && kind != "Job" {
			continue
		}
		seen++
		pod := podSpec(o)
		secret, files, mode := mountedSecretFiles(t, pod)
		if kind == "Job" {
			if secret != "inventory-migrate" || strings.Join(files, ",") != "database-url" {
				t.Errorf("the migrator mounts only its migration URL: %s %v", secret, files)
			}
		} else if w := want[name]; secret != w.secret || strings.Join(files, ",") != strings.Join(w.files, ",") {
			t.Errorf("%s mounts %s %v, want %s %v", name, secret, files, w.secret, w.files)
		}
		if name == "inventory-api" {
			for _, f := range files {
				if f == "erp-token" {
					t.Error("the API must never mount the ERP credential")
				}
			}
		}
		if int(mode)&0o007 != 0 || int(mode)&0o200 != 0 {
			t.Errorf("%s: credential files must not be world-readable or writable: %#o", name, int(mode))
		}
		sc := pod["securityContext"].(obj)
		if sc["runAsNonRoot"] != true || sc["runAsUser"] != float64(65532) || sc["fsGroup"] != float64(65532) {
			t.Errorf("%s: the non-root user needs group access to the credential files through fsGroup: %v", name, sc)
		}
		c := pod["containers"].([]any)[0].(obj)
		csc := c["securityContext"].(obj)
		if csc["allowPrivilegeEscalation"] != false || csc["readOnlyRootFilesystem"] != true || csc["privileged"] != false {
			t.Errorf("%s: restricted container settings: %v", name, csc)
		}
		if img := c["image"].(string); !strings.Contains(img, "@sha256:") {
			t.Errorf("%s: the image is pulled by digest only: %s", name, img)
		}
		if pod["automountServiceAccountToken"] != false {
			t.Errorf("%s: no service account token", name)
		}
	}
	if seen != 3 {
		t.Fatalf("expected API, worker and migration Job, saw %d", seen)
	}
}

func TestManifestNamespaceConfinementAndReceiptAnnotations(t *testing.T) {
	for _, o := range renderPlan(t) {
		md := o["metadata"].(obj)
		if md["namespace"] != ns {
			t.Errorf("%v escapes the namespace", md["name"])
		}
		if md["annotations"].(obj)[delivery.AnnotationDeploymentID] != "dep_1" {
			t.Errorf("%v lacks the receipt annotation", md["name"])
		}
	}
}

func TestSecretFilesMatchRenderedProjection(t *testing.T) {
	if got := strings.Join(delivery.SecretFiles("api"), ","); got != "database-url,cursor-key" {
		t.Fatal(got)
	}
	if len(delivery.SecretFiles("unknown")) != 0 {
		t.Fatal("an unknown component mounts nothing")
	}
}
