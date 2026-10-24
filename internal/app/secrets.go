package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gcpkms "cloud.google.com/go/kms/apiv1"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	"github.com/optimiware/kodama/internal/secrets/kms/production"
)

// SecretsConfig selects installation-level key custody, never a tenant value.
type SecretsConfig struct {
	Adapter string          `yaml:"adapter" json:"adapter"`
	Keyfile *KeyfileConfig  `yaml:"keyfile,omitempty" json:"keyfile,omitempty"`
	KMS     *CloudKMSConfig `yaml:"kms,omitempty" json:"kms,omitempty"`
}

type KeyfileConfig struct {
	Current string            `yaml:"current" json:"current"`
	Files   map[string]string `yaml:"files" json:"files"`
}

type CloudKMSConfig struct {
	Provider       string            `yaml:"provider" json:"provider"`
	KEKID          string            `yaml:"kekId" json:"kekId"`
	Current        string            `yaml:"current" json:"current"`
	Resources      map[string]string `yaml:"resources" json:"resources"`
	StartupTimeout time.Duration     `yaml:"startupTimeout,omitempty" json:"startupTimeout,omitempty"`
	AWS            *AWSKMSConfig     `yaml:"aws,omitempty" json:"aws,omitempty"`
	SCW            *SCWKMSConfig     `yaml:"scw,omitempty" json:"scw,omitempty"`
}

type AWSKMSConfig struct {
	Region string `yaml:"region" json:"region"`
}
type SCWKMSConfig struct {
	CredentialsFile string `yaml:"credentialsFile" json:"credentialsFile"`
}

func (c CloudKMSConfig) productionConfig() production.Config {
	provider := c.Provider
	// scw is the public configuration name. Persisted KEK identities retain
	// the established "scaleway" format so existing envelopes remain readable.
	if provider == "scw" {
		provider = production.Scaleway
	}
	return production.Config{Provider: provider, KEKID: c.KEKID, Current: c.Current, Resources: c.Resources, ProbeTimeout: c.StartupTimeout}
}

func (c SecretsConfig) Validate(env Env) error {
	if !env.Valid() {
		return ErrEnvUnknown
	}
	switch c.Adapter {
	case "keyfile":
		if env == Production {
			return &AdapterError{Env: env, Port: PortKeyWrapper, Adapter: "keyfile", Reason: "it is the development key-encryption key"}
		}
		if c.Keyfile == nil || c.KMS != nil {
			return errors.New("secrets: keyfile requires only its keyfile configuration")
		}
		if c.Keyfile.Current == "" || c.Keyfile.Files[c.Keyfile.Current] == "" {
			return errors.New("secrets: current key label must name configured key files")
		}
		for label, path := range c.Keyfile.Files {
			if label == "" || path == "" {
				return errors.New("secrets: key files require nonempty labels and paths")
			}
		}
	case "kms":
		if c.KMS == nil || c.Keyfile != nil {
			return errors.New("secrets: kms requires only its kms configuration")
		}
		k := c.KMS
		switch k.Provider {
		case "aws":
			if k.AWS == nil || k.AWS.Region == "" || k.SCW != nil {
				return errors.New("secrets: aws requires only aws.region")
			}
		case "gcp":
			if k.AWS != nil || k.SCW != nil {
				return errors.New("secrets: gcp uses application-default credentials; foreign cloud settings are invalid")
			}
		case "scw":
			if k.SCW == nil || k.SCW.CredentialsFile == "" || k.AWS != nil {
				return errors.New("secrets: scw requires only scw.credentialsFile")
			}
		default:
			return errors.New("secrets: kms.provider must be aws, gcp or scw")
		}
		if err := k.productionConfig().Validate(); err != nil {
			return err
		}
		if env == Test {
			return errors.New("test composes the local keyfile wrapper")
		}
	default:
		return errors.New("secrets: key provider must be selected with adapter keyfile or kms")
	}
	return nil
}

// loadSecretsConfig layers environment settings over the rendered document.
// JSON maps replace entire maps; no retained-key map is implicitly merged.
func loadSecretsConfig(getenv func(string) string, dir string) (SecretsConfig, error) {
	var c SecretsConfig
	if raw := getenv("KODAMA_SECRETS_CONFIG"); raw != "" {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return c, errors.New("invalid KODAMA_SECRETS_CONFIG JSON")
		}
		if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
			return c, errors.New("KODAMA_SECRETS_CONFIG must contain one JSON object")
		}
	}
	if getenv("KODAMA_KMS_PROVIDER") != "" || getenv("KODAMA_KMS_KEYFILE_DEVELOPMENT") != "" {
		return c, errors.New("KODAMA_KMS_* has been replaced by KODAMA_SECRETS_ADAPTER and KODAMA_SECRETS_CONFIG")
	}
	if a := getenv("KODAMA_SECRETS_ADAPTER"); a != "" {
		c.Adapter = a
	}
	if c.Adapter == "keyfile" && c.Keyfile == nil {
		c.Keyfile = &KeyfileConfig{Current: "v1", Files: map[string]string{"v1": filepath.Join(dir, "master-key")}}
	}
	return c, nil
}

type kmsClientFactory func(context.Context, CloudKMSConfig) (production.Clients, func(), error)

func buildKMSClients(ctx context.Context, c CloudKMSConfig) (production.Clients, func(), error) {
	clients, closeFn := production.Clients{}, func() {}
	switch c.Provider {
	case "aws":
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.AWS.Region))
		if err != nil {
			return clients, closeFn, errors.New("secrets: AWS credential configuration failed")
		}
		clients.AWS = awskms.NewFromConfig(cfg)
	case "gcp":
		client, err := gcpkms.NewKeyManagementClient(ctx)
		if err != nil {
			return clients, closeFn, errors.New("secrets: GCP credential configuration failed")
		}
		clients.GCP, closeFn = client, func() { _ = client.Close() }
	case "scw":
		b, err := os.ReadFile(c.SCW.CredentialsFile)
		if err != nil {
			return clients, closeFn, errors.New("secrets: scw credential file cannot be read")
		}
		clients.ScalewayToken = strings.TrimSpace(string(b))
		clear(b)
		if clients.ScalewayToken == "" {
			return clients, closeFn, errors.New("secrets: scw credential file is empty")
		}
	}
	return clients, closeFn, nil
}

func buildSecretsWrapper(ctx context.Context, env Env, c SecretsConfig, clients kmsClientFactory) (secrets.KeyWrapper, func(), error) {
	noop := func() {}
	if env == Development {
		return nil, noop, fmt.Errorf("%w: development injects its ephemeral wrapper", errNotComposed)
	}
	if err := c.Validate(env); err != nil {
		return nil, noop, err
	}
	if c.Adapter == "keyfile" {
		p, err := keyfile.New(keyfile.Config{Development: true, Current: c.Keyfile.Current, Files: c.Keyfile.Files})
		return p, noop, err
	}
	k := *c.KMS
	timeout := k.StartupTimeout
	if timeout == 0 {
		timeout = production.DefaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cloud, cleanup, err := clients(ctx, k)
	if cleanup == nil {
		cleanup = noop
	}
	closeOnce := sync.OnceFunc(cleanup)
	if err != nil {
		closeOnce()
		return nil, noop, err
	}
	p, err := production.New(ctx, k.productionConfig(), cloud)
	if err != nil {
		closeOnce()
		return nil, noop, err
	}
	return p, closeOnce, nil
}
