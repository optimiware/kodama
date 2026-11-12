package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

const (
	// DefaultMaxResponseBytes bounds one non-log API response.
	DefaultMaxResponseBytes int64 = 8 << 20
	// DefaultRequestTimeout bounds one API call. A caller context may impose a
	// shorter deadline.
	DefaultRequestTimeout = 30 * time.Second

	// workloadContainerName is the pinned name of the single Job container.
	workloadContainerName = "workload"

	// jsonContentType is the media type of every JSON request body.
	jsonContentType = "application/json"
	// mergePatchContentType is the media type of the metadata-only claim patch.
	mergePatchContentType = "application/merge-patch+json"

	// userAgent identifies this driver without carrying any identity.
	userAgent = "kodama-kubernetes-driver/1"
	// errorDrainBytes bounds how much of an error response is drained so a
	// keep-alive connection can be reused. The bytes are never inspected.
	errorDrainBytes = 4 << 10
)

// ErrAmbiguous reports an external effect whose outcome cannot be classified:
// a transport failure, a timeout, a cancellation, a refused redirect, an
// untrusted certificate or an oversized response. The adapter above this seam
// reports an uncertain effect, never a fabricated result.
var ErrAmbiguous = errors.New("kubernetes driver: ambiguous external effect")

// ClusterConfig binds one client to one Kubernetes API server.
type ClusterConfig struct {
	// Kubeconfig is the raw JSON document produced by
	// `kubectl config view --raw --minify -o json`. It is mutually exclusive
	// with InCluster.
	Kubeconfig []byte
	// InCluster explicitly selects in-cluster service-account authentication.
	// It is mutually exclusive with Kubeconfig; a kubeconfig alone never falls
	// back to the conventional service-account paths.
	InCluster *InClusterConfig
	// Context selects one kubeconfig context. Empty uses current-context.
	Context string
	// MaxResponseBytes bounds one non-log API response. Zero uses
	// DefaultMaxResponseBytes; a negative value is refused.
	MaxResponseBytes int64
	// Timeout bounds one API call. Zero uses DefaultRequestTimeout; a negative
	// value disables the internal timeout.
	Timeout time.Duration
}

// Cluster is the net/http Kubernetes client. It implements
// runtimekube.Cluster and contacts the API server only when a seam operation
// asks for an external effect.
type Cluster struct {
	client         *http.Client
	baseURL        string
	maxResponse    int64
	requestTimeout time.Duration
	token          string
	tokenFile      string
}

var _ runtimekube.Cluster = (*Cluster)(nil)

// kubeconfigDocument is the subset of the exported kubeconfig the driver reads.
type kubeconfigDocument struct {
	CurrentContext string `json:"current-context"`
	Clusters       []struct {
		Name    string            `json:"name"`
		Cluster kubeconfigCluster `json:"cluster"`
	} `json:"clusters"`
	Contexts []struct {
		Name    string            `json:"name"`
		Context kubeconfigContext `json:"context"`
	} `json:"contexts"`
	Users []struct {
		Name string         `json:"name"`
		User kubeconfigUser `json:"user"`
	} `json:"users"`
}

// kubeconfigCluster is the server and trust material of one cluster entry.
type kubeconfigCluster struct {
	Server                   string `json:"server"`
	CertificateAuthorityData string `json:"certificate-authority-data"`
	CertificateAuthority     string `json:"certificate-authority"`
	InsecureSkipTLSVerify    bool   `json:"insecure-skip-tls-verify"`
}

// kubeconfigContext selects one cluster and one user.
type kubeconfigContext struct {
	Cluster string `json:"cluster"`
	User    string `json:"user"`
}

// kubeconfigUser is the credential material of one user entry. An exec plugin
// is deliberately not executed: a kubeconfig the driver cannot authenticate
// with is refused instead of silently used anonymously.
type kubeconfigUser struct {
	Token                 string `json:"token"`
	TokenFile             string `json:"tokenFile"`
	ClientCertificateData string `json:"client-certificate-data"`
	ClientKeyData         string `json:"client-key-data"`
	ClientCertificate     string `json:"client-certificate"`
	ClientKey             string `json:"client-key"`
	Username              string `json:"username"`
	Password              string `json:"password"`
}

// NewCluster validates one configuration and builds a client. It makes no
// network call: an unusable kubeconfig or credential set is refused before any
// request.
func NewCluster(config ClusterConfig) (*Cluster, error) {
	if config.InCluster != nil {
		if len(bytes.TrimSpace(config.Kubeconfig)) != 0 {
			return nil, fault.New(fault.InvalidInput,
				"kubernetes driver refuses mixed kubeconfig and in-cluster configuration")
		}
		return newInCluster(config)
	}
	if len(bytes.TrimSpace(config.Kubeconfig)) == 0 {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver requires a kubeconfig document")
	}
	var document kubeconfigDocument
	if err := json.Unmarshal(config.Kubeconfig, &document); err != nil {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver kubeconfig is not valid JSON")
	}

	contextName := strings.TrimSpace(config.Context)
	if contextName == "" {
		contextName = strings.TrimSpace(document.CurrentContext)
	}
	if contextName == "" {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver kubeconfig has no selected context")
	}
	var selected *kubeconfigContext
	for index := range document.Contexts {
		if document.Contexts[index].Name == contextName {
			selected = &document.Contexts[index].Context
			break
		}
	}
	if selected == nil {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver kubeconfig does not define the selected context")
	}
	clusterName := strings.TrimSpace(selected.Cluster)
	if clusterName == "" {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver kubeconfig context names no cluster")
	}
	var clusterEntry *kubeconfigCluster
	for index := range document.Clusters {
		if document.Clusters[index].Name == clusterName {
			clusterEntry = &document.Clusters[index].Cluster
			break
		}
	}
	if clusterEntry == nil {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver kubeconfig does not define the selected cluster")
	}

	baseURL, err := clusterBaseURL(clusterEntry.Server)
	if err != nil {
		return nil, err
	}
	if clusterEntry.InsecureSkipTLSVerify {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver refuses an unverified TLS connection")
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	roots, err := certificatePool(clusterEntry)
	if err != nil {
		return nil, err
	}
	if roots != nil {
		tlsConfig.RootCAs = roots
	}

	var user kubeconfigUser
	if name := strings.TrimSpace(selected.User); name != "" {
		found := false
		for index := range document.Users {
			if document.Users[index].Name == name {
				user = document.Users[index].User
				found = true
				break
			}
		}
		if !found {
			return nil, fault.New(fault.InvalidInput, "kubernetes driver kubeconfig does not define the selected user")
		}
	}
	certificates, err := clientCertificates(user)
	if err != nil {
		return nil, err
	}
	if len(certificates) > 0 {
		tlsConfig.Certificates = certificates
	}

	token := strings.TrimSpace(user.Token)
	tokenFile := strings.TrimSpace(user.TokenFile)
	if token == "" && tokenFile == "" && len(certificates) == 0 {
		return nil, fault.New(fault.InvalidInput,
			"kubernetes driver requires a bearer token, a token file or a client certificate")
	}
	if user.Username != "" || user.Password != "" {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver does not support basic authentication")
	}

	maxResponse, timeout, err := resolveLimits(config)
	if err != nil {
		return nil, err
	}

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
		token:          token,
		tokenFile:      tokenFile,
	}, nil
}

// resolveLimits validates the shared response bound and request timeout. Zero
// selects the default, a negative bound is refused and a negative timeout
// disables the internal bound.
func resolveLimits(config ClusterConfig) (int64, time.Duration, error) {
	maxResponse := config.MaxResponseBytes
	switch {
	case maxResponse < 0:
		return 0, 0, fault.New(fault.InvalidInput, "kubernetes driver response bound is negative")
	case maxResponse == 0:
		maxResponse = DefaultMaxResponseBytes
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}
	return maxResponse, timeout, nil
}

// Close releases the transport this driver owns. It closes idle connections,
// leaves a call still in flight alone and is safe to call more than once.
func (c *Cluster) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	if transport, ok := c.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	return nil
}

// clusterBaseURL validates and normalizes one cluster server URL.
func clusterBaseURL(server string) (string, error) {
	trimmed := strings.TrimSpace(server)
	if trimmed == "" {
		return "", fault.New(fault.InvalidInput, "kubernetes driver kubeconfig cluster has no server")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fault.New(fault.InvalidInput, "kubernetes driver cluster server is not a valid URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fault.New(fault.InvalidInput, "kubernetes driver cluster server must be an http or https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fault.New(fault.InvalidInput, "kubernetes driver cluster server carries unsupported URL components")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// certificatePool builds the trust pool from inline CA data or a CA file. Both
// are refused when unusable; neither falls back to an insecure connection.
func certificatePool(cluster *kubeconfigCluster) (*x509.CertPool, error) {
	var pemData []byte
	switch {
	case strings.TrimSpace(cluster.CertificateAuthorityData) != "":
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cluster.CertificateAuthorityData))
		if err != nil {
			return nil, fault.New(fault.InvalidInput, "kubernetes driver certificate-authority-data is not valid base64")
		}
		pemData = decoded
	case strings.TrimSpace(cluster.CertificateAuthority) != "":
		read, err := os.ReadFile(strings.TrimSpace(cluster.CertificateAuthority))
		if err != nil {
			return nil, fault.New(fault.InvalidInput, "kubernetes driver certificate-authority file is unreadable")
		}
		pemData = read
	default:
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fault.New(fault.InvalidInput, "kubernetes driver certificate authority is not a usable certificate")
	}
	return pool, nil
}

// clientCertificates builds the mutual-TLS certificate from inline data or a
// certificate/key file pair. A half-configured pair is refused.
func clientCertificates(user kubeconfigUser) ([]tls.Certificate, error) {
	certData := strings.TrimSpace(user.ClientCertificateData)
	keyData := strings.TrimSpace(user.ClientKeyData)
	certFile := strings.TrimSpace(user.ClientCertificate)
	keyFile := strings.TrimSpace(user.ClientKey)
	if certData != "" || keyData != "" {
		if certData == "" || keyData == "" {
			return nil, fault.New(fault.InvalidInput, "kubernetes driver requires both client certificate and key data")
		}
		certPEM, err := base64.StdEncoding.DecodeString(certData)
		if err != nil {
			return nil, fault.New(fault.InvalidInput, "kubernetes driver client-certificate-data is not valid base64")
		}
		keyPEM, err := base64.StdEncoding.DecodeString(keyData)
		if err != nil {
			return nil, fault.New(fault.InvalidInput, "kubernetes driver client-key-data is not valid base64")
		}
		certificate, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fault.New(fault.InvalidInput, "kubernetes driver client certificate and key do not form a pair")
		}
		return []tls.Certificate{certificate}, nil
	}
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, fault.New(fault.InvalidInput, "kubernetes driver requires both client certificate and key files")
		}
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fault.New(fault.InvalidInput, "kubernetes driver client certificate files are unreadable or mismatched")
		}
		return []tls.Certificate{certificate}, nil
	}
	return nil, nil
}

// endpoint renders one API path with an optional query string.
func (c *Cluster) endpoint(path string, query url.Values) string {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return target
}

// authorize attaches the current bearer token. A configured token file is read
// for every request, so a rotated credential is used immediately and a
// rotated-away file is a dependency failure before any request is sent.
func (c *Cluster) authorize(request *http.Request) error {
	request.Header.Set("Accept", jsonContentType)
	request.Header.Set("User-Agent", userAgent)
	if c.tokenFile != "" {
		data, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "kubernetes driver bearer token file is unreadable", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return fault.New(fault.DependencyUnavailable, "kubernetes driver bearer token file is empty")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	return nil
}

// callContext bounds one call with the configured timeout. A negative timeout
// disables the internal bound and leaves only the caller context.
func (c *Cluster) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.requestTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.requestTimeout)
}

// do builds and sends one request. A build or credential failure is returned as
// the already-classified fault; a transport failure is ErrAmbiguous.
func (c *Cluster) do(
	requestCtx context.Context,
	method, path string,
	query url.Values,
	payload []byte,
	contentType string,
) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(requestCtx, method, c.endpoint(path, query), body)
	if err != nil {
		return nil, fault.New(fault.InternalFailure, "kubernetes driver request could not be built")
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if err := c.authorize(request); err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, ambiguousRequest(method+" "+path, requestCtx, err)
	}
	return response, nil
}

// exchange sends one request and returns the bounded response body and status.
// A transport failure, timeout, cancellation, refused redirect or oversized
// response is ErrAmbiguous; the caller classifies the HTTP status.
func (c *Cluster) exchange(
	ctx context.Context,
	method, path string,
	query url.Values,
	payload []byte,
	contentType string,
) ([]byte, int, error) {
	requestCtx, cancel := c.callContext(ctx)
	response, err := c.do(requestCtx, method, path, query, payload, contentType)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	defer cancel()
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Drain a bounded prefix so the connection can be reused. The bytes
		// are never inspected and never appear in an error.
		_, _ = io.CopyN(io.Discard, response.Body, errorDrainBytes)
		return nil, response.StatusCode, nil
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, c.maxResponse+1))
	if readErr != nil {
		return nil, response.StatusCode, ambiguousRequest(method+" "+path, requestCtx, readErr)
	}
	if int64(len(data)) > c.maxResponse {
		return nil, response.StatusCode, ambiguous(
			fmt.Sprintf("kubernetes API %s %s response exceeds %d bytes", method, path, c.maxResponse))
	}
	return data, response.StatusCode, nil
}

// statusError renders one non-secret HTTP failure. The response body is never
// included because a credential value could appear in it.
func statusError(operation string, status int, create bool) error {
	switch {
	case status == http.StatusNotFound:
		return fault.Wrap(fault.NotFound, "kubernetes "+operation+": object not found", runtimekube.ErrNotFound)
	case status == http.StatusConflict && create:
		return fmt.Errorf("kubernetes %s: object already exists: %w", operation, runtimekube.ErrAlreadyExists)
	case status == http.StatusConflict:
		return fault.Wrap(fault.VersionConflict, "kubernetes "+operation+": object conflict", runtimekube.ErrConflict)
	case status >= 400 && status < 500:
		return fault.New(fault.DependencyUnavailable,
			fmt.Sprintf("kubernetes %s rejected with status %d", operation, status))
	default:
		return ambiguous(fmt.Sprintf("kubernetes %s returned status %d", operation, status))
	}
}

// ambiguous builds an ErrAmbiguous fault. The cause stays reachable through
// errors.Is/errors.As but is never rendered, so a transport error or a
// credential value cannot leak through the public message.
func ambiguous(message string, causes ...error) error {
	cause := ErrAmbiguous
	parts := []error{ErrAmbiguous}
	for _, candidate := range causes {
		if candidate != nil {
			parts = append(parts, candidate)
		}
	}
	if len(parts) > 1 {
		cause = errors.Join(parts...)
	}
	return fault.Wrap(fault.UncertainExternalEffect, message, cause)
}

// ambiguousRequest classifies one failed exchange. The context cause is joined
// explicitly so a timeout or cancellation stays reachable even when the
// transport error only carries a boolean timeout flag.
func ambiguousRequest(operation string, requestCtx context.Context, cause error) error {
	causes := []error{cause}
	if ctxErr := requestCtx.Err(); ctxErr != nil {
		causes = append(causes, ctxErr)
	}
	return ambiguous("kubernetes API "+operation+" failed", causes...)
}

// undecodable reports a successful response the driver cannot interpret. The
// effect may have happened, so the outcome stays ambiguous.
func undecodable(operation string) error {
	return ambiguous("kubernetes " + operation + " returned an undecodable response")
}
