// Package realdelivery_test runs the five ordinary delivery scripts against real
// delivery dependencies: the tools image in a container runtime, an OCI registry,
// a Kubernetes cluster reached with namespace-scoped credentials, PostgreSQL, a
// development OIDC provider and the controlled ERP peer. The only fixture is the
// factory's runtime callback boundary (test/support/factoryfake), whose shapes
// are validated against the merged runtime and delivery contracts by the root
// acceptance drivers.
//
// The tests need a provisioned scope of the repository's development
// environment (make dev-up DEV_SCOPE=...) and are driven by the root drivers in
// templates/go-postgres/acceptance. Without INVENTORY_DEV_ENV they skip; a skip
// is not acceptance and the drivers reject it.
//
// Every resource carries the INVENTORY_REAL_PREFIX (default "t043") and a random
// suffix, and is removed again on cleanup.
package realdelivery_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hostName reaches services on the developer's machine from containers that use
// the VM's network namespace.
const hostName = "host.containers.internal"

type devEnv struct {
	dir          string
	scope        string
	cluster      string
	registryPort string
	kubectl      string
	podman       string
	adminKube    string
	pgPassword   string
	prefix       string
	hostIP       string // the address cluster nodes use to reach the developer's machine
}

var (
	devOnce sync.Once
	devVal  *devEnv
	devErr  error
)

// requireDev loads the provisioned development scope or skips.
func requireDev(t *testing.T) *devEnv {
	t.Helper()
	dir := os.Getenv("INVENTORY_DEV_ENV")
	if dir == "" {
		t.Skip("real delivery needs INVENTORY_DEV_ENV (dev/.state/environments/<scope> from make dev-up)")
	}
	devOnce.Do(func() { devVal, devErr = loadDev(dir) })
	if devErr != nil {
		t.Fatalf("the development environment is not usable: %v", devErr)
	}
	return devVal
}

func loadDev(dir string) (*devEnv, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("no scope state in %s: %w (run make dev-up)", dir, err)
	}
	var st struct {
		Scope    string `json:"scope"`
		Cluster  string `json:"cluster"`
		Port     int    `json:"port"`
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &st); err != nil || st.Cluster == "" || st.Port == 0 {
		return nil, fmt.Errorf("unreadable scope state: %v", err)
	}
	e := &devEnv{dir: dir, scope: st.Scope, cluster: st.Cluster, registryPort: fmt.Sprint(st.Port), prefix: getenv("INVENTORY_REAL_PREFIX", "t043"),
		adminKube: filepath.Join(dir, "kubeconfig")}
	e.podman = getenv("INVENTORY_CONTAINER_RUNTIME", "podman")
	if st.Provider != "" && os.Getenv("INVENTORY_CONTAINER_RUNTIME") == "" {
		e.podman = st.Provider
	}
	if p, err := exec.LookPath(e.podman); err == nil {
		e.podman = p
	} else {
		return nil, fmt.Errorf("container runtime %q not found", e.podman)
	}
	e.kubectl = os.Getenv("INVENTORY_KUBECTL")
	if e.kubectl == "" {
		matches, _ := filepath.Glob(filepath.Join(dir, "..", "..", ".tools", "kubectl-*"))
		if len(matches) > 0 {
			e.kubectl = matches[0]
		} else if p, err := exec.LookPath("kubectl"); err == nil {
			e.kubectl = p
		} else {
			return nil, fmt.Errorf("kubectl not found")
		}
	}
	pw, err := os.ReadFile(filepath.Join(dir, "fixtures", "postgres-password"))
	if err != nil {
		return nil, err
	}
	e.pgPassword = strings.TrimSpace(string(pw))
	if _, err := e.kube("get", "--raw", "/readyz"); err != nil {
		return nil, fmt.Errorf("the cluster is not reachable: %w", err)
	}
	out, err := e.run(nil, e.podman, "exec", e.cluster+"-control-plane", "getent", "hosts", hostName)
	if err != nil || len(strings.Fields(out)) == 0 {
		return nil, fmt.Errorf("cluster nodes cannot resolve %s: %v", hostName, err)
	}
	e.hostIP = strings.Fields(out)[0]
	return e, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// run executes a command and returns stdout (stderr is appended to the error).
func (e *devEnv) run(stdin []byte, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %v: %s", filepath.Base(name), strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// kube runs kubectl with the bootstrap administrator's kubeconfig. It is used only
// to provision and inspect; the scripts under test never receive it.
func (e *devEnv) kube(args ...string) (string, error) {
	return e.run(nil, e.kubectl, append([]string{"--kubeconfig", e.adminKube, "--request-timeout", "60s"}, args...)...)
}

func (e *devEnv) kubeAs(kubeconfig string, args ...string) (string, error) {
	return e.run(nil, e.kubectl, append([]string{"--kubeconfig", kubeconfig, "--request-timeout", "60s"}, args...)...)
}

func (e *devEnv) mustKube(t *testing.T, args ...string) string {
	t.Helper()
	out, err := e.kube(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// applyErr creates objects with the administrator's credential.
func (e *devEnv) applyErr(objs ...map[string]any) error {
	raw, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objs})
	_, err := e.run(raw, e.kubectl, "--kubeconfig", e.adminKube, "apply", "-f", "-")
	return err
}

func (e *devEnv) apply(t *testing.T, objs ...map[string]any) {
	t.Helper()
	if err := e.applyErr(objs...); err != nil {
		t.Fatal(err)
	}
}

// getJSON returns an object as a generic map using the given kubeconfig.
func (e *devEnv) getJSON(kubeconfig string, args ...string) (map[string]any, error) {
	out, err := e.kubeAs(kubeconfig, append(append([]string{"get"}, args...), "-o", "json")...)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// ---- PostgreSQL (the scope's dependency database, reachable from Pods) ----

type database struct {
	name, owner, runtime   string
	ownerPW, runtimePW     string
	ownerURL, runtimeURL   string // as seen from Pods
	hostOwnerURL, hostName string
}

func (e *devEnv) psql(db, sql string) error {
	_, err := e.run([]byte(sql), e.podman, "exec", "-i", "-e", "PGPASSWORD="+e.pgPassword, e.cluster+"-postgres", "psql", "-h", "127.0.0.1", "-U", "postgres", "-d", db, "-v", "ON_ERROR_STOP=1", "-q")
	return err
}

// newDatabase provisions an isolated database with an owner (migration) role and a
// least-privilege runtime role, and removes them on cleanup.
func (e *devEnv) newDatabase(t *testing.T, label string) database {
	t.Helper()
	id := strings.ReplaceAll(fmt.Sprintf("%s_%s_%s", e.prefix, label, randHex(3)), "-", "_")
	d := database{name: id, owner: id + "_own", runtime: id + "_rt", ownerPW: randHex(12), runtimePW: randHex(12)}
	sql := fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEROLE NOBYPASSRLS;
CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEROLE NOBYPASSRLS;
CREATE DATABASE %s OWNER %s;`, d.owner, d.ownerPW, d.runtime, d.runtimePW, d.name, d.owner)
	if err := e.psql("postgres", sql); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = e.psql("postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE); DROP ROLE IF EXISTS %s; DROP ROLE IF EXISTS %s;", d.name, d.runtime, d.owner))
	})
	if err := e.psql(d.name, fmt.Sprintf("REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE ON SCHEMA public TO %s;", d.runtime)); err != nil {
		t.Fatal(err)
	}
	const host = "postgres.kodama-deps.svc.cluster.local:5432"
	d.ownerURL = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", d.owner, d.ownerPW, host, d.name)
	d.runtimeURL = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", d.runtime, d.runtimePW, host, d.name)
	return d
}

// ---- development OIDC provider (the pinned oauthsonas image of the scope) ----

type oidcProvider struct {
	issuer, audience string
	hostBase         string // loopback address of the published port
}

const oidcImage = "ghcr.io/optimiware/oauthsonas:1.4.0@sha256:450a042217e49ac568a7b200d9636443e473c0e18e326231f18d2d88bc466c33"

var (
	oidcOnce sync.Once
	oidcVal  oidcProvider
	oidcErr  error
)

// oidc starts (once per test process) the development OIDC provider with two
// personas, the reviewer and an outsider, and makes it resolvable from every
// namespace under one issuer. Cleanup is registered on the first caller's test
// and runs when the process ends via the Cleanup of the parent test.
func (e *devEnv) oidc(t *testing.T) oidcProvider {
	t.Helper()
	oidcOnce.Do(func() { oidcVal, oidcErr = e.startOIDC(t) })
	if oidcErr != nil {
		t.Fatalf("oidc provider: %v", oidcErr)
	}
	return oidcVal
}

func (e *devEnv) startOIDC(t *testing.T) (oidcProvider, error) {
	id := e.prefix + "-oidc-" + randHex(3)
	issuer := fmt.Sprintf("http://oidc.%s.svc.cluster.local:8181", id)
	audience := "http://inventory.example.test/api"
	cfg := map[string]any{"issuer": issuer, "api_audience": audience, "token_ttl": "30m", "authorization_code_ttl": "5m", "refresh_token_ttl": "1h",
		"clients": []map[string]any{{"id": "inventory-e2e", "name": "inventory e2e", "public": true, "redirect_uris": []string{"http://127.0.0.1:18999/callback"},
			"post_logout_redirect_uris": []string{"http://127.0.0.1:18999/"}, "allowed_origins": []string{"http://127.0.0.1:18999"}}},
		"personas": []map[string]any{
			{"id": "reviewer", "subject": "oauthsonas|reviewer", "email": "reviewer@example.test", "name": "Reviewer", "roles": []string{"reviewer"}},
			{"id": "outsider", "subject": "oauthsonas|outsider", "email": "outsider@example.test", "name": "Outsider", "roles": []string{"outsider"}},
		}}
	dir, err := os.MkdirTemp("", e.prefix+"-oidc-")
	if err != nil {
		return oidcProvider{}, err
	}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "oauthsonas.yaml"), raw, 0o644); err != nil {
		return oidcProvider{}, err
	}
	_ = os.Chmod(dir, 0o755)
	if _, err := e.run(nil, e.podman, "run", "-d", "--name", id, "--network", "kind", "-p", "127.0.0.1::8181", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-e", "OAUTHSONAS_ALLOW_NON_LOOPBACK=true", "-v", dir+":/cfg:ro", oidcImage, "--config", "/cfg/oauthsonas.yaml", "--listen", "0.0.0.0:8181"); err != nil {
		return oidcProvider{}, err
	}
	e.registerProcessCleanup(func() {
		_, _ = e.run(nil, e.podman, "rm", "-f", id)
		_, _ = e.kube("delete", "namespace", id, "--ignore-not-found", "--wait=false")
		_ = os.RemoveAll(dir)
	})
	ip, err := e.run(nil, e.podman, "inspect", "-f", "{{.NetworkSettings.Networks.kind.IPAddress}}", id)
	if err != nil || strings.TrimSpace(ip) == "" {
		return oidcProvider{}, fmt.Errorf("the provider has no address on the cluster network: %v", err)
	}
	port, err := e.run(nil, e.podman, "port", id, "8181/tcp")
	if err != nil {
		return oidcProvider{}, err
	}
	hostBase := "http://" + strings.TrimSpace(strings.SplitN(strings.TrimSpace(port), "\n", 2)[0])
	e.apply(t,
		map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": id}},
		serviceAndEndpoints(id, "oidc", 8181, strings.TrimSpace(ip)),
		serviceOnly(id, "oidc", 8181))
	// Wait until it answers on the published port.
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(hostBase + "/.well-known/openid-configuration")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			logs, _ := e.run(nil, e.podman, "logs", id)
			return oidcProvider{}, fmt.Errorf("the OIDC provider did not start: %s", logs)
		}
		time.Sleep(time.Second)
	}
	return oidcProvider{issuer: issuer, audience: audience, hostBase: hostBase}, nil
}

// token performs the provider's authorization-code flow with PKCE for a persona
// and returns the access token for the API audience.
func (p oidcProvider) token(persona string) (string, error) {
	rewrite := func(u string) string { return p.hostBase + strings.TrimPrefix(u, p.issuer) }
	var disco struct {
		Auth  string `json:"authorization_endpoint"`
		Token string `json:"token_endpoint"`
	}
	resp, err := http.Get(p.hostBase + "/.well-known/openid-configuration")
	if err != nil {
		return "", err
	}
	err = json.NewDecoder(resp.Body).Decode(&disco)
	resp.Body.Close()
	if err != nil {
		return "", err
	}
	verifier := randHex(32)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {"inventory-e2e"}, "response_type": {"code"}, "scope": {"openid profile email"}, "redirect_uri": {"http://127.0.0.1:18999/callback"},
		"state": {randHex(8)}, "nonce": {randHex(8)}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "login_hint": {persona}}
	noRedirect := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := noRedirect.Get(rewrite(disco.Auth) + "?" + q.Encode())
	if err != nil {
		return "", err
	}
	r.Body.Close()
	loc, err := r.Location()
	if err != nil || loc.Query().Get("code") == "" {
		return "", fmt.Errorf("authorization for %s returned HTTP %d without a code", persona, r.StatusCode)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {"inventory-e2e"}, "redirect_uri": {"http://127.0.0.1:18999/callback"}, "code": {loc.Query().Get("code")}, "code_verifier": {verifier}}
	tr, err := http.Post(rewrite(disco.Token), "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer tr.Body.Close()
	body, _ := io.ReadAll(tr.Body)
	var tok struct {
		Access string `json:"access_token"`
	}
	if tr.StatusCode != 200 || json.Unmarshal(body, &tok) != nil || tok.Access == "" {
		return "", fmt.Errorf("token exchange for %s: HTTP %d", persona, tr.StatusCode)
	}
	return tok.Access, nil
}

// serviceOnly is a selector-less Service; serviceAndEndpoints supplies its address.
func serviceOnly(ns, name string, port int) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{"ports": []map[string]any{{"name": "http", "port": port, "targetPort": port}}}}
}

func serviceAndEndpoints(ns, name string, port int, ip string) map[string]any {
	return map[string]any{"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
		"metadata":    map[string]any{"name": name, "namespace": ns, "labels": map[string]any{"kubernetes.io/service-name": name}},
		"addressType": "IPv4", "ports": []map[string]any{{"name": "http", "port": port, "protocol": "TCP"}},
		"endpoints": []map[string]any{{"addresses": []string{ip}}}}
}

// ---- process-level cleanup (resources shared by several tests) ----

var (
	cleanupMu  sync.Mutex
	cleanupFns []func()
)

func (e *devEnv) registerProcessCleanup(f func()) {
	cleanupMu.Lock()
	cleanupFns = append(cleanupFns, f)
	cleanupMu.Unlock()
}

// runProcessCleanup removes the shared resources; TestMain calls it.
func runProcessCleanup() {
	cleanupMu.Lock()
	fns := cleanupFns
	cleanupFns = nil
	cleanupMu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	runProcessCleanup()
	os.Exit(code)
}
