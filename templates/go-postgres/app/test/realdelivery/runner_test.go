package realdelivery_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"example.invalid/inventory/internal/delivery"
	"example.invalid/inventory/test/support/clusterfake"
	"example.invalid/inventory/test/support/factoryfake"
)

// sourceApp is the generated application under test: a Git repository whose HEAD is
// the candidate. The root drivers generate it and pass its path.
type sourceApp struct {
	dir    string
	commit string
}

func requireSource(t *testing.T) sourceApp {
	t.Helper()
	dir := os.Getenv("INVENTORY_APP_SRC")
	if dir == "" {
		wd, _ := os.Getwd()
		dir = filepath.Join(wd, "..", "..")
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("%s is not a Git checkout of the candidate: %v", dir, err)
	}
	if st, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); len(bytes.TrimSpace(st)) > 0 {
		t.Fatalf("the candidate checkout %s is not clean:\n%s", dir, st)
	}
	return sourceApp{dir: dir, commit: strings.TrimSpace(string(out))}
}

// ---- the tools image: built from the candidate's Containerfile, pushed, pinned by digest ----

type toolsImage struct {
	ref    string // repository@sha256:digest, reachable by the registry address of the scope
	digest string
}

var (
	toolsOnce sync.Once
	toolsVal  toolsImage
	toolsErr  error
)

func (e *devEnv) registry() string { return "localhost:" + e.registryPort }

// tools builds deploy/tools/Containerfile of the candidate (using the pinned,
// checksum-verified binaries fetched by fetch.sh), pushes it to the scope's
// registry and returns its digest-pinned reference. It is built once per process.
func (e *devEnv) tools(t *testing.T, src sourceApp) toolsImage {
	t.Helper()
	toolsOnce.Do(func() { toolsVal, toolsErr = e.buildTools(src) })
	if toolsErr != nil {
		t.Fatalf("tools image: %v", toolsErr)
	}
	return toolsVal
}

// lockShared serializes the expensive image builds of concurrently running test
// processes (the root drivers run the real suites as separate processes against
// one scope), so the second finds the first's result in the registry instead of
// pushing the same content at the same time. The lock file lives in the scope's
// own state directory.
func (e *devEnv) lockShared(name string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(e.dir, e.prefix+"-"+name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (e *devEnv) buildTools(src sourceApp) (toolsImage, error) {
	unlock, err := e.lockShared("tools")
	if err != nil {
		return toolsImage{}, err
	}
	defer unlock()
	ctxDir := filepath.Join(src.dir, "deploy", "tools")
	if out, err := e.run(nil, "sh", filepath.Join(ctxDir, "fetch.sh")); err != nil {
		return toolsImage{}, fmt.Errorf("fetch.sh: %w\n%s", err, out)
	}
	local := "localhost/" + e.prefix + "-tools:" + src.commit[:12] + "-" + randHex(3)
	if _, err := e.run(nil, e.podman, "build", "-q", "-f", filepath.Join(ctxDir, "Containerfile"), "-t", local, ctxDir); err != nil {
		return toolsImage{}, err
	}
	e.registerProcessCleanup(func() { _, _ = e.run(nil, e.podman, "rmi", local) })
	digestFile := filepath.Join(os.TempDir(), e.prefix+"-tools-"+randHex(4)+".digest")
	defer os.Remove(digestFile)
	repo := e.registry() + "/" + e.prefix + "/tools"
	if _, err := e.run(nil, e.podman, "push", "--tls-verify=false", "--digestfile", digestFile, local, "docker://"+repo+":"+src.commit[:12]); err != nil {
		return toolsImage{}, err
	}
	raw, err := os.ReadFile(digestFile)
	if err != nil {
		return toolsImage{}, err
	}
	d := strings.TrimSpace(string(raw))
	// Running the pinned reference pulls it into the container runtime's store; remove that copy too.
	e.registerProcessCleanup(func() { _, _ = e.run(nil, e.podman, "rmi", repo+"@"+d) })
	return toolsImage{ref: repo + "@" + d, digest: d}, nil
}

// ---- script execution under the step entrypoint's contract, in the tools image ----

type scriptSpec struct {
	script  string            // ci/<script>.sh
	src     sourceApp         // cloned into a scratch workspace; the mount is read-only
	env     map[string]string // explicit variables
	secrets map[string]string // delivered secret files (never environment)
	token   string            // runtime token file content; empty for scripts that do not call the factory
	mounts  []string          // extra podman -v mounts
	path    string            // optional PATH override
	ca      []byte            // trust anchor for the factory fixture's https origin (SSL_CERT_FILE)
}

type scriptResult struct {
	out      string
	exit     int
	outputs  string // host directory of declared file artifacts
	results  string // host path of the results manifest
	manifest map[string]any
}

func (r scriptResult) reason() string { s, _ := r.manifest["reason"].(string); return s }
func (r scriptResult) outcome() string {
	s, _ := r.manifest["outcome"].(string)
	return s
}
func (r scriptResult) output(name string) string {
	o, _ := r.manifest["outputs"].(map[string]any)
	s, _ := o[name].(string)
	return s
}

// runScript executes one ordinary script exactly as the step entrypoint would: a
// non-root user, read-only root filesystem, no added capabilities, no socket and a
// bounded scratch space. Host networking is the VM's network namespace, which is
// how the scope's published loopback ports (registry, API server) are reached.
func (e *devEnv) runScript(t *testing.T, tools toolsImage, s scriptSpec) scriptResult {
	t.Helper()
	k := t.TempDir()
	_ = os.Chmod(k, 0o777)
	for _, d := range []string{"outputs", "secrets"} {
		if err := os.MkdirAll(filepath.Join(k, d), 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(filepath.Join(k, d), 0o777)
	}
	for name, v := range s.secrets {
		if err := os.WriteFile(filepath.Join(k, "secrets", name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"KODAMA_OUTPUTS_DIR": "/kodama/outputs", "KODAMA_RESULTS_FILE": "/kodama/results.json", "KODAMA_SECRETS_DIR": "/kodama/secrets"}
	if s.token != "" {
		if err := os.WriteFile(filepath.Join(k, "token"), []byte(s.token+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		env["KODAMA_RUNTIME_TOKEN_FILE"] = "/kodama/token"
	}
	if len(s.ca) > 0 {
		if err := os.WriteFile(filepath.Join(k, "ca.pem"), s.ca, 0o644); err != nil {
			t.Fatal(err)
		}
		env["SSL_CERT_FILE"] = "/kodama/ca.pem"
	}
	for kk, v := range s.env {
		env[kk] = v
	}
	if s.path != "" {
		env["PATH"] = s.path
	}
	args := []string{"run", "--tls-verify=false", "--rm", "--network=host", "--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges", "--user", "65532:65532",
		"--pids-limit=2048", "--memory=3g", "--tmpfs", "/tmp:rw,exec,size=3g,mode=1777", "--tmpfs", "/workspace:rw,exec,size=1g,mode=1777",
		"-v", s.src.dir + ":/src:ro", "-v", k + ":/kodama:rw"}
	for _, m := range s.mounts {
		args = append(args, "-v", m)
	}
	names := make([]string, 0, len(env))
	for kk := range env {
		names = append(names, kk)
	}
	sort.Strings(names)
	for _, kk := range names {
		args = append(args, "-e", kk+"="+env[kk])
	}
	args = append(args, tools.ref, "sh", "-c", "set -e; HOME=/tmp; export HOME; git config --global --add safe.directory /src/.git; git clone -q --no-hardlinks /src /workspace/app; cd /workspace/app; exec sh ci/"+s.script)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.podman, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	res := scriptResult{outputs: filepath.Join(k, "outputs"), results: filepath.Join(k, "results.json")}
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.exit = ee.ExitCode()
		} else {
			res.exit = -1
		}
	}
	res.out = out.String()
	if raw, err := os.ReadFile(res.results); err == nil {
		_ = json.Unmarshal(raw, &res.manifest)
	}
	keepEvidence(t, s.script, res)
	return res
}

// keepEvidence copies results and declared artifacts to INVENTORY_REAL_EVIDENCE_DIR.
func keepEvidence(t *testing.T, script string, res scriptResult) {
	dir := os.Getenv("INVENTORY_REAL_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	dst := filepath.Join(dir, strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()), strings.TrimSuffix(script, ".sh"))
	_ = os.MkdirAll(dst, 0o755)
	if raw, err := os.ReadFile(res.results); err == nil {
		_ = os.WriteFile(filepath.Join(dst, "results.json"), raw, 0o644)
	}
	entries, _ := os.ReadDir(res.outputs)
	for _, en := range entries {
		if raw, err := os.ReadFile(filepath.Join(res.outputs, en.Name())); err == nil && !en.IsDir() {
			_ = os.WriteFile(filepath.Join(dst, en.Name()), raw, 0o644)
		}
	}
	_ = os.WriteFile(filepath.Join(dst, "console.log"), []byte(tail(res.out, 20000)), 0o644)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func (r scriptResult) must(t *testing.T, what string) {
	t.Helper()
	if r.exit != 0 || r.outcome() != "completed" {
		t.Fatalf("%s: exit %d, results %v\n%s", what, r.exit, r.manifest, tail(r.out, 6000))
	}
}

// ---- built application image ----

type appImage struct {
	repo, digest string
	meta         buildMeta
}

type buildMeta struct {
	SourceCommit string   `json:"sourceCommit"`
	ToolImage    string   `json:"toolImage"`
	GoVersion    string   `json:"goVersion"`
	Platforms    []string `json:"platforms"`
	Image        struct {
		Repository string `json:"repository"`
		Digest     string `json:"digest"`
	} `json:"image"`
}

func (a appImage) ref() string { return a.repo + "@" + a.digest }

var (
	buildMu   sync.Mutex
	buildDone *appImage
)

// build returns the image of the candidate commit for deployments. Within a
// process it is built once; across the processes of the drivers, an image the
// registry already holds under the commit's tag in the shared repository is
// reused (the build is reproducible, so it is the same digest by construction;
// the reproducibility itself is proven by TestRealDelivery). Otherwise ci/build.sh
// builds and pushes it.
func (e *devEnv) build(t *testing.T, src sourceApp, tools toolsImage) appImage {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if buildDone != nil {
		return *buildDone
	}
	unlock, err := e.lockShared("build")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	repo := e.registry() + "/" + e.prefix + "/inventory"
	if d, ok := e.taggedDigest(repo, src.commit[:12]); ok {
		img := appImage{repo: repo, digest: d}
		buildDone = &img
		return img
	}
	img := e.buildOnce(t, src, tools, repo)
	buildDone = &img
	return img
}

// adopt makes an image the process's candidate image.
func adopt(img appImage) {
	buildMu.Lock()
	defer buildMu.Unlock()
	buildDone = &img
}

// taggedDigest asks the registry for the digest a tag points at.
func (e *devEnv) taggedDigest(repo, tag string) (string, bool) {
	_, path, _ := strings.Cut(repo, "/")
	req, _ := http.NewRequest(http.MethodHead, "http://"+e.registry()+"/v2/"+path+"/manifests/"+tag, nil)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", false
	}
	resp.Body.Close()
	d := resp.Header.Get("Docker-Content-Digest")
	return d, resp.StatusCode == http.StatusOK && strings.HasPrefix(d, "sha256:")
}

func (e *devEnv) buildOnce(t *testing.T, src sourceApp, tools toolsImage, repo string) appImage {
	t.Helper()
	res := e.runScript(t, tools, scriptSpec{script: "build.sh", src: src, env: map[string]string{"KO_DOCKER_REPO": repo, "REGISTRY_INSECURE": "true", "TOOLS_IMAGE": tools.ref}})
	res.must(t, "build.sh")
	img := appImage{repo: repo}
	raw, err := os.ReadFile(filepath.Join(res.outputs, "build-metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &img.meta); err != nil {
		t.Fatal(err)
	}
	img.digest = img.meta.Image.Digest
	return img
}

// ---- the factory callback boundary: the one fixture ----

type deployment struct {
	t       *testing.T
	dev     *devEnv
	env     *environment
	src     sourceApp
	tools   toolsImage
	image   appImage
	fac     *factoryfake.Server
	ca      []byte // trust anchor of the fixture's https origin
	log     *clusterfake.Timeline
	attempt string
	action  string
	snap    delivery.Snapshot
	// secrets are the delivered credential files by binding name.
	secrets map[string]string
	// ctx is the pinned attempt context served by the factory fixture.
	ctx map[string]any
	// extraBindings are context-only bindings the snapshot does not list.
	replicas string
}

// newDeployment prepares the pinned context and the explicit snapshot for one
// logical deployment action of an environment.
func (e *devEnv) newDeployment(t *testing.T, env *environment, src sourceApp, tools toolsImage, img appImage, replicas string) *deployment {
	t.Helper()
	log := &clusterfake.Timeline{}
	d := &deployment{t: t, dev: e, env: env, src: src, tools: tools, image: img, log: log, replicas: replicas,
		attempt: "att_" + env.kind + "_" + randHex(3), action: "run_" + env.kind + "_" + randHex(3) + ":" + env.kind}
	d.fac, d.ca = factoryfake.NewTLS(t, log, hostName, "127.0.0.1")
	kc, err := os.ReadFile(env.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	d.secrets = map[string]string{
		"KUBECONFIG": strings.TrimSpace(string(kc)), "DATABASE_RUNTIME_URL": env.db.runtimeURL, "DATABASE_MIGRATION_URL": env.db.ownerURL,
		"CURSOR_KEY": env.cursorKey, "ERP_TOKEN": env.erpToken,
	}
	d.snap = delivery.Snapshot{
		Format: delivery.SnapshotFormat, Tenant: "t_real", Project: "p_inventory", Environment: "env_" + env.kind + "_" + randHex(3), Kind: env.kind, Target: "kind", Namespace: env.ns,
		Config: map[string]string{
			"OIDC_ISSUER": env.oidc.issuer, "OIDC_AUDIENCE": env.oidc.audience, "OIDC_ALLOW_INSECURE": "true",
			"ERP_BASE_URL": env.erpURL, "ERP_ALLOW_INSECURE": "true", "WORKER_TENANT": tenantA, "WORKER_SERVICE_ID": env.workerID,
			"DATABASE_RUNTIME_ROLE": env.db.runtime, "REPLICAS": replicas, "APP_URL": "http://inventory-api." + env.ns + ".svc.cluster.local:8080",
		},
		Kubeconfig: "KUBECONFIG",
		Secrets: []delivery.Binding{
			{Ref: "secret://project/kubeconfig-" + env.kind, Version: 1, Name: "KUBECONFIG", Delivery: "file"},
			{Ref: "secret://project/db-runtime-" + env.kind, Version: 1, Name: "DATABASE_RUNTIME_URL", Delivery: "file"},
			{Ref: "secret://project/db-migrate-" + env.kind, Version: 1, Name: "DATABASE_MIGRATION_URL", Delivery: "file"},
			{Ref: "secret://project/cursor-" + env.kind, Version: 1, Name: "CURSOR_KEY", Delivery: "file"},
			{Ref: "secret://project/erp-" + env.kind, Version: 1, Name: "ERP_TOKEN", Delivery: "file"},
		},
	}
	if env.kind == "preview" {
		d.snap.TTLSeconds = 86400
	}
	d.install(nil)
	return d
}

// install (re)publishes the pinned context from the current snapshot. mutate may
// alter the context document (for example to drop a binding) after the digest was pinned.
func (d *deployment) install(mutate func(map[string]any)) {
	dig, err := d.snap.Digest()
	if err != nil {
		d.t.Fatal(err)
	}
	raw, _ := json.Marshal(d.snap)
	var bindings []map[string]any
	for _, b := range d.snap.Secrets {
		bindings = append(bindings, map[string]any{"ref": b.Ref, "version": b.Version, "name": b.Name, "delivery": b.Delivery})
	}
	d.ctx = map[string]any{
		"tenantId": d.snap.Tenant, "projectId": d.snap.Project, "runId": strings.SplitN(d.action, ":", 2)[0], "stepId": d.env.kind, "attemptId": d.attempt, "actionKey": d.action,
		"command":   map[string]any{"image": d.tools.ref},
		"workspace": map[string]any{"access": "read", "source": map[string]any{"repositoryUrl": "https://git.example.invalid/acme/inventory.git", "commit": d.src.commit, "branch": nil, "expectedTip": nil}},
		"inputs": map[string]any{"image": "art_image_" + d.image.digest[7:15], "image_digest": d.image.digest, "image_repository": d.image.repo,
			"environment_snapshot": string(raw)},
		"secrets": bindings, "environment": map[string]any{"id": d.snap.Environment, "configDigest": dig},
		"x-namespace": d.snap.Namespace, "x-kind": d.env.kind,
	}
	if mutate != nil {
		mutate(d.ctx)
	}
	d.fac.SetContext(d.attempt, d.ctx)
}

func (d *deployment) snapshotJSON() string {
	raw, _ := json.Marshal(d.snap)
	return string(raw)
}

// deploy runs ci/deploy-<kind>.sh in the tools image with the scoped kubeconfig.
func (d *deployment) deploy(extraEnv map[string]string, mounts []string, path string) scriptResult {
	d.t.Helper()
	env := map[string]string{"KODAMA_RUNTIME_URL": factoryURL(d.fac), "KODAMA_ATTEMPT_ID": d.attempt, "KODAMA_INPUT_ENVIRONMENT_SNAPSHOT": d.snapshotJSON(), "REGISTRY_INSECURE": "true"}
	for k, v := range extraEnv {
		env[k] = v
	}
	return d.dev.runScript(d.t, d.tools, scriptSpec{script: "deploy-" + d.env.kind + ".sh", src: d.src, env: env, secrets: d.secrets, token: "runtime-token-" + d.attempt, mounts: mounts, path: path, ca: d.ca})
}

// factoryURL is the callback origin as the containers see it: https on the
// host's gateway name, which the fixture's certificate covers.
func factoryURL(f *factoryfake.Server) string {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.URL(), "https://"))
	return "https://" + hostName + ":" + port
}

// ---- inspecting the real cluster ----

// objectUIDs returns the UID of every named object of the deployment, using the
// scoped credential.
func (d *deployment) objectUIDs() map[string]string {
	d.t.Helper()
	out := map[string]string{}
	plan := delivery.Plan{Namespace: d.env.ns, ImageDigest: d.image.digest, ConfigDigest: mustDigest(d.t, d.snap)}
	for _, ref := range plan.Refs() {
		o, err := d.dev.getJSON(d.env.kubeconfig, strings.ToLower(ref.Kind)+"/"+ref.Name)
		if err != nil {
			if strings.Contains(err.Error(), "NotFound") {
				continue
			}
			d.t.Fatalf("observing %s/%s: %v", ref.Kind, ref.Name, err)
		}
		md, _ := o["metadata"].(map[string]any)
		out[ref.Kind+"/"+ref.Name], _ = md["uid"].(string)
	}
	return out
}

func mustDigest(t *testing.T, s delivery.Snapshot) string {
	d, err := s.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// portForward exposes the deployed API on the developer machine through the
// scoped credential and returns its address as containers see it.
func (d *deployment) portForward() (url string, stop func()) {
	d.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, d.dev.kubectl, "--kubeconfig", d.env.kubeconfig, "--namespace", d.env.ns, "port-forward", "--address", "127.0.0.1", "svc/inventory-api", ":8080")
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		cancel()
		d.t.Fatal(err)
	}
	found := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		acc := ""
		for {
			n, err := pr.Read(buf)
			acc += string(buf[:n])
			if i := strings.Index(acc, "Forwarding from 127.0.0.1:"); i >= 0 {
				rest := acc[i+len("Forwarding from 127.0.0.1:"):]
				if j := strings.Index(rest, " "); j > 0 {
					select {
					case found <- rest[:j]:
					default:
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case port := <-found:
		return "http://" + hostName + ":" + port, func() { cancel(); _ = cmd.Wait(); _ = pw.Close() }
	case <-time.After(60 * time.Second):
		cancel()
		d.t.Fatal("kubectl port-forward did not start")
		return "", nil
	}
}

// httpGet is a small helper for assertions against the deployed API.
func httpGet(url, token string) (int, []byte, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}
