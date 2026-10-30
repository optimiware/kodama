package devenv

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	gitjwt "github.com/optimiware/kodama/internal/gitauth/jwt"
	"github.com/optimiware/kodama/internal/kernel/serve"
)

// Native Git hosting of the services stack (DEV_GIT=native).
//
// The scope holds the control plane's two signing keys and the control token in a
// server-only directory; the server under test reads them through KODAMA_GIT_FORWARDING_KEY_FILE,
// KODAMA_GIT_IDENTITY_KEY_FILE and KODAMA_GIT_CONTROL_TOKEN (host-env.json) and
// reaches a tenant's worker through KODAMA_GIT_WORKER_URL, a template naming the
// worker's container by tenant ID (endpoints.json). A worker receives only what
// it needs and is entitled to: the forwarding public key, the control token and
// an identity token bound to its tenant. It never sees a signing key.
//
// The signing keys and the control token live in <scope>/git-server, readable by
// the host user only; they are not fixtures: the fixtures directory is mounted
// into PostgreSQL, NATS and oauthsonas. A worker's own files live in
// <scope>/git-workers/<tenant>, world-readable because the worker runs as another
// UID, and hold nothing but what that worker is entitled to.

const (
	gitForwardingKey    = "git-forwarding-key.pem"
	gitIdentityKey      = "git-identity-key.pem"
	gitForwardingPublic = "git-forwarding-public.pem"
	gitControlToken     = "git-control-token"
	gitServerDir        = "git-server"
	gitWorkersDir       = "git-workers"
	// gitIdentityLifetime keeps a worker identity inside the contract's one hour.
	gitIdentityLifetime = 55 * time.Minute
)

// gitServerPath is the host-only directory of the control plane's Git secrets.
func (e *Environment) gitServerPath() string { return filepath.Join(e.Dir, gitServerDir) }

// nativeGitFixtures writes, once per scope, the control plane's signing keys and
// control token, and the forwarding public key a worker verifies with, into the
// server-only directory. Existing files are kept so a restart of the stack keeps
// its identities.
func (e *Environment) nativeGitFixtures() error {
	directory := e.gitServerPath()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	for _, name := range []string{gitForwardingKey, gitIdentityKey} {
		path := filepath.Join(directory, name)
		if exists(path) {
			continue
		}
		key, err := gitjwt.GenerateKey()
		if err != nil {
			return err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return err
		}
	}
	if !exists(filepath.Join(directory, gitControlToken)) {
		token, err := tokenHex(32)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(directory, gitControlToken), []byte(token), 0o600); err != nil {
			return err
		}
	}
	signer, err := serve.NewGitSigner("test", filepath.Join(directory, gitForwardingKey), filepath.Join(directory, gitIdentityKey))
	if err != nil {
		return err
	}
	public, err := gitjwt.MarshalPublicKeyPEM(signer.ForwardingPublicKeys().Current)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, gitForwardingPublic), public, 0o600)
}

// GitWorkerContainer is the container name of a tenant's native worker, which is
// also the name a workload on the scoped network resolves it by.
func (e *Environment) GitWorkerContainer(tenant string) string {
	return e.State.Cluster + "-git-" + tenant
}

// GitWorkerURLTemplate is KODAMA_GIT_WORKER_URL for a server that runs on the
// scoped network: {tenantId} is replaced by the tenant's identifier.
func (e *Environment) GitWorkerURLTemplate() string {
	return fmt.Sprintf("http://%s-git-{tenantId}:%d", e.State.Cluster, GitWorkerPort)
}

// gitWorkerProject is the Compose project of one tenant's worker.
func (e *Environment) gitWorkerProject(tenant string) string {
	return e.State.Cluster + "-git-" + tenant
}

func (e *Environment) gitWorkerDir(tenant string) string {
	return filepath.Join(e.Dir, gitWorkersDir, tenant)
}

// gitWorkerVariables are what the worker's Compose service interpolates.
func (e *Environment) gitWorkerVariables(tenant, controlURL string) []string {
	return []string{
		"KODAMA_GIT_TENANT=" + tenant,
		"KODAMA_GIT_WORKER_DIR=" + e.gitWorkerDir(tenant),
		"KODAMA_GIT_CONTROL_URL=" + controlURL,
	}
}

// DefaultGitControlURL is where a worker container reaches a server running on
// the host: the provider's name for the host and the server's default port.
func (e *Environment) DefaultGitControlURL() string {
	return "http://" + CallbackHosts[e.Provider] + ":8080"
}

func validateControlURL(raw string) error {
	address, err := url.Parse(raw)
	if err != nil || (address.Scheme != "http" && address.Scheme != "https") || address.Hostname() == "" ||
		address.User != nil || (address.Path != "" && address.Path != "/") || address.RawQuery != "" || address.Fragment != "" {
		return fmt.Errorf("the control URL is the server's base URL, http(s)://host[:port], not %q", raw)
	}
	return nil
}

// GitWorker starts (or refreshes) the native Git worker of one tenant: a
// `kodama git serve` container with its own data volume, in its own Compose
// project, on the scoped network. Running it again is safe: it renews the
// worker's identity token, which is valid for under an hour and re-read by the
// worker at every call, and leaves a running container and its data alone.
func (e *Environment) GitWorker(tenant, controlURL string) error {
	if err := e.SelectStack(); err != nil {
		return err
	}
	if err := e.SelectGitHosting(); err != nil {
		return err
	}
	if e.State == nil {
		return fmt.Errorf("run make dev-up DEV_STACK=%s DEV_GIT=%s for this scope first", StackServices, GitNative)
	}
	if e.GitMode() != GitNative {
		return fmt.Errorf("scope %s hosts Git with the %s fixture; create a scope with DEV_GIT=%s to run native workers", e.Scope, e.GitMode(), GitNative)
	}
	if err := ValidateGitTenant(tenant); err != nil {
		return err
	}
	if controlURL == "" {
		controlURL = e.DefaultGitControlURL()
	}
	if err := validateControlURL(controlURL); err != nil {
		return err
	}
	if err := e.Prepare(); err != nil {
		return err
	}
	secrets := e.gitServerPath()
	signer, err := serve.NewGitSigner("test", filepath.Join(secrets, gitForwardingKey), filepath.Join(secrets, gitIdentityKey))
	if err != nil {
		return fmt.Errorf("the scope has no Git signing keys; run make dev-up DEV_GIT=%s again: %w", GitNative, err)
	}
	now := time.Now().UTC()
	identity, err := signer.SignWorkerIdentity(context.Background(), gitv1.WorkerIdentity{
		Audience: gitv1.ControlAudience, TenantID: tenant, WorkerInstance: "devctl-" + tenant,
		IssuedAt:  gitv1.Timestamp(now.Format(time.RFC3339Nano)),
		ExpiresAt: gitv1.Timestamp(now.Add(gitIdentityLifetime).Format(time.RFC3339Nano)),
	})
	if err != nil {
		return err
	}
	directory := e.gitWorkerDir(tenant)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(directory), 0o755); err != nil {
		return err
	}
	for name, from := range map[string]string{"control-token": gitControlToken, "forwarding-public.pem": gitForwardingPublic} {
		data, err := os.ReadFile(filepath.Join(secrets, from))
		if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(directory, name), data); err != nil {
			return err
		}
	}
	if err := writeAtomic(filepath.Join(directory, "identity-token"), []byte(identity)); err != nil {
		return err
	}
	// The journal names the tenant before its container exists, so teardown finds it.
	if !containsString(e.State.GitTenants, tenant) {
		e.State.GitTenants = append(e.State.GitTenants, tenant)
		sort.Strings(e.State.GitTenants)
		if err := e.Save(); err != nil {
			return err
		}
	}
	if err := e.composeProject(e.gitWorkerProject(tenant), []string{profileGitNative}, e.gitWorkerVariables(tenant, controlURL), "up", "-d", "git-worker"); err != nil {
		return err
	}
	if err := e.writeGitInventory(controlURL); err != nil {
		return err
	}
	fmt.Printf("Git worker of tenant %s: %s (workload URL %s)\n", tenant, e.GitWorkerContainer(tenant),
		strings.ReplaceAll(e.GitWorkerURLTemplate(), "{tenantId}", tenant))
	return nil
}

// writeGitInventory records the tenants' workers and how to reach them.
func (e *Environment) writeGitInventory(controlURL string) error {
	type worker struct {
		Container   string `json:"container"`
		WorkloadURL string `json:"workloadUrl"`
		HostURL     string `json:"hostUrl,omitempty"`
	}
	workers := map[string]worker{}
	for _, tenant := range e.State.GitTenants {
		entry := worker{Container: e.GitWorkerContainer(tenant), WorkloadURL: strings.ReplaceAll(e.GitWorkerURLTemplate(), "{tenantId}", tenant)}
		if container, err := e.Inspect(entry.Container); err == nil && container != nil {
			if bindings := container.NetworkSettings.Ports[fmt.Sprintf("%d/tcp", GitWorkerPort)]; len(bindings) > 0 {
				entry.HostURL = "http://127.0.0.1:" + bindings[0].HostPort
			}
		}
		workers[tenant] = entry
	}
	return WriteJSON(filepath.Join(e.Dir, "git-workers.json"), map[string]any{
		"hosting":             GitNative,
		"workloadUrlTemplate": e.GitWorkerURLTemplate(),
		"controlUrl":          controlURL,
		"workers":             workers,
	})
}

// removeGitWorkers removes every tenant's worker project with its volume. A
// project that is already gone is not an error: teardown is repeatable.
func (e *Environment) removeGitWorkers() {
	for _, tenant := range e.State.GitTenants {
		_ = e.composeProject(e.gitWorkerProject(tenant), []string{profileGitNative}, e.gitWorkerVariables(tenant, e.DefaultGitControlURL()), "down", "-v")
	}
}

// nativeHostEnv adds what the server under test needs to host Git natively.
func (e *Environment) nativeHostEnv(values map[string]string) {
	directory := e.gitServerPath()
	values["KODAMA_GIT_FORWARDING_KEY_FILE"] = filepath.Join(directory, gitForwardingKey)
	values["KODAMA_GIT_IDENTITY_KEY_FILE"] = filepath.Join(directory, gitIdentityKey)
	if token, err := readTrimmed(filepath.Join(directory, gitControlToken)); err == nil {
		values["KODAMA_GIT_CONTROL_TOKEN"] = token
	}
}

func writeAtomic(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
