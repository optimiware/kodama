package kubernetes_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Test_run_object_operations: the run-resource operations use the right paths and
// kinds, keep the server's raw document, carry UID preconditions on every delete
// and report a lost mutation as an unknown outcome.
func Test_run_object_operations(t *testing.T) {
	ctx := context.Background()
	uid := kubernetes.DeleteOptions{UID: "u-1"}

	t.Run("claims", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) {
			switch req.Method {
			case http.MethodPost:
				reply(w, 201, map[string]any{"metadata": map[string]any{"name": "c", "uid": "u-1"}, "spec": map[string]any{"volumeName": "pv-1", "storageClassName": "fast"}, "status": map[string]any{"phase": "Bound"}})
			case http.MethodGet:
				reply(w, 200, map[string]any{"metadata": map[string]any{"name": "c", "uid": "u-1"}, "spec": map[string]any{"volumeName": "pv-1", "unmodeled": true}})
			default:
				reply(w, 200, map[string]any{})
			}
		})
		class := "fast"
		created, err := cl.CreatePVC(ctx, ns, kubernetes.PersistentVolumeClaim{Metadata: kubernetes.ObjectMeta{Name: "c"}, Spec: kubernetes.PVCSpec{
			AccessModes: []string{"ReadWriteOncePod"}, StorageClassName: &class, Resources: kubernetes.VolumeResources{Requests: map[string]string{"storage": "1Gi"}}}})
		if err != nil || created.Status.Phase != "Bound" || created.Metadata.UID != "u-1" || len(created.Raw) == 0 {
			t.Fatalf("create %+v %v", created, err)
		}
		got, err := cl.GetPVC(ctx, ns, "c")
		if err != nil || !strings.Contains(string(got.Raw), "unmodeled") {
			t.Fatalf("get keeps the raw document: %+v %v", got, err)
		}
		if err := cl.DeletePVC(ctx, ns, "c", uid); err != nil {
			t.Fatal(err)
		}
		if err := cl.DeletePVC(ctx, ns, "c", kubernetes.DeleteOptions{}); err == nil || !errors.Is(err, kubernetes.ErrInvalid) {
			t.Fatalf("a delete without a UID precondition: %v", err)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.reqs[0].URL.Path != "/api/v1/namespaces/"+ns+"/persistentvolumeclaims" || r.reqs[1].URL.Path != "/api/v1/namespaces/"+ns+"/persistentvolumeclaims/c" {
			t.Fatalf("paths %s %s", r.reqs[0].URL.Path, r.reqs[1].URL.Path)
		}
		var create map[string]any
		_ = json.Unmarshal(r.body[0], &create)
		if create["kind"] != "PersistentVolumeClaim" || create["apiVersion"] != "v1" {
			t.Fatalf("create body %v", create)
		}
		var del map[string]any
		_ = json.Unmarshal(r.body[2], &del)
		if del["preconditions"].(map[string]any)["uid"] != "u-1" {
			t.Fatalf("delete body %v", del)
		}
	})

	t.Run("volumes and attachments are read only", func(t *testing.T) {
		pages := 0
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) {
			switch {
			case strings.HasSuffix(req.URL.Path, "/persistentvolumes/pv-1"):
				reply(w, 200, map[string]any{"metadata": map[string]any{"name": "pv-1"}, "spec": map[string]any{"claimRef": map[string]any{"namespace": ns, "name": "c", "uid": "u-1"}, "persistentVolumeReclaimPolicy": "Delete"}})
			case strings.HasSuffix(req.URL.Path, "/storageclasses/fast"):
				reply(w, 200, map[string]any{"metadata": map[string]any{"name": "fast"}, "provisioner": "csi.x", "reclaimPolicy": "Delete", "volumeBindingMode": "WaitForFirstConsumer"})
			case strings.HasSuffix(req.URL.Path, "/volumeattachments"):
				pages++
				cont := ""
				if pages == 1 {
					cont = "next"
				}
				reply(w, 200, map[string]any{"metadata": map[string]any{"continue": cont}, "items": []any{map[string]any{"metadata": map[string]any{"name": "csi-" + string(rune('a'+pages))}, "spec": map[string]any{"attacher": "x", "source": map[string]any{"persistentVolumeName": "pv-1"}}}}})
			default:
				status(w, 404, "NotFound", "")
			}
		})
		pv, err := cl.GetPV(ctx, "pv-1")
		if err != nil || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != "u-1" || pv.Spec.PersistentVolumeReclaimPolicy != "Delete" {
			t.Fatalf("pv %+v %v", pv, err)
		}
		if _, err := cl.GetPV(ctx, "gone"); !errors.Is(err, kubernetes.ErrNotFound) {
			t.Fatalf("absent volume: %v", err)
		}
		sc, err := cl.GetStorageClass(ctx, "fast")
		if err != nil || sc.Provisioner != "csi.x" || sc.ReclaimPolicy != "Delete" || sc.VolumeBindingMode != "WaitForFirstConsumer" {
			t.Fatalf("storage class %+v %v", sc, err)
		}
		if _, err := cl.GetStorageClass(ctx, "gone"); !errors.Is(err, kubernetes.ErrNotFound) {
			t.Fatalf("absent class: %v", err)
		}
		vas, err := cl.ListVolumeAttachments(ctx)
		if err != nil || len(vas) != 2 || vas[0].Spec.Source.PersistentVolumeName == nil || *vas[0].Spec.Source.PersistentVolumeName != "pv-1" {
			t.Fatalf("attachments %+v %v", vas, err)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, req := range r.reqs {
			if req.Method != http.MethodGet {
				t.Fatalf("a read-only operation sent %s", req.Method)
			}
		}
	})

	t.Run("services, pods and policies", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/networkpolicies") {
				reply(w, 200, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "p", "uid": "u-p"}, "spec": map[string]any{"podSelector": map[string]any{}}}}})
				return
			}
			reply(w, 201, map[string]any{"metadata": map[string]any{"name": "x", "uid": "u-1"}, "spec": map[string]any{}})
		})
		if s, err := cl.CreateService(ctx, ns, kubernetes.Service{Metadata: kubernetes.ObjectMeta{Name: "s-1"}, Spec: kubernetes.ServiceSpec{Type: "ClusterIP"}}); err != nil || s.Metadata.UID != "u-1" || len(s.Raw) == 0 {
			t.Fatalf("service %+v %v", s, err)
		}
		if p, err := cl.CreatePod(ctx, ns, kubernetes.Pod{Metadata: kubernetes.ObjectMeta{Name: "p-1"}, Spec: kubernetes.PodSpec{Containers: []kubernetes.Container{{Name: "c", Image: "i"}}}}); err != nil || p.Metadata.UID != "u-1" || len(p.Raw) == 0 {
			t.Fatalf("pod %+v %v", p, err)
		}
		if p, err := cl.CreateNetworkPolicy(ctx, ns, kubernetes.NetworkPolicy{Metadata: kubernetes.ObjectMeta{Name: "np"}}); err != nil || p.Metadata.UID != "u-1" {
			t.Fatalf("policy %+v %v", p, err)
		}
		pols, err := cl.SelectNetworkPolicies(ctx, ns, "a=b")
		if err != nil || len(pols) != 1 || len(pols[0].Raw) == 0 {
			t.Fatalf("select %+v %v", pols, err)
		}
		for name, del := range map[string]func() error{
			"service": func() error { return cl.DeleteService(ctx, ns, "s-1", kubernetes.DeleteOptions{}) },
			"policy":  func() error { return cl.DeleteNetworkPolicy(ctx, ns, "np", kubernetes.DeleteOptions{}) },
		} {
			if err := del(); !errors.Is(err, kubernetes.ErrInvalid) {
				t.Fatalf("%s delete without a precondition: %v", name, err)
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		var kinds []string
		for i, req := range r.reqs {
			if req.Method == http.MethodPost {
				var d map[string]any
				_ = json.Unmarshal(r.body[i], &d)
				kinds = append(kinds, d["kind"].(string))
			}
		}
		if strings.Join(kinds, ",") != "Service,Pod,NetworkPolicy" {
			t.Fatalf("kinds %v", kinds)
		}
		if got := r.reqs[3].URL.Query().Get("labelSelector"); got != "a=b" {
			t.Fatalf("selector %q", got)
		}
	})

	t.Run("a lost create is an unknown outcome and a missing object a not-found", func(t *testing.T) {
		cl, _ := client(t, func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodPost {
				hj := w.(http.Hijacker)
				c, _, _ := hj.Hijack()
				_ = c.Close()
				return
			}
			status(w, 404, "NotFound", "")
		})
		if _, err := cl.CreatePVC(ctx, ns, kubernetes.PersistentVolumeClaim{Metadata: kubernetes.ObjectMeta{Name: "c"}}); !errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("lost create: %v", err)
		}
		if _, err := cl.GetService(ctx, ns, "s"); !errors.Is(err, kubernetes.ErrNotFound) {
			t.Fatalf("absent service: %v", err)
		}
		if _, err := cl.GetNetworkPolicy(ctx, ns, "p"); !errors.Is(err, kubernetes.ErrNotFound) {
			t.Fatalf("absent policy: %v", err)
		}
		if _, err := cl.GetPVC(ctx, "Bad_NS", "c"); !errors.Is(err, kubernetes.ErrInvalid) {
			t.Fatalf("invalid names must be refused before the request: %v", err)
		}
	})
}
