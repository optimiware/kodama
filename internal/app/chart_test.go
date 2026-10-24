package app

// The chart join (T-027), checked with no cluster and no helm binary. The
// templates of deploy/helm/kodama are rendered by a small text/template
// renderer that implements the functions the chart uses (an unknown function
// fails the test), the values are validated against values.schema.json, and the
// rendered Deployment, settings document, Secret projection and tenant
// namespace objects are fed to the real consumers: LoadCoreConfig (the server's
// startup configuration) and the Kubernetes namespace preflight. A selection
// the core assembly cannot serve must fail closed, by name, never be replaced:
// the production guards are not weakened to make a profile start.
//
// Installing the chart on a cluster is T-089's; helm's own lint/template checks
// stay in deploy/helm/kodama (chart_test.go, helm required).

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/internal/secrets/kms/production"
)

type object = map[string]any

const (
	chartRelease   = "kodama"
	chartNamespace = "kodama-system"
)

// ---- a small Helm-compatible renderer ----------------------------------------

func chartDir(t testing.TB) string { return filepath.Join(repoRoot(t), "deploy", "helm", "kodama") }

func readYAMLObject(t testing.TB, path string) object {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out object
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

// mergeValues overlays src onto dst as Helm does: maps merge, everything else
// (lists included) replaces, and null deletes the key.
func mergeValues(dst, src object) object {
	out := object{}
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if v == nil {
			delete(out, k)
			continue
		}
		if sm, ok := v.(object); ok {
			if dm, ok := out[k].(object); ok {
				out[k] = mergeValues(dm, sm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func emptyValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() == 0
	case reflect.Bool:
		return !rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() == 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() == 0
	}
	return false
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func yamlText(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		panic(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

func indentText(n int, s string) string {
	pad := strings.Repeat(" ", n)
	return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
}

type chartRenderer struct {
	t    testing.TB
	tpl  *template.Template
	data object
	docs []string
}

func newChartRenderer(t testing.TB, values object) *chartRenderer {
	t.Helper()
	dir := chartDir(t)
	chart := readYAMLObject(t, filepath.Join(dir, "Chart.yaml"))
	r := &chartRenderer{t: t}
	funcs := template.FuncMap{
		"base": filepath.Base,
		"include": func(name string, data any) (string, error) {
			var b bytes.Buffer
			if err := r.tpl.ExecuteTemplate(&b, name, data); err != nil {
				return "", err
			}
			return b.String(), nil
		},
		"toYaml":  yamlText,
		"nindent": func(n int, s string) string { return "\n" + indentText(n, s) },
		"indent":  indentText,
		"quote": func(v any) string {
			if v == nil {
				return ""
			}
			return strconv.Quote(fmt.Sprint(v))
		},
		"default": func(d, given any) any {
			if emptyValue(given) {
				return d
			}
			return given
		},
		"trunc": func(n int, s string) string {
			if len(s) > n {
				return s[:n]
			}
			return s
		},
		"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
		"contains":   func(sub, s string) bool { return strings.Contains(s, sub) },
		"replace":    func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
		"trim":       strings.TrimSpace,
		"lower":      strings.ToLower,
		"dict": func(kv ...any) object {
			m := object{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"list": func(v ...any) []any { return v },
		"concat": func(lists ...any) []any {
			out := []any{}
			for _, l := range lists {
				if l == nil {
					continue
				}
				rv := reflect.ValueOf(l)
				for i := 0; i < rv.Len(); i++ {
					out = append(out, rv.Index(i).Interface())
				}
			}
			return out
		},
		"append": func(list any, item any) []any {
			out := []any{}
			if list != nil {
				rv := reflect.ValueOf(list)
				for i := 0; i < rv.Len(); i++ {
					out = append(out, rv.Index(i).Interface())
				}
			}
			return append(out, item)
		},
		"first": func(list any) any {
			rv := reflect.ValueOf(list)
			if list == nil || rv.Len() == 0 {
				return nil
			}
			return rv.Index(0).Interface()
		},
		"int": toInt,
		"sha256sum": func(s string) string {
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:])
		},
		"fail": func(msg string) (string, error) { return "", errors.New(msg) },
	}
	r.tpl = template.New("kodama").Funcs(funcs).Option("missingkey=zero")
	files, err := filepath.Glob(filepath.Join(dir, "templates", "*"))
	if err != nil {
		t.Fatal(err)
	}
	var render []string
	for _, f := range files {
		base := filepath.Base(f)
		if base == "NOTES.txt" {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := "kodama/templates/" + base
		if _, err := r.tpl.New(name).Parse(string(body)); err != nil {
			t.Fatalf("%s: %v (the renderer may lack a function the chart now uses)", base, err)
		}
		if strings.HasSuffix(base, ".yaml") {
			render = append(render, name)
		}
	}
	sort.Strings(render)
	r.data = object{
		"Values":   values,
		"Release":  object{"Name": chartRelease, "Namespace": chartNamespace, "Service": "Helm"},
		"Chart":    object{"Name": chart["name"], "Version": chart["version"], "AppVersion": chart["appVersion"]},
		"Template": object{"BasePath": "kodama/templates"},
	}
	for _, name := range render {
		var b bytes.Buffer
		if err := r.tpl.ExecuteTemplate(&b, name, r.data); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		r.docs = append(r.docs, strings.ReplaceAll(b.String(), "<no value>", ""))
	}
	return r
}

func (r *chartRenderer) objects() []object {
	r.t.Helper()
	var out []object
	for _, text := range r.docs {
		for _, part := range strings.Split(text, "\n---") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			var o object
			if err := yaml.Unmarshal([]byte(part), &o); err != nil {
				r.t.Fatalf("rendered document is not YAML: %v\n%s", err, part)
			}
			if o != nil {
				out = append(out, o)
			}
		}
	}
	return out
}

// validateChartValues checks values against the chart's strict schema, as
// `helm template` does before rendering.
func validateChartValues(t testing.TB, values object) error {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir(t), "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("values.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	js, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	return sch.Validate(inst)
}

// chartValues is the values of a profile: values.yaml, then the named value
// files of the chart directory, then the in-memory overlays.
func chartValues(t testing.TB, files []string, overlays ...object) object {
	t.Helper()
	v := readYAMLObject(t, filepath.Join(chartDir(t), "values.yaml"))
	for _, f := range files {
		v = mergeValues(v, readYAMLObject(t, filepath.Join(chartDir(t), f)))
	}
	for _, o := range overlays {
		v = mergeValues(v, o)
	}
	return v
}

// ---- helpers over rendered objects ---------------------------------------------

func dig(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(object)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asObject(v any) object {
	m, _ := v.(object)
	return m
}

func text(v any) string {
	s, _ := v.(string)
	return s
}

func ofKind(docs []object, kind string) []object {
	var out []object
	for _, d := range docs {
		if text(d["kind"]) == kind {
			out = append(out, d)
		}
	}
	return out
}

func nameOf(o object) string { return text(dig(o, "metadata", "name")) }

func stringList(v any) []string {
	var out []string
	for _, x := range asList(v) {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// ---- the profiles --------------------------------------------------------------

type chartProfile struct {
	name     string
	files    []string
	overlays []object
	// want lists the fail-closed refusals the server must report, each by a
	// fragment of its message. Nothing else may be reported.
	want []string
}

func kmsOverlay(provider string, block object) object {
	resource := block["keyId"]
	if provider == "gcp" {
		resource = block["keyName"]
	}
	k := object{"provider": provider, "kekId": "kodama-example", "current": "v1", "resources": object{"v1": resource}, "aws": nil, "scw": nil}
	if provider == "aws" {
		k["aws"] = object{"region": block["region"]}
	}
	if provider == "scw" {
		k["scw"] = object{"credentialsSecret": block["credentialsSecret"]}
	}
	return object{"secrets": object{"adapter": "kms", "keyfile": nil, "kms": k}}
}

var staticS3 = object{"config": object{"s3": object{"credentials": "static"}}}

const (
	refusesKeyfile   = "it is the development key-encryption key"
	refusesFixtureS3 = "an object store reached over plain HTTP is the test fixture"
	refusesWorkloadS = "object store credentials \"workloadIdentity\" are not supported by the core assembly"
)

func chartProfiles() []chartProfile {
	aws := kmsOverlay("aws", object{"keyId": "11111111-1111-4111-8111-111111111111", "region": "eu-west-1"})
	gcp := kmsOverlay("gcp", object{"keyName": "projects/p/locations/europe-west1/keyRings/kodama/cryptoKeys/kek"})
	scw := kmsOverlay("scw", object{"keyId": "fr-par/11111111-1111-4111-8111-111111111111", "credentialsSecret": "kodama-kms-scw"})
	return []chartProfile{
		// The development key is development custody: production refuses it.
		{name: "default", want: []string{refusesKeyfile}},
		// Plaintext in-cluster fixtures and the development key: refused.
		{name: "dev", files: []string{"values-dev.yaml"}, want: []string{refusesKeyfile, refusesFixtureS3}},
		// Workload-identity object store credentials are not served by the core
		// assembly (and no key files are projected for them); its other settings
		// validate, and the key refusal is reached with static credentials below.
		{name: "external-aws", files: []string{"values-external.yaml"}, want: []string{refusesWorkloadS, "secret file s3-access-key", "secret file s3-secret-key"}},
		{name: "external-aws-static-s3", files: []string{"values-external.yaml"}, overlays: []object{staticS3}},
		{name: "external-gcp-static-s3", files: []string{"values-external.yaml"}, overlays: []object{gcp, staticS3}},
		{name: "external-scw-static-s3", files: []string{"values-external.yaml"}, overlays: []object{scw, staticS3}},
		{name: "external-aws-selected", files: []string{"values-external.yaml"}, overlays: []object{aws, staticS3}},
	}
}

// ---- the rendered server, materialized ------------------------------------------

type renderedServer struct {
	docs       []object
	deployment object
	container  object
	env        map[string]string
	root       string // the file system the mounts are materialized under
	secretKeys []string
}

func (s *renderedServer) hostPath(p string) string { return filepath.Join(s.root, p) }

// materialize lays the Deployment's volumes out on disk under a scratch root,
// the way the kubelet would project them, with synthetic contents.
func materialize(t *testing.T, docs []object) *renderedServer {
	t.Helper()
	var dep object
	for _, d := range ofKind(docs, "Deployment") {
		dep = d
	}
	if dep == nil {
		t.Fatal("no Deployment rendered")
	}
	spec := asObject(dig(dep, "spec", "template", "spec"))
	cs := asList(spec["containers"])
	if len(cs) != 1 {
		t.Fatalf("the server Pod has %d containers", len(cs))
	}
	s := &renderedServer{docs: docs, deployment: dep, container: asObject(cs[0]), env: map[string]string{}, root: t.TempDir()}
	mounts := map[string]string{} // volume -> mount path
	for _, m := range asList(s.container["volumeMounts"]) {
		mounts[text(asObject(m)["name"])] = text(asObject(m)["mountPath"])
	}
	configMaps := map[string]object{}
	for _, cm := range ofKind(docs, "ConfigMap") {
		configMaps[nameOf(cm)] = asObject(cm["data"])
	}
	secrets := synthSecrets(t)
	for _, v := range asList(spec["volumes"]) {
		vol := asObject(v)
		path, ok := mounts[text(vol["name"])]
		if !ok {
			continue
		}
		dir := s.hostPath(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		write := func(name, content string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if sec := asObject(vol["secret"]); sec != nil {
			for _, item := range asList(sec["items"]) {
				key, file := text(asObject(item)["key"]), text(asObject(item)["path"])
				content, known := secrets[key]
				if !known {
					content = "synthetic"
				}
				write(file, content)
				s.secretKeys = append(s.secretKeys, key)
			}
		}
		if cm := asObject(vol["configMap"]); cm != nil {
			data, ok := configMaps[text(cm["name"])]
			if !ok {
				t.Fatalf("the server mounts ConfigMap %q, which is not rendered", text(cm["name"]))
			}
			for k, c := range data {
				write(k, text(c))
			}
		}
	}
	for _, e := range asList(s.container["env"]) {
		m := asObject(e)
		if m["valueFrom"] != nil {
			t.Fatalf("env %s comes from a reference; the server takes plain settings", text(m["name"]))
		}
		value := text(m["value"])
		orig := value
		for _, path := range mounts {
			if orig == path || strings.HasPrefix(orig, path+"/") {
				value = s.hostPath(orig)
				break
			}
		}
		s.env[text(m["name"])] = value
	}
	// The runtime Secret is mounted at the server's default secrets directory.
	if _, ok := mounts["secrets"]; !ok || mounts["secrets"] != "/secrets" {
		t.Fatalf("the runtime Secret is mounted at %q, not at the default secrets directory /secrets", mounts["secrets"])
	}
	s.env["KODAMA_SECRETS_DIR"] = s.hostPath("/secrets")
	// The kubelet's in-cluster variables.
	s.env["KUBERNETES_SERVICE_HOST"], s.env["KUBERNETES_SERVICE_PORT"] = "10.96.0.1", "443"
	return s
}

func (s *renderedServer) getenv(name string) string { return s.env[name] }

func synthSecrets(t testing.TB) map[string]string {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-runtime-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return map[string]string{
		"database-runtime-url": "postgres://kodama_runtime:pw@db:5432/kodama",
		"nats-token":           "token",
		"s3-access-key":        "access", "s3-secret-key": "secret",
		"cursor-key":           strings.Repeat("c", 32),
		"idempotency-hash-key": strings.Repeat("h", 32),
		"master-key":           strings.Repeat("m", 32),
		"runtime-signing-key":  "synthetic-pem",
		"runtime-ca.crt":       ca,
		"ca.crt":               ca,
		"git-read-token":       "read", "git-write-token": "write",
		"git-credentials.json": `[{"ref":"secret://installation/checkout","username":"kodama","passwordFile":"git-read-token","urlPrefix":"https://git.example.invalid/","tenants":["*"]}]`,
	}
}

func leaves(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, leaves(e)...)
		}
		return out
	}
	return []error{err}
}

// ---- the tests -------------------------------------------------------------------

// TestChartValuesFollowTheSchema: every profile validates against the strict
// schema, and the schema refuses what the join must never accept.
func TestChartValuesFollowTheSchema(t *testing.T) {
	for _, p := range chartProfiles() {
		if err := validateChartValues(t, chartValues(t, p.files, p.overlays...)); err != nil {
			t.Errorf("%s: %v", p.name, err)
		}
	}
	base := func(o object) object { return chartValues(t, []string{"values-external.yaml"}, o) }
	for name, bad := range map[string]object{
		"a plain-http runtime origin":     base(object{"execution": object{"runtime": object{"url": "http://kodama.example.invalid"}}}),
		"a tenant without its tenant id":  base(object{"execution": object{"tenants": []any{object{"namespace": "kodama-tenant-x"}}}}),
		"no runtime egress":               base(object{"execution": object{"runtime": object{"egress": []any{}}}}),
		"a runtime callback to postgres":  base(object{"execution": object{"runtime": object{"egress": []any{object{"name": "db", "cidr": "10.0.0.0/8", "ports": []any{object{"port": 5432}}}}}}}),
		"a runtime CA value in the chart": base(object{"execution": object{"runtime": object{"caPem": "-----BEGIN CERTIFICATE-----"}}}),
		"no installation id":              base(object{"installation": nil}),
		"a runtime role with a password":  base(object{"migration": object{"runtimeRole": "kodama:secret@host"}}),
	} {
		if err := validateChartValues(t, bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestChartSecretsStartupBudget keeps liveness from interrupting KMS startup.
func TestChartSecretsStartupBudget(t *testing.T) {
	values := chartValues(t, []string{"values-external.yaml"}, object{"secrets": object{"kms": object{"startupTimeout": production.MaxProbeTimeout.String()}}})
	if err := validateChartValues(t, values); err != nil {
		t.Fatal(err)
	}
	s := materialize(t, newChartRenderer(t, values).objects())
	p := asObject(s.container["startupProbe"])
	if text(dig(p, "httpGet", "path")) != "/healthz" {
		t.Fatal("startup probe must wait for the completed assembly")
	}
	budget := time.Duration(toInt(p["periodSeconds"])*toInt(p["failureThreshold"])) * time.Second
	if budget < production.MaxProbeTimeout+30*time.Second {
		t.Fatalf("startup probe budget %s is shorter than KMS plus dependency initialization", budget)
	}
}

// TestChartSelectsTheCoreAssemblyAndSuppliesItsSettings: the rendered server is
// the composed product, and every setting and secret file the core assembly
// reads is supplied by the chart. What the core assembly cannot serve is
// refused by name under the production environment, and nothing else is.
func TestChartSelectsTheCoreAssemblyAndSuppliesItsSettings(t *testing.T) {
	for _, p := range chartProfiles() {
		t.Run(p.name, func(t *testing.T) {
			values := chartValues(t, p.files, p.overlays...)
			if err := validateChartValues(t, values); err != nil {
				t.Fatal(err)
			}
			docs := newChartRenderer(t, values).objects()
			s := materialize(t, docs)

			// Explicit selection of the real assembly and of the environment.
			if got := stringList(s.container["args"]); !reflect.DeepEqual(got, []string{"server", "--assembly", "core"}) {
				t.Fatalf("server args %v: the chart must select the core assembly explicitly", got)
			}
			if s.env["KODAMA_ENV"] != "production" {
				t.Fatalf("KODAMA_ENV=%q: the chart's runtime is Kubernetes, the production environment", s.env["KODAMA_ENV"])
			}
			// The same arguments and environment parse to the core assembly.
			var out bytes.Buffer
			cfg, err := parseServerConfig(stringList(s.container["args"])[1:], s.getenv, &out)
			if err != nil || cfg.Assembly != AssemblyCore {
				t.Fatalf("the rendered arguments do not select the core assembly: %+v %v", cfg, err)
			}

			core, err := LoadCoreConfig(s.getenv, cfg)
			// Every refusal is a named fail-closed one; nothing is missing or invalid.
			matched := map[string]bool{}
			for _, e := range leaves(err) {
				ok := false
				for _, w := range p.want {
					if strings.Contains(e.Error(), w) {
						matched[w], ok = true, true
					}
				}
				if !ok {
					t.Errorf("unexpected configuration error: %v", e)
				}
			}
			for _, w := range p.want {
				if !matched[w] {
					t.Errorf("the server did not refuse %q (errors: %v)", w, err)
				}
			}
			if len(p.want) > 0 && err == nil {
				t.Fatal("a profile the core assembly cannot serve was accepted")
			}

			// The settings arrived: from the document, from the environment and from the files.
			doc := values
			rt := asObject(dig(doc, "execution", "runtime"))
			if core.Env != Production || core.InstallationID != text(dig(doc, "installation", "id")) || core.NATS.URL != text(dig(doc, "config", "nats", "url")) {
				t.Errorf("settings: env=%s installation=%q nats=%q", core.Env, core.InstallationID, core.NATS.URL)
			}
			if core.Runtime.ServerURL != text(rt["url"]) || core.Runtime.SigningGeneration != int64(toInt(rt["signingGeneration"])) {
				t.Errorf("runtime settings %+v", core.Runtime)
			}
			if core.Runtime.Kubernetes.APIURL != "https://10.96.0.1:443" {
				t.Errorf("the in-cluster API origin is %q", core.Runtime.Kubernetes.APIURL)
			}
			if core.Runtime.SigningKeyFile != s.hostPath("/secrets/runtime-signing-key") {
				t.Errorf("signing key file %q", core.Runtime.SigningKeyFile)
			}
			if len(core.CursorKey) < 32 || len(core.IdempotencyHashKey) < 32 {
				t.Error("the signing keys were not read from the runtime Secret")
			}
			if text(dig(doc, "config", "tls", "customCA")) == "true" || dig(doc, "config", "tls", "customCA") == true {
				if core.Git.CAFile != s.hostPath("/secrets/ca.crt") {
					t.Errorf("git CA file %q", core.Git.CAFile)
				}
			}
			if len(core.Git.Credentials) != 1 {
				t.Errorf("git credentials %+v", core.Git.Credentials)
			}
			if core.Secrets.Adapter != text(dig(doc, "secrets", "adapter")) {
				t.Fatal("secrets adapter was lost in configuration loading")
			}
			if k := core.Secrets.KMS; k != nil {
				if k.Provider != text(dig(doc, "secrets", "kms", "provider")) || k.Current != "v1" || k.Resources["v1"] != text(dig(doc, "secrets", "kms", "resources", "v1")) {
					t.Fatal("KMS key selection was lost in configuration loading")
				}
				if k.Provider == "scw" {
					b, err := os.ReadFile(s.hostPath(k.SCW.CredentialsFile))
					if err != nil || string(b) != "synthetic" {
						t.Fatalf("scw credential projection: %v", err)
					}
				}
			}

			// The execution policy is the chart's, validated by the executor's own constructor.
			pol := core.Runtime.Kubernetes.Policy
			if pol == nil {
				t.Fatal("no execution policy was supplied")
			}
			exec, err := kube.NewExecutionPolicy(*pol)
			if err != nil {
				t.Fatalf("execution policy: %v", err)
			}
			wantTenants := 0
			for _, tn := range asList(dig(doc, "execution", "tenants")) {
				wantTenants++
				id := text(asObject(tn)["tenantId"])
				tgt, err := exec.Tenant(domain.TenantID(id))
				if err != nil || tgt.Namespace != text(asObject(tn)["namespace"]) || tgt.ServiceAccount != text(dig(doc, "execution", "serviceAccountName")) {
					t.Errorf("tenant %s: %+v %v", id, tgt, err)
				}
			}
			if len(pol.Tenants) != wantTenants {
				t.Errorf("policy tenants %d, chart tenants %d", len(pol.Tenants), wantTenants)
			}
			if pol.RuntimeURL != text(rt["url"]) || !strings.HasPrefix(pol.InjectorImage, text(dig(doc, "image", "repository"))) || !strings.Contains(pol.InjectorImage, "@sha256:") {
				t.Errorf("policy origin %q image %q", pol.RuntimeURL, pol.InjectorImage)
			}
			if got, want := len(pol.RuntimeEgress), len(asList(rt["egress"]))+len(asList(dig(doc, "execution", "network", "egress"))); got != want {
				t.Errorf("policy egress rules %d, chart rules %d", got, want)
			}
			if pol.RunAsUser != int64(toInt(dig(doc, "execution", "job", "runAsUser"))) || pol.TerminationGrace != time.Duration(toInt(dig(doc, "execution", "job", "terminationGracePeriodSeconds")))*time.Second {
				t.Errorf("policy identity/grace: %d %s", pol.RunAsUser, pol.TerminationGrace)
			}
		})
	}
}

// TestChartKeysAreProjectedExactly: the Secret items the server mounts are the
// files the core assembly can read, and the migration Job gets the owner
// credential and only the runtime role's name.
func TestChartKeysAreProjectedExactly(t *testing.T) {
	docs := newChartRenderer(t, chartValues(t, []string{"values-external.yaml"}, staticS3)).objects()
	s := materialize(t, docs)
	got := append([]string(nil), s.secretKeys...)
	sort.Strings(got)
	want := []string{"cursor-key", "database-runtime-url", "git-credentials.json", "git-read-token", "git-write-token", "idempotency-hash-key", "nats-token", "runtime-ca.crt", "runtime-signing-key", "s3-access-key", "s3-secret-key"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projected keys\n got %v\nwant %v", got, want)
	}
	for _, d := range ofKind(docs, "Job") {
		spec := asObject(dig(d, "spec", "template", "spec"))
		c := asObject(asList(spec["containers"])[0])
		if got := stringList(c["args"]); !reflect.DeepEqual(got, []string{"migrate"}) {
			t.Errorf("migration args %v", got)
		}
		env := map[string]string{}
		for _, e := range asList(c["env"]) {
			env[text(asObject(e)["name"])] = text(asObject(e)["value"])
		}
		if env["KODAMA_DATABASE_RUNTIME_ROLE"] != "kodama_runtime" {
			t.Errorf("the migration Job does not name the runtime role: %v", env)
		}
		for _, v := range asList(spec["volumes"]) {
			for _, item := range asList(dig(asObject(v), "secret", "items")) {
				if k := text(asObject(item)["key"]); k == "database-runtime-url" {
					t.Error("the migration Job mounts the runtime credential")
				}
			}
		}
	}
}

// TestChartNamespacesPassTheServersPreflight: the tenant namespace, service
// account, quota and network policies the chart renders are accepted by the
// executor's own namespace preflight for the chart's execution policy, and a
// network shape the preflight cannot evaluate is refused (the control).
func TestChartNamespacesPassTheServersPreflight(t *testing.T) {
	values := chartValues(t, nil)
	docs := newChartRenderer(t, values).objects()
	s := materialize(t, docs)
	core, _ := LoadCoreConfig(s.getenv, Config{Assembly: AssemblyCore, ShutdownTimeout: time.Second})
	if core.Runtime.Kubernetes.Policy == nil {
		t.Fatal("no policy")
	}
	policy, err := kube.NewExecutionPolicy(*core.Runtime.Kubernetes.Policy)
	if err != nil {
		t.Fatal(err)
	}

	convert := func(d object, out any) {
		t.Helper()
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v", nameOf(d), err)
		}
	}
	fake := kubetest.NewFake()
	for _, d := range ofKind(docs, "Namespace") {
		var ns kubernetes.Namespace
		convert(d, &ns)
		ns.Status.Phase = "Active"
		fake.Namespaces[ns.Metadata.Name] = ns
	}
	for _, d := range ofKind(docs, "ServiceAccount") {
		if ns := text(dig(d, "metadata", "namespace")); ns != "" {
			var sa kubernetes.ServiceAccount
			convert(d, &sa)
			fake.ServiceAccounts[ns+"/"+sa.Metadata.Name] = sa
		}
	}
	for _, d := range ofKind(docs, "ResourceQuota") {
		hard := object{}
		for k, v := range asObject(dig(d, "spec", "hard")) {
			hard[k] = fmt.Sprint(v)
		}
		d["spec"] = object{"hard": hard}
		var q kubernetes.ResourceQuota
		convert(d, &q)
		ns := text(dig(d, "metadata", "namespace"))
		fake.Quotas[ns] = append(fake.Quotas[ns], q)
	}
	for _, d := range ofKind(docs, "NetworkPolicy") {
		var np kubernetes.NetworkPolicy
		convert(d, &np)
		ns := text(dig(d, "metadata", "namespace"))
		fake.NetPols[ns] = append(fake.NetPols[ns], np)
	}
	check := func(tenant string) error {
		return kube.NewPreflight(fake, policy, 0, nil).Check(context.Background(), domain.TenantID(tenant))
	}
	for _, tn := range asList(dig(values, "execution", "tenants")) {
		id := text(asObject(tn)["tenantId"])
		if err := check(id); err != nil {
			t.Errorf("tenant %s: the chart's namespace is refused by the preflight: %v", id, err)
		}
	}

	// Controls: each breaks one property the chart provides.
	id := text(asObject(asList(dig(values, "execution", "tenants"))[0])["tenantId"])
	ns := text(asObject(asList(dig(values, "execution", "tenants"))[0])["namespace"])
	t.Run("a DNS-style selector peer is refused", func(t *testing.T) {
		saved := fake.NetPols[ns]
		defer func() { fake.NetPols[ns] = saved }()
		var np kubernetes.NetworkPolicy
		np.Metadata.Name = "allow-dns"
		np.Spec.PolicyTypes = []string{"Egress"}
		np.Spec.Egress = []kubernetes.NetworkEgress{{
			To:    []kubernetes.NetworkPeer{{NamespaceSelector: &kubernetes.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}}},
			Ports: []kubernetes.NetworkPort{{Protocol: "UDP", Port: float64(53)}},
		}}
		fake.NetPols[ns] = append(append([]kubernetes.NetworkPolicy(nil), saved...), np)
		if err := check(id); err == nil {
			t.Fatal("the preflight accepted an egress rule that is not an address block")
		}
	})
	t.Run("a namespace without its tenant binding is refused", func(t *testing.T) {
		saved := fake.Namespaces[ns]
		defer func() { fake.Namespaces[ns] = saved }()
		n := saved
		n.Metadata.Annotations = map[string]string{}
		fake.Namespaces[ns] = n
		if err := check(id); err == nil {
			t.Fatal("the preflight accepted a namespace not bound to its tenant")
		}
	})
}

// TestChartSettingsDocumentIsStrict: the document is read strictly and the
// environment always wins over it.
func TestChartSettingsDocumentIsStrict(t *testing.T) {
	docs := newChartRenderer(t, chartValues(t, nil)).objects()
	var good string
	for _, cm := range ofKind(docs, "ConfigMap") {
		if strings.HasSuffix(nameOf(cm), "-config") {
			good = text(dig(cm, "data", "config.yaml"))
		}
	}
	if good == "" {
		t.Fatal("no settings document rendered")
	}
	if _, err := ParseChartConfig([]byte(good)); err != nil {
		t.Fatalf("the rendered document does not parse: %v", err)
	}
	for name, mutate := range map[string]func(string) string{
		"an unknown member":      func(s string) string { return s + "surprise: true\n" },
		"another version":        func(s string) string { return strings.Replace(s, "version: 2", "version: 1", 1) },
		"a second document":      func(s string) string { return s + "---\nversion: 1\n" },
		"a misspelled setting":   func(s string) string { return strings.Replace(s, "installationId:", "installationID:", 1) },
		"a credential member":    func(s string) string { return strings.Replace(s, "nats:\n", "nats:\n  token: abc\n", 1) },
		"a nested unknown field": func(s string) string { return strings.Replace(s, "secrets:\n", "secrets:\n  keyFile: /x\n", 1) },
	} {
		if _, err := ParseChartConfig([]byte(mutate(good))); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime-ca.crt"), []byte(synthSecrets(t)["runtime-ca.crt"]), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{ConfigFileVariable: path, "KODAMA_SECRETS_DIR": dir, "KODAMA_NATS_URL": "nats://override:4222"}
	lookup, pol, err := withConfigFile(func(k string) string { return env[k] }, dir)
	if err != nil {
		t.Fatal(err)
	}
	if lookup("KODAMA_NATS_URL") != "nats://override:4222" {
		t.Error("the document replaced a variable that was set")
	}
	if lookup("KODAMA_INSTALLATION_ID") != "kodama-example" || pol == nil {
		t.Errorf("the document did not supply its settings: %q %v", lookup("KODAMA_INSTALLATION_ID"), pol)
	}
	// The document never selects the environment or the assembly.
	if lookup("KODAMA_ENV") != "" || lookup("KODAMA_ASSEMBLY") != "" {
		t.Error("the document selected the environment or the assembly")
	}

	doc, _ := ParseChartConfig([]byte(good))
	hosted := doc
	hosted.Authority = "hosted"
	if _, err := hosted.Environment(dir); err == nil {
		t.Error("a hosted authority was accepted")
	}
	unknown := doc
	unknown.Secrets.Adapter = "vault"
	if err := unknown.Secrets.Validate(Production); err == nil {
		t.Error("an unknown key provider was accepted")
	}
	noCA := doc
	noCA.Runtime.CAFile = "../escape"
	if _, err := noCA.Policy(dir); err == nil {
		t.Error("a runtime CA outside the secrets directory was accepted")
	}
	plain := doc
	plain.Runtime.URL = "http://kodama.example.invalid"
	if _, err := plain.Policy(dir); err == nil {
		t.Error("a plain-http runtime origin was accepted")
	}
	if _, _, err := withConfigFile(func(k string) string {
		if k == ConfigFileVariable {
			return filepath.Join(dir, "missing.yaml")
		}
		return ""
	}, dir); err == nil {
		t.Error("a missing settings document was ignored")
	}
}
