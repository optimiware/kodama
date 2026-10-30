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

func bodyOf(t *testing.T, r *rec, i int) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var d map[string]any
	if err := json.Unmarshal(r.body[i], &d); err != nil {
		t.Fatalf("request %d body: %v", i, err)
	}
	return d
}

func lostMutation(w http.ResponseWriter, req *http.Request) bool {
	if req.Method == http.MethodGet {
		return false
	}
	c, _, _ := w.(http.Hijacker).Hijack()
	_ = c.Close()
	return true
}

// Test_gitworker_namespace_and_account_operations: the namespace and service
// account operations of the tenant Git hosting use the right paths and kinds,
// send an explicit token-less service account, carry UID and resourceVersion
// preconditions on every replace and delete, and report a lost mutation as an
// unknown outcome.
func Test_gitworker_namespace_and_account_operations(t *testing.T) {
	ctx := context.Background()

	t.Run("namespace create, replace and delete", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) {
			switch req.Method {
			case http.MethodPost, http.MethodPut:
				reply(w, 201, map[string]any{"metadata": map[string]any{"name": "kodama-git-x", "uid": "u-ns", "resourceVersion": "7"}, "status": map[string]any{"phase": "Active"}})
			default:
				reply(w, 200, map[string]any{})
			}
		})
		n := kubernetes.Namespace{Metadata: kubernetes.ObjectMeta{Name: "kodama-git-x", Namespace: "ignored", Labels: map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}}}
		got, err := cl.CreateNamespace(ctx, n)
		if err != nil || got.Metadata.UID != "u-ns" || got.Status.Phase != "Active" {
			t.Fatalf("create %+v %v", got, err)
		}
		if _, err := cl.ReplaceNamespace(ctx, kubernetes.Namespace{Metadata: kubernetes.ObjectMeta{Name: "kodama-git-x"}}); !errors.Is(err, kubernetes.ErrInvalid) {
			t.Fatalf("a replace without preconditions: %v", err)
		}
		n.Metadata.UID, n.Metadata.ResourceVersion = "u-ns", "7"
		if _, err := cl.ReplaceNamespace(ctx, n); err != nil {
			t.Fatal(err)
		}
		if err := cl.DeleteNamespace(ctx, "kodama-git-x", kubernetes.DeleteOptions{}); !errors.Is(err, kubernetes.ErrInvalid) {
			t.Fatalf("a delete without a UID precondition: %v", err)
		}
		if err := cl.DeleteNamespace(ctx, "kodama-git-x", kubernetes.DeleteOptions{UID: "u-ns"}); err != nil {
			t.Fatal(err)
		}
		r.mu.Lock()
		paths := []string{r.reqs[0].Method + " " + r.reqs[0].URL.Path, r.reqs[1].Method + " " + r.reqs[1].URL.Path, r.reqs[2].Method + " " + r.reqs[2].URL.Path}
		r.mu.Unlock()
		if strings.Join(paths, ";") != "POST /api/v1/namespaces;PUT /api/v1/namespaces/kodama-git-x;DELETE /api/v1/namespaces/kodama-git-x" {
			t.Fatalf("paths %v", paths)
		}
		create := bodyOf(t, r, 0)
		if create["kind"] != "Namespace" || create["apiVersion"] != "v1" || create["metadata"].(map[string]any)["namespace"] != nil {
			t.Fatalf("create body %v", create)
		}
		put := bodyOf(t, r, 1)
		if put["metadata"].(map[string]any)["uid"] != "u-ns" || put["metadata"].(map[string]any)["resourceVersion"] != "7" {
			t.Fatalf("replace body %v", put)
		}
		del := bodyOf(t, r, 2)
		if del["preconditions"].(map[string]any)["uid"] != "u-ns" || del["propagationPolicy"] != "Background" {
			t.Fatalf("delete body %v", del)
		}
	})

	t.Run("service account", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) {
			reply(w, 201, map[string]any{"metadata": map[string]any{"name": "git-worker", "uid": "u-sa"}, "automountServiceAccountToken": false})
		})
		no := false
		got, err := cl.CreateServiceAccount(ctx, ns, kubernetes.ServiceAccount{Metadata: kubernetes.ObjectMeta{Name: "git-worker"}, AutomountServiceAccountToken: &no})
		if err != nil || got.Metadata.UID != "u-sa" || got.AutomountServiceAccountToken == nil || *got.AutomountServiceAccountToken {
			t.Fatalf("%+v %v", got, err)
		}
		b := bodyOf(t, r, 0)
		if b["kind"] != "ServiceAccount" || b["apiVersion"] != "v1" || b["automountServiceAccountToken"] != false {
			t.Fatalf("the token-less account must be explicit: %v", b)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.reqs[0].URL.Path != "/api/v1/namespaces/"+ns+"/serviceaccounts" {
			t.Fatalf("path %s", r.reqs[0].URL.Path)
		}
	})

	t.Run("lost mutations are unknown and absent objects not found", func(t *testing.T) {
		cl, _ := client(t, func(w http.ResponseWriter, req *http.Request) {
			if lostMutation(w, req) {
				return
			}
			status(w, 404, "NotFound", "")
		})
		if _, err := cl.CreateNamespace(ctx, kubernetes.Namespace{Metadata: kubernetes.ObjectMeta{Name: "a"}}); !errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("create namespace: %v", err)
		}
		if err := cl.DeleteNamespace(ctx, "a", kubernetes.DeleteOptions{UID: "u"}); !errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("delete namespace: %v", err)
		}
		if _, err := cl.CreateServiceAccount(ctx, ns, kubernetes.ServiceAccount{Metadata: kubernetes.ObjectMeta{Name: "a"}}); !errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("create serviceaccount: %v", err)
		}
		if _, err := cl.GetNamespace(ctx, "gone"); !errors.Is(err, kubernetes.ErrNotFound) {
			t.Fatalf("absent namespace: %v", err)
		}
	})

	t.Run("invalid names are refused before the request", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) { reply(w, 200, map[string]any{}) })
		if _, err := cl.CreateNamespace(ctx, kubernetes.Namespace{Metadata: kubernetes.ObjectMeta{Name: "Bad_NS"}}); !errors.Is(err, kubernetes.ErrInvalid) {
			t.Fatal(err)
		}
		if _, err := cl.CreateServiceAccount(ctx, "Bad_NS", kubernetes.ServiceAccount{Metadata: kubernetes.ObjectMeta{Name: "a"}}); !errors.Is(err, kubernetes.ErrInvalid) {
			t.Fatal(err)
		}
		if err := cl.DeleteNamespace(ctx, "../x", kubernetes.DeleteOptions{UID: "u"}); !errors.Is(err, kubernetes.ErrInvalid) {
			t.Fatal(err)
		}
		if r.n() != 0 {
			t.Fatalf("%d requests were sent", r.n())
		}
	})
}

// Test_gitworker_deployment_operations: a Deployment is read with its server
// document and generation, created, and replaced only under UID and
// resourceVersion preconditions; a conflicting or lost replace is classified; a
// ReplicaSet listing keeps its owner references; a NetworkPolicy replace carries
// the same preconditions.
func Test_gitworker_deployment_operations(t *testing.T) {
	ctx := context.Background()
	doc := func(uid string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"name": "git-worker", "uid": uid, "resourceVersion": "9", "generation": 4},
			"spec": map[string]any{
				"replicas": 1, "strategy": map[string]any{"type": "Recreate"}, "unmodeled": true,
				"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
					"name": "git-worker", "image": "i", "livenessProbe": map[string]any{"httpGet": map[string]any{"path": "/healthz", "port": 8443, "scheme": "HTTPS"}},
				}}}},
			},
			"status": map[string]any{"observedGeneration": 4, "replicas": 1, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1},
		}
	}

	t.Run("get keeps the document, generation and liveness probe", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) { reply(w, 200, doc("u-d")) })
		d, err := cl.GetDeployment(ctx, ns, "git-worker")
		if err != nil || d.Metadata.UID != "u-d" || d.Generation != 4 || d.Status.ObservedGeneration != 4 || d.Status.ReadyReplicas != 1 || !strings.Contains(string(d.Raw), "unmodeled") {
			t.Fatalf("%+v %v", d, err)
		}
		c := d.Spec.Template.Spec.Containers[0]
		if c.Name != "git-worker" || c.LivenessProbe == nil || c.LivenessProbe.HTTPGet.Scheme != "HTTPS" {
			t.Fatalf("container %+v", c)
		}
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != "Recreate" {
			t.Fatalf("spec %+v", d.Spec)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.reqs[0].URL.Path != "/apis/apps/v1/namespaces/"+ns+"/deployments/git-worker" {
			t.Fatalf("path %s", r.reqs[0].URL.Path)
		}
	})

	t.Run("create sets the kind and the namespace", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) { reply(w, 201, doc("u-d")) })
		replicas := int32(1)
		d := kubernetes.Deployment{Metadata: kubernetes.ObjectMeta{Name: "git-worker"}, Spec: kubernetes.DeploymentSpec{Replicas: &replicas, Strategy: kubernetes.DeploymentStrategy{Type: "Recreate"}}}
		d.Spec.Template.Spec.Containers = []kubernetes.WorkerContainer{{Container: kubernetes.Container{Name: "git-worker", Image: "i"}, LivenessProbe: &kubernetes.Probe{HTTPGet: &kubernetes.HTTPGetAction{Path: "/healthz", Port: 8443, Scheme: "HTTPS"}}}}
		got, err := cl.CreateDeployment(ctx, ns, d)
		if err != nil || got.Metadata.UID != "u-d" || got.Generation != 4 {
			t.Fatalf("%+v %v", got, err)
		}
		b := bodyOf(t, r, 0)
		if b["kind"] != "Deployment" || b["apiVersion"] != "apps/v1" || b["metadata"].(map[string]any)["namespace"] != ns {
			t.Fatalf("body %v", b)
		}
		c := b["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		if c["name"] != "git-worker" || c["livenessProbe"] == nil {
			t.Fatalf("the container is not flattened with its probe: %v", c)
		}
	})

	t.Run("replace requires both preconditions and sends them", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) { reply(w, 200, doc("u-d")) })
		for name, d := range map[string]kubernetes.Deployment{
			"no uid":             {Metadata: kubernetes.ObjectMeta{Name: "git-worker", ResourceVersion: "9"}},
			"no resourceVersion": {Metadata: kubernetes.ObjectMeta{Name: "git-worker", UID: "u-d"}},
		} {
			if _, err := cl.ReplaceDeployment(ctx, ns, d); !errors.Is(err, kubernetes.ErrInvalid) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if r.n() != 0 {
			t.Fatal("a request was sent without its preconditions")
		}
		d := kubernetes.Deployment{Metadata: kubernetes.ObjectMeta{Name: "git-worker", UID: "u-d", ResourceVersion: "9"}}
		if _, err := cl.ReplaceDeployment(ctx, ns, d); err != nil {
			t.Fatal(err)
		}
		r.mu.Lock()
		method, path := r.reqs[0].Method, r.reqs[0].URL.Path
		r.mu.Unlock()
		if method != http.MethodPut || path != "/apis/apps/v1/namespaces/"+ns+"/deployments/git-worker" {
			t.Fatalf("%s %s", method, path)
		}
		md := bodyOf(t, r, 0)["metadata"].(map[string]any)
		if md["uid"] != "u-d" || md["resourceVersion"] != "9" {
			t.Fatalf("metadata %v", md)
		}
	})

	t.Run("a replaced object is a conflict, a lost replace an unknown outcome", func(t *testing.T) {
		cl, _ := client(t, func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/apis/apps/v1/namespaces/"+ns+"/deployments/lost" {
				lostMutation(w, req)
				return
			}
			status(w, 409, "Conflict", "the object has been modified")
		})
		d := kubernetes.Deployment{Metadata: kubernetes.ObjectMeta{Name: "git-worker", UID: "u-d", ResourceVersion: "9"}}
		if _, err := cl.ReplaceDeployment(ctx, ns, d); !errors.Is(err, kubernetes.ErrConflict) || errors.Is(err, kubernetes.ErrAlreadyExists) {
			t.Fatalf("replace: %v", err)
		}
		d.Metadata.Name = "lost"
		if _, err := cl.ReplaceDeployment(ctx, ns, d); !errors.Is(err, kubernetes.ErrUnknownOutcome) {
			t.Fatalf("lost replace: %v", err)
		}
		if _, err := cl.CreateDeployment(ctx, ns, kubernetes.Deployment{Metadata: kubernetes.ObjectMeta{Name: "git-worker"}}); !errors.Is(err, kubernetes.ErrConflict) {
			t.Fatalf("create on a conflicting name: %v", err)
		}
	})

	t.Run("an already existing name is its own class", func(t *testing.T) {
		cl, _ := client(t, func(w http.ResponseWriter, req *http.Request) { status(w, 409, "AlreadyExists", "exists") })
		if _, err := cl.CreateDeployment(ctx, ns, kubernetes.Deployment{Metadata: kubernetes.ObjectMeta{Name: "git-worker"}}); !errors.Is(err, kubernetes.ErrAlreadyExists) {
			t.Fatal(err)
		}
	})

	t.Run("replicasets keep their owners and are listed in pages", func(t *testing.T) {
		pages := 0
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) {
			pages++
			cont := ""
			if pages == 1 {
				cont = "more"
			}
			reply(w, 200, map[string]any{"metadata": map[string]any{"continue": cont}, "items": []any{map[string]any{
				"metadata": map[string]any{"name": "rs-" + string(rune('a'+pages)), "uid": "u-rs", "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "git-worker", "uid": "u-d", "controller": true}}},
				"spec":     map[string]any{"replicas": 1}, "status": map[string]any{"replicas": 1, "readyReplicas": 1},
			}}})
		})
		got, err := cl.ListReplicaSets(ctx, ns, "app=x")
		if err != nil || len(got) != 2 || !got[0].Metadata.OwnerReferences[0].Controlled() || got[0].Metadata.OwnerReferences[0].UID != "u-d" || got[0].Status.ReadyReplicas != 1 {
			t.Fatalf("%+v %v", got, err)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.reqs[0].URL.Path != "/apis/apps/v1/namespaces/"+ns+"/replicasets" || r.reqs[0].URL.Query().Get("labelSelector") != "app=x" {
			t.Fatalf("request %s?%s", r.reqs[0].URL.Path, r.reqs[0].URL.RawQuery)
		}
	})

	t.Run("network policy replace", func(t *testing.T) {
		cl, r := client(t, func(w http.ResponseWriter, req *http.Request) {
			reply(w, 200, map[string]any{"metadata": map[string]any{"name": "p", "uid": "u-p", "resourceVersion": "3"}, "spec": map[string]any{"podSelector": map[string]any{}}})
		})
		if _, err := cl.ReplaceNetworkPolicy(ctx, ns, kubernetes.NetworkPolicy{Metadata: kubernetes.ObjectMeta{Name: "p"}}); !errors.Is(err, kubernetes.ErrInvalid) {
			t.Fatalf("no preconditions: %v", err)
		}
		got, err := cl.ReplaceNetworkPolicy(ctx, ns, kubernetes.NetworkPolicy{Metadata: kubernetes.ObjectMeta{Name: "p", UID: "u-p", ResourceVersion: "3"}})
		if err != nil || len(got.Raw) == 0 {
			t.Fatalf("%+v %v", got, err)
		}
		b := bodyOf(t, r, 0)
		if b["kind"] != "NetworkPolicy" || b["apiVersion"] != "networking.k8s.io/v1" || b["metadata"].(map[string]any)["uid"] != "u-p" {
			t.Fatalf("body %v", b)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.reqs[0].Method != http.MethodPut || r.reqs[0].URL.Path != "/apis/networking.k8s.io/v1/namespaces/"+ns+"/networkpolicies/p" {
			t.Fatalf("%s %s", r.reqs[0].Method, r.reqs[0].URL.Path)
		}
	})

	t.Run("errors never echo bodies", func(t *testing.T) {
		cl, _ := client(t, func(w http.ResponseWriter, req *http.Request) {
			status(w, 422, "Invalid", "token: super-secret-value")
		})
		_, err := cl.CreateDeployment(ctx, ns, kubernetes.Deployment{Metadata: kubernetes.ObjectMeta{Name: "git-worker"}})
		if !errors.Is(err, kubernetes.ErrInvalid) || strings.Contains(err.Error(), "super-secret") {
			t.Fatalf("%v", err)
		}
	})
}
