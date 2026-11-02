package devenv

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProbeEnv is the workload endpoint inventory. host names a service as
// workloads resolve it; nil selects the cluster stack's Pod URLs.
func ProbeEnv(scope string, host func(string) string) map[string]string {
	if host == nil {
		host = func(service string) string {
			return service + "." + DepsNamespace + ".svc.cluster.local"
		}
	}
	return map[string]string{
		"KODAMA_TEST_SCOPE":      scope,
		"KODAMA_SERVER_URL":      "http://" + host("kodama") + ":8080",
		"KODAMA_NATS_URL":        "nats://" + host("nats") + ":4222",
		"KODAMA_S3_ENDPOINT":     "http://" + host("localstack") + ":4566",
		"KODAMA_S3_BUCKET":       "kodama-" + scope,
		"KODAMA_S3_PREFIX":       scope + "/",
		"KODAMA_GIT_URL":         "https://" + host("git") + ":8443/fixture.git",
		"KODAMA_OIDC_ISSUER":     "http://" + host("oauthsonas") + ":8181",
		"KODAMA_OIDC_AUDIENCE":   "https://kodama.example.test/api",
		"KODAMA_REGISTRY_URL":    "http://" + host("registry") + ":5000",
		"KODAMA_REGISTRY_SOURCE": scope + "/scaffold",
	}
}

// HostEnv is the private inventory for tests running on the host. It never
// carries a Pod URL.
func (e *Environment) HostEnv() (map[string]string, error) {
	port := func(service string) (string, error) {
		number, ok := ServicePorts[service]
		if !ok {
			return "", fmt.Errorf("unknown service %q", service)
		}
		container, err := e.Inspect(e.Container(service))
		if err != nil {
			return "", err
		}
		if container == nil {
			return "", fmt.Errorf("container %s is missing", e.Container(service))
		}
		bindings := container.NetworkSettings.Ports[fmt.Sprintf("%d/tcp", number)]
		if len(bindings) == 0 {
			return "", fmt.Errorf("container %s publishes no %s port", e.Container(service), service)
		}
		return bindings[0].HostPort, nil
	}
	password, err := readTrimmed(filepath.Join(e.Dir, "fixtures", "postgres-password"))
	if err != nil {
		return nil, err
	}
	token, err := readTrimmed(filepath.Join(e.Dir, "fixtures", "nats-token"))
	if err != nil {
		return nil, err
	}
	ports := map[string]string{}
	for _, service := range []string{"postgres", "nats", "localstack", "registry"} {
		if ports[service], err = port(service); err != nil {
			return nil, err
		}
	}
	values := map[string]string{
		"DEV_PROVIDER":                   e.Provider,
		"DEV_SCOPE":                      e.Scope,
		"DEV_STACK":                      e.Stack,
		"DEV_GIT":                        e.GitMode(),
		"KODAMA_TEST_POSTGRES_ADMIN_URL": fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%s/postgres?sslmode=disable", url.QueryEscape(password), ports["postgres"]),
		"KODAMA_NATS_URL":                fmt.Sprintf("nats://%s@127.0.0.1:%s", url.QueryEscape(token), ports["nats"]),
		"KODAMA_S3_ENDPOINT":             "http://127.0.0.1:" + ports["localstack"],
		"KODAMA_S3_BUCKET":               "kodama-" + e.Scope,
		"KODAMA_S3_PREFIX":               e.Scope + "/",
		"KODAMA_SECRETS_DIR":             filepath.Join(e.Dir, "fixtures"),
		"KODAMA_REGISTRY_URL":            "http://127.0.0.1:" + ports["registry"],
	}
	if e.Provides("kubeconfig") {
		values["KODAMA_TEST_KUBECONFIG"] = filepath.Join(e.Dir, "kubeconfig")
		values["KUBECTL"] = e.Kubectl
	} else {
		values["KODAMA_TEST_PODMAN_NETWORK"] = e.State.Network
		values["KODAMA_TEST_SERVER_CALLBACK_HOST"] = CallbackHosts[e.Provider]
	}
	if e.GitMode() == GitNative {
		e.nativeHostEnv(values)
	}
	return values, nil
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// ReachableHostPort reports whether a TCP endpoint accepts a connection.
func ReachableHostPort(host, port string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// reachableURL reports whether an HTTP endpoint answers.
func reachableURL(rawURL string, timeout time.Duration) bool {
	client := http.Client{Timeout: timeout}
	response, err := client.Get(rawURL)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return true
}
