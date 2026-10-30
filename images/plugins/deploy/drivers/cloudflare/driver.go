package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/optimiware/kodama/images/plugins/deploy/bundle"
	"github.com/optimiware/kodama/images/plugins/deploy/driver"
)

const (
	driverName    = "cloudflare-workers"
	driverVersion = "1"
)

// Config is the Cloudflare Workers recipe configuration. The API token is
// delivered through the existing secret mechanism and is never read here.
type Config struct {
	AccountID      string `json:"accountId"`
	Worker         string `json:"worker"`
	BundlePath     string `json:"bundlePath,omitempty"`
	Wrangler       string `json:"wrangler,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

func (c Config) validate() error {
	if c.AccountID == "" || c.Worker == "" {
		return fmt.Errorf("cloudflare driver: accountId and worker are required")
	}
	if c.BundlePath == "" {
		return fmt.Errorf("cloudflare driver: bundlePath is required")
	}
	return nil
}

// Driver implements a bounded Wrangler-based Workers deployment.
type Driver struct {
	Wrangler string
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
		ResourceKey:   "cloudflare/" + cfg.AccountID + "/" + cfg.Worker,
		ArtifactKinds: []driver.ArtifactKind{driver.ArtifactFile},
		Rollback:      "native",
		Observations:  []string{"provider_version", "worker_identity"},
		Preconditions: []string{"account", "worker"},
	}, nil
}

func (d *Driver) Apply(ctx context.Context, req driver.Request, checkpoint driver.Checkpoint) (driver.Result, error) {
	cfg, dir, cleanup, err := extractConfig(req.Configuration)
	if err != nil {
		return driver.Result{}, err
	}
	defer cleanup()
	if checkpoint != nil {
		if err := checkpoint(ctx, "upload-intent", mustJSON(map[string]string{"worker": cfg.Worker, "accountId": cfg.AccountID})); err != nil {
			return driver.Result{}, err
		}
	}
	config := firstFile(dir, "wrangler.jsonc", "wrangler.json", "wrangler.toml")
	args := []string{"deploy", "--name", cfg.Worker}
	if config != "" {
		args = append(args, "--config", config)
	}
	out, err := d.run(ctx, cfg, dir, args...)
	if err != nil {
		return driver.Result{Effect: "possible", Detail: mustJSON(map[string]string{"output": string(out)})}, fmt.Errorf("cloudflare driver: deploy: %w", err)
	}
	obs, err := d.Observe(ctx, req)
	if err != nil {
		return driver.Result{Effect: "possible"}, err
	}
	if obs.State != driver.Ready {
		return driver.Result{Effect: "possible", Detail: mustJSON(obs)}, fmt.Errorf("cloudflare driver: worker is %s", obs.State)
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
	out, err := d.run(ctx, cfg, "", "deployments", "list", "--name", cfg.Worker, "--json")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(strings.ToLower(err.Error()), "does not exist") {
			return driver.Observation{State: driver.Absent, ObservedAt: time.Now().UTC()}, nil
		}
		return driver.Observation{State: driver.Unknown, ObservedAt: time.Now().UTC(), Detail: mustJSON(map[string]string{"error": err.Error()})}, nil
	}
	var deployments []struct {
		ID       string `json:"id"`
		Versions []struct {
			VersionID string `json:"version_id"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(out, &deployments); err != nil {
		return driver.Observation{State: driver.Unknown, ObservedAt: time.Now().UTC(), Detail: mustJSON(map[string]string{"error": "unrecognized wrangler output"})}, nil
	}
	if len(deployments) == 0 {
		return driver.Observation{State: driver.Absent, ObservedAt: time.Now().UTC()}, nil
	}
	latest := deployments[0]
	version := ""
	if len(latest.Versions) > 0 {
		version = latest.Versions[0].VersionID
	}
	state := driver.Ready
	if latest.ID == "" || version == "" {
		state = driver.Progressing
	}
	return driver.Observation{
		State: state, ObservedAt: time.Now().UTC(), ProviderRelease: latest.ID,
		ProviderVersion: version, ObservedRelease: cfg.Worker,
		Checks: []string{"worker_deployment", "version"},
	}, nil
}

func (d *Driver) Destroy(ctx context.Context, req driver.Request, checkpoint driver.Checkpoint) (driver.Result, error) {
	cfg, _, cleanup, err := extractConfig(req.Configuration)
	if err != nil {
		return driver.Result{}, err
	}
	defer cleanup()
	if checkpoint != nil {
		if err := checkpoint(ctx, "delete-intent", mustJSON(map[string]string{"worker": cfg.Worker})); err != nil {
			return driver.Result{}, err
		}
	}
	out, err := d.run(ctx, cfg, "", "delete", "--name", cfg.Worker)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "not found") {
		return driver.Result{Effect: "possible", Detail: mustJSON(map[string]string{"output": string(out)})}, fmt.Errorf("cloudflare driver: delete: %w", err)
	}
	return driver.Result{Effect: "confirmed", Detail: mustJSON(map[string]string{"output": string(out)})}, nil
}

func (d *Driver) Rollback(ctx context.Context, req driver.Request, checkpoint driver.Checkpoint) (driver.Result, error) {
	cfg, _, cleanup, err := extractConfig(req.Configuration)
	if err != nil {
		return driver.Result{}, err
	}
	defer cleanup()
	if req.DesiredRelease == "" {
		return driver.Result{}, fmt.Errorf("cloudflare driver: a provider version is required for rollback")
	}
	if checkpoint != nil {
		if err := checkpoint(ctx, "rollback-intent", mustJSON(map[string]string{"worker": cfg.Worker, "version": req.DesiredRelease})); err != nil {
			return driver.Result{}, err
		}
	}
	if out, err := d.run(ctx, cfg, "", "rollback", req.DesiredRelease, "--name", cfg.Worker); err != nil {
		return driver.Result{Effect: "possible", Detail: mustJSON(map[string]string{"output": string(out)})}, err
	}
	obs, err := d.Observe(ctx, req)
	if err != nil || obs.State != driver.Ready {
		return driver.Result{Effect: "possible"}, fmt.Errorf("cloudflare driver: rollback was not observed ready")
	}
	return driver.Result{Effect: "confirmed", ProviderRelease: obs.ProviderRelease, ProviderVersion: obs.ProviderVersion, Detail: mustJSON(obs)}, nil
}

func (d *Driver) run(ctx context.Context, cfg Config, dir string, args ...string) ([]byte, error) {
	bin := d.Wrangler
	if bin == "" {
		bin = "wrangler"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "CLOUDFLARE_ACCOUNT_ID="+cfg.AccountID)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func extractConfig(raw json.RawMessage) (Config, string, func(), error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, "", func() {}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, "", func() {}, err
	}
	files, err := bundle.Read(cfg.BundlePath)
	if err != nil {
		return Config{}, "", func() {}, err
	}
	dir, err := os.MkdirTemp("", "kodama-cloudflare-*")
	if err != nil {
		return Config{}, "", func() {}, err
	}
	for name, data := range files {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			os.RemoveAll(dir)
			return Config{}, "", func() {}, err
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			os.RemoveAll(dir)
			return Config{}, "", func() {}, err
		}
	}
	return cfg, dir, func() { _ = os.RemoveAll(dir) }, nil
}

func firstFile(dir string, names ...string) string {
	for _, name := range names {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
