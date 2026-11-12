package kube

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
)

const (
	// DefaultServiceAccountTokenFile is the projected service-account token
	// path a pod mounts when no explicit token file is configured.
	DefaultServiceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// DefaultServiceAccountCAFile is the service-account CA bundle path a pod
	// mounts when no explicit CA file is configured.
	DefaultServiceAccountCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// defaultTokenFile and defaultCAFile are where an explicitly selected
// in-cluster configuration reads the conventional service-account credential
// and CA bundle when its file fields are empty. They are variables so a test
// can substitute isolated temporary files; production never changes them.
var (
	defaultTokenFile = DefaultServiceAccountTokenFile
	defaultCAFile    = DefaultServiceAccountCAFile
)

// InClusterConfig selects the in-cluster service-account authentication. It is
// only used when ClusterConfig.InCluster is set explicitly: a kubeconfig alone
// never falls back to the conventional service-account paths.
type InClusterConfig struct {
	// Server is the HTTPS Kubernetes API endpoint. Required.
	Server string
	// TokenFile is the projected service-account token path. It is re-read for
	// every request, so a rotated token takes effect immediately. Empty uses
	// DefaultServiceAccountTokenFile.
	TokenFile string
	// CAFile is the service-account CA bundle path. It is required and must
	// contain at least one usable certificate. Empty uses
	// DefaultServiceAccountCAFile.
	CAFile string
	// InsecureSkipTLSVerify is accepted only as false. An in-cluster connection
	// is never unverified.
	InsecureSkipTLSVerify bool
}

// newInCluster validates one explicitly selected in-cluster configuration and
// builds a client. It makes no network call: a missing server, an unverified
// TLS request, an unusable CA or an absent token is refused before any request.
func newInCluster(config ClusterConfig) (*Cluster, error) {
	in := config.InCluster
	if in == nil {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver in-cluster configuration is missing")
	}
	if in.InsecureSkipTLSVerify {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver refuses an unverified TLS connection")
	}
	baseURL, err := inClusterBaseURL(in.Server)
	if err != nil {
		return nil, err
	}

	tokenFile := strings.TrimSpace(in.TokenFile)
	if tokenFile == "" {
		tokenFile = defaultTokenFile
	}
	caFile := strings.TrimSpace(in.CAFile)
	if caFile == "" {
		caFile = defaultCAFile
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver service-account CA file is unreadable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver service-account CA is not a usable certificate")
	}
	tokenData, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver service-account token file is unreadable")
	}
	if strings.TrimSpace(string(tokenData)) == "" {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver service-account token file is empty")
	}

	maxResponse, timeout, err := resolveLimits(config)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A bearer token is never forwarded to an environment-configured proxy.
	transport.Proxy = nil
	transport.TLSClientConfig = tlsConfig
	client := &http.Client{
		Transport: transport,
		// Refuse every redirect: a token must never be forwarded to another
		// endpoint, and a redirect is never a successful API answer.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Cluster{
		client:         client,
		baseURL:        baseURL,
		maxResponse:    maxResponse,
		requestTimeout: timeout,
		tokenFile:      tokenFile,
	}, nil
}

// inClusterBaseURL validates and normalizes one in-cluster API server URL. Only
// https is accepted: an in-cluster connection is never plaintext.
func inClusterBaseURL(server string) (string, error) {
	trimmed := strings.TrimSpace(server)
	if trimmed == "" {
		return "", fault.New(fault.InvalidInput, "kubernetes driver in-cluster configuration has no server")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fault.New(fault.InvalidInput, "kubernetes driver in-cluster server is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return "", fault.New(fault.InvalidInput, "kubernetes driver in-cluster server must be an https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fault.New(fault.InvalidInput, "kubernetes driver in-cluster server carries unsupported URL components")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}
