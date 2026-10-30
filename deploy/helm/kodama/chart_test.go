// Package chart_test verifies the Kodama Helm chart (T-018) without a cluster.
//
// It implements two named acceptance checks:
//
//   - Test_helm_render_profiles: `helm lint` and `helm template` for the default, development and
//     external profiles (including each supported KMS provider), plus package contents.
//   - Test_helm_policy_assertions: structural policy assertions over the rendered manifests
//     (replicas, Pod hardening, no plaintext credentials, RBAC, tenant namespace guardrails,
//     NetworkPolicy, Job fixtures, pinned images) and negative cases that MUST fail validation.
//
// The assertions are structural. There are no golden files. Run them with:
//
//	KODAMA_REQUIRE_HELM=1 go test -count=1 ./deploy/helm/kodama/...
//
// When helm is not installed the tests skip unless KODAMA_REQUIRE_HELM=1, which makes a missing
// helm binary a failure.
package chart_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const (
	releaseName      = "kodama"
	releaseNamespace = "kodama-system"
)

type object = map[string]any

// profile is one rendering of the chart. files are value files relative to the chart directory.
type profile struct {
	name    string
	files   []string
	tenants int
	kms     string
	ingress bool
}

func baseProfiles(t *testing.T) []profile {
	t.Helper()
	return []profile{
		{name: "default", tenants: 1, kms: "keyfile"},
		{name: "dev", files: []string{"values-dev.yaml"}, tenants: 1, kms: "keyfile"},
		{name: "external-aws", files: []string{"values-external.yaml"}, tenants: 2, kms: "aws", ingress: true},
	}
}

// cloudProfiles adds the external profile with each other supported KMS provider. The overlay is
// a full copy of values-external.yaml with only the KMS selection replaced.
func cloudProfiles(t *testing.T) []profile {
	t.Helper()
	external := readYAMLFile(t, "values-external.yaml")
	variants := map[string]object{
		"gcp": {
			"provider": "gcp",
			"kekId":    "kodama-example", "current": "v1", "resources": object{"v1": "projects/p/locations/europe-west1/keyRings/kodama/cryptoKeys/kek"},
		},
		"scw": {
			"provider": "scw", "kekId": "kodama-example", "current": "v1", "resources": object{"v1": "fr-par/11111111-1111-4111-8111-111111111111"},
			"scw": object{"credentialsSecret": "kodama-kms-scw"},
		},
	}
	var out []profile
	for _, name := range []string{"gcp", "scw"} {
		values := cloneMap(external)
		asMap(values["secrets"])["kms"] = variants[name]
		path := filepath.Join(t.TempDir(), "values-external-"+name+".yaml")
		data, err := yaml.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		out = append(out, profile{name: "external-" + name, files: []string{path}, tenants: 2, kms: name, ingress: true})
	}
	return out
}

func allProfiles(t *testing.T) []profile {
	return append(baseProfiles(t), cloudProfiles(t)...)
}

// --- helm invocation ---------------------------------------------------------------------------

func helmBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("KODAMA_REQUIRE_HELM") == "1" {
			t.Fatalf("helm is required (KODAMA_REQUIRE_HELM=1) but was not found on PATH: %v", err)
		}
		t.Skip("helm not found on PATH; skipping chart checks (set KODAMA_REQUIRE_HELM=1 to make this a failure)")
	}
	return path
}

// helm runs the helm CLI from the chart directory with isolated, per-test helm state.
func helm(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command(helmBinary(t), args...)
	cmd.Dir = "."
	home := t.TempDir()
	cmd.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(home, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(home, "config"),
		"HELM_DATA_HOME="+filepath.Join(home, "data"),
	)
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err = cmd.Run()
	return out.String(), errBuf.String(), err
}

func valueArgs(p profile) []string {
	var args []string
	for _, f := range p.files {
		args = append(args, "-f", f)
	}
	return args
}

var renderCache = map[string][]object{}

// render runs `helm template` for the profile and returns every non-empty document.
func render(t *testing.T, p profile) []object {
	t.Helper()
	key := p.name + "|" + strings.Join(p.files, ",")
	if docs, ok := renderCache[key]; ok {
		return docs
	}
	args := append([]string{"template", releaseName, ".", "--namespace", releaseNamespace}, valueArgs(p)...)
	stdout, stderr, err := helm(t, args...)
	if err != nil {
		t.Fatalf("helm %s failed: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	docs := parseDocuments(t, stdout)
	renderCache[key] = docs
	return docs
}

func parseDocuments(t *testing.T, text string) []object {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(text))
	var docs []object
	for {
		var doc object
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("rendered output is not valid YAML: %v", err)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
}

// --- generic YAML helpers ----------------------------------------------------------------------

func asMap(v any) object {
	m, _ := v.(object)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func dig(v any, path ...string) any {
	for _, key := range path {
		m := asMap(v)
		if m == nil {
			return nil
		}
		v = m[key]
	}
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strList(v any) []string {
	var out []string
	for _, item := range asList(v) {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func kindOf(doc object) string { return str(doc["kind"]) }
func nameOf(doc object) string { return str(dig(doc, "metadata", "name")) }
func namespaceOf(doc object) string {
	return str(dig(doc, "metadata", "namespace"))
}

func ofKind(docs []object, kind string) []object {
	var out []object
	for _, d := range docs {
		if kindOf(d) == kind {
			out = append(out, d)
		}
	}
	return out
}

func readYAMLFile(t *testing.T, path string) object {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m object
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func cloneMap(m object) object {
	out := object{}
	for k, v := range m {
		if sub, ok := v.(object); ok {
			out[k] = cloneMap(sub)
		} else {
			out[k] = v
		}
	}
	return out
}

// --- pod spec extraction -----------------------------------------------------------------------

type role string

const (
	roleServer    role = "server"
	roleMigration role = "migration"
	roleFixture   role = "tenant-job-fixture"
)

type podSpec struct {
	role  role
	owner string // "Kind/name" for diagnostics
	job   object // the Job or Deployment document
	spec  object
}

func collectPodSpecs(t *testing.T, docs []object) []podSpec {
	t.Helper()
	var out []podSpec
	for _, d := range docs {
		switch kindOf(d) {
		case "Deployment":
			out = append(out, podSpec{roleServer, "Deployment/" + nameOf(d), d, asMap(dig(d, "spec", "template", "spec"))})
		case "Job":
			out = append(out, podSpec{roleMigration, "Job/" + nameOf(d), d, asMap(dig(d, "spec", "template", "spec"))})
		case "ConfigMap":
			text := str(dig(d, "data", "job-example.yaml"))
			if text == "" {
				continue
			}
			fixtures := parseDocuments(t, text)
			if len(fixtures) != 1 || kindOf(fixtures[0]) != "Job" {
				t.Fatalf("job-example.yaml must contain exactly one Job, got %d documents", len(fixtures))
			}
			out = append(out, podSpec{roleFixture, "fixture Job/" + nameOf(fixtures[0]), fixtures[0], asMap(dig(fixtures[0], "spec", "template", "spec"))})
		}
	}
	return out
}

func containers(spec object) []object {
	var out []object
	for _, key := range []string{"initContainers", "containers"} {
		for _, c := range asList(spec[key]) {
			out = append(out, asMap(c))
		}
	}
	return out
}

func podByRole(t *testing.T, pods []podSpec, r role) podSpec {
	t.Helper()
	var found []podSpec
	for _, p := range pods {
		if p.role == r {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s pod spec, got %d", r, len(found))
	}
	return found[0]
}

// --- Test_helm_render_profiles -----------------------------------------------------------------

func Test_helm_render_profiles(t *testing.T) {
	helmBinary(t)

	t.Run("schema_is_strict", func(t *testing.T) {
		data, err := os.ReadFile("values.schema.json")
		if err != nil {
			t.Fatal(err)
		}
		var schema object
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("values.schema.json is not valid JSON: %v", err)
		}
		if schema["additionalProperties"] != false {
			t.Fatal("root additionalProperties must be false")
		}
		for _, path := range [][]string{
			{"properties", "secrets", "additionalProperties"},
			{"properties", "server", "additionalProperties"},
			{"properties", "execution", "additionalProperties"},
			{"definitions", "secretKMS", "additionalProperties"},
			{"properties", "image", "$ref"},
		} {
			if dig(schema, path...) == nil {
				t.Errorf("schema is missing %s", strings.Join(path, "."))
			}
		}
		if got := dig(schema, "properties", "server", "properties", "replicaCount", "const"); got != float64(1) {
			t.Errorf("server.replicaCount must be const 1, got %v", got)
		}
		if got := str(dig(schema, "definitions", "digest", "pattern")); got != "^sha256:[a-f0-9]{64}$" {
			t.Errorf("digest pattern = %q", got)
		}
	})

	for _, p := range allProfiles(t) {
		p := p
		t.Run("lint_"+p.name, func(t *testing.T) {
			args := append([]string{"lint", "--strict", ".", "--namespace", releaseNamespace}, valueArgs(p)...)
			stdout, stderr, err := helm(t, args...)
			if err != nil {
				t.Fatalf("helm %s: %v\n%s%s", strings.Join(args, " "), err, stdout, stderr)
			}
		})
		t.Run("template_"+p.name, func(t *testing.T) {
			docs := render(t, p)
			if got := len(ofKind(docs, "Deployment")); got != 1 {
				t.Fatalf("want one Deployment, got %d", got)
			}
			if got := len(ofKind(docs, "Service")); got != 1 {
				t.Errorf("want one Service, got %d", got)
			}
			if got := len(ofKind(docs, "Ingress")); (got == 1) != p.ingress {
				t.Errorf("ingress documents = %d, ingress expected = %v", got, p.ingress)
			}
			if got := len(ofKind(docs, "Namespace")); got != p.tenants {
				t.Errorf("tenant namespaces = %d, want %d", got, p.tenants)
			}
			for _, kind := range []string{"ResourceQuota", "LimitRange", "Role", "RoleBinding"} {
				if got := len(ofKind(docs, kind)); got != p.tenants {
					t.Errorf("%s documents = %d, want one per tenant (%d)", kind, got, p.tenants)
				}
			}
			var migrations int
			for _, job := range ofKind(docs, "Job") {
				if strings.Contains(str(dig(job, "metadata", "annotations", "helm.sh/hook")), "pre-upgrade") {
					migrations++
				}
			}
			if migrations != 1 {
				t.Errorf("want one pre-install/pre-upgrade migration Job hook, got %d", migrations)
			}
			// The selected KMS provider is explicit in the rendered configuration.
			config := configYAML(t, docs, "kodama-config", "config.yaml")
			got := str(dig(config, "secrets", "kms", "provider"))
			if p.kms == "keyfile" {
				got = str(dig(config, "secrets", "adapter"))
			}
			if got != p.kms {
				t.Errorf("rendered kms.provider = %q, want %q", got, p.kms)
			}
			// Jobs are created at runtime by the server, never rendered as workloads by Helm.
			for _, job := range ofKind(docs, "Job") {
				if str(dig(job, "metadata", "annotations", "helm.sh/hook")) == "" {
					t.Errorf("Job %s is not a Helm hook; tenant Jobs are created by the server at runtime", nameOf(job))
				}
			}
		})
	}

	t.Run("package_excludes_tests", func(t *testing.T) {
		dir := t.TempDir()
		if _, stderr, err := helm(t, "package", ".", "--destination", dir); err != nil {
			t.Fatalf("helm package: %v\n%s", err, stderr)
		}
		archives, _ := filepath.Glob(filepath.Join(dir, "kodama-*.tgz"))
		if len(archives) != 1 {
			t.Fatalf("want one packaged chart, got %v", archives)
		}
		f, err := os.Open(archives[0])
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(zr)
		seen := map[string]bool{}
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			seen[h.Name] = true
			if strings.HasSuffix(h.Name, "_test.go") {
				t.Errorf("package contains test file %s", h.Name)
			}
		}
		for _, want := range []string{"kodama/Chart.yaml", "kodama/values.yaml", "kodama/values.schema.json", "kodama/templates/server-deployment.yaml"} {
			if !seen[want] {
				t.Errorf("package is missing %s", want)
			}
		}
	})
}

func configYAML(t *testing.T, docs []object, configMap, key string) object {
	t.Helper()
	for _, d := range ofKind(docs, "ConfigMap") {
		if nameOf(d) != configMap {
			continue
		}
		var out object
		if err := yaml.Unmarshal([]byte(str(dig(d, "data", key))), &out); err != nil {
			t.Fatalf("%s/%s is not YAML: %v", configMap, key, err)
		}
		return out
	}
	t.Fatalf("ConfigMap %s not rendered", configMap)
	return nil
}

// --- Test_helm_policy_assertions ---------------------------------------------------------------

var (
	digestRef = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
	// serverEnv is the whole environment of the server: the frozen process configuration
	// (dev/configuration.md), the environment selection and the settings document location.
	// Everything else reaches the core assembly through the settings document and the Secret.
	frozenEnv = map[string]bool{
		"KODAMA_HTTP_ADDR": true, "KODAMA_LOG_LEVEL": true, "KODAMA_SHUTDOWN_TIMEOUT": true,
		"KODAMA_ENV": true, "KODAMA_CONFIG_FILE": true, "KODAMA_GIT_CA_FILE": true,
	}
	credentialKey = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[-_]?key|private[-_]?key|access[-_]?key)`)
	userinfoURL   = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s@]+:[^/\s@]*@`)
	forbiddenPort = map[int]bool{5432: true, 6432: true, 4222: true, 6222: true, 8222: true}
)

func Test_helm_policy_assertions(t *testing.T) {
	helmBinary(t)

	for _, p := range allProfiles(t) {
		p := p
		t.Run(p.name, func(t *testing.T) {
			docs := render(t, p)
			pods := collectPodSpecs(t, docs)
			t.Run("single_server_replica", func(t *testing.T) { assertSingleReplica(t, docs) })
			t.Run("pod_hardening", func(t *testing.T) { assertPodHardening(t, pods) })
			t.Run("frozen_server_configuration", func(t *testing.T) { assertServerConfiguration(t, pods) })
			t.Run("service_accounts", func(t *testing.T) { assertServiceAccounts(t, docs, pods) })
			t.Run("secret_split", func(t *testing.T) { assertSecretSplit(t, pods) })
			t.Run("rbac", func(t *testing.T) { assertRBAC(t, docs, pods) })
			t.Run("tenant_namespaces", func(t *testing.T) { assertTenantNamespaces(t, docs, p.tenants) })
			t.Run("network_policy", func(t *testing.T) { assertNetworkPolicies(t, docs) })
			t.Run("job_fixture", func(t *testing.T) { assertJobFixtures(t, pods) })
			t.Run("images_pinned", func(t *testing.T) { assertImagesPinned(t, docs, pods) })
			t.Run("no_plaintext_credentials", func(t *testing.T) { assertNoPlaintextCredentials(t, docs) })
		})
	}

	t.Run("values_files_have_no_plaintext_credentials", func(t *testing.T) {
		files, err := filepath.Glob("values*.yaml")
		if err != nil || len(files) < 3 {
			t.Fatalf("expected values.yaml, values-dev.yaml and values-external.yaml, got %v (%v)", files, err)
		}
		for _, f := range files {
			assertValuesAreReferences(t, f, readYAMLFile(t, f))
		}
	})

	t.Run("negative_cases_fail_validation", func(t *testing.T) { assertNegativeCases(t) })
}

func assertSingleReplica(t *testing.T, docs []object) {
	deployments := ofKind(docs, "Deployment")
	if len(deployments) != 1 {
		t.Fatalf("want exactly one Deployment (the server), got %d", len(deployments))
	}
	if got := dig(deployments[0], "spec", "replicas"); got != 1 {
		t.Errorf("server replicas = %v, want 1", got)
	}
	for _, kind := range []string{"StatefulSet", "DaemonSet", "CronJob", "HorizontalPodAutoscaler", "CustomResourceDefinition"} {
		if n := len(ofKind(docs, kind)); n != 0 {
			t.Errorf("unexpected %s documents: %d (no operator, CRDs or autoscaling in the initial profile)", kind, n)
		}
	}
}

func assertPodHardening(t *testing.T, pods []podSpec) {
	podLevel := func(spec object) object { return asMap(spec["securityContext"]) }
	for _, p := range pods {
		spec := p.spec
		for _, key := range []string{"hostNetwork", "hostPID", "hostIPC"} {
			if spec[key] == true {
				t.Errorf("%s: %s must not be true", p.owner, key)
			}
		}
		for _, v := range asList(spec["volumes"]) {
			if asMap(v)["hostPath"] != nil {
				t.Errorf("%s: hostPath volume %v is forbidden", p.owner, asMap(v)["name"])
			}
			if ed := asMap(asMap(v)["emptyDir"]); ed != nil && ed["sizeLimit"] == nil {
				t.Errorf("%s: emptyDir %v has no sizeLimit", p.owner, asMap(v)["name"])
			}
		}
		if len(containers(spec)) == 0 {
			t.Fatalf("%s: no containers", p.owner)
		}
		for _, c := range containers(spec) {
			name := p.owner + " container " + str(c["name"])
			sc := asMap(c["securityContext"])
			if sc == nil {
				t.Errorf("%s: missing securityContext", name)
				continue
			}
			if sc["privileged"] == true {
				t.Errorf("%s: privileged", name)
			}
			if sc["allowPrivilegeEscalation"] != false {
				t.Errorf("%s: allowPrivilegeEscalation must be false, got %v", name, sc["allowPrivilegeEscalation"])
			}
			if sc["runAsNonRoot"] != true && podLevel(spec)["runAsNonRoot"] != true {
				t.Errorf("%s: runAsNonRoot is not true", name)
			}
			if user, ok := sc["runAsUser"]; ok && user == 0 {
				t.Errorf("%s: runAsUser 0", name)
			}
			caps := asMap(sc["capabilities"])
			if drop := strList(caps["drop"]); len(drop) != 1 || drop[0] != "ALL" {
				t.Errorf("%s: capabilities.drop = %v, want [ALL]", name, drop)
			}
			if add := asList(caps["add"]); len(add) != 0 {
				t.Errorf("%s: capabilities.add = %v, want none", name, add)
			}
			seccomp := str(dig(sc, "seccompProfile", "type"))
			if seccomp == "" {
				seccomp = str(dig(podLevel(spec), "seccompProfile", "type"))
			}
			if seccomp != "RuntimeDefault" {
				t.Errorf("%s: seccompProfile = %q, want RuntimeDefault", name, seccomp)
			}
			if dig(c, "resources", "limits", "memory") == nil || dig(c, "resources", "requests") == nil {
				t.Errorf("%s: resource requests and a memory limit are required", name)
			}
		}
		// Server and migration containers (control plane) also use a read-only root filesystem.
		if p.role != roleFixture {
			for _, c := range containers(spec) {
				if dig(c, "securityContext", "readOnlyRootFilesystem") != true {
					t.Errorf("%s container %v: readOnlyRootFilesystem must be true", p.owner, c["name"])
				}
			}
		}
	}
}

func assertServerConfiguration(t *testing.T, pods []podSpec) {
	server := podByRole(t, pods, roleServer)
	cs := asList(server.spec["containers"])
	if len(cs) != 1 {
		t.Fatalf("server pod must have exactly one container, got %d", len(cs))
	}
	c := asMap(cs[0])
	if got := strList(c["args"]); strings.Join(got, " ") != "server --assembly core" {
		t.Errorf("server args = %v, want [server --assembly core]: the chart starts the composed product", got)
	}
	for _, e := range asList(c["env"]) {
		name := str(asMap(e)["name"])
		if !frozenEnv[name] {
			t.Errorf("env %s is not a server setting the chart may render", name)
		}
		if asMap(e)["valueFrom"] != nil {
			t.Errorf("env %s: process configuration is plain, non-secret values", name)
		}
	}
	envNames := map[string]bool{}
	for _, e := range asList(c["env"]) {
		envNames[str(asMap(e)["name"])] = true
	}
	for name := range frozenEnv {
		if !envNames[name] {
			t.Errorf("env %s is not rendered", name)
		}
	}
	env := map[string]string{}
	for _, e := range asList(c["env"]) {
		env[str(asMap(e)["name"])] = str(asMap(e)["value"])
	}
	if env["KODAMA_ENV"] != "production" {
		t.Errorf("KODAMA_ENV = %q, want production (the Kubernetes runtime's environment)", env["KODAMA_ENV"])
	}
	if got := env["KODAMA_CONFIG_FILE"]; got != "/etc/kodama/config/config.yaml" {
		t.Errorf("KODAMA_CONFIG_FILE = %q, want the mounted settings document", got)
	}
	if got := env["KODAMA_GIT_CA_FILE"]; got != "/secrets/ca.crt" && got != "/secrets/runtime-ca.crt" {
		t.Errorf("KODAMA_GIT_CA_FILE = %q, want the private CA or the public runtime roots", got)
	}
	if got := str(dig(c, "livenessProbe", "httpGet", "path")); got != "/healthz" {
		t.Errorf("liveness path = %q, want /healthz", got)
	}
	if got := str(dig(c, "readinessProbe", "httpGet", "path")); got != "/readyz" {
		t.Errorf("readiness path = %q, want /readyz", got)
	}
}

func assertServiceAccounts(t *testing.T, docs []object, pods []podSpec) {
	server := str(podByRole(t, pods, roleServer).spec["serviceAccountName"])
	migration := str(podByRole(t, pods, roleMigration).spec["serviceAccountName"])
	tenant := str(podByRole(t, pods, roleFixture).spec["serviceAccountName"])
	for what, name := range map[string]string{"server": server, "migration": migration, "tenant execution": tenant} {
		if name == "" || name == "default" {
			t.Errorf("%s pod must use a dedicated service account, got %q", what, name)
		}
	}
	if server == migration || server == tenant || migration == tenant {
		t.Errorf("service accounts must be distinct: server=%q migration=%q tenant=%q", server, migration, tenant)
	}
	exists := map[string]object{}
	for _, sa := range ofKind(docs, "ServiceAccount") {
		exists[namespaceOf(sa)+"/"+nameOf(sa)] = sa
	}
	if _, ok := exists["/"+server]; !ok {
		t.Errorf("server ServiceAccount %q is not rendered", server)
	}
	if _, ok := exists["/"+migration]; !ok {
		t.Errorf("migration ServiceAccount %q is not rendered", migration)
	}
	for _, ns := range ofKind(docs, "Namespace") {
		sa, ok := exists[nameOf(ns)+"/"+tenant]
		if !ok {
			t.Errorf("tenant namespace %s has no ServiceAccount %q", nameOf(ns), tenant)
			continue
		}
		if sa["automountServiceAccountToken"] != false {
			t.Errorf("tenant ServiceAccount %s/%s must disable token automount", nameOf(ns), tenant)
		}
	}
	// Only the control-plane server uses the Kubernetes API; migration and tenant Pods never mount a token.
	for _, p := range pods {
		got := p.spec["automountServiceAccountToken"]
		if p.role == roleServer && got != true {
			t.Errorf("%s: server must explicitly automount its own token, got %v", p.owner, got)
		}
		if p.role != roleServer && got != false {
			t.Errorf("%s: automountServiceAccountToken must be false, got %v", p.owner, got)
		}
	}
}

// volumeSecrets returns the Secret names and projected keys mounted by a pod.
func volumeSecrets(spec object) (names []string, keys []string) {
	for _, v := range asList(spec["volumes"]) {
		secret := asMap(asMap(v)["secret"])
		if secret == nil {
			continue
		}
		names = append(names, str(secret["secretName"]))
		for _, item := range asList(secret["items"]) {
			keys = append(keys, str(asMap(item)["key"]))
		}
	}
	return names, keys
}

func assertSecretSplit(t *testing.T, pods []podSpec) {
	serverNames, serverKeys := volumeSecrets(podByRole(t, pods, roleServer).spec)
	migrationNames, migrationKeys := volumeSecrets(podByRole(t, pods, roleMigration).spec)
	fixtureNames, _ := volumeSecrets(podByRole(t, pods, roleFixture).spec)
	if len(fixtureNames) != 0 {
		t.Errorf("tenant Job fixture must not mount Kubernetes Secrets, got %v", fixtureNames)
	}
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	if has(serverKeys, "database-migration-url") || has(serverNames, "kodama-migration") {
		t.Error("the server must never receive the migration database role")
	}
	if has(migrationKeys, "database-runtime-url") || has(migrationNames, "kodama-runtime") {
		t.Error("the migration Job must never receive the runtime database role")
	}
	if !has(serverKeys, "database-runtime-url") || !has(migrationKeys, "database-migration-url") {
		t.Errorf("each role must mount its own database URL file: server=%v migration=%v", serverKeys, migrationKeys)
	}
}

func assertRBAC(t *testing.T, docs []object, pods []podSpec) {
	serverSA := str(podByRole(t, pods, roleServer).spec["serviceAccountName"])
	migrationSA := str(podByRole(t, pods, roleMigration).spec["serviceAccountName"])
	tenantSA := str(podByRole(t, pods, roleFixture).spec["serviceAccountName"])
	tenants := map[string]bool{}
	for _, ns := range ofKind(docs, "Namespace") {
		tenants[nameOf(ns)] = true
	}
	var rules []object
	for _, kind := range []string{"Role", "ClusterRole"} {
		for _, r := range ofKind(docs, kind) {
			if kind == "Role" && !tenants[namespaceOf(r)] {
				t.Errorf("Role %s is outside the tenant execution namespaces: %q", nameOf(r), namespaceOf(r))
			}
			for _, rule := range asList(r["rules"]) {
				rules = append(rules, asMap(rule))
			}
			if kind == "ClusterRole" {
				for _, rule := range asList(r["rules"]) {
					m := asMap(rule)
					// The one other cluster-scoped rule is the preflight's access review (create only;
					// Test_helm_preflight_access_review_rbac pins its exact shape).
					if slices.Equal(strList(m["resources"]), []string{"subjectaccessreviews"}) {
						continue
					}
					if got := strList(m["resources"]); len(got) != 1 || got[0] != "namespaces" || len(strList(m["verbs"])) != 1 || strList(m["verbs"])[0] != "get" || len(asList(m["resourceNames"])) == 0 {
						t.Errorf("ClusterRole %s must only get named tenant namespaces (and create access reviews), got %v", nameOf(r), m)
					}
				}
			}
		}
	}
	if len(rules) == 0 {
		t.Fatal("no RBAC rules rendered")
	}
	for _, rule := range rules {
		for _, field := range []string{"apiGroups", "resources", "verbs"} {
			for _, v := range strList(rule[field]) {
				if v == "*" {
					t.Errorf("RBAC wildcard in %s: %v", field, rule)
				}
			}
		}
		for _, verb := range strList(rule["verbs"]) {
			switch verb {
			case "escalate", "bind", "impersonate", "deletecollection":
				t.Errorf("RBAC verb %q is not allowed: %v", verb, rule)
			}
		}
		for _, res := range strList(rule["resources"]) {
			if res == "secrets" || res == "roles" || res == "rolebindings" || res == "clusterroles" || res == "clusterrolebindings" {
				t.Errorf("RBAC rule grants %s without opt-in: %v", res, rule)
			}
		}
	}
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		for _, b := range ofKind(docs, kind) {
			if got := str(dig(b, "roleRef", "name")); got == "cluster-admin" || strings.HasPrefix(got, "system:") {
				t.Errorf("%s %s binds privileged role %q", kind, nameOf(b), got)
			}
			for _, s := range asList(b["subjects"]) {
				sub := asMap(s)
				if str(sub["kind"]) != "ServiceAccount" || str(sub["name"]) != serverSA || str(sub["namespace"]) != releaseNamespace {
					t.Errorf("%s %s has unexpected subject %v (only the server service account %s/%s is bound)", kind, nameOf(b), sub, releaseNamespace, serverSA)
				}
				if str(sub["name"]) == migrationSA || str(sub["name"]) == tenantSA {
					t.Errorf("%s %s binds a migration or tenant service account", kind, nameOf(b))
				}
			}
			if kind == "RoleBinding" && !tenants[namespaceOf(b)] {
				t.Errorf("RoleBinding %s is outside the tenant namespaces", nameOf(b))
			}
		}
	}
}

func assertTenantNamespaces(t *testing.T, docs []object, want int) {
	namespaces := ofKind(docs, "Namespace")
	if len(namespaces) != want {
		t.Fatalf("tenant namespaces = %d, want %d", len(namespaces), want)
	}
	byNamespace := func(kind string) map[string][]object {
		m := map[string][]object{}
		for _, d := range ofKind(docs, kind) {
			m[namespaceOf(d)] = append(m[namespaceOf(d)], d)
		}
		return m
	}
	quotas, limits, policies, accounts := byNamespace("ResourceQuota"), byNamespace("LimitRange"), byNamespace("NetworkPolicy"), byNamespace("ServiceAccount")
	for _, ns := range namespaces {
		name := nameOf(ns)
		if !strings.HasPrefix(name, "kodama-") || name == releaseNamespace {
			t.Errorf("tenant namespace %q must be prefixed kodama- and differ from the release namespace", name)
		}
		for _, mode := range []string{"enforce", "audit", "warn"} {
			if got := str(dig(ns, "metadata", "labels", "pod-security.kubernetes.io/"+mode)); got != "restricted" {
				t.Errorf("%s: pod-security %s = %q, want restricted", name, mode, got)
			}
		}
		if got := str(dig(ns, "metadata", "annotations", "helm.sh/resource-policy")); got != "keep" {
			t.Errorf("%s: namespace must carry helm.sh/resource-policy: keep, got %q", name, got)
		}
		if len(quotas[name]) != 1 || dig(quotas[name][0], "spec", "hard", "pods") == nil || dig(quotas[name][0], "spec", "hard", "requests.memory") == nil || dig(quotas[name][0], "spec", "hard", "count/jobs.batch") == nil {
			t.Errorf("%s: ResourceQuota with pods, jobs and memory limits is required", name)
		}
		if len(limits[name]) != 1 || len(asList(dig(limits[name][0], "spec", "limits"))) == 0 {
			t.Errorf("%s: LimitRange is required", name)
		}
		var denyAll bool
		for _, np := range policies[name] {
			types := strList(dig(np, "spec", "policyTypes"))
			sort.Strings(types)
			selector := asMap(dig(np, "spec", "podSelector"))
			if len(selector) == 0 && strings.Join(types, ",") == "Egress,Ingress" && dig(np, "spec", "egress") == nil && dig(np, "spec", "ingress") == nil {
				denyAll = true
			}
		}
		if !denyAll {
			t.Errorf("%s: default-deny NetworkPolicy (ingress and egress, empty selector, no rules) is required", name)
		}
		for _, kind := range []string{"ResourceQuota", "LimitRange", "NetworkPolicy", "ServiceAccount"} {
			for _, d := range byNamespace(kind)[name] {
				if got := str(dig(d, "metadata", "annotations", "helm.sh/resource-policy")); got != "keep" {
					t.Errorf("%s %s/%s must keep its retention policy, got %q", kind, name, nameOf(d), got)
				}
			}
		}
		if len(accounts[name]) != 1 {
			t.Errorf("%s: want one execution ServiceAccount, got %d", name, len(accounts[name]))
		}
	}
}

func assertNetworkPolicies(t *testing.T, docs []object) {
	policies := ofKind(docs, "NetworkPolicy")
	if len(policies) == 0 {
		t.Fatal("no NetworkPolicy rendered")
	}
	for _, np := range policies {
		name := namespaceOf(np) + "/" + nameOf(np)
		if len(asList(dig(np, "spec", "ingress"))) != 0 {
			t.Errorf("%s: tenant namespaces admit no ingress", name)
		}
		for _, r := range asList(dig(np, "spec", "egress")) {
			rule := asMap(r)
			ports := asList(rule["ports"])
			if len(ports) == 0 {
				t.Errorf("%s: egress rule without ports allows every port: %v", name, rule)
			}
			for _, port := range ports {
				n, ok := asMap(port)["port"].(int)
				if !ok {
					t.Errorf("%s: egress port %v must be numeric", name, port)
					continue
				}
				if forbiddenPort[n] {
					t.Errorf("%s: egress to database/NATS port %d is forbidden", name, n)
				}
			}
			to := asList(rule["to"])
			if len(to) == 0 {
				t.Errorf("%s: egress rule without destinations allows everything: %v", name, rule)
			}
			for _, d := range to {
				dest := asMap(d)
				if dest["ipBlock"] == nil && dest["podSelector"] == nil {
					t.Errorf("%s: unsupported destination %v", name, dest)
				}
				if sel := asMap(dest["podSelector"]); sel != nil {
					labels := asMap(sel["matchLabels"])
					dns := labels["k8s-app"] == "kube-dns"
					server := labels["app.kubernetes.io/component"] == "server"
					if !dns && !server {
						t.Errorf("%s: pod destination %v is neither DNS nor the Kodama server", name, labels)
					}
					if dest["namespaceSelector"] == nil {
						t.Errorf("%s: pod destination must name its namespace: %v", name, dest)
					}
				}
			}
		}
	}
}

func assertJobFixtures(t *testing.T, pods []podSpec) {
	for _, p := range pods {
		if p.role == roleServer {
			continue
		}
		job := p.job
		if got := dig(job, "spec", "backoffLimit"); p.role == roleFixture && got != 0 {
			t.Errorf("%s: backoffLimit = %v, want 0 (PX-04)", p.owner, got)
		}
		if p.spec["restartPolicy"] != "Never" {
			t.Errorf("%s: restartPolicy = %v, want Never", p.owner, p.spec["restartPolicy"])
		}
		if deadline, ok := dig(job, "spec", "activeDeadlineSeconds").(int); !ok || deadline <= 0 {
			t.Errorf("%s: activeDeadlineSeconds must be a positive integer", p.owner)
		}
	}
	fixture := podByRole(t, pods, roleFixture)
	if got := len(asList(fixture.spec["containers"])); got != 1 {
		t.Fatalf("fixture Job must have exactly one primary container, got %d", got)
	}
	inits := asList(fixture.spec["initContainers"])
	if len(inits) != 1 || str(asMap(inits[0])["name"]) != "inject-kodama" {
		t.Errorf("fixture Job must have exactly the inject-kodama init container, got %v", inits)
	}
	if fixture.spec["serviceAccountName"] == "" || fixture.spec["enableServiceLinks"] != false {
		t.Error("fixture Job must use its own service account and disable service links")
	}
	mounts := map[string]object{}
	for _, m := range asList(asMap(asList(fixture.spec["containers"])[0])["volumeMounts"]) {
		mounts[str(asMap(m)["mountPath"])] = asMap(m)
	}
	for _, path := range []string{"/workspace", "/kodama/outputs", "/kodama/secrets", "/kodama/identity", "/kodama/bin"} {
		if mounts[path] == nil {
			t.Errorf("fixture workload does not mount %s", path)
		}
	}
	if asMap(mounts["/kodama/bin"])["readOnly"] != true {
		t.Error("/kodama/bin must be read-only to the workload command")
	}
	volumes := map[string]object{}
	for _, v := range asList(fixture.spec["volumes"]) {
		volumes[str(asMap(v)["name"])] = asMap(v)
	}
	for _, name := range []string{"secrets", "identity"} {
		if got := str(dig(volumes[name], "emptyDir", "medium")); got != "Memory" {
			t.Errorf("volume %s medium = %q, want Memory", name, got)
		}
	}
	for _, name := range []string{"workspace", "outputs", "tools"} {
		if volumes[name]["emptyDir"] == nil {
			t.Errorf("volume %s must be a private emptyDir, got %v", name, volumes[name])
		}
	}
	if len(volumes) != 5 {
		t.Errorf("fixture Job volumes = %d, want exactly workspace/outputs/secrets/identity/tools", len(volumes))
	}
}

func assertImagesPinned(t *testing.T, docs []object, pods []podSpec) {
	var images []string
	for _, p := range pods {
		for _, c := range containers(p.spec) {
			images = append(images, str(c["image"]))
		}
	}
	execution := configYAML(t, docs, "kodama-execution", "execution.yaml")
	images = append(images, str(dig(execution, "entrypoint", "image")))
	if len(images) < 5 {
		t.Fatalf("expected to inspect at least 5 image references, got %v", images)
	}
	for _, image := range images {
		if !digestRef.MatchString(image) {
			t.Errorf("image %q is not pinned by sha256 digest", image)
		}
		if strings.HasSuffix(strings.SplitN(image, "@", 2)[0], ":latest") {
			t.Errorf("image %q uses the latest tag", image)
		}
	}
	// Entrypoint injection comes from the same release image as the server.
	server := podByRole(t, pods, roleServer)
	serverImage := str(asMap(asList(server.spec["containers"])[0])["image"])
	if got := str(dig(execution, "entrypoint", "image")); got != serverImage {
		t.Errorf("entrypoint injection image %q must equal the server release image %q", got, serverImage)
	}
	fixture := podByRole(t, pods, roleFixture)
	if got := str(asMap(asList(fixture.spec["initContainers"])[0])["image"]); got != serverImage {
		t.Errorf("injector init container image %q must equal the server release image %q", got, serverImage)
	}
}

// credentialReferenceKeys are keys whose scalar value names a Kubernetes object or flag; they
// match the credential pattern but carry no secret material.
var credentialReferenceKeys = map[string]bool{
	"secretName":                   true,
	"serviceAccountName":           true,
	"automountServiceAccountToken": true,
	"existingSecret":               true,
	"credentialsSecret":            true,
	"secretFiles":                  true,
	"imagePullSecrets":             true,
	"manageSecrets":                true,
}

// looksLikeCredentialKey reports whether a key name suggests secret material. Size limits and the
// reference keys above are not credentials.
func looksLikeCredentialKey(key string) bool {
	return credentialKey.MatchString(key) && !credentialReferenceKeys[key] && !strings.HasSuffix(key, "SizeLimit")
}

// walkScalars visits every scalar with its key and path, descending into maps and lists and into
// YAML text embedded in ConfigMap data.
func walkScalars(v any, path string, key string, visit func(path, key string, value any)) {
	switch x := v.(type) {
	case object:
		for k, child := range x {
			walkScalars(child, path+"."+k, k, visit)
		}
	case []any:
		for i, child := range x {
			walkScalars(child, fmt.Sprintf("%s[%d]", path, i), key, visit)
		}
	default:
		visit(path, key, v)
	}
}

func assertNoPlaintextCredentials(t *testing.T, docs []object) {
	for _, d := range docs {
		if kindOf(d) == "Secret" {
			t.Errorf("Secret %s rendered: the chart only references existing Secrets", nameOf(d))
		}
		// Expand YAML embedded in ConfigMaps so its keys are checked too.
		expanded := []any{d}
		if kindOf(d) == "ConfigMap" {
			for key, text := range asMap(d["data"]) {
				var parsed any
				if err := yaml.Unmarshal([]byte(str(text)), &parsed); err == nil && parsed != nil {
					expanded = append(expanded, parsed)
				} else {
					t.Errorf("ConfigMap %s data %s is not parseable YAML: %v", nameOf(d), key, err)
				}
			}
		}
		for _, doc := range expanded {
			walkScalars(doc, kindOf(d)+"/"+nameOf(d), "", func(path, key string, value any) {
				text, isString := value.(string)
				if isString && userinfoURL.MatchString(text) {
					t.Errorf("%s: URL with embedded credentials", path)
				}
				if isString && strings.Contains(text, "-----BEGIN") {
					t.Errorf("%s: PEM material in rendered output", path)
				}
				if looksLikeCredentialKey(key) && value != nil && value != false && value != "" {
					t.Errorf("%s: key %q looks like a credential with a literal value %v", path, key, value)
				}
			})
		}
	}
	for _, p := range collectPodSpecs(t, docs) {
		for _, c := range containers(p.spec) {
			for _, e := range asList(c["env"]) {
				env := asMap(e)
				name := str(env["name"])
				if credentialKey.MatchString(name) && env["valueFrom"] == nil {
					t.Errorf("%s: env %s must come from a secret reference", p.owner, name)
				}
				if env["valueFrom"] != nil && asMap(env["valueFrom"])["secretKeyRef"] == nil && asMap(env["valueFrom"])["fieldRef"] == nil {
					t.Errorf("%s: env %s has an unsupported valueFrom", p.owner, name)
				}
			}
		}
	}
}

func assertValuesAreReferences(t *testing.T, file string, values object) {
	walkScalars(values, file, "", func(path, key string, value any) {
		text, isString := value.(string)
		if isString && userinfoURL.MatchString(text) {
			t.Errorf("%s: URL with embedded credentials", path)
		}
		if isString && strings.Contains(text, "-----BEGIN") {
			t.Errorf("%s: PEM material in values", path)
		}
		if looksLikeCredentialKey(key) && value != nil && value != false && value != "" {
			t.Errorf("%s: key %q is a credential-looking value; use a secret reference", path, key)
		}
		if (key == "existingSecret" || key == "credentialsSecret") && isString && !regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`).MatchString(text) {
			t.Errorf("%s: %q is not a Secret name", path, text)
		}
	})
}

func assertNegativeCases(t *testing.T) {
	const schemaFailure = "values don't meet the specifications of the schema"
	external := []string{"-f", "values-external.yaml"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"replicaCount_2", []string{"--set", "server.replicaCount=2"}, schemaFailure},
		{"replicaCount_0", []string{"--set", "server.replicaCount=0"}, schemaFailure},
		{"plaintext_password_value", []string{"--set", "secrets.runtime.password=hunter2"}, "password"},
		{"plaintext_token_value", []string{"--set", "config.nats.token=abc123"}, "token"},
		{"database_url_with_credentials", []string{"--set", "config.nats.url=nats://user:pw@nats.example.invalid:4222"}, schemaFailure},
		{"privileged_true", []string{"--set", "securityContext.privileged=true"}, schemaFailure},
		{"run_as_root_disabled", []string{"--set", "securityContext.runAsNonRoot=false"}, schemaFailure},
		{"zero_adapters_missing_selection", []string{"--set", "secrets.adapter=null"}, schemaFailure},
		{"zero_kms_providers_empty_block", append(append([]string{}, external...), "--set", "secrets.kms.aws=null"), schemaFailure},
		{"kms_provider_unknown", append(append([]string{}, external...), "--set", "secrets.kms.provider=azure"), schemaFailure},
		{"multiple_kms_providers", append(append([]string{}, external...), "--set", "secrets.kms.scw.credentialsSecret=other"), schemaFailure},
		{"dev_keyfile_in_external_profile", []string{"--set", "profile=external"}, schemaFailure},
		{"unpinned_image_empty_digest", []string{"--set", "image.digest="}, schemaFailure},
		{"unpinned_image_tag_as_digest", []string{"--set", "image.digest=latest"}, schemaFailure},
		{"unpinned_image_latest_tag", []string{"--set", "image.tag=latest"}, schemaFailure},
		{"unpinned_example_job_image", []string{"--set", "execution.exampleJob.image.digest="}, schemaFailure},
		{"tenant_namespace_outside_prefix", []string{"--set", "execution.tenants[0].namespace=kube-system"}, schemaFailure},
		{"tenant_egress_to_postgres", []string{"--set", "execution.network.egress[0].name=db,execution.network.egress[0].cidr=10.0.0.0/8,execution.network.egress[0].ports[0].port=5432"}, schemaFailure},
		{"tenant_egress_to_nats", []string{"--set", "execution.network.egress[0].name=bus,execution.network.egress[0].cidr=10.0.0.0/8,execution.network.egress[0].ports[0].port=4222"}, schemaFailure},
		{"tenant_egress_without_ports", []string{"--set", "execution.network.egress[0].name=any,execution.network.egress[0].cidr=10.0.0.0/8"}, schemaFailure},
		{"untrusted_execution_profile", []string{"--set", "execution.trustProfile=untrusted-hosted"}, schemaFailure},
		{"external_profile_requires_tls", append(append([]string{}, external...), "--set", "config.git.url=http://git.example.invalid/repo.git"), schemaFailure},
		{"hosted_authority_has_no_fallback", []string{"--set", "authority.mode=hosted"}, "unsupported"},
		{"tenant_namespace_equals_release_namespace", []string{"--namespace", "kodama-tenant-example"}, "must differ"},
		{"unknown_top_level_value", []string{"--set", "operator.enabled=true"}, schemaFailure},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"template", releaseName, ".", "--namespace", releaseNamespace}, tc.args...)
			stdout, stderr, err := helm(t, args...)
			if err == nil {
				t.Fatalf("helm %s unexpectedly succeeded:\n%s", strings.Join(args, " "), stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("error does not mention %q:\n%s", tc.want, stderr)
			}
		})
	}
}

// Tenant guardrails carry helm.sh/resource-policy: keep, so Helm never deletes them. The external
// egress policy must therefore render even with no extra rules, so that emptying
// execution.network.egress replaces the old rules in place instead of leaving them live; the
// runtime callback destination (execution.runtime.egress) is always present.
func Test_helm_empty_egress_still_renders_policy(t *testing.T) {
	helmBinary(t)
	stdout, stderr, err := helm(t, "template", releaseName, ".", "--namespace", releaseNamespace,
		"-f", "values-external.yaml", "--set-json", "execution.network.egress=[]")
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, stderr)
	}
	var found int
	for _, np := range ofKind(parseDocuments(t, stdout), "NetworkPolicy") {
		if nameOf(np) != "allow-external-egress" {
			continue
		}
		found++
		// The runtime callback remains, plus the fixed cluster-DNS peer. Emptying network.egress
		// removes every operator rule in place; DNS is not an operator rule.
		rules := asList(dig(np, "spec", "egress"))
		if len(rules) != 2 {
			t.Errorf("%s: want the runtime callback and cluster DNS, got %d rules", namespaceOf(np), len(rules))
		}
		var dns bool
		for _, r := range rules {
			for _, dest := range asList(asMap(r)["to"]) {
				labels := asMap(asMap(dest)["podSelector"])["matchLabels"]
				if asMap(labels)["k8s-app"] == "kube-dns" {
					dns = true
				}
			}
		}
		if !dns {
			t.Errorf("%s: cluster DNS peer is missing", namespaceOf(np))
		}
		if got := asList(dig(np, "spec", "policyTypes")); len(got) != 1 || got[0] != "Egress" {
			t.Errorf("%s: policyTypes = %v, want [Egress] so an empty rule list denies egress", namespaceOf(np), got)
		}
	}
	if found == 0 {
		t.Fatal("allow-external-egress was not rendered with an empty egress list")
	}
}

// Test_helm_secret_management_rbac guards the opt-in Secret rule of the tenant Roles: the server's
// reconciler lists the managed admission Secrets of every tenant namespace, so `list` must be granted
// with the verbs that create and remove them (a Role without it left a freshly installed server unable
// to finish a reconcile pass). `update` must be granted too: the workload record lives in an annotation
// of that Secret and is replaced as the attempt advances, and a Role without it leaves every dispatch
// unable to save the record. Nothing broader.
func Test_helm_secret_management_rbac(t *testing.T) {
	args := []string{"template", releaseName, ".", "--namespace", releaseNamespace, "--set", "execution.rbac.manageSecrets=true"}
	stdout, stderr, err := helm(t, args...)
	if err != nil {
		t.Fatalf("helm %s failed: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	docs := parseDocuments(t, stdout)
	want := []string{"create", "delete", "get", "list", "update"}
	found := 0
	for _, role := range ofKind(docs, "Role") {
		for _, r := range asList(role["rules"]) {
			rule := asMap(r)
			for _, res := range strList(rule["resources"]) {
				if res != "secrets" {
					continue
				}
				found++
				got := strList(rule["verbs"])
				sort.Strings(got)
				if !slices.Equal(got, want) {
					t.Errorf("Role %s grants Secret verbs %v, want exactly %v", nameOf(role), got, want)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("manageSecrets=true rendered no Secret rule")
	}
}

// Test_helm_preflight_access_review_rbac guards the cluster-scoped rule the tenant preflight needs: the server
// asks the API server (SubjectAccessReview) what each tenant's execution service account may do, and a chart
// that does not allow it leaves every dispatch refused on a real installation.
func Test_helm_preflight_access_review_rbac(t *testing.T) {
	docs := render(t, profile{name: "default", tenants: 1, kms: "keyfile"})
	found := false
	for _, role := range ofKind(docs, "ClusterRole") {
		for _, r := range asList(role["rules"]) {
			rule := asMap(r)
			if !slices.Contains(strList(rule["resources"]), "subjectaccessreviews") {
				continue
			}
			found = true
			if got := strList(rule["verbs"]); len(got) != 1 || got[0] != "create" || !slices.Equal(strList(rule["apiGroups"]), []string{"authorization.k8s.io"}) {
				t.Errorf("ClusterRole %s grants %v on subjectaccessreviews in %v, want exactly create in authorization.k8s.io", nameOf(role), got, strList(rule["apiGroups"]))
			}
		}
	}
	if !found {
		t.Fatal("no ClusterRole lets the server create SubjectAccessReviews: the tenant preflight would refuse every dispatch")
	}
}

// Test_helm_run_shared_render checks the run_shared model (O-30) with real Helm. Disabled (the
// default) the chart renders none of it and the tenant namespaces stay `restricted`. Enabled it
// renders the settings section, `baseline` namespaces at the pinned version, the PVC/Service quota
// and the controller RBAC (namespaced rules for claims, Services, Pods and policies; read-only
// cluster-scoped rules for volumes, attachments and storage classes); lint passes; and the chart
// refuses to render it without the record Secrets the run resources need.
func Test_helm_run_shared_render(t *testing.T) {
	helmBinary(t)
	enable := []string{
		"--set", "runShared.enabled=true", "--set", "runShared.nodeProfilesInstalled=true", "--set", "runShared.admission.runShared=open", "--set", "execution.rbac.manageSecrets=true",
		"--set-json", `runShared.clusterCIDRs=["10.32.0.0/12"]`,
	}
	base := []string{"template", releaseName, ".", "--namespace", releaseNamespace}

	off, stderr, err := helm(t, base...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, stderr)
	}
	if strings.Contains(off, "runShared:") || strings.Contains(off, "persistentvolumeclaims") || strings.Contains(off, "volumeattachments") {
		t.Fatal("the default render carries run_shared")
	}

	stdout, stderr, err := helm(t, append(append([]string{}, base...), enable...)...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, stderr)
	}
	docs := parseDocuments(t, stdout)
	for _, ns := range ofKind(docs, "Namespace") {
		labels := asMap(dig(ns, "metadata", "labels"))
		if labels["pod-security.kubernetes.io/enforce"] != "baseline" || labels["pod-security.kubernetes.io/enforce-version"] != "v1.31" {
			t.Errorf("namespace %s labels %v", nameOf(ns), labels)
		}
	}
	var settings string
	for _, cm := range ofKind(docs, "ConfigMap") {
		if strings.HasSuffix(nameOf(cm), "-config") {
			settings = str(dig(cm, "data", "config.yaml"))
		}
	}
	for _, want := range []string{"runShared:", "legacyFormat: open", "runShared: open", "storageClass: \"sbs-default\"", "isolation: tenant", "publicEgress: true",
		"seccomp: \"kodama-rootless-build-v1\"", "capabilitiesAdd: [\"SETUID\",\"SETGID\"]", "fsGroup: 1000"} {
		if !strings.Contains(settings, want) {
			t.Errorf("the settings document lacks %q", want)
		}
	}
	for _, q := range ofKind(docs, "ResourceQuota") {
		hard := asMap(dig(q, "spec", "hard"))
		if hard["persistentvolumeclaims"] == nil || hard["requests.storage"] == nil {
			t.Errorf("quota %v", hard)
		}
	}
	var clusterReads int
	for _, role := range ofKind(docs, "ClusterRole") {
		for _, r := range asList(role["rules"]) {
			rule := asMap(r)
			for _, res := range strList(rule["resources"]) {
				switch res {
				case "persistentvolumes", "volumeattachments", "storageclasses":
					clusterReads++
					for _, verb := range strList(rule["verbs"]) {
						if verb != "get" && verb != "list" {
							t.Errorf("ClusterRole %s grants %q on %s", nameOf(role), verb, res)
						}
					}
				}
			}
		}
	}
	if clusterReads != 3 {
		t.Errorf("want read rules for persistentvolumes, volumeattachments and storageclasses, got %d", clusterReads)
	}

	if _, stderr, err := helm(t, append([]string{"lint", "."}, enable...)...); err != nil {
		t.Fatalf("helm lint with run_shared enabled failed: %v\n%s", err, stderr)
	}
	if _, stderr, err := helm(t, append(append([]string{}, base...), "--set", "runShared.enabled=true", "--set", "runShared.nodeProfilesInstalled=true")...); err == nil || !strings.Contains(stderr, "manageSecrets") {
		t.Fatalf("run_shared rendered without record Secrets: %v\n%s", err, stderr)
	}
	if _, stderr, err := helm(t, append(append([]string{}, base...), "--set", "runShared.enabled=true", "--set", "execution.rbac.manageSecrets=true")...); err == nil || !strings.Contains(stderr, "node profiles") {
		t.Fatalf("run_shared rendered without the node profiles attested: %v\n%s", err, stderr)
	}
	if _, _, err := helm(t, append(append([]string{}, base...), "--set", "runShared.enabled=true", "--set", "runShared.podSecurity.version=latest")...); err == nil {
		t.Fatal("an unpinned Pod Security version was accepted")
	}
}
