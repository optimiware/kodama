package gcp

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
	driverName    = "gcp-cloud-run"
	driverVersion = "1"
)

// Config is the Cloud Run recipe configuration. Image is the digest-pinned OCI
// reference supplied by the core artifact binding.
type Config struct {
	Project        string `json:"project"`
	Region         string `json:"region"`
	Service        string `json:"service"`
	Image          string `json:"image"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

func (c Config) validate() error {
	if c.Project == "" || c.Region == "" || c.Service == "" {
		return fmt.Errorf("gcp driver: project, region and service are required")
	}
	return nil
}

type Driver struct {
	Gcloud string
}

func (d *Driver) Identify(_ context.Context, req driver.IdentityRequest) (driver.Identity, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Identity{}, err
	}
	if err := cfg.validate(); err != nil {
		return driver.Identity{}, err
	}
	return driver.Identity{
		Driver: driverName, DriverVersion: driverVersion,
		ResourceKey:   "gcp/" + cfg.Project + "/" + cfg.Region + "/" + cfg.Service,
		ArtifactKinds: []driver.ArtifactKind{driver.ArtifactOCIImage},
		Rollback:      "native",
		Observations:  []string{"service_status", "traffic_revision"},
		Preconditions: []string{"project", "region", "service"},
	}, nil
}

func (d *Driver) Apply(ctx context.Context, req driver.Request, checkpoint driver.Checkpoint) (driver.Result, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Result{}, err
	}
	if err := cfg.validate(); err != nil {
		return driver.Result{}, err
	}
	if cfg.Image == "" {
		return driver.Result{}, fmt.Errorf("gcp driver: image is required")
	}
	if checkpoint != nil {
		if err := checkpoint(ctx, "deploy-intent", mustJSON(map[string]string{"service": cfg.Service, "image": cfg.Image})); err != nil {
			return driver.Result{}, err
		}
	}
	out, err := d.run(ctx, "run", "deploy", cfg.Service, "--image", cfg.Image,
		"--project", cfg.Project, "--region", cfg.Region, "--quiet")
	if err != nil {
		return driver.Result{Effect: "possible", Detail: mustJSON(map[string]string{"output": string(out)})}, fmt.Errorf("gcp driver: deploy: %w", err)
	}
	obs, err := d.Observe(ctx, req)
	if err != nil || obs.State != driver.Ready {
		if err == nil {
			err = fmt.Errorf("gcp driver: service is %s", obs.State)
		}
		return driver.Result{Effect: "possible", Detail: mustJSON(obs)}, err
	}
	return driver.Result{Effect: "confirmed", ProviderRelease: obs.ProviderRelease, ProviderVersion: obs.ProviderVersion, Detail: mustJSON(obs)}, nil
}

func (d *Driver) Observe(ctx context.Context, req driver.Request) (driver.Observation, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Observation{}, err
	}
	if err := cfg.validate(); err != nil {
		return driver.Observation{}, err
	}
	out, err := d.run(ctx, "run", "services", "describe", cfg.Service,
		"--project", cfg.Project, "--region", cfg.Region, "--format=json")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return driver.Observation{State: driver.Absent, ObservedAt: time.Now().UTC()}, nil
		}
		return driver.Observation{State: driver.Unknown, ObservedAt: time.Now().UTC(), Detail: mustJSON(map[string]string{"error": err.Error()})}, nil
	}
	var service struct {
		Status struct {
			LatestReadyRevisionName string `json:"latestReadyRevisionName"`
			Traffic                 []struct {
				LatestRevision bool   `json:"latestRevision"`
				RevisionName   string `json:"revisionName"`
				Percent        int    `json:"percent"`
			} `json:"traffic"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &service); err != nil {
		return driver.Observation{State: driver.Unknown, ObservedAt: time.Now().UTC()}, nil
	}
	version := service.Status.LatestReadyRevisionName
	state := driver.Progressing
	if version != "" {
		state = driver.Ready
	}
	return driver.Observation{
		State: state, ObservedAt: time.Now().UTC(), ProviderVersion: version,
		ObservedRelease: cfg.Service, Checks: []string{"latest_ready_revision"},
	}, nil
}

func (d *Driver) Destroy(ctx context.Context, req driver.Request, checkpoint driver.Checkpoint) (driver.Result, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Result{}, err
	}
	if err := cfg.validate(); err != nil {
		return driver.Result{}, err
	}
	if checkpoint != nil {
		if err := checkpoint(ctx, "delete-intent", mustJSON(map[string]string{"service": cfg.Service})); err != nil {
			return driver.Result{}, err
		}
	}
	out, err := d.run(ctx, "run", "services", "delete", cfg.Service,
		"--project", cfg.Project, "--region", cfg.Region, "--quiet")
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "not found") {
		return driver.Result{Effect: "possible", Detail: mustJSON(map[string]string{"output": string(out)})}, err
	}
	return driver.Result{Effect: "confirmed", Detail: mustJSON(map[string]string{"output": string(out)})}, nil
}

func (d *Driver) Rollback(ctx context.Context, req driver.Request, checkpoint driver.Checkpoint) (driver.Result, error) {
	var cfg Config
	if err := json.Unmarshal(req.Configuration, &cfg); err != nil {
		return driver.Result{}, err
	}
	if err := cfg.validate(); err != nil {
		return driver.Result{}, err
	}
	if req.DesiredRelease == "" {
		return driver.Result{}, fmt.Errorf("gcp driver: a revision is required for rollback")
	}
	if checkpoint != nil {
		if err := checkpoint(ctx, "rollback-intent", mustJSON(map[string]string{"service": cfg.Service, "revision": req.DesiredRelease})); err != nil {
			return driver.Result{}, err
		}
	}
	if out, err := d.run(ctx, "run", "services", "update-traffic", cfg.Service,
		"--project", cfg.Project, "--region", cfg.Region,
		"--to-revisions", req.DesiredRelease+"=100", "--quiet"); err != nil {
		return driver.Result{Effect: "possible", Detail: mustJSON(map[string]string{"output": string(out)})}, err
	}
	obs, err := d.Observe(ctx, req)
	if err != nil || obs.State != driver.Ready {
		return driver.Result{Effect: "possible"}, fmt.Errorf("gcp driver: rollback was not observed ready")
	}
	return driver.Result{Effect: "confirmed", ProviderVersion: obs.ProviderVersion, Detail: mustJSON(obs)}, nil
}

func (d *Driver) run(ctx context.Context, args ...string) ([]byte, error) {
	bin := d.Gcloud
	if bin == "" {
		bin = "gcloud"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
