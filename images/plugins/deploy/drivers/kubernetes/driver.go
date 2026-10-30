package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/optimiware/kodama/images/plugins/deploy/driver"
)

const (
	driverName    = "kubernetes"
	driverVersion = "2"
)

// Config is the typed configuration for a Kubernetes namespace deployment.
// Cluster is the stable operator-selected cluster identity; it is not the
// mutable kubeconfig context name.
type Config struct {
	Cluster           string   `json:"cluster"`
	Namespace         string   `json:"namespace"`
	Release           string   `json:"release"`
	Kubeconfig        string   `json:"kubeconfig,omitempty"`
	ManifestPath      string   `json:"manifestPath,omitempty"`
	BundlePath        string   `json:"bundlePath,omitempty"`
	Image             string   `json:"image,omitempty"`
	AuxiliaryImages   []string `json:"auxiliaryImages,omitempty"`
	AllowMutableImage bool     `json:"allowMutableImage,omitempty"`
	// AllowAnyImage is an internal rollback mode: the retained bundle was
	// already verified as the release input, so a maintenance command may not
	// have the original image flag.
	AllowAnyImage  bool `json:"-"`
	TimeoutSeconds int  `json:"timeoutSeconds,omitempty"`
}

func (c *Config) validate() error {
	if c.Cluster == "" || c.Namespace == "" || c.Release == "" {
		return fmt.Errorf("kubernetes driver: cluster, namespace and release are required")
	}
	if c.ManifestPath == "" && c.BundlePath == "" {
		return fmt.Errorf("kubernetes driver: a manifest or bundle is required")
	}
	return nil
}

// Driver deploys a bounded, namespace-scoped manifest inventory.
type Driver struct {
	Kubectl string
}

func resourceKey(cfg Config) string {
	return "kubernetes/" + cfg.Cluster + "/" + cfg.Namespace + "/" + cfg.Release
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
		ResourceKey:   resourceKey(cfg),
		ArtifactKinds: []driver.ArtifactKind{driver.ArtifactFile},
		Rollback:      "reapply",
		Observations:  []string{"owned_resource_inventory", "workload_readiness", "image_reference"},
		Preconditions: []string{"namespace", "kind", "uid", "resourceVersion"},
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
	_, objects, err := loadManifest(cfg)
	if err != nil {
		return driver.Result{}, err
	}
	if err := validateImages(objects, cfg); err != nil {
		return driver.Result{}, err
	}
	if !cfg.AllowAnyImage {
		verified := false
		for _, artifact := range req.Artifacts {
			if artifact.Kind == driver.ArtifactOCIImage && artifact.Registry == cfg.Image {
				verified = true
				break
			}
		}
		if !verified {
			return driver.Result{}, fmt.Errorf("kubernetes driver: the primary image has no verified OCI artifact")
		}
	}
	refs := objectRefs(objects)
	if err := d.ensureOwned(ctx, cfg, refs); err != nil {
		return driver.Result{}, err
	}
	rendered, err := render(objects, resourceKey(cfg), cfg.Release, req.OperationID)
	if err != nil {
		return driver.Result{}, err
	}
	if checkpoint != nil {
		if err := checkpoint(ctx, "manifest-accepted", mustJSON(map[string]string{
			"release": cfg.Release, "namespace": cfg.Namespace, "resourceKey": resourceKey(cfg),
			"manifestDigest": manifestDigest(rendered), "operation": req.OperationID,
		})); err != nil {
			return driver.Result{}, err
		}
	}
	manifestFile, err := writeTempManifest(rendered)
	if err != nil {
		return driver.Result{}, err
	}
	defer os.Remove(manifestFile)
	if out, err := d.run(ctx, cfg, "apply", "-f", manifestFile); err != nil {
		return driver.Result{Effect: "possible", Detail: mustJSON(map[string]string{"output": string(out)})},
			fmt.Errorf("kubernetes driver: apply: %w", err)
	}
	obs, err := d.waitReady(ctx, cfg, refs)
	if err != nil {
		return driver.Result{Effect: "possible", Resources: obs.Resources, Detail: mustJSON(obs)},
			fmt.Errorf("kubernetes driver: apply did not become ready: %w", err)
	}
	return driver.Result{
		Effect: "confirmed", ProviderRelease: cfg.Release, ProviderVersion: obs.ProviderVersion,
		Resources: obs.Resources, Detail: mustJSON(obs),
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
	resources := previousResources(req.Previous)
	if len(resources) == 0 {
		_, objects, err := loadManifest(cfg)
		if err != nil {
			return driver.Observation{State: driver.Unknown, ObservedAt: time.Now().UTC(), Detail: mustJSON(map[string]string{"error": err.Error()})}, nil
		}
		resources = objectRefs(objects)
	}
	return d.observeStates(ctx, cfg, resources)
}

func (d *Driver) Destroy(ctx context.Context, req driver.Request, checkpoint driver.Checkpoint) (driver.Result, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Result{}, fmt.Errorf("kubernetes driver: decode configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return driver.Result{}, err
	}
	resources := previousResources(req.Previous)
	if len(resources) == 0 {
		return driver.Result{Effect: "none"}, fmt.Errorf("kubernetes driver: refusing cleanup without a recorded resource inventory")
	}
	if checkpoint != nil {
		if err := checkpoint(ctx, "delete-intent", mustJSON(map[string]any{
			"namespace": cfg.Namespace, "release": cfg.Release, "resourceKey": resourceKey(cfg), "resources": resources,
		})); err != nil {
			return driver.Result{}, err
		}
	}
	for i := len(resources) - 1; i >= 0; i-- {
		resource := resources[i]
		if checkpoint != nil {
			if err := checkpoint(ctx, "delete-"+strings.ToLower(resource.Kind)+"/"+resource.Name, mustJSON(resource)); err != nil {
				return driver.Result{Effect: "none"}, err
			}
		}
		state, ok, err := d.getObject(ctx, cfg, resource)
		if err != nil {
			return driver.Result{Effect: "possible"}, err
		}
		if !ok {
			continue
		}
		if state.Annotations["kodama.optimi.com/resource-key"] != resourceKey(cfg) {
			return driver.Result{Effect: "none"}, fmt.Errorf("kubernetes driver: refusing to delete unowned %s/%s", resource.Kind, resource.Name)
		}
		if resource.UID != "" && state.UID != resource.UID {
			return driver.Result{Effect: "none"}, fmt.Errorf("kubernetes driver: %s/%s was replaced; refusing to delete it", resource.Kind, resource.Name)
		}
		if err := d.deleteWithPreconditions(ctx, cfg, state); err != nil {
			return driver.Result{Effect: "possible"}, err
		}
	}
	obs, err := d.observeStates(ctx, cfg, resources)
	if err != nil {
		return driver.Result{Effect: "possible"}, err
	}
	if obs.State != driver.Absent {
		return driver.Result{Effect: "possible", Detail: mustJSON(obs)},
			fmt.Errorf("kubernetes driver: deletion is not confirmed: %s", obs.State)
	}
	return driver.Result{Effect: "confirmed", Detail: mustJSON(map[string]string{"deleted": cfg.Release})}, nil
}

func (d *Driver) Rollback(ctx context.Context, req driver.Request, checkpoint driver.Checkpoint) (driver.Result, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Result{}, fmt.Errorf("kubernetes driver: decode configuration: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return driver.Result{}, err
	}
	cfg.BundlePath, cfg.ManifestPath = "", ""
	for _, artifact := range req.Artifacts {
		if artifact.Kind != driver.ArtifactFile || artifact.Path == "" {
			continue
		}
		if strings.HasSuffix(artifact.Path, ".yaml") || strings.HasSuffix(artifact.Path, ".yml") {
			cfg.ManifestPath = artifact.Path
		} else {
			cfg.BundlePath = artifact.Path
		}
	}
	if cfg.BundlePath == "" && cfg.ManifestPath == "" {
		return driver.Result{Effect: "none"}, fmt.Errorf("kubernetes driver: rollback requires a retained manifest or bundle artifact")
	}
	cfg.AllowAnyImage = true
	return d.Apply(ctx, req, checkpoint)
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

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

var _ = exec.Command
