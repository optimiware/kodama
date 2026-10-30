package kubernetes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/images/plugins/deploy/driver"
)

type objectState struct {
	APIVersion          string
	Kind                string
	Name                string
	Namespace           string
	UID                 string
	RV                  string
	Annotations         map[string]string
	Generation          int64
	Replicas            *int32
	ObservedGeneration  *int64
	AvailableReplicas   *int32
	ReadyReplicas       *int32
	UnavailableReplicas *int32
	NumberReady         *int32
	DesiredScheduled    *int32
	Succeeded           *int64
	Completions         *int64
	Phase               string
	Conditions          []struct {
		Type   string `json:"type"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	Raw map[string]any
}

type objectJSON struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		UID         string            `json:"uid"`
		RV          string            `json:"resourceVersion"`
		Generation  int64             `json:"generation"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration  *int64 `json:"observedGeneration"`
		Replicas            *int32 `json:"replicas"`
		AvailableReplicas   *int32 `json:"availableReplicas"`
		ReadyReplicas       *int32 `json:"readyReplicas"`
		UnavailableReplicas *int32 `json:"unavailableReplicas"`
		NumberReady         *int32 `json:"numberReady"`
		DesiredNumber       *int32 `json:"desiredNumberScheduled"`
		Succeeded           *int64 `json:"succeeded"`
		Completions         *int64 `json:"completions"`
		Phase               string `json:"phase"`
		Conditions          []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"conditions"`
	} `json:"status"`
}

func (d *Driver) getObject(ctx context.Context, cfg Config, resource driver.Resource) (objectState, bool, error) {
	if resource.Namespace != "" && resource.Namespace != cfg.Namespace {
		return objectState{}, false, fmt.Errorf("kubernetes driver: resource is outside the target namespace")
	}
	out, err := d.run(ctx, cfg, "get", strings.ToLower(resource.Kind)+"/"+resource.Name, "-n", cfg.Namespace, "-o", "json")
	if err != nil {
		if isNotFound(err) {
			return objectState{}, false, nil
		}
		return objectState{}, false, err
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return objectState{}, false, err
	}
	var doc objectJSON
	if err := json.Unmarshal(out, &doc); err != nil {
		return objectState{}, false, err
	}
	state := objectState{
		APIVersion: resource.APIVersion, Kind: resource.Kind, Name: doc.Metadata.Name,
		Namespace: doc.Metadata.Namespace, UID: doc.Metadata.UID, RV: doc.Metadata.RV,
		Annotations: doc.Metadata.Annotations, Generation: doc.Metadata.Generation,
		Replicas: doc.Spec.Replicas, ObservedGeneration: doc.Status.ObservedGeneration,
		AvailableReplicas: doc.Status.AvailableReplicas, ReadyReplicas: doc.Status.ReadyReplicas,
		UnavailableReplicas: doc.Status.UnavailableReplicas, NumberReady: doc.Status.NumberReady,
		DesiredScheduled: doc.Status.DesiredNumber, Succeeded: doc.Status.Succeeded,
		Completions: doc.Status.Completions, Phase: doc.Status.Phase, Conditions: doc.Status.Conditions,
		Raw: raw,
	}
	return state, true, nil
}

func (s objectState) resource() driver.Resource {
	return driver.Resource{APIVersion: s.APIVersion, Kind: s.Kind, Namespace: s.Namespace, Name: s.Name, UID: s.UID, ResourceVersion: s.RV}
}

func (s objectState) ready() bool {
	switch s.Kind {
	case "Deployment", "StatefulSet":
		desired := int32(1)
		if s.Replicas != nil {
			desired = *s.Replicas
		}
		observed := int64(0)
		if s.ObservedGeneration != nil {
			observed = *s.ObservedGeneration
		}
		available := int32(0)
		if s.Kind == "Deployment" && s.AvailableReplicas != nil {
			available = *s.AvailableReplicas
		}
		if s.Kind == "StatefulSet" && s.ReadyReplicas != nil {
			available = *s.ReadyReplicas
		}
		return s.Generation <= observed && (desired == 0 || available >= desired)
	case "DaemonSet":
		desired := int32(0)
		if s.DesiredScheduled != nil {
			desired = *s.DesiredScheduled
		}
		ready := int32(0)
		if s.NumberReady != nil {
			ready = *s.NumberReady
		}
		return ready >= desired
	case "Job":
		for _, c := range s.Conditions {
			if c.Type == "Complete" && c.Status == "True" {
				return true
			}
		}
		want := int64(1)
		if s.Completions != nil {
			want = *s.Completions
		}
		got := int64(0)
		if s.Succeeded != nil {
			got = *s.Succeeded
		}
		return got >= want
	case "Pod":
		if s.Phase == "Succeeded" {
			return true
		}
		if s.Phase != "Running" {
			return false
		}
		for _, c := range s.Conditions {
			if c.Type == "Ready" {
				return c.Status == "True"
			}
		}
		return false
	default:
		return true
	}
}

func (s objectState) failed() bool {
	for _, c := range s.Conditions {
		if c.Status == "True" && (c.Type == "Failed" || c.Reason == "ProgressDeadlineExceeded") {
			return true
		}
	}
	return s.Phase == "Failed"
}

func (d *Driver) observeStates(ctx context.Context, cfg Config, resources []driver.Resource) (driver.Observation, error) {
	obs := driver.Observation{ObservedAt: time.Now().UTC(), Checks: []string{"owned_resource_inventory", "workload_readiness"}}
	if len(resources) == 0 {
		obs.State = driver.Unknown
		obs.Detail = mustJSON(map[string]string{"reason": "no recorded resource inventory"})
		return obs, nil
	}
	var seen []driver.Resource
	present := 0
	unhealthy := false
	progressing := false
	for _, resource := range resources {
		state, ok, err := d.getObject(ctx, cfg, resource)
		if err != nil {
			obs.State = driver.Unknown
			obs.Detail = mustJSON(map[string]string{"error": err.Error()})
			return obs, nil
		}
		if !ok {
			progressing = true
			continue
		}
		present++
		if owner := state.Annotations["kodama.optimi.com/resource-key"]; owner != resourceKey(cfg) {
			obs.State = driver.Conflict
			obs.Detail = mustJSON(map[string]string{"reason": "resource owner changed"})
			return obs, nil
		}
		if !imagesAllowed(state, cfg) {
			unhealthy = true
		}
		if state.failed() {
			unhealthy = true
		}
		if !state.ready() {
			progressing = true
		}
		seen = append(seen, state.resource())
	}
	obs.Resources = seen
	obs.ObservedRelease = cfg.Release
	if present == 0 {
		obs.State = driver.Absent
		obs.Checks = append(obs.Checks, "all_absent")
		return obs, nil
	}
	obs.ProviderVersion = inventoryVersion(seen)
	switch {
	case unhealthy:
		obs.State = driver.Unhealthy
	case progressing:
		obs.State = driver.Progressing
	default:
		obs.State = driver.Ready
	}
	return obs, nil
}

func imagesAllowed(state objectState, cfg Config) bool {
	if cfg.AllowAnyImage {
		return true
	}
	allowed := map[string]bool{cfg.Image: true}
	for _, image := range cfg.AuxiliaryImages {
		allowed[image] = true
	}
	for _, image := range genericImages(state.Raw) {
		if !allowed[image] {
			return false
		}
	}
	return true
}

func genericImages(doc map[string]any) []string {
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			for key, value := range node {
				if key == "containers" || key == "initContainers" {
					if list, ok := value.([]any); ok {
						for _, item := range list {
							if c, ok := item.(map[string]any); ok {
								if image := text(c["image"]); image != "" {
									out = append(out, image)
								}
							}
						}
					}
				}
				walk(value)
			}
		case []any:
			for _, item := range node {
				walk(item)
			}
		}
	}
	walk(doc)
	sort.Strings(out)
	return out
}

func inventoryVersion(resources []driver.Resource) string {
	var parts []string
	for _, r := range resources {
		parts = append(parts, r.APIVersion+"/"+r.Kind+"/"+r.Namespace+"/"+r.Name+"/"+r.UID+"/"+r.ResourceVersion)
	}
	sort.Strings(parts)
	return manifestDigest([]byte(strings.Join(parts, "\n")))
}

func (d *Driver) waitReady(ctx context.Context, cfg Config, resources []driver.Resource) (driver.Observation, error) {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var last driver.Observation
	for {
		obs, err := d.observeStates(waitCtx, cfg, resources)
		if err != nil {
			return obs, err
		}
		last = obs
		switch obs.State {
		case driver.Ready:
			return obs, nil
		case driver.Unhealthy, driver.Conflict:
			return obs, fmt.Errorf("kubernetes driver: deployment is %s", obs.State)
		}
		select {
		case <-waitCtx.Done():
			return last, fmt.Errorf("kubernetes driver: readiness deadline exceeded: %s", last.State)
		case <-ticker.C:
		}
	}
}

func (d *Driver) ensureOwned(ctx context.Context, cfg Config, resources []driver.Resource) error {
	for _, resource := range resources {
		state, ok, err := d.getObject(ctx, cfg, resource)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if state.Annotations["kodama.optimi.com/resource-key"] != resourceKey(cfg) {
			return fmt.Errorf("kubernetes driver: %s/%s already exists and is not owned by this release", resource.Kind, resource.Name)
		}
	}
	return nil
}

func (d *Driver) run(ctx context.Context, cfg Config, args ...string) ([]byte, error) {
	kubectl := d.Kubectl
	if kubectl == "" {
		kubectl = "kubectl"
	}
	if cfg.Kubeconfig != "" {
		args = append([]string{"--kubeconfig", cfg.Kubeconfig}, args...)
	}
	cmd := exec.CommandContext(ctx, kubectl, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func isNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "notfound") || strings.Contains(msg, "does not exist")
}

// deleteWithPreconditions uses kubectl proxy to send a Kubernetes DELETE with
// UID and resourceVersion preconditions. kubectl delete itself does not expose
// those preconditions, and a get-then-delete sequence is not a fence.
func (d *Driver) deleteWithPreconditions(ctx context.Context, cfg Config, state objectState) error {
	if state.UID == "" || state.RV == "" {
		return fmt.Errorf("kubernetes driver: refusing a delete without UID and resourceVersion")
	}
	base, stop, err := d.proxy(ctx, cfg)
	if err != nil {
		return err
	}
	defer stop()
	path, err := resourcePath(state.APIVersion, state.Kind, state.Namespace, state.Name)
	if err != nil {
		return err
	}
	body := `{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":` + strconv.Quote(state.UID) +
		`,"resourceVersion":` + strconv.Quote(state.RV) + `}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+path, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("kubernetes driver: delete request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return fmt.Errorf("kubernetes driver: delete %s/%s: %s: %s", state.Kind, state.Name, resp.Status, strings.TrimSpace(string(raw)))
}

func resourcePath(apiVersion, kind, namespace, name string) (string, error) {
	group, version := "", apiVersion
	if before, after, ok := strings.Cut(apiVersion, "/"); ok {
		group, version = before, after
	}
	plural := map[string]string{
		"Deployment": "deployments", "StatefulSet": "statefulsets", "DaemonSet": "daemonsets", "PersistentVolumeClaim": "persistentvolumeclaims",
		"ReplicaSet": "replicasets", "Pod": "pods", "Job": "jobs", "CronJob": "cronjobs",
		"Service": "services", "ConfigMap": "configmaps", "Secret": "secrets", "Ingress": "ingresses",
		"NetworkPolicy": "networkpolicies", "ServiceAccount": "serviceaccounts", "Role": "roles",
		"RoleBinding": "rolebindings", "PodDisruptionBudget": "poddisruptionbudgets",
		"HorizontalPodAutoscaler": "horizontalpodautoscalers",
	}[kind]
	if plural == "" {
		return "", fmt.Errorf("kubernetes driver: no API resource mapping for %s", kind)
	}
	p := "/"
	if group != "" {
		p += "apis/" + group + "/"
	} else {
		p += "api/"
	}
	return p + version + "/namespaces/" + namespace + "/" + plural + "/" + name, nil
}

var proxyLine = regexp.MustCompile(`127\.0\.0\.1:([0-9]+)`)

func (d *Driver) proxy(ctx context.Context, cfg Config) (string, func(), error) {
	kubectl := d.Kubectl
	if kubectl == "" {
		kubectl = "kubectl"
	}
	args := []string{"proxy", "--address=127.0.0.1", "--port=0"}
	if cfg.Kubeconfig != "" {
		args = append([]string{"--kubeconfig", cfg.Kubeconfig}, args...)
	}
	cmd := exec.CommandContext(ctx, kubectl, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", func() {}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", func() {}, err
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if m := proxyLine.FindStringSubmatch(scanner.Text()); m != nil {
				lines <- "http://127.0.0.1:" + m[1]
				return
			}
		}
		lines <- ""
	}()
	select {
	case <-ctx.Done():
		stop()
		return "", func() {}, ctx.Err()
	case <-time.After(10 * time.Second):
		stop()
		return "", func() {}, fmt.Errorf("kubernetes driver: kubectl proxy did not start: %s", strings.TrimSpace(stderr.String()))
	case base := <-lines:
		if base == "" {
			stop()
			return "", func() {}, fmt.Errorf("kubernetes driver: kubectl proxy output: %s", strings.TrimSpace(stderr.String()))
		}
		return base, stop, nil
	}
}
