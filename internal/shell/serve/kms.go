package serve

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	gcpkms "cloud.google.com/go/kms/apiv1"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"

	secretkeys "github.com/optimiware/kodama/pkg/secretkeys"
)

// KMSConfig is the installation's key-encryption configuration. It is a file,
// not a set of environment values: the provider credentials are projected as
// files beside it and never appear in a process environment or a Helm value.
//
// The stage uses the Scaleway Key Manager adapter. AWS and GCP are accepted so
// the same kernel can run on the other supported installations; a missing
// provider credential is a startup failure, never a fallback to the keyfile.
type KMSConfig struct {
	Provider       string            `json:"provider"`
	Current        string            `json:"current"`
	Resources      map[string]string `json:"resources"`
	StartupTimeout string            `json:"startupTimeout,omitempty"`
	SCW            *SCWKMS           `json:"scw,omitempty"`
	AWS            *AWSKMS           `json:"aws,omitempty"`
	GCP            *GCPKMS           `json:"gcp,omitempty"`
}

type SCWKMS struct {
	CredentialsFile string `json:"credentialsFile"`
}

type AWSKMS struct {
	Region string `json:"region"`
}

type GCPKMS struct{}

// loadKMSConfig reads the strict configuration document.
func loadKMSConfig(path string) (KMSConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return KMSConfig{}, fmt.Errorf("kernel: KMS configuration: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var cfg KMSConfig
	if err := dec.Decode(&cfg); err != nil {
		return KMSConfig{}, fmt.Errorf("kernel: KMS configuration: %w", err)
	}
	if dec.More() {
		return KMSConfig{}, errors.New("kernel: KMS configuration has trailing data")
	}
	if cfg.Provider == "" || cfg.Current == "" || len(cfg.Resources) == 0 {
		return KMSConfig{}, errors.New("kernel: KMS configuration needs provider, current and resources")
	}
	if _, ok := cfg.Resources[cfg.Current]; !ok {
		return KMSConfig{}, errors.New("kernel: KMS current names no configured resource")
	}
	return cfg, nil
}

// newKMSProvider constructs the selected cloud provider and proves it can wrap
// and unwrap before the server starts. The probe uses a random key and the
// installation binding, so a wrong key, a denied identity or an unavailable
// service is a startup failure rather than a late request failure.
func newKMSProvider(ctx context.Context, cfg KMSConfig, installationID string) (secretkeys.Provider, func(), error) {
	timeout := 10 * time.Second
	if cfg.StartupTimeout != "" {
		d, err := time.ParseDuration(cfg.StartupTimeout)
		if err != nil || d <= 0 || d > 2*time.Minute {
			return nil, nil, errors.New("kernel: KMS startupTimeout is a positive duration of at most 2m")
		}
		timeout = d
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resource := cfg.Resources[cfg.Current]
	retained := make([]string, 0, len(cfg.Resources))
	for name, id := range cfg.Resources {
		if name != cfg.Current {
			retained = append(retained, id)
		}
	}

	var provider secretkeys.Provider
	var closeFn func() = func() {}
	switch cfg.Provider {
	case "scw":
		if cfg.SCW == nil || cfg.SCW.CredentialsFile == "" {
			return nil, nil, errors.New("kernel: scw KMS needs scw.credentialsFile")
		}
		raw, err := os.ReadFile(cfg.SCW.CredentialsFile)
		if err != nil {
			return nil, nil, fmt.Errorf("kernel: Scaleway KMS credential: %w", err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return nil, nil, errors.New("kernel: Scaleway KMS credential is empty")
		}
		id := strings.TrimPrefix(resource, secretkeys.ScalewayRefPrefix)
		trimmedRetained := make([]string, 0, len(retained))
		for _, r := range retained {
			trimmedRetained = append(trimmedRetained, strings.TrimPrefix(r, secretkeys.ScalewayRefPrefix))
		}
		p, err := secretkeys.NewScaleway(token, id, trimmedRetained)
		if err != nil {
			return nil, nil, fmt.Errorf("kernel: Scaleway KMS: %w", err)
		}
		provider = p
	case "aws":
		if cfg.AWS == nil || cfg.AWS.Region == "" {
			return nil, nil, errors.New("kernel: aws KMS needs aws.region")
		}
		awscfg, err := awsconfig.LoadDefaultConfig(pctx, awsconfig.WithRegion(cfg.AWS.Region))
		if err != nil {
			return nil, nil, fmt.Errorf("kernel: AWS KMS credentials: %w", err)
		}
		client := awskms.NewFromConfig(awscfg)
		p, err := secretkeys.NewAWS(client, resource, retained)
		if err != nil {
			return nil, nil, fmt.Errorf("kernel: AWS KMS: %w", err)
		}
		provider = p
	case "gcp":
		client, err := gcpkms.NewKeyManagementClient(pctx)
		if err != nil {
			return nil, nil, fmt.Errorf("kernel: GCP KMS client: %w", err)
		}
		p, err := secretkeys.NewGCP(client, resource, retained)
		if err != nil {
			_ = client.Close()
			return nil, nil, fmt.Errorf("kernel: GCP KMS: %w", err)
		}
		provider = p
		closeFn = func() { _ = client.Close() }
	default:
		return nil, nil, fmt.Errorf("kernel: unknown KMS provider %q", cfg.Provider)
	}

	probe := make([]byte, 32)
	if _, err := rand.Read(probe); err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("kernel: KMS startup probe entropy: %w", err)
	}
	defer clear(probe)
	binding := []byte("kodama installation " + installationID)
	wrapped, err := provider.Wrap(pctx, probe, binding)
	if err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("kernel: KMS startup wrap: %w", err)
	}
	got, err := provider.Unwrap(pctx, wrapped, binding)
	if err != nil || string(got) != string(probe) {
		clear(got)
		closeFn()
		return nil, nil, errors.New("kernel: KMS startup unwrap did not return the probe key")
	}
	clear(got)
	return provider, closeFn, nil
}
