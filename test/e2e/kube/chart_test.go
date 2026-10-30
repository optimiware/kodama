package kube_test

// The installed chart (kube_chart_install): the T-018/T-027 chart installed on
// the scope's kind cluster from a clean scope with the pinned Helm, over the
// scope's own PostgreSQL, JetStream, S3, Git and OIDC services, with the release
// image built from this tree (the real cmd/kodama).

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed fixtures/Containerfile.kodama
var kodamaContainerfile []byte

// helmVersion is the Helm the chart is developed and tested with (the chart
// README). The release archives' SHA-256 are checked before anything runs.
const helmVersion = "4.3.1"

var helmSums = map[string]string{
	"linux-amd64":  "0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b",
	"linux-arm64":  "8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88",
	"darwin-arm64": "6b4e61c8cd27d99e5a801e63ff25dd5c70b36875c2cd659d3e9ce6b76fc23198",
	"darwin-amd64": "34bec4eba761ba5c16d1965ee6b8f5b0abe5993974a802274e2a4f0418893d89",
}

// helmBinary returns the pinned Helm: KODAMA_HELM when it reports the pinned
// version, else the checksum-verified release archive unpacked under dev/.tools.
func helmBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("KODAMA_HELM"); p != "" {
		return p
	}
	platform := goruntimeOS() + "-" + goruntimeArch()
	sum, ok := helmSums[platform]
	if !ok {
		t.Fatalf("no pinned Helm for %s", platform)
	}
	dir := filepath.Join(repoRoot(), "dev", ".tools", "helm-"+helmVersion)
	bin := filepath.Join(dir, "helm")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	url := "https://get.helm.sh/helm-v" + helmVersion + "-" + platform + ".tar.gz"
	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Get(url)
	must(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("downloading %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	must(t, err)
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != sum {
		t.Fatalf("the Helm archive does not match its pinned checksum")
	}
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	must(t, err)
	tr := tar.NewReader(gz)
	must(t, os.MkdirAll(dir, 0o755))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		must(t, err)
		if h.Name != platform+"/helm" {
			continue
		}
		tmp := bin + fmt.Sprintf(".tmp%d", os.Getpid())
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY, 0o755)
		must(t, err)
		_, err = io.Copy(f, tr) //nolint:gosec // a checksum-verified archive
		must(t, err)
		must(t, f.Close())
		must(t, os.Rename(tmp, bin))
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("the Helm archive holds no helm binary: %v", err)
	}
	return bin
}

// kmsChoice is the key custody a release selects.
type kmsChoice struct {
	values string // the chart's kms: block
	devKey bool   // whether the development master key file is projected
}

var (
	// awsKMS selects a production key provider the core assembly does not wire
	// yet (T-029): the production key is missing as far as the server can tell.
	awsKMS = kmsChoice{values: "kms:\n  provider: aws\n  aws: {keyId: alias/kodama-t089, region: eu-west-1}"}
	// devKMS selects the development key file, which production must refuse.
	devKMS = kmsChoice{values: "kms:\n  provider: devKeyfile\n  devKeyfile: {file: master-key}", devKey: true}
)

// release is one installation of the chart.
type release struct {
	t         *testing.T
	c         *cluster
	helm      string
	home      string
	name      string
	namespace string
	tenantNS  string
	values    string
	image     string
}

func (r *release) helmRun(args ...string) (string, error) {
	cmd := exec.Command(r.helm, append([]string{"--kubeconfig", r.c.kubeconfig}, args...)...)
	cmd.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(r.home, "cache"), "HELM_CONFIG_HOME="+filepath.Join(r.home, "config"), "HELM_DATA_HOME="+filepath.Join(r.home, "data"))
	cmd.Dir = repoRoot()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (r *release) kubectl(args ...string) (string, error) {
	return run(kubectlBin(), append([]string{"--kubeconfig", r.c.kubeconfig}, args...)...)
}

func (r *release) mustKubectl(args ...string) string {
	r.t.Helper()
	out, err := r.kubectl(args...)
	must(r.t, err)
	return out
}

func goruntimeOS() string   { return goruntime.GOOS }
func goruntimeArch() string { return goruntime.GOARCH }

// rewriteHost points a database URL at the host (and port) of another one.
func rewriteHost(dbURL, like string) (string, error) {
	u, err := url.Parse(dbURL)
	if err != nil {
		return "", err
	}
	l, err := url.Parse(like)
	if err != nil {
		return "", err
	}
	u.Host = l.Host
	return u.String(), nil
}

// newRelease prepares a clean scope for one installation: a fresh release
// namespace and tenant namespace name, the release image loaded into the node,
// the secrets the chart references and the values file.
func newRelease(t *testing.T, kms kmsChoice) *release {
	t.Helper()
	c := requireCluster(t)
	r := &release{t: t, c: c, helm: helmBinary(t), home: t.TempDir()}
	suffix := randHex(3)
	r.name, r.namespace, r.tenantNS = "t089-"+suffix, "kodama-t089-"+suffix, "kodama-tenant-t089"+suffix
	for _, ns := range []string{r.namespace, r.tenantNS} {
		if _, err := r.kubectl("get", "namespace", ns); err == nil {
			t.Fatalf("namespace %s exists: the scope is not clean", ns)
		}
	}
	t.Cleanup(func() {
		_, _ = r.helmRun("uninstall", r.name, "--namespace", r.namespace, "--ignore-not-found", "--wait", "--timeout", "2m")
		_, _ = r.kubectl("delete", "namespace", r.namespace, r.tenantNS, "--wait=false", "--ignore-not-found")
	})

	// The release image: the real cmd/kodama, loaded into the node and pinned by
	// the digest of its OCI archive index.
	ref, err := c.loadImage(c.build, "t089.local/kodama", kodamaContainerfile)
	must(t, err)
	repo, digest, _ := strings.Cut(ref, "@")
	r.image = ref

	scope := scopeFiles(t)
	r.mustKubectl("create", "namespace", r.namespace)
	secret := func(name string, files map[string][]byte, devKey ...bool) {
		if len(devKey) > 0 && devKey[0] {
			files["master-key"] = scope["master-key"]
		}
		dir := t.TempDir()
		args := []string{"-n", r.namespace, "create", "secret", "generic", name}
		for k, v := range files {
			must(t, os.WriteFile(filepath.Join(dir, k), v, 0o600))
			args = append(args, "--from-file="+k+"="+filepath.Join(dir, k))
		}
		r.mustKubectl(args...)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	must(t, err)
	creds := fmt.Sprintf(`[{"ref":"secret://installation/checkout","username":"git","passwordFile":"git-read-token","urlPrefix":"https://git.kodama-deps.svc.cluster.local:8443/","tenants":["t_t089%s"]},`+
		`{"ref":"secret://installation/publisher","username":"git","passwordFile":"git-write-token","urlPrefix":"https://git.kodama-deps.svc.cluster.local:8443/","tenants":["t_t089%s"]}]`, suffix, suffix)
	secret("kodama-runtime", map[string][]byte{
		"database-runtime-url": scope["database-runtime-url"], "nats-token": scope["nats-token"],
		"git-read-token": scope["git-read-token"], "git-write-token": scope["git-write-token"], "git-credentials.json": []byte(creds),
		"cursor-key": randBytes(32), "idempotency-hash-key": randBytes(32),
		"runtime-signing-key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), "runtime-ca.crt": scope["ca.crt"],
		"s3-access-key": scope["s3-access-key"], "s3-secret-key": scope["s3-secret-key"], "ca.crt": scope["ca.crt"],
	}, kms.devKey)
	secret("kodama-migration", map[string][]byte{"database-migration-url": scope["database-migration-url"], "ca.crt": scope["ca.crt"]})

	cidr := kindSubnet(t, c)
	values := fmt.Sprintf(`profile: development
installation: {id: kodama-t089}
image:
  repository: %s
  tag: dev
  digest: %s
  pullPolicy: IfNotPresent
secrets:
  runtime: {existingSecret: kodama-runtime}
  migration: {existingSecret: kodama-migration}
config:
  nats: {url: "nats://nats.kodama-deps.svc.cluster.local:4222"}
  s3:
    endpoint: "http://localstack.kodama-deps.svc.cluster.local:4566"
    region: us-east-1
    bucket: kodama-t089
    prefix: "t089/"
    pathStyle: true
    credentials: static
  git: {url: "https://git.kodama-deps.svc.cluster.local:8443/fixture.git"}
  registry: {url: "http://registry.kodama-deps.svc.cluster.local:5000"}
  oidc:
    issuer: "http://oauthsonas.kodama-deps.svc.cluster.local:8181"
    audience: "https://kodama.example.test/api"
    cliClientId: kodama-cli
    redirectUri: "http://127.0.0.1:18999/callback"
  tls: {customCA: true}
%s
execution:
  tenants:
    - {namespace: %s, tenantId: t_t089%s}
  runtime:
    url: "https://kodama-t089.example.test"
    signingGeneration: 1
    egress:
      - {name: runtime-callback, cidr: %s, ports: [{port: 443}]}
  network:
    egress:
      - {name: dev-git, cidr: %s, ports: [{port: 8443}]}
      - {name: dev-s3, cidr: %s, ports: [{port: 4566}]}
      - {name: dev-registry, cidr: %s, ports: [{port: 5000}]}
migration:
  runtimeRole: kodama_runtime
`, repo, digest, kms.values, r.tenantNS, suffix, cidr, cidr, cidr, cidr)
	r.values = filepath.Join(t.TempDir(), "values.yaml")
	must(t, os.WriteFile(r.values, []byte(values), 0o600))
	return r
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// scopeFiles reads the scope's generated fixture files (dev/configuration.md).
func scopeFiles(t *testing.T) map[string][]byte {
	t.Helper()
	dir := filepath.Join(filepath.Dir(os.Getenv("KODAMA_TEST_KUBECONFIG")), "fixtures")
	out := map[string][]byte{}
	for _, n := range []string{"database-runtime-url", "database-migration-url", "nats-token", "git-read-token", "git-write-token", "ca.crt", "s3-access-key", "s3-secret-key", "master-key"} {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("scope fixture %s: %v", n, err)
		}
		out[n] = b
	}
	return out
}

// kindSubnet is the IPv4 subnet of the cluster's container network, where the
// scope's dependency containers live: the destinations the tenant policy allows.
func kindSubnet(t *testing.T, c *cluster) string {
	t.Helper()
	ip := strings.TrimSpace(mustRun(t, kubectlBin(), "--kubeconfig", c.kubeconfig, "get", "nodes", "-o", `jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}`))
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		t.Fatalf("node address %q", ip)
	}
	return strings.Join(parts[:3], ".") + ".0/24"
}

func mustRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := run(name, args...)
	must(t, err)
	return out
}

func (r *release) install(extra ...string) (string, error) {
	args := []string{"upgrade", "--install", r.name, filepath.Join(repoRoot(), "deploy", "charts", "kodama"), "--namespace", r.namespace,
		"-f", r.values, "--timeout", "5m"}
	return r.helmRun(append(args, extra...)...)
}

// Test_kube_chart_fail_closed (kube_chart_fail_closed; IT-07, IT-08, PX-04): the
// real chart, which runs the server with KODAMA_ENV=production, installs from a
// clean scope on kind with the pinned Helm. The migration hook runs to completion,
// the tenant namespace carries exactly the restricted baseline the chart
// promises, and the server, whose production configuration cannot be satisfied by
// anything the scope provides, refuses to start with the specific configuration
// error: it never becomes ready, serves no endpoint and admits nothing. A crash
// for another reason, an image that does not pull or an unrelated installation
// error does not pass. No development key is enabled and no guard is weakened to
// start the server: the fully running installed server is T-104's acceptance
// (kube_chart_install), after the production key wiring (T-029).
func Test_kube_chart_fail_closed(t *testing.T) {
	t.Run("a missing production key and plain-HTTP dependencies", func(t *testing.T) {
		r := newRelease(t, awsKMS)
		r.lintAndRender(t)
		if !t.Run("install from a clean scope", func(t *testing.T) { r.installAndAssertBaseline(t) }) {
			return
		}
		r.expectRefusal(t,
			"the aws KMS provider is not wired in the core assembly",
			"production refuses the localstack-s3 adapter of the objects port")
		t.Run("uninstall keeps the tenant boundary", func(t *testing.T) {
			if out, err := r.helmRun("uninstall", r.name, "--namespace", r.namespace, "--wait", "--timeout", "3m"); err != nil {
				t.Fatalf("helm uninstall: %v\n%s", err, out)
			}
			if out, err := r.kubectl("-n", r.namespace, "get", "deployment", r.name); err == nil {
				t.Fatalf("the server Deployment survived the uninstall: %s", out)
			}
			r.assertTenantBaseline(t) // namespace, quota, limits and policies are kept
		})
		t.Run("a reinstall adopts the kept tenant boundary", func(t *testing.T) {
			if out, err := r.install(); err != nil {
				t.Fatalf("helm reinstall: %v\n%s", err, out)
			}
			r.assertTenantBaseline(t)
		})
	})

	t.Run("the development key is refused under production", func(t *testing.T) {
		r := newRelease(t, devKMS)
		r.lintAndRender(t)
		if !t.Run("install from a clean scope", func(t *testing.T) { r.installAndAssertBaseline(t) }) {
			return
		}
		r.expectRefusal(t, "production refuses the keyfile adapter of the key-wrapper port")
	})
}

// lintAndRender checks the profile with the pinned Helm.
func (r *release) lintAndRender(t *testing.T) {
	t.Helper()
	if out, err := r.helmRun("version", "--short"); err != nil || !strings.Contains(out, "v"+helmVersion) {
		t.Fatalf("helm version: %v %s", err, out)
	}
	chart := filepath.Join(repoRoot(), "deploy", "charts", "kodama")
	if out, err := r.helmRun("lint", "--strict", chart, "-f", r.values); err != nil {
		t.Fatalf("helm lint: %v\n%s", err, out)
	}
	out, err := r.helmRun("template", r.name, chart, "--namespace", r.namespace, "-f", r.values)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if !strings.Contains(out, `value: "production"`) {
		t.Fatal("the rendered server does not run in the production environment")
	}
}

// installAndAssertBaseline installs and checks the schema and tenant baseline.
func (r *release) installAndAssertBaseline(t *testing.T) {
	t.Helper()
	if out, err := r.install(); err != nil {
		t.Fatalf("helm install (the pre-install migration hook must complete): %v\n%s", err, out)
	}
	// The migration hook ran as the owner and applied the declared schema before
	// the server existed: the sentinel tables are in the database.
	assertMigrated(t)
	r.assertTenantBaseline(t)
}

// expectRefusal requires the server container to have run and exited with the
// production configuration refusal (every wanted phrase in its log), and the
// server to stay unready, without an endpoint and without admitting anything.
func (r *release) expectRefusal(t *testing.T, want ...string) {
	t.Helper()
	var logs string
	deadline := time.Now().Add(5 * time.Minute)
	for {
		status, _ := r.kubectl("-n", r.namespace, "get", "pods", "-l", "app.kubernetes.io/component=server", "-o",
			`jsonpath={range .items[*]}{.status.containerStatuses[0].state.waiting.reason}|{.status.containerStatuses[0].lastState.terminated.reason}|{.status.containerStatuses[0].state.terminated.reason}{"\n"}{end}`)
		for _, bad := range []string{"ErrImagePull", "ImagePullBackOff", "ErrImageNeverPull", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError"} {
			if strings.Contains(status, bad) {
				t.Fatalf("the server did not run (%s): this is not the configuration refusal", bad)
			}
		}
		logs, _ = r.kubectl("-n", r.namespace, "logs", "-l", "app.kubernetes.io/component=server", "--previous", "--tail=40")
		if logs == "" {
			logs, _ = r.kubectl("-n", r.namespace, "logs", "-l", "app.kubernetes.io/component=server", "--tail=40")
		}
		if strings.Contains(logs, "core configuration:") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never reported a configuration refusal; log: %q (status %q)", logs, status)
		}
		time.Sleep(3 * time.Second)
	}
	for _, w := range want {
		if !strings.Contains(logs, w) {
			t.Fatalf("the refusal lacks %q:\n%s", w, logs)
		}
	}
	for _, other := range []string{"panic:", "runtime error", "nil pointer", "x509:", "connection refused"} {
		if strings.Contains(logs, other) {
			t.Fatalf("the server stopped for an unrelated reason (%q):\n%s", other, logs)
		}
	}
	// It stays unready: never a ready replica, never a ready endpoint.
	for i := 0; i < 3; i++ {
		if ready, _ := r.kubectl("-n", r.namespace, "get", "deployment", r.name, "-o", "jsonpath={.status.readyReplicas}"); ready != "" && ready != "0" {
			t.Fatalf("the refused server is ready: %s", ready)
		}
		if addrs, _ := r.kubectl("-n", r.namespace, "get", "endpoints", r.name, "-o", "jsonpath={.subsets[*].addresses[*].ip}"); strings.TrimSpace(addrs) != "" {
			t.Fatalf("the refused server has ready endpoints: %s", addrs)
		}
		time.Sleep(2 * time.Second)
	}
	// Nothing executable: no Job, Pod or Secret in the tenant namespace, and no
	// attempt or admission grant in the database.
	if out := r.mustKubectl("-n", r.tenantNS, "get", "jobs,pods,secrets", "-o", "name"); strings.TrimSpace(out) != "" {
		t.Fatalf("the refused server created workload objects: %s", out)
	}
	assertNoAdmission(t)
}

// assertMigrated checks, as the owner role, that the declared schema exists.
func assertMigrated(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	scope := scopeFiles(t)
	u := strings.TrimSpace(string(scope["database-migration-url"]))
	// The URL names the cluster Service; the host reaches the same database through
	// the scope's published PostgreSQL port.
	admin := os.Getenv("KODAMA_TEST_POSTGRES_ADMIN_URL")
	dbURL, err := rewriteHost(u, admin)
	must(t, err)
	conn, err := pgx.Connect(ctx, dbURL)
	must(t, err)
	defer conn.Close(ctx)
	for _, table := range []string{"tenants", "runs", "step_attempts", "runtime_grants", "outbox"} {
		var reg *string
		must(t, conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, table).Scan(&reg))
		if reg == nil {
			t.Fatalf("table %s does not exist: the migration hook did not apply the schema", table)
		}
	}
}

// assertTenantBaseline checks the tenant namespace as the chart renders it.
func (r *release) assertTenantBaseline(t *testing.T) {
	t.Helper()
	get := func(args ...string) string {
		out, err := r.kubectl(args...)
		if err != nil {
			t.Fatalf("kubectl %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	ns := r.tenantNS
	for k, want := range map[string]string{
		"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/audit": "restricted", "pod-security.kubernetes.io/warn": "restricted",
	} {
		if got := get("get", "namespace", ns, "-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/`+strings.TrimPrefix(k, "pod-security.kubernetes.io/")+`}`); got != want {
			t.Fatalf("namespace label %s = %q, want %q", k, got, want)
		}
	}
	if got := get("get", "namespace", ns, "-o", `jsonpath={.metadata.annotations.kodama\.optimi\.dev/tenant}`); !strings.HasPrefix(got, "t_t089") {
		t.Fatalf("the namespace is not bound to its tenant: %q", got)
	}
	if got := get("-n", ns, "get", "serviceaccount", "kodama-execution", "-o", "jsonpath={.automountServiceAccountToken}"); got != "false" {
		t.Fatalf("the execution service account mounts its token: %q", got)
	}
	for _, kind := range []string{"resourcequota", "limitrange"} {
		if get("-n", ns, "get", kind, "-o", "name") == "" {
			t.Fatalf("no %s in the tenant namespace", kind)
		}
	}
	policies := get("-n", ns, "get", "networkpolicy", "-o", "jsonpath={.items[*].metadata.name}")
	for _, want := range []string{"default-deny", "allow-external-egress"} {
		if !strings.Contains(policies, want) {
			t.Fatalf("NetworkPolicy %s missing: %s", want, policies)
		}
	}
	if got := get("-n", ns, "get", "networkpolicy", "default-deny", "-o", "jsonpath={.spec.policyTypes}"); !strings.Contains(got, "Ingress") || !strings.Contains(got, "Egress") {
		t.Fatalf("default-deny policy types: %s", got)
	}
	// No rule opens PostgreSQL or NATS to tenant Pods.
	rules := get("-n", ns, "get", "networkpolicy", "allow-external-egress", "-o", "jsonpath={.spec.egress[*].ports[*].port}")
	for _, p := range strings.Fields(rules) {
		switch p {
		case "5432", "6432", "4222", "6222", "8222":
			t.Fatalf("the tenant policy opens platform port %s", p)
		}
	}
	// The control plane can only create Jobs and read policy in the tenant
	// namespace; the release namespace's server account holds nothing else there.
	sa := "system:serviceaccount:" + r.namespace + ":" + r.name
	for _, c := range []struct{ verb, resource string }{{"create", "jobs"}, {"get", "networkpolicies"}} {
		if out, _ := r.kubectl("auth", "can-i", c.verb, c.resource, "--as", sa, "-n", ns); strings.TrimSpace(out) != "yes" && r.deploymentExists() {
			t.Fatalf("the control plane may not %s %s in %s: %q", c.verb, c.resource, ns, out)
		}
	}
	if out, _ := r.kubectl("auth", "can-i", "create", "clusterroles", "--as", sa); strings.TrimSpace(out) != "no" {
		t.Fatalf("the control plane may create cluster roles: %q", out)
	}
}

func (r *release) deploymentExists() bool {
	_, err := r.kubectl("-n", r.namespace, "get", "deployment", r.name)
	return err == nil
}

// assertNoAdmission checks, as the owner role, that no attempt was dispatched and
// no admission grant exists in the scope's database.
func assertNoAdmission(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	scope := scopeFiles(t)
	dbURL, err := rewriteHost(strings.TrimSpace(string(scope["database-migration-url"])), os.Getenv("KODAMA_TEST_POSTGRES_ADMIN_URL"))
	must(t, err)
	conn, err := pgx.Connect(ctx, dbURL)
	must(t, err)
	defer conn.Close(ctx)
	for _, table := range []string{"runs", "step_attempts", "runtime_grants"} {
		var n int
		must(t, conn.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n))
		if n != 0 {
			t.Fatalf("%d rows in %s: the refused server admitted work", n, table)
		}
	}
}
