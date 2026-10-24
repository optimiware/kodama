package realdelivery_test

import (
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
	"testing"
	"time"

	"example.invalid/inventory/internal/delivery"
)

// qaAdminURL provisions a dedicated disposable superuser on the scope's
// dependency PostgreSQL and returns its URI as the quality script's containers
// reach it (the server's address on the cluster network, which the VM's host
// network namespace routes). The scope's bootstrap credential never leaves the
// provisioning code; the role is dropped on cleanup.
func (e *devEnv) qaAdminURL(t *testing.T) string {
	t.Helper()
	role, pw := fmt.Sprintf("%s_qa_%s", e.prefix, randHex(3)), randHex(12)
	if err := e.psql("postgres", fmt.Sprintf("CREATE ROLE %s LOGIN SUPERUSER PASSWORD '%s';", role, pw)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.psql("postgres", fmt.Sprintf("DROP ROLE IF EXISTS %s;", role)) })
	// A host-network tool container reaches the scope's published PostgreSQL
	// proxy, not the dependency container's address on the kind network. The
	// coordinator exports this host URL when the runtime cannot route between
	// the two networks.
	if raw := os.Getenv("KODAMA_TEST_POSTGRES_ADMIN_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("KODAMA_TEST_POSTGRES_ADMIN_URL is invalid: %v", err)
		}
		u.User = url.UserPassword(role, pw)
		return u.String()
	}
	ip, err := e.run(nil, e.podman, "inspect", "-f", "{{.NetworkSettings.Networks.kind.IPAddress}}", e.cluster+"-postgres")
	if err != nil || strings.TrimSpace(ip) == "" {
		t.Fatalf("the dependency database has no address on the cluster network: %v", err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:5432/postgres?sslmode=disable", role, pw, strings.TrimSpace(ip))
}

// queryOne runs a statement as the bootstrap role (test orchestration only) and
// returns its single trimmed value.
func (e *devEnv) queryOne(db, sql string) (string, error) {
	out, err := e.run(nil, e.podman, "exec", "-i", "-e", "PGPASSWORD="+e.pgPassword, e.cluster+"-postgres", "psql", "-h", "127.0.0.1", "-U", "postgres", "-d", db, "-v", "ON_ERROR_STOP=1", "-At", "-c", sql)
	return strings.TrimSpace(out), err
}

// pullAndVerify downloads the manifest (and, for an index, every platform
// manifest, config and layer) from the registry over its distribution API and
// checks each against its digest.
func (e *devEnv) pullAndVerify(t *testing.T, repo, digest string) {
	t.Helper()
	host, path, _ := strings.Cut(repo, "/")
	get := func(p, accept string) []byte {
		req, _ := http.NewRequest(http.MethodGet, "http://"+host+"/v2/"+path+p, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: HTTP %d", p, resp.StatusCode)
		}
		return b
	}
	sum := func(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }
	accept := "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
	var pull func(ref string, depth int)
	pull = func(ref string, depth int) {
		body := get("/manifests/"+ref, accept)
		if got := sum(body); got != ref {
			t.Fatalf("manifest %s hashes to %s", ref, got)
		}
		var m struct {
			Manifests []struct{ Digest string } `json:"manifests"`
			Config    struct{ Digest string }   `json:"config"`
			Layers    []struct{ Digest string } `json:"layers"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		for _, sub := range m.Manifests {
			if depth > 2 {
				t.Fatal("manifest nesting too deep")
			}
			pull(sub.Digest, depth+1)
		}
		blobs := m.Layers
		if m.Config.Digest != "" {
			blobs = append(blobs, struct{ Digest string }{m.Config.Digest})
		}
		for _, b := range blobs {
			if got := sum(get("/blobs/"+b.Digest, "")); got != b.Digest {
				t.Fatalf("blob %s hashes to %s", b.Digest, got)
			}
		}
	}
	pull(digest, 0)
}

// seedAndSettle waits for the deployment's migration Job, seeds the controlled
// application tenants and memberships, and waits for the worker to be ready
// again afterwards (it needs its membership row to claim work).
func (d *deployment) seedAndSettle() {
	d.t.Helper()
	if err := d.dev.seed(d.env, d.image.ref(), nil); err != nil {
		d.t.Fatal(err)
	}
}

// e2e runs ci/e2e.sh in the tools image against url with the given expectations.
func (d *deployment) e2e(url, deploymentID, configDigest string, reviewerToken, outsiderToken string) scriptResult {
	d.t.Helper()
	env := map[string]string{"APP_URL": url, "DEPLOYMENT_ID": deploymentID, "CONFIGURATION_DIGEST": configDigest, "IMAGE_DIGEST": d.image.digest,
		"SOURCE_COMMIT": d.src.commit, "E2E_TENANT": tenantA, "E2E_OTHER_TENANT": tenantB}
	return d.dev.runScript(d.t, d.tools, scriptSpec{script: "e2e.sh", src: d.src, env: env,
		secrets: map[string]string{"E2E_TOKEN_REVIEWER": reviewerToken, "E2E_TOKEN_OUTSIDER": outsiderToken}})
}

// tokens returns the two personas' access tokens from the development provider.
func (d *deployment) tokens() (reviewer, outsider string) {
	d.t.Helper()
	var err error
	if reviewer, err = d.env.oidc.token("reviewer"); err != nil {
		d.t.Fatal(err)
	}
	if outsider, err = d.env.oidc.token("outsider"); err != nil {
		d.t.Fatal(err)
	}
	return
}

// deployed is the deployment evidence a successful deploy script retained.
type deployed struct {
	DeploymentID        string `json:"deploymentId"`
	ConfigurationDigest string `json:"configurationDigest"`
	Namespace           string `json:"namespace"`
	ToolImage           string `json:"toolImage"`
	Source              struct {
		Commit string `json:"commit"`
	} `json:"source"`
	Image struct {
		Digest     string `json:"digest"`
		Repository string `json:"repository"`
	} `json:"image"`
	Objects []struct {
		Kind, Name, UID string
		Ready           *bool
	} `json:"objects"`
	Annotations map[string]string `json:"annotations"`
	ExpiresAt   string            `json:"expiresAt"`
}

func readDeployed(t *testing.T, res scriptResult) deployed {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(res.outputs, "deployment.json"))
	if err != nil {
		t.Fatalf("deployment.json: %v", err)
	}
	var dep deployed
	if err := json.Unmarshal(raw, &dep); err != nil {
		t.Fatal(err)
	}
	return dep
}

// liveImage returns the image reference a live Deployment of the namespace runs.
func (d *deployment) liveImage(name string) string {
	d.t.Helper()
	o, err := d.dev.getJSON(d.env.kubeconfig, "deployment/"+name)
	if err != nil {
		d.t.Fatal(err)
	}
	spec, _ := o["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	ps, _ := tpl["spec"].(map[string]any)
	cs, _ := ps["containers"].([]any)
	if len(cs) != 1 {
		d.t.Fatalf("deployment %s has %d containers", name, len(cs))
	}
	img, _ := cs[0].(map[string]any)["image"].(string)
	return img
}

// podImageIDs returns the resolved image digests the namespace's running pods
// of the component report, proving the kubelet actually pulled and ran them.
func (d *deployment) runningDigests(component string) []string {
	d.t.Helper()
	o, err := d.dev.getJSON(d.env.kubeconfig, "pods", "-l", "app.kubernetes.io/component="+component)
	if err != nil {
		d.t.Fatal(err)
	}
	var out []string
	items, _ := o["items"].([]any)
	for _, it := range items {
		st, _ := it.(map[string]any)["status"].(map[string]any)
		cs, _ := st["containerStatuses"].([]any)
		for _, c := range cs {
			if id, _ := c.(map[string]any)["imageID"].(string); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

func mustPlan(d *deployment, id string) delivery.Plan {
	return delivery.Plan{Namespace: d.env.ns, ImageDigest: d.image.digest, ConfigDigest: mustDigest(d.t, d.snap), DeploymentID: id}
}

// restrictedArgs are the container flags every execution in the tools image uses.
func restrictedArgs() []string {
	return []string{"--rm", "--network=host", "--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532", "--pids-limit=2048",
		"--memory=3g", "--tmpfs", "/tmp:rw,exec,size=1g,mode=1777", "--tmpfs", "/workspace:rw,exec,size=512m,mode=1777"}
}

// runScriptRaw runs a shell snippet in the tools image under the restricted
// baseline, with no source mounted, and returns its combined output.
func (e *devEnv) runScriptRaw(t *testing.T, tools toolsImage, script string) string {
	t.Helper()
	args := append([]string{"run", "--tls-verify=false"}, restrictedArgs()...)
	args = append(args, tools.ref, "sh", "-c", script)
	out, err := e.runCombined(args...)
	if err != nil {
		t.Fatalf("restricted run: %v\n%s", err, tail(out, 4000))
	}
	return out
}

// runImage runs the application image from the registry under the same
// restricted baseline.
func (e *devEnv) runImage(t *testing.T, ref string, cmd ...string) string {
	t.Helper()
	args := append([]string{"run", "--tls-verify=false"}, restrictedArgs()...)
	args = append(args, ref)
	args = append(args, cmd...)
	e.registerProcessCleanup(func() { _, _ = e.run(nil, e.podman, "rmi", ref) })
	out, err := e.runCombined(args...)
	if err != nil {
		t.Fatalf("running %s: %v\n%s", ref, err, tail(out, 4000))
	}
	return out
}

func (e *devEnv) runCombined(args ...string) (string, error) {
	cmd := execCommand(e.podman, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var execCommand = exec.Command

// diagnose logs the namespace's objects, events and container logs; it is
// called when a deployment does not reach readiness so the failure is explained
// by what the cluster reported.
func (d *deployment) diagnose() {
	d.t.Helper()
	ns := d.env.ns
	for _, args := range [][]string{
		{"get", "pods,jobs,deployments,endpoints", "--namespace", ns, "-o", "wide"},
		{"get", "events", "--namespace", ns, "--sort-by=.lastTimestamp"},
	} {
		out, err := d.dev.kube(args...)
		d.t.Logf("kubectl %s:\n%s %v", strings.Join(args, " "), tail(out, 4000), err)
	}
	pods, _ := d.dev.kube("get", "pods", "--namespace", ns, "-o", "name")
	for _, p := range strings.Fields(pods) {
		out, err := d.dev.kube("logs", "--namespace", ns, p, "--all-containers", "--tail=40")
		d.t.Logf("logs %s:\n%s %v", p, tail(out, 3000), err)
	}
}

// deployAndSeed runs the environment's deploy script, requires it to complete,
// explains a failure from the cluster, then seeds the application tenants.
func (d *deployment) deployAndSeed() (scriptResult, deployed) {
	d.t.Helper()
	res := d.deploy(nil, nil, "")
	if res.exit != 0 {
		d.diagnose()
	}
	res.must(d.t, "deploy-"+d.env.kind+".sh")
	dep := readDeployed(d.t, res)
	d.seedAndSettle()
	return res, dep
}

// exceptionCount counts the application's exceptions in the environment's
// database as the bootstrap role (which bypasses row security): test
// orchestration observing what the workflow wrote.
func (d *deployment) exceptionCount() int {
	d.t.Helper()
	out, err := d.dev.queryOne(d.env.db.name, "SELECT count(*) FROM exceptions")
	if err != nil {
		d.t.Fatal(err)
	}
	var n int
	if _, err := fmt.Sscan(out, &n); err != nil {
		d.t.Fatalf("count %q: %v", out, err)
	}
	return n
}

// secretValue reads one key of a Secret with the administrator credential.
func (d *deployment) secretValue(name, key string) string {
	d.t.Helper()
	out, err := d.dev.kube("get", "secret", name, "--namespace", d.env.ns, "-o", "jsonpath={.data."+strings.ReplaceAll(key, ".", "\\.")+"}")
	if err != nil {
		d.t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		d.t.Fatal(err)
	}
	return string(raw)
}

// replicasReady returns the ready replica count of a live Deployment.
func (d *deployment) readyReplicas(name string) int {
	d.t.Helper()
	out, err := d.dev.kube("get", "deployment", name, "--namespace", d.env.ns, "-o", "jsonpath={.status.readyReplicas}")
	if err != nil {
		d.t.Fatal(err)
	}
	var n int
	_, _ = fmt.Sscan(strings.TrimSpace(out), &n)
	return n
}

// e2eOutcome reads the retained e2e report of a script run.
func e2eReport(t *testing.T, res scriptResult) (passed bool, state string, failed []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(res.outputs, "e2e.json"))
	if err != nil {
		return false, "", []string{"no e2e.json: " + err.Error()}
	}
	var rep struct {
		Passed          bool
		ResolutionState string
		Checks          []struct {
			ID     string
			Passed bool
		}
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Checks {
		if !c.Passed {
			failed = append(failed, c.ID)
		}
	}
	return rep.Passed, rep.ResolutionState, failed
}
