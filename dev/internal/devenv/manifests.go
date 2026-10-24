package devenv

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var hardened = map[string]any{
	"allowPrivilegeEscalation": false,
	"capabilities":             map[string]any{"drop": []string{"ALL"}},
}

// Manifests renders the cluster stack's namespaces, fixture secret,
// selector-less Services, EndpointSlices and server Deployment.
func (e *Environment) Manifests(fixtures string, addresses map[string]string) ([]map[string]any, error) {
	data := map[string][]byte{}
	entries, err := os.ReadDir(fixtures)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(fixtures, entry.Name()))
		if err != nil {
			return nil, err
		}
		data[entry.Name()] = content
	}
	secrets := map[string]string{}
	for _, name := range ProbeSecrets {
		if content, ok := data[name]; ok {
			secrets[name] = base64.StdEncoding.EncodeToString(content)
		}
	}
	items := []map[string]any{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": DepsNamespace}},
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{
			"name": TestsNamespace, "labels": map[string]string{"pod-security.kubernetes.io/enforce": "restricted"},
		}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "fixtures", "namespace": TestsNamespace}, "data": secrets},
	}
	for _, name := range []string{"postgres", "nats", "localstack", "git", "oauthsonas", "registry"} {
		port := ServicePorts[name]
		items = append(items,
			map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": name, "namespace": DepsNamespace},
				"spec": map[string]any{"ports": []map[string]any{{"name": "tcp", "port": port, "targetPort": port}}}},
			map[string]any{"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
				"metadata":    map[string]any{"name": name, "namespace": DepsNamespace, "labels": map[string]string{"kubernetes.io/service-name": name}},
				"addressType": "IPv4", "ports": []map[string]any{{"name": "tcp", "protocol": "TCP", "port": port}},
				"endpoints": []map[string]any{{"addresses": []string{addresses[name]}, "conditions": map[string]bool{"ready": true}}}},
		)
	}
	items = append(items,
		map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "kodama", "namespace": DepsNamespace},
			"spec": map[string]any{
				"replicas": 1,
				"selector": map[string]any{"matchLabels": map[string]string{"app": "kodama"}},
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]string{"app": "kodama"}},
					"spec": map[string]any{
						"automountServiceAccountToken": false,
						"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532,
							"seccompProfile": map[string]string{"type": "RuntimeDefault"}},
						"containers": []map[string]any{{
							"name": "kodama", "image": e.State.Image, "ports": []map[string]any{{"containerPort": 8080}},
							"readinessProbe":  map[string]any{"httpGet": map[string]any{"path": "/readyz", "port": 8080}, "periodSeconds": 2},
							"resources":       map[string]any{"requests": map[string]any{"cpu": "50m", "memory": "64Mi"}, "limits": map[string]any{"memory": "256Mi"}},
							"securityContext": hardened,
						}},
					},
				},
			}},
		map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "kodama", "namespace": DepsNamespace},
			"spec": map[string]any{"selector": map[string]string{"app": "kodama"}, "ports": []map[string]any{{"port": 8080, "targetPort": 8080}}}},
	)
	for _, item := range items {
		metadata := item["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]string)
		if labels == nil {
			labels = map[string]string{}
			metadata["labels"] = labels
		}
		labels[ScopeLabel] = e.State.Owner
	}
	return items, nil
}

// Apply applies documents to the scope's cluster.
func (e *Environment) Apply(items []map[string]any) error {
	document, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	if err != nil {
		return err
	}
	_, err = e.KubeWith(document, "apply", "-f", "-")
	return err
}

// Job runs one probe Job and returns its logs. A failed Job is an error.
func (e *Environment) Job(name, image string, command []string, probe bool) (string, error) {
	container := map[string]any{
		"name": "check", "image": image, "imagePullPolicy": "Always", "command": command,
		"resources":       map[string]any{"requests": map[string]any{"cpu": "50m", "memory": "64Mi"}, "limits": map[string]any{"memory": "512Mi"}},
		"securityContext": hardened,
	}
	spec := map[string]any{
		"restartPolicy": "Never", "automountServiceAccountToken": false,
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532,
			"seccompProfile": map[string]string{"type": "RuntimeDefault"}},
		"containers": []map[string]any{container},
	}
	if probe {
		env := make([]map[string]string, 0)
		for key, value := range ProbeEnv(e.Scope, nil) {
			env = append(env, map[string]string{"name": key, "value": value})
		}
		container["env"] = env
		container["volumeMounts"] = []map[string]any{{"name": "secrets", "mountPath": "/secrets"}}
		spec["volumes"] = []map[string]any{{"name": "secrets", "secret": map[string]any{"secretName": "fixtures", "defaultMode": 0o444}}}
	}
	if err := e.Apply([]map[string]any{{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": TestsNamespace, "labels": map[string]string{ScopeLabel: e.State.Owner}},
		"spec":     map[string]any{"backoffLimit": 0, "activeDeadlineSeconds": 420, "template": map[string]any{"spec": spec}},
	}}); err != nil {
		return "", err
	}
	deadline := time.Now().Add(440 * time.Second)
	for time.Now().Before(deadline) {
		stdout, err := e.Kube("-n", TestsNamespace, "get", "job", name, "-o", "json")
		if err != nil {
			return "", err
		}
		var status struct {
			Status struct {
				Succeeded int `json:"succeeded"`
				Failed    int `json:"failed"`
			} `json:"status"`
		}
		_ = json.Unmarshal([]byte(stdout), &status)
		if status.Status.Succeeded > 0 || status.Status.Failed > 0 {
			logs, _ := e.Kube("-n", TestsNamespace, "logs", "job/"+name)
			fmt.Print(logs)
			if status.Status.Succeeded == 0 {
				return "", fmt.Errorf("probe Job failed: %s", name)
			}
			return logs, nil
		}
		sleep(2 * time.Second)
	}
	return "", fmt.Errorf("probe Job timed out: %s", name)
}

// KubeWith runs kubectl with stdin.
func (e *Environment) KubeWith(stdin []byte, args ...string) (string, error) {
	full := append([]string{e.Kubectl, "--kubeconfig", filepath.Join(e.Dir, "kubeconfig"), "--context", "kind-" + e.State.Cluster}, args...)
	result, err := runCmd(Cmd{Args: full, Stdin: bytes.NewReader(stdin), Capture: true})
	return result.Stdout + result.Stderr, err
}
