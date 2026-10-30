package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/images/plugins/deploy/bundle"
	"github.com/optimiware/kodama/images/plugins/deploy/driver"
)

const (
	driverName    = "kubernetes"
	driverVersion = "1"
)

// Config is the typed configuration for a Kubernetes namespace deployment.
type Config struct {
	Namespace      string `json:"namespace"`
	Release        string `json:"release"`
	Kubeconfig     string `json:"kubeconfig,omitempty"`
	ManifestPath   string `json:"manifestPath,omitempty"`
	BundlePath     string `json:"bundlePath,omitempty"`
	Image          string `json:"image,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

func (c *Config) validate() error {
	if c.Namespace == "" || c.Release == "" {
		return fmt.Errorf("kubernetes driver: namespace and release are required")
	}
	if c.ManifestPath == "" && c.BundlePath == "" {
		return fmt.Errorf("kubernetes driver: a manifest or bundle is required")
	}
	return nil
}

// Driver deploys an immutable manifest inventory to one namespace.
type Driver struct {
	Kubectl string
}

func (d *Driver) Identify(_ context.Context, req driver.IdentityRequest) (driver.Identity, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Identity{}, fmt.Errorf("kubernetes driver: decode configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return driver.Identity{}, err
	}
	return driver.Identity{
		Driver: driverName, DriverVersion: driverVersion,
		ResourceKey:   "kubernetes/" + cfg.Kubeconfig + "/" + cfg.Namespace + "/" + cfg.Release,
		ArtifactKinds: []driver.ArtifactKind{driver.ArtifactFile},
		Rollback:      "reapply",
		Observations:  []string{"deployment_status", "resource_inventory", "image_reference"},
		Preconditions: []string{"namespace", "kind"},
	}, nil
}

func (d *Driver) Apply(ctx context.Context, req driver.Request, checkpoint driver.Checkpoint) (driver.Result, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Result{}, fmt.Errorf("kubernetes driver: decode configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return driver.Result{}, err
	}
	manifest, cleanup, err := readManifest(cfg)
	if err != nil {
		return driver.Result{}, err
	}
	defer cleanup()

	if checkpoint != nil {
		if err := checkpoint(ctx, "manifest-accepted", mustJSON(map[string]string{
			"release": cfg.Release, "namespace": cfg.Namespace, "manifestDigest": manifestDigest(manifest),
		})); err != nil {
			return driver.Result{}, fmt.Errorf("kubernetes driver: checkpoint: %w", err)
		}
	}
	manifestFile, err := writeTempManifest(manifest)
	if err != nil {
		return driver.Result{}, err
	}
	defer os.Remove(manifestFile)
	if _, err := d.run(ctx, cfg, "apply", "-f", manifestFile); err != nil {
		return driver.Result{Effect: "possible"}, fmt.Errorf("kubernetes driver: apply: %w", err)
	}
	// The marker is evidence of ownership for observation and future cleanup.
	if _, err := d.run(ctx, cfg, "annotate", "deployment", cfg.Release,
		"-n", cfg.Namespace,
		"kodama.optimi.com/release="+cfg.Release,
		"kodama.optimi.com/operation="+req.OperationID,
		"--overwrite"); err != nil {
		// A manifest need not contain a Deployment. The inventory observation
		// below is the transferable readiness check; an absent Deployment is
		// handled explicitly rather than turning every manifest into a Deployment.
		if !strings.Contains(strings.ToLower(err.Error()), "notfound") && !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return driver.Result{Effect: "possible"}, fmt.Errorf("kubernetes driver: annotate: %w", err)
		}
	}
	if cfg.TimeoutSeconds > 0 {
		out, err := d.run(ctx, cfg, "rollout", "status", "deployment/"+cfg.Release,
			"-n", cfg.Namespace, "--timeout="+strconv.Itoa(cfg.TimeoutSeconds)+"s")
		if err != nil {
			return driver.Result{Effect: "possible", Detail: mustJSON(map[string]string{"output": string(out)})},
				fmt.Errorf("kubernetes driver: rollout: %w", err)
		}
	}
	resources, err := d.inventory(ctx, cfg)
	if err != nil {
		return driver.Result{Effect: "possible"}, fmt.Errorf("kubernetes driver: inventory: %w", err)
	}
	if cfg.Image != "" {
		if err := d.checkImage(ctx, cfg); err != nil {
			return driver.Result{Effect: "possible", Resources: resources}, err
		}
	}
	obs, err := d.Observe(ctx, withConfig(req, cfg))
	if err != nil {
		return driver.Result{Effect: "possible", Resources: resources}, err
	}
	if obs.State != driver.Ready {
		return driver.Result{Effect: "possible", Resources: resources, Detail: mustJSON(obs)},
			fmt.Errorf("kubernetes driver: deployment is %s", obs.State)
	}
	return driver.Result{
		Effect: "confirmed", ProviderRelease: cfg.Release, Resources: resources,
		Detail: mustJSON(obs),
	}, nil
}

func (d *Driver) Observe(ctx context.Context, req driver.Request) (driver.Observation, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Observation{}, fmt.Errorf("kubernetes driver: decode configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return driver.Observation{}, err
	}
	out, err := d.run(ctx, cfg, "get", "deployment", cfg.Release, "-n", cfg.Namespace, "-o", "json")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "notfound") || strings.Contains(strings.ToLower(err.Error()), "not found") {
			return driver.Observation{State: driver.Absent, ObservedAt: time.Now().UTC(), Checks: []string{"deployment_absent"}}, nil
		}
		return driver.Observation{State: driver.Unknown, ObservedAt: time.Now().UTC(), Detail: mustJSON(map[string]string{"error": err.Error()})}, nil
	}
	var dep deploymentJSON
	if err := json.Unmarshal(out, &dep); err != nil {
		return driver.Observation{}, fmt.Errorf("kubernetes driver: decode deployment: %w", err)
	}
	obs := driver.Observation{
		ObservedAt: time.Now().UTC(), ProviderRelease: cfg.Release,
		Resources: []driver.Resource{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: cfg.Namespace, Name: cfg.Release, UID: dep.Metadata.UID}},
		Checks:    []string{"observed_generation", "available_replicas"},
	}
	if dep.Metadata.Generation > 0 && dep.Status.ObservedGeneration >= dep.Metadata.Generation &&
		dep.Status.AvailableReplicas >= maxInt32(dep.Spec.Replicas, 1) {
		obs.State = driver.Ready
		obs.ObservedRelease = cfg.Release
		return obs, nil
	}
	if dep.Status.UnavailableReplicas > 0 || dep.Status.Replicas > 0 {
		obs.State = driver.Progressing
	} else {
		obs.State = driver.Unknown
	}
	return obs, nil
}

func (d *Driver) Destroy(ctx context.Context, req driver.Request, checkpoint driver.Checkpoint) (driver.Result, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Result{}, fmt.Errorf("kubernetes driver: decode configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return driver.Result{}, err
	}
	if checkpoint != nil {
		if err := checkpoint(ctx, "delete-intent", mustJSON(map[string]string{"namespace": cfg.Namespace, "release": cfg.Release})); err != nil {
			return driver.Result{}, fmt.Errorf("kubernetes driver: checkpoint: %w", err)
		}
	}
	resources := previousResources(req.Previous)
	var outputs []string
	if len(resources) == 0 {
		resources = []driver.Resource{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: cfg.Namespace, Name: cfg.Release}}
	}
	for _, resource := range resources {
		if resource.Namespace != "" && resource.Namespace != cfg.Namespace {
			return driver.Result{Effect: "none"}, fmt.Errorf("kubernetes driver: refusing to delete a resource outside the target namespace")
		}
		out, err := d.run(ctx, cfg, "delete", strings.ToLower(resource.Kind)+"/"+resource.Name,
			"-n", cfg.Namespace, "--ignore-not-found=true", "--wait=true")
		outputs = append(outputs, strings.TrimSpace(string(out)))
		if err != nil {
			return driver.Result{Effect: "possible", Detail: mustJSON(map[string]any{"outputs": outputs})},
				fmt.Errorf("kubernetes driver: delete %s: %w", resource.Name, err)
		}
	}
	return driver.Result{Effect: "confirmed", Detail: mustJSON(map[string]any{"outputs": outputs})}, nil
}

func previousResources(raw json.RawMessage) []driver.Resource {
	if len(raw) == 0 {
		return nil
	}
	var doc struct {
		Resources []driver.Resource `json:"resources"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return doc.Resources
}

func (d *Driver) Rollback(context.Context, driver.Request, driver.Checkpoint) (driver.Result, error) {
	return driver.Result{}, fmt.Errorf("kubernetes driver: rollback requires a retained manifest; use a new apply operation")
}

func (d *Driver) run(ctx context.Context, cfg Config, args ...string) ([]byte, error) {
	kubectl := d.Kubectl
	if kubectl == "" {
		kubectl = "kubectl"
	}
	cmd := exec.CommandContext(ctx, kubectl, args...)
	if cfg.Kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+cfg.Kubeconfig)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (d *Driver) inventory(ctx context.Context, cfg Config) ([]driver.Resource, error) {
	manifest, cleanup, err := readManifest(cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// kubectl needs a file for `get -f`; a temporary file is the simplest
	// provider-neutral way to retain the exact applied inventory.
	f, err := os.CreateTemp("", "kodama-manifest-*.yaml")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(manifest); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	out, err := d.run(ctx, cfg, "get", "-f", name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				UID       string `json:"uid"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	resources := make([]driver.Resource, 0, len(list.Items))
	for _, item := range list.Items {
		resources = append(resources, driver.Resource{APIVersion: item.APIVersion, Kind: item.Kind, Name: item.Metadata.Name, Namespace: item.Metadata.Namespace, UID: item.Metadata.UID})
	}
	return resources, nil
}

func (d *Driver) checkImage(ctx context.Context, cfg Config) error {
	out, err := d.run(ctx, cfg, "get", "deployment", cfg.Release, "-n", cfg.Namespace, "-o", "json")
	if err != nil {
		return fmt.Errorf("kubernetes driver: image observation: %w", err)
	}
	var dep deploymentJSON
	if err := json.Unmarshal(out, &dep); err != nil {
		return err
	}
	for _, container := range dep.Spec.Template.Spec.Containers {
		if container.Image == cfg.Image {
			return nil
		}
	}
	return fmt.Errorf("kubernetes driver: deployment does not run the requested image")
}

type deploymentJSON struct {
	Metadata struct {
		Name       string `json:"name"`
		UID        string `json:"uid"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Replicas int32 `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration  int64 `json:"observedGeneration"`
		Replicas            int32 `json:"replicas"`
		AvailableReplicas   int32 `json:"availableReplicas"`
		UnavailableReplicas int32 `json:"unavailableReplicas"`
	} `json:"status"`
}

func maxInt32(v, fallback int32) int32 {
	if v <= 0 {
		return fallback
	}
	return v
}

func readManifest(cfg Config) ([]byte, func(), error) {
	if cfg.ManifestPath != "" {
		data, err := os.ReadFile(cfg.ManifestPath)
		return data, func() {}, err
	}
	files, err := bundle.Read(cfg.BundlePath)
	if err != nil {
		return nil, func() {}, fmt.Errorf("kubernetes driver: %w", err)
	}
	manifest, ok := files.Manifest()
	if !ok {
		return nil, func() {}, fmt.Errorf("kubernetes driver: bundle has no manifest.yaml")
	}
	return manifest, func() {}, nil
}

func writeTempManifest(data []byte) (string, error) {
	f, err := os.CreateTemp("", "kodama-manifest-*.yaml")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func manifestDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func withConfig(req driver.Request, cfg Config) driver.Request {
	raw, _ := json.Marshal(cfg)
	req.Configuration = raw
	return req
}
