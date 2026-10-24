package realdelivery_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.invalid/inventory/test/support/erpfake"
)

const (
	tenantA = "11111111-1111-4111-8111-111111111111"
	tenantB = "22222222-2222-4222-8222-222222222222"
)

// environment is one application delivery target: a namespace with a
// namespace-scoped deployment identity, its own database and ERP credential, the
// controlled ERP peer and the shared OIDC provider.
type environment struct {
	kind       string // preview or stage
	ns         string
	db         database
	kubeconfig string // namespace-scoped; the only cluster credential the scripts receive
	cursorKey  string
	erpToken   string
	erp        *erpfake.Server
	erpURL     string
	workerID   string
	oidc       oidcProvider
}

func obj(kind, apiVersion, ns, name string, rest map[string]any) map[string]any {
	md := map[string]any{"name": name}
	if ns != "" {
		md["namespace"] = ns
	}
	o := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": md}
	for k, v := range rest {
		o[k] = v
	}
	return o
}

// newEnvironment provisions the namespace, the scoped deployment identity, the
// database, the ERP peer and the OIDC wiring. Everything is removed on cleanup.
func (e *devEnv) newEnvironment(t *testing.T, kind string) *environment {
	t.Helper()
	ns := fmt.Sprintf("%s-%s-%s", e.prefix, kind, randHex(3))
	env := &environment{kind: kind, ns: ns, cursorKey: randHex(24), erpToken: "erp-" + randHex(12), workerID: "svc-worker-" + kind, oidc: e.oidc(t)}
	env.db = e.newDatabase(t, kind)
	t.Cleanup(func() { _, _ = e.kube("delete", "namespace", ns, "--ignore-not-found", "--wait=false") })

	// The controlled ERP peer runs on the developer machine; the namespace reaches it through a Service.
	env.erp = erpfake.New(t, map[string]string{tenantA: env.erpToken, tenantB: "unused-" + randHex(6)})
	port := 0
	if _, err := fmt.Sscanf(env.erp.URL()[strings.LastIndex(env.erp.URL(), ":")+1:], "%d", &port); err != nil {
		t.Fatalf("erp peer address %q: %v", env.erp.URL(), err)
	}
	env.erpURL = fmt.Sprintf("http://erp.%s.svc.cluster.local:%d", ns, port)

	verbs := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	e.apply(t,
		obj("Namespace", "v1", "", ns, map[string]any{"metadata": map[string]any{"name": ns, "labels": map[string]any{
			"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "latest", "dev.kodama.scope": e.scope}}}),
		obj("ServiceAccount", "v1", ns, "deployer", map[string]any{"automountServiceAccountToken": false}),
		obj("Role", "rbac.authorization.k8s.io/v1", ns, "deployer", map[string]any{"rules": []map[string]any{
			{"apiGroups": []string{""}, "resources": []string{"configmaps", "secrets", "services"}, "verbs": verbs},
			{"apiGroups": []string{"apps"}, "resources": []string{"deployments"}, "verbs": verbs},
			{"apiGroups": []string{"batch"}, "resources": []string{"jobs"}, "verbs": verbs},
			{"apiGroups": []string{""}, "resources": []string{"pods", "pods/log"}, "verbs": []string{"get", "list", "watch"}},
			{"apiGroups": []string{""}, "resources": []string{"pods/portforward"}, "verbs": []string{"create"}},
		}}),
		obj("RoleBinding", "rbac.authorization.k8s.io/v1", ns, "deployer", map[string]any{
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "deployer"},
			"subjects": []map[string]any{{"kind": "ServiceAccount", "name": "deployer", "namespace": ns}}}),
		serviceOnly(ns, "erp", port), serviceAndEndpoints(ns, "erp", port, e.hostIP))

	tok := strings.TrimSpace(e.mustKube(t, "create", "token", "deployer", "--namespace", ns, "--duration=3h"))
	var view struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CA     string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal([]byte(e.mustKube(t, "config", "view", "--raw", "--minify", "-o", "json")), &view); err != nil || len(view.Clusters) != 1 {
		t.Fatalf("the administrator kubeconfig has no single cluster: %v", err)
	}
	cl := view.Clusters[0].Cluster
	kc, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Config", "current-context": "deployer",
		"clusters": []map[string]any{{"name": "dev", "cluster": map[string]any{"server": cl.Server, "certificate-authority-data": cl.CA}}},
		"users":    []map[string]any{{"name": "deployer", "user": map[string]any{"token": tok}}},
		"contexts": []map[string]any{{"name": "deployer", "context": map[string]any{"cluster": "dev", "user": "deployer", "namespace": ns}}}})
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o755)
	env.kubeconfig = filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(env.kubeconfig, kc, 0o644); err != nil {
		t.Fatal(err)
	}
	return env
}

// seed provisions the application tenants and memberships with the application's
// own seed command, run as a Job from the deployed image after the migration Job
// has succeeded. It uses the migration credential Secret of the deployment.
func (e *devEnv) seed(env *environment, imageRef string, done <-chan struct{}) error {
	if err := e.waitFor(env, "job", "-l app.kubernetes.io/component=migrate", "--for=condition=complete", 5*time.Minute, done); err != nil {
		return fmt.Errorf("waiting for the migration Job: %w", err)
	}
	seeds := map[string]map[string]any{
		"seed-a": {"tenantId": tenantA, "name": "Tenant A", "members": []map[string]string{{"subject": "oauthsonas|reviewer", "role": "reviewer"}, {"subject": env.workerID, "role": "worker"}}},
		"seed-b": {"tenantId": tenantB, "name": "Tenant B", "members": []map[string]string{{"subject": "oauthsonas|outsider", "role": "reviewer"}}},
	}
	restricted := map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "runAsNonRoot": true, "capabilities": map[string]any{"drop": []string{"ALL"}}, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}
	for name, doc := range seeds {
		raw, _ := json.Marshal(doc)
		if err := e.applyErr(
			obj("ConfigMap", "v1", env.ns, name, map[string]any{"data": map[string]any{"seed.json": string(raw)}}),
			obj("Job", "batch/v1", env.ns, name, map[string]any{"spec": map[string]any{"backoffLimit": 3, "ttlSecondsAfterFinished": 3600, "template": map[string]any{"spec": map[string]any{
				"restartPolicy": "Never", "automountServiceAccountToken": false,
				"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
				"containers": []map[string]any{{"name": "seed", "image": imageRef, "args": []string{"seed", "--config", "/etc/inventory/migrate.json", "--file", "/etc/seed/seed.json"}, "securityContext": restricted,
					"volumeMounts": []map[string]any{{"name": "config", "mountPath": "/etc/inventory", "readOnly": true}, {"name": "secrets", "mountPath": "/run/secrets/inventory", "readOnly": true}, {"name": "seed", "mountPath": "/etc/seed", "readOnly": true}}}},
				"volumes": []map[string]any{
					{"name": "config", "configMap": map[string]any{"name": "inventory-config"}},
					{"name": "secrets", "secret": map[string]any{"secretName": "inventory-migrate", "defaultMode": 288}},
					{"name": "seed", "configMap": map[string]any{"name": name}}}}}}})); err != nil {
			return err
		}
	}
	for name := range seeds {
		if err := e.waitFor(env, "job/"+name, "", "--for=condition=complete", 3*time.Minute, done); err != nil {
			logs, _ := e.kube("logs", "--namespace", env.ns, "job/"+name)
			return fmt.Errorf("seeding %s: %w\n%s", name, err, logs)
		}
	}
	return nil
}

// waitFor waits with the administrator credential (test orchestration only) until
// the condition holds, the deadline passes or done is closed.
func (e *devEnv) waitFor(env *environment, resource, selector, cond string, d time.Duration, done <-chan struct{}) error {
	args := []string{"wait", "--namespace", env.ns, "--timeout", "30s", cond, resource}
	if selector != "" {
		args = append(args, strings.Fields(selector)...)
	}
	deadline := time.Now().Add(d)
	for {
		_, err := e.kube(args...)
		if err == nil {
			return nil
		}
		select {
		case <-done:
			return fmt.Errorf("stopped: %w", err)
		default:
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Second)
	}
}
