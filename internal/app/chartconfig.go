package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube"
)

// ConfigFileVariable names the settings document an installation renders for
// the server (the Helm chart's `config.yaml`). It supplies defaults for the
// KODAMA_* settings it covers; a variable that is set always wins, and the
// document never selects the environment, the assembly or any secret.
const ConfigFileVariable = "KODAMA_CONFIG_FILE"

// ChartConfigVersion is the only supported version of the document.
const ChartConfigVersion = 2

// ChartConfig is the settings document of deploy/charts/kodama
// (templates/configmap-server.yaml). It is read strictly: an unknown member is
// an error, so a chart and the server cannot drift silently. It carries
// endpoints, identifiers and the execution policy, never a credential: secrets
// are files in the runtime Secret (KODAMA_SECRETS_DIR).
type ChartConfig struct {
	Version        int           `yaml:"version"`
	Profile        string        `yaml:"profile"`
	Authority      string        `yaml:"authority"`
	InstallationID string        `yaml:"installationId"`
	Secrets        SecretsConfig `yaml:"secrets"`
	NATS           struct {
		URL string `yaml:"url"`
	} `yaml:"nats"`
	S3 struct {
		Endpoint    string `yaml:"endpoint"`
		Region      string `yaml:"region"`
		Bucket      string `yaml:"bucket"`
		Prefix      string `yaml:"prefix"`
		PathStyle   bool   `yaml:"pathStyle"`
		Credentials string `yaml:"credentials"`
	} `yaml:"s3"`
	OIDC struct {
		Issuer   string `yaml:"issuer"`
		Audience string `yaml:"audience"`
	} `yaml:"oidc"`
	TLS struct {
		CustomCA bool `yaml:"customCA"`
	} `yaml:"tls"`
	Runtime   ChartRuntime   `yaml:"runtime"`
	Execution ChartExecution `yaml:"execution"`
	// RunSharedSection is the optional run_shared opt-in (see ChartRunShared); the
	// chart renders it only when runShared.enabled is set.
	RunSharedSection *ChartRunShared `yaml:"runShared"`
}

// ChartRuntime is the runtime API as workloads reach it.
type ChartRuntime struct {
	// URL is the https origin; CAFile names, inside the secrets directory, the
	// PEM trust roots workloads use for it.
	URL               string        `yaml:"url"`
	CAFile            string        `yaml:"caFile"`
	SigningGeneration int64         `yaml:"signingGeneration"`
	Egress            []ChartEgress `yaml:"egress"`
}

// ChartEgress is one destination workloads may reach.
type ChartEgress struct {
	Name   string   `yaml:"name"`
	CIDR   string   `yaml:"cidr"`
	Except []string `yaml:"except"`
	Ports  []struct {
		Port     int32  `yaml:"port"`
		Protocol string `yaml:"protocol"`
	} `yaml:"ports"`
}

// ChartExecution is the execution policy the chart renders.
type ChartExecution struct {
	ServiceAccountName string `yaml:"serviceAccountName"`
	Tenants            []struct {
		Namespace string `yaml:"namespace"`
		TenantID  string `yaml:"tenantId"`
	} `yaml:"tenants"`
	// Image is the digest-pinned release image that provides the entrypoint
	// binary; InjectorCommand copies it into the tools volume.
	Image                   string          `yaml:"image"`
	ImagePullPolicy         string          `yaml:"imagePullPolicy"`
	InjectorCommand         []string        `yaml:"injectorCommand"`
	RunAsUser               int64           `yaml:"runAsUser"`
	RunAsGroup              int64           `yaml:"runAsGroup"`
	TerminationGraceSeconds int64           `yaml:"terminationGraceSeconds"`
	Workload                ChartResources  `yaml:"workload"`
	Injector                ChartResources  `yaml:"injector"`
	Volumes                 ChartVolumeSize `yaml:"volumes"`
}

// ChartResources are the requests and limits of one container, keyed by the
// Kubernetes resource names (cpu, memory, ephemeral-storage).
type ChartResources struct {
	Requests map[string]string `yaml:"requests"`
	Limits   map[string]string `yaml:"limits"`
}

// ChartVolumeSize are the sizes of the private Pod volumes.
type ChartVolumeSize struct {
	Workspace string `yaml:"workspaceSizeLimit"`
	Outputs   string `yaml:"outputsSizeLimit"`
	Scratch   string `yaml:"scratchSizeLimit"`
	Results   string `yaml:"resultsSizeLimit"`
	Tools     string `yaml:"toolsSizeLimit"`
	Secrets   string `yaml:"secretsSizeLimit"`
	Identity  string `yaml:"identitySizeLimit"`
}

// ParseChartConfig decodes the document strictly.
func ParseChartConfig(data []byte) (ChartConfig, error) {
	var c ChartConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return ChartConfig{}, fmt.Errorf("settings document: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ChartConfig{}, errors.New("settings document: more than one YAML document")
	}
	if c.Version != ChartConfigVersion {
		return ChartConfig{}, fmt.Errorf("settings document: version %d is not supported (want %d)", c.Version, ChartConfigVersion)
	}
	return c, nil
}

// Environment returns the KODAMA_* settings the document supplies. Hosted
// authority and workload-identity object store credentials remain unsupported.
// Secrets settings are validated after environment overrides in CoreConfig.
// secretsDir is where the runtime Secret is mounted.
func (c ChartConfig) Environment(secretsDir string) (map[string]string, error) {
	var errs []error
	if c.Authority != "standalone" {
		errs = append(errs, fmt.Errorf("authority %q is not served by the core assembly (standalone only)", c.Authority))
	}
	if c.S3.Credentials != "static" {
		errs = append(errs, fmt.Errorf("object store credentials %q are not supported by the core assembly: it reads the s3-access-key and s3-secret-key files", c.S3.Credentials))
	}
	env := map[string]string{
		"KODAMA_INSTALLATION_ID": c.InstallationID,
		"KODAMA_NATS_URL":        c.NATS.URL,
		"KODAMA_S3_ENDPOINT":     c.S3.Endpoint, "KODAMA_S3_REGION": c.S3.Region, "KODAMA_S3_BUCKET": c.S3.Bucket, "KODAMA_S3_PREFIX": c.S3.Prefix,
		"KODAMA_S3_PATH_STYLE": strconv.FormatBool(c.S3.PathStyle),
		"KODAMA_OIDC_ISSUER":   c.OIDC.Issuer, "KODAMA_OIDC_AUDIENCE": c.OIDC.Audience,
		"KODAMA_RUNTIME_URL": c.Runtime.URL,
	}
	if c.Runtime.SigningGeneration != 0 {
		env["KODAMA_RUNTIME_KEY_GENERATION"] = strconv.FormatInt(c.Runtime.SigningGeneration, 10)
	}
	encoded, err := json.Marshal(c.Secrets)
	if err != nil {
		errs = append(errs, err)
	}
	env["KODAMA_SECRETS_CONFIG"] = string(encoded)
	if c.TLS.CustomCA {
		env["KODAMA_GIT_CA_FILE"] = filepath.Join(secretsDir, "ca.crt")
	}
	return env, errors.Join(errs...)
}

// Policy builds and validates the Kubernetes execution policy of the document.
func (c ChartConfig) Policy(secretsDir string) (kube.PolicyConfig, error) {
	e := c.Execution
	if c.Runtime.CAFile == "" || c.Runtime.CAFile != filepath.Base(c.Runtime.CAFile) {
		return kube.PolicyConfig{}, errors.New("execution policy: the runtime CA must be a file of the secrets directory")
	}
	ca, err := os.ReadFile(filepath.Join(secretsDir, c.Runtime.CAFile))
	if err != nil {
		return kube.PolicyConfig{}, fmt.Errorf("execution policy: runtime CA: %w", err)
	}
	cfg := kube.PolicyConfig{
		Tenants:         map[domain.TenantID]kube.TenantTarget{},
		InjectorImage:   e.Image,
		InjectorCommand: e.InjectorCommand,
		ImagePullPolicy: e.ImagePullPolicy,
		RuntimeURL:      c.Runtime.URL,
		RuntimeCA:       ca,
		Workload:        e.Workload.bounds(),
		Injector:        e.Injector.bounds(),
		Volumes: kube.VolumeSizes{
			Workspace: e.Volumes.Workspace, Outputs: e.Volumes.Outputs, Scratch: e.Volumes.Scratch, Results: e.Volumes.Results,
			Tools: e.Volumes.Tools, Secrets: e.Volumes.Secrets, Identity: e.Volumes.Identity,
		},
		RunAsUser: e.RunAsUser, RunAsGroup: e.RunAsGroup,
		TerminationGrace: time.Duration(e.TerminationGraceSeconds) * time.Second,
	}
	if rs := c.RunSharedSection; rs != nil {
		cfg.RunShared = &kube.RunSharedIdentity{RunAsUser: rs.Identity.RunAsUser, RunAsGroup: rs.Identity.RunAsGroup, FSGroup: rs.Identity.FSGroup}
	}
	for _, t := range e.Tenants {
		id := domain.TenantID(t.TenantID)
		if _, dup := cfg.Tenants[id]; dup {
			return kube.PolicyConfig{}, fmt.Errorf("execution policy: tenant %q is listed twice", t.TenantID)
		}
		cfg.Tenants[id] = kube.TenantTarget{Namespace: t.Namespace, ServiceAccount: e.ServiceAccountName}
	}
	for _, r := range c.Runtime.Egress {
		// An egress rule has one protocol; the chart allows one per port, so a
		// rule with mixed protocols becomes one policy rule per protocol.
		byProto := map[string][]int32{}
		var order []string
		for _, p := range r.Ports {
			proto := p.Protocol
			if proto == "" {
				proto = "TCP"
			}
			if _, seen := byProto[proto]; !seen {
				order = append(order, proto)
			}
			byProto[proto] = append(byProto[proto], p.Port)
		}
		for _, proto := range order {
			cfg.RuntimeEgress = append(cfg.RuntimeEgress, kube.EgressRule{CIDR: r.CIDR, Protocol: proto, Ports: byProto[proto]})
		}
	}
	if _, err := kube.NewExecutionPolicy(cfg); err != nil {
		return kube.PolicyConfig{}, fmt.Errorf("execution policy: %w", err)
	}
	return cfg, nil
}

func (r ChartResources) bounds() kube.ResourceBounds {
	return kube.ResourceBounds{
		CPURequest: r.Requests["cpu"], CPULimit: r.Limits["cpu"],
		MemoryRequest: r.Requests["memory"], MemoryLimit: r.Limits["memory"],
		EphemeralRequest: r.Requests["ephemeral-storage"], EphemeralLimit: r.Limits["ephemeral-storage"],
	}
}

// configDocument is what the rendered settings document supplies beyond the
// environment: the Kubernetes execution policy and the run_shared opt-in.
type configDocument struct {
	Policy    *kube.PolicyConfig
	RunShared *RunSharedConfig
}

// withConfigFile layers the rendered settings document under the environment:
// a variable that is set wins. It returns the environment lookup to use, the
// execution policy of the document and any error.
func withConfigFile(getenv func(string) string, secretsDir string) (func(string) string, *kube.PolicyConfig, error) {
	lookup, doc, err := withConfigDocument(getenv, secretsDir)
	return lookup, doc.Policy, err
}

// withConfigDocument is withConfigFile with the whole document's contribution.
func withConfigDocument(getenv func(string) string, secretsDir string) (func(string) string, configDocument, error) {
	path := getenv(ConfigFileVariable)
	if path == "" {
		return getenv, configDocument{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return getenv, configDocument{}, fmt.Errorf("settings document: %w", err)
	}
	doc, err := ParseChartConfig(data)
	if err != nil {
		return getenv, configDocument{}, err
	}
	overlay, envErr := doc.Environment(secretsDir)
	policy, policyErr := doc.Policy(secretsDir)
	shared, sharedErr := doc.RunShared()
	var out configDocument
	if policyErr == nil {
		out.Policy = &policy
	}
	if sharedErr == nil {
		out.RunShared = shared
	}
	lookup := func(name string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return overlay[name]
	}
	return lookup, out, errors.Join(envErr, policyErr, sharedErr)
}

// inClusterKubeAPI is the Kubernetes API origin of a Pod, from the variables
// the kubelet sets, or "" outside a cluster.
func inClusterKubeAPI(getenv func(string) string) string {
	host, port := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return ""
	}
	return "https://" + joinHostPort(host, port)
}

func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}
