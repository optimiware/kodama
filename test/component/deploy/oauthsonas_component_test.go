//go:build component

// Rendered-chart checks for the optional OAuthSonas development identity
// provider. The internal/installation twin inspects the chart as text; these
// tests render the real chart with helm, so a template edit that makes the
// provider default-on, exposes it, or sources its access code from a value is
// caught by the same command the operator runs.
//
// The chart is copied into a test-owned temporary directory and its file
// dependency is built there (stageStackChart), so nothing is written into the
// repository. helm runs locally; nothing contacts a cluster or network.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// oauthSonasBaseValues is a minimal, valid OAuthSonas configuration document.
// OAuthSonas has a single string claim: stage maps the tenant and project
// claims to org_id, and the project is named like the tenant.
const oauthSonasBaseValues = `kodama:
  config:
    extraEnv:
      KODAMA_OIDC_ISSUER: https://issuer.example
      KODAMA_OIDC_AUDIENCE: kodama
      KODAMA_OIDC_JWKS_URL: https://issuer.example/jwks
      KODAMA_OIDC_TENANT_CLAIM: org_id
oauthsonas:
  enabled: true
  config: |
    issuer: https://oauthsonas.kodama-system.svc.cluster.local:8443
    clients:
      - id: kodactl
        redirect_uris:
          - http://127.0.0.1:52790/callback
    personas:
      - sub: tenant-a-admin
        claims:
          org_id: tenant-a
`

// oauthSonasURLSentinel is a value that must never appear in a rendered
// argument, literal environment value or values-provided access code.
const oauthSonasCodeSentinel = "OAUTHSONAS-LITERAL-CODE-SENTINEL"

// oauthSonasValuesFile writes a values document and returns its path.
func oauthSonasValuesFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oauthsonas-values.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write oauthsonas values: %v", err)
	}
	return path
}

// oauthSonasRender renders the stack chart with an optional values file and
// optional raw helm arguments.
func oauthSonasRender(t *testing.T, helm, chart, namespace, valuesFile string, extra ...string) (string, error) {
	t.Helper()
	args := []string{"template", "kodama-stack", chart, "--namespace", namespace}
	if valuesFile != "" {
		args = append(args, "-f", valuesFile)
	}
	args = append(args, productionOIDCArgs("kodama.")...)
	args = append(args, extra...)
	command := exec.Command(helm, args...)
	command.Dir = chart
	output, err := command.CombinedOutput()
	return string(output), err
}

// oauthSonasDocument returns the rendered document with the given kind and
// name, or "".
func oauthSonasDocument(rendered, kind, name string) string {
	for document := range strings.SplitSeq(rendered, "\n---") {
		if !regexp.MustCompile(`(?m)^kind:\s*` + regexp.QuoteMeta(kind) + `\s*$`).MatchString(document) {
			continue
		}
		if !regexp.MustCompile(`(?m)^\s+name:\s*` + regexp.QuoteMeta(name) + `\s*$`).MatchString(document) {
			continue
		}
		return document
	}
	return ""
}

// oauthSonasImages returns every container image reference in a document.
func oauthSonasImages(document string) []string {
	var images []string
	for _, match := range regexp.MustCompile(`(?m)^\s+image:\s*(\S+)\s*$`).FindAllStringSubmatch(document, -1) {
		images = append(images, strings.Trim(match[1], `"'`))
	}
	return images
}

// assertOAuthSonasHardened applies the container-level invariants to one
// rendered Deployment.
func assertOAuthSonasHardened(t *testing.T, label, deployment string) {
	t.Helper()
	if deployment == "" {
		t.Fatalf("%s: rendered output has no oauthsonas Deployment", label)
	}
	for _, want := range []string{
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"type: RuntimeDefault",
	} {
		if !strings.Contains(deployment, want) {
			t.Errorf("%s: oauthsonas Deployment lacks %q", label, want)
		}
	}
	if got := strings.Count(deployment, "allowPrivilegeEscalation: false"); got < 2 {
		t.Errorf("%s: allowPrivilegeEscalation: false appears %d times, want both containers", label, got)
	}
	if got := strings.Count(deployment, "readOnlyRootFilesystem: true"); got < 2 {
		t.Errorf("%s: readOnlyRootFilesystem: true appears %d times, want both containers", label, got)
	}
	if got := regexp.MustCompile(`drop:[ \t]*\[[ \t]*"ALL"[ \t]*\]`).FindAllString(deployment, -1); len(got) < 2 {
		t.Errorf("%s: capabilities drop ALL appears %d times, want both containers", label, len(got))
	}
	for _, forbidden := range []string{
		"privileged: true",
		"hostPath:",
		"hostPort",
		"hostNetwork",
		"readOnlyRootFilesystem: false",
		"allowPrivilegeEscalation: true",
		"runAsNonRoot: false",
		"runAsUser: 0",
	} {
		if strings.Contains(deployment, forbidden) {
			t.Errorf("%s: oauthsonas Deployment contains %q", label, forbidden)
		}
	}
	env := oauthSonasEnvBlock(deployment, "OAUTHSONAS_ACCESS_CODE")
	if env == "" {
		t.Fatalf("%s: oauthsonas Deployment has no OAUTHSONAS_ACCESS_CODE environment entry", label)
	}
	for _, want := range []string{"secretKeyRef:", `name: "oauthsonas-access"`, `key: "access_code"`} {
		if !strings.Contains(env, want) {
			t.Errorf("%s: OAUTHSONAS_ACCESS_CODE does not come from oauthsonas-access/access_code (%q missing):\n%s", label, want, env)
		}
	}
	if regexp.MustCompile(`(?m)^\s+value:`).MatchString(env) {
		t.Errorf("%s: OAUTHSONAS_ACCESS_CODE has a literal value:\n%s", label, env)
	}
}

// oauthSonasEnvBlock returns the YAML stanza of one environment variable.
func oauthSonasEnvBlock(document, variable string) string {
	lines := strings.Split(document, "\n")
	start := -1
	for index, line := range lines {
		if strings.Contains(line, "name: "+variable) {
			start = index
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for index := start + 1; index < len(lines); index++ {
		trimmed := strings.TrimSpace(lines[index])
		if strings.HasPrefix(trimmed, "- name:") || trimmed == "volumeMounts:" || trimmed == "resources:" || trimmed == "ports:" {
			end = index
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// TestOAuthSonasChartDisabledRendersNothing pins opt-in rendering with the
// real template engine: the default chart, and the chart with disabled plus an
// otherwise-valid configuration, contain no oauthsonas object. A disabled
// stack upgrade therefore must not recreate or delete a stage identity
// provider.
func TestOAuthSonasChartDisabledRendersNothing(t *testing.T) {
	helm, chart := stageStackChart(t)

	rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", "")
	if err != nil {
		t.Fatalf("helm template default values failed: %v\n%s", err, rendered)
	}
	if strings.Contains(rendered, "oauthsonas") {
		t.Errorf("default render mentions oauthsonas; the provider must be opt-in:\n%s", rendered)
	}

	disabled := oauthSonasValuesFile(t, strings.Replace(oauthSonasBaseValues, "enabled: true", "enabled: false", 1))
	rendered, err = oauthSonasRender(t, helm, chart, "kodama-system", disabled)
	if err != nil {
		t.Fatalf("helm template with oauthsonas disabled failed: %v\n%s", err, rendered)
	}
	if strings.Contains(rendered, "oauthsonas") {
		t.Errorf("disabled render mentions oauthsonas; the template must render nothing:\n%s", rendered)
	}
}

// TestOAuthSonasChartEnabledIsClusterIPOnly renders the provider and pins the
// reachability surface: a ClusterIP Service, no Ingress and no node/host
// networking.
func TestOAuthSonasChartEnabledIsClusterIPOnly(t *testing.T) {
	helm, chart := stageStackChart(t)
	values := oauthSonasValuesFile(t, oauthSonasBaseValues)
	rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values)
	if err != nil {
		t.Fatalf("helm template enabled oauthsonas failed: %v\n%s", err, rendered)
	}

	for _, object := range []struct{ kind, name string }{
		{"ConfigMap", "oauthsonas-config"},
		{"ServiceAccount", "oauthsonas"},
		{"Deployment", "oauthsonas"},
		{"Service", "oauthsonas"},
	} {
		if oauthSonasDocument(rendered, object.kind, object.name) == "" {
			t.Errorf("enabled render has no %s named %s", object.kind, object.name)
		}
	}
	if strings.Contains(rendered, "kind: Ingress") {
		t.Errorf("enabled render contains an Ingress; OAuthSonas is ClusterIP-only")
	}
	service := oauthSonasDocument(rendered, "Service", "oauthsonas")
	for _, forbidden := range []string{"type: NodePort", "type: LoadBalancer", "hostPort", "hostNetwork", "externalIPs"} {
		if strings.Contains(service, forbidden) {
			t.Errorf("oauthsonas Service contains %q", forbidden)
		}
	}
	if !regexp.MustCompile(`(?m)^\s+type:\s*ClusterIP\s*$`).MatchString(service) {
		t.Errorf("oauthsonas Service is not type ClusterIP:\n%s", service)
	}
	if !strings.Contains(service, "port: 8443") {
		t.Errorf("oauthsonas Service does not expose the TLS port 8443:\n%s", service)
	}
	if !strings.Contains(service, "targetPort: https") {
		t.Errorf("oauthsonas Service does not target the named https port:\n%s", service)
	}
}

// TestOAuthSonasChartImagesPinnedByDigest pins both containers to digests.
func TestOAuthSonasChartImagesPinnedByDigest(t *testing.T) {
	helm, chart := stageStackChart(t)
	values := oauthSonasValuesFile(t, oauthSonasBaseValues)
	rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values)
	if err != nil {
		t.Fatalf("helm template enabled oauthsonas failed: %v\n%s", err, rendered)
	}
	deployment := oauthSonasDocument(rendered, "Deployment", "oauthsonas")
	images := oauthSonasImages(deployment)
	if len(images) != 2 {
		t.Fatalf("oauthsonas Deployment has %d container images, want 2:\n%s", len(images), deployment)
	}
	digest := regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
	for _, image := range images {
		if !digest.MatchString(image) {
			t.Errorf("oauthsonas image %q is not pinned by digest", image)
		}
	}
}

// TestOAuthSonasChartHardensPods pins pod and container hardening.
func TestOAuthSonasChartHardensPods(t *testing.T) {
	helm, chart := stageStackChart(t)
	values := oauthSonasValuesFile(t, oauthSonasBaseValues)
	rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values)
	if err != nil {
		t.Fatalf("helm template enabled oauthsonas failed: %v\n%s", err, rendered)
	}
	if !strings.Contains(rendered, "automountServiceAccountToken: false") {
		t.Errorf("rendered oauthsonas objects do not disable service account token automount")
	}
	assertOAuthSonasHardened(t, "enabled", oauthSonasDocument(rendered, "Deployment", "oauthsonas"))
	if !strings.Contains(oauthSonasDocument(rendered, "Deployment", "oauthsonas"), "serviceAccountName: oauthsonas") {
		t.Errorf("oauthsonas Deployment does not use its own ServiceAccount")
	}
	deployment := oauthSonasDocument(rendered, "Deployment", "oauthsonas")
	if !regexp.MustCompile(`(?ms)^\s*strategy:\s*\n\s*type:\s*RollingUpdate\s*\n\s*rollingUpdate:\s*\n\s*maxSurge:\s*0\s*\n\s*maxUnavailable:\s*1\s*$`).MatchString(deployment) {
		t.Errorf("oauthsonas Deployment does not render RollingUpdate with maxSurge 0 and maxUnavailable 1; one replica must never overlap two pods with different signing keys:\n%s", deployment)
	}
	if !strings.Contains(deployment, `"/usr/local/bin/oauthsonas", "--healthcheck", "--listen", "127.0.0.1:8181"`) {
		t.Errorf("oauthsonas Deployment has no --healthcheck liveness/readiness probe:\n%s", deployment)
	}
	if !regexp.MustCompile(`(?m)^\s+replicas:\s*1\s*$`).MatchString(deployment) {
		t.Errorf("oauthsonas Deployment does not pin exactly one replica (its signing key is in memory):\n%s", deployment)
	}
	if !strings.Contains(deployment, "checksum/config:") {
		t.Errorf("oauthsonas Deployment has no config checksum annotation; a host/port or config change would not roll the pod:\n%s", deployment)
	}
	if strings.Contains(deployment, "OAUTHSONAS_ALLOW_NON_LOOPBACK") {
		t.Errorf("oauthsonas Deployment acknowledges non-loopback exposure although it listens on 127.0.0.1")
	}
}

// TestOAuthSonasChartUsesTLSSidecarAndA2SecretNames pins the TLS path: a Caddy
// sidecar terminates TLS with the certificate bootstrap-secrets issues from
// the installation CA into oauthsonas-tls, and the provider listens only on
// loopback.
func TestOAuthSonasChartUsesTLSSidecarAndA2SecretNames(t *testing.T) {
	helm, chart := stageStackChart(t)
	values := oauthSonasValuesFile(t, oauthSonasBaseValues)
	rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values)
	if err != nil {
		t.Fatalf("helm template enabled oauthsonas failed: %v\n%s", err, rendered)
	}
	configMap := oauthSonasDocument(rendered, "ConfigMap", "oauthsonas-config")
	if !strings.Contains(configMap, "tls /etc/oauthsonas-tls/tls.crt /etc/oauthsonas-tls/tls.key") {
		t.Errorf("Caddyfile does not load the oauthsonas-tls certificate:\n%s", configMap)
	}
	if !strings.Contains(configMap, "reverse_proxy 127.0.0.1:8181") {
		t.Errorf("Caddyfile does not proxy to the loopback listener:\n%s", configMap)
	}
	if !strings.Contains(configMap, "issuer: https://oauthsonas.kodama-system.svc.cluster.local:8443") {
		t.Errorf("rendered OAuthSonas config does not carry the https issuer:\n%s", configMap)
	}

	deployment := oauthSonasDocument(rendered, "Deployment", "oauthsonas")
	if !strings.Contains(deployment, `args: ["--config", "/etc/oauthsonas/config.yaml", "--listen", "127.0.0.1:8181"]`) {
		t.Errorf("oauthsonas container does not run the config file on the loopback listener:\n%s", deployment)
	}
	for _, want := range []string{
		`secretName: "oauthsonas-tls"`,
		"key: tls.crt",
		"key: tls.key",
		"mountPath: /etc/oauthsonas-tls",
		"readOnly: true",
	} {
		if !strings.Contains(deployment, want) {
			t.Errorf("oauthsonas Deployment lacks %q:\n%s", want, deployment)
		}
	}
	if strings.Contains(rendered, oauthSonasCodeSentinel) {
		t.Errorf("rendered output contains the literal access-code sentinel")
	}
}

// TestOAuthSonasChartRefusesLiteralAccessCodeFromValues pins credential
// custody at render time. A value, a config document or an empty Secret
// reference must abort the render instead of producing a provider whose code
// is visible in Helm history.
func TestOAuthSonasChartRefusesLiteralAccessCodeFromValues(t *testing.T) {
	helm, chart := stageStackChart(t)
	values := oauthSonasValuesFile(t, oauthSonasBaseValues)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "accessCode value",
			args: []string{"--set", "oauthsonas.accessCode=" + oauthSonasCodeSentinel},
			want: "accessCode",
		},
		{
			name: "access_code value",
			args: []string{"--set", "oauthsonas.access_code=" + oauthSonasCodeSentinel},
			want: "access_code",
		},
		{
			name: "accessCodeFile value",
			args: []string{"--set", "oauthsonas.accessCodeFile=/tmp/code"},
			want: "accessCodeFile",
		},
		{
			name: "empty accessCodeSecret",
			args: []string{"--set", "oauthsonas.accessCodeSecret="},
			want: "accessCodeSecret",
		},
		{
			name: "empty accessCodeKey",
			args: []string{"--set", "oauthsonas.accessCodeKey="},
			want: "accessCodeKey",
		},
		{
			name: "empty tlsSecret",
			args: []string{"--set", "oauthsonas.tlsSecret="},
			want: "tlsSecret",
		},
		{
			name: "access code inside config",
			args: []string{"--set-string", "oauthsonas.config=issuer: https://oauthsonas.kodama-system.svc.cluster.local:8443\naccess_code: " + oauthSonasCodeSentinel},
			want: "access code",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values, testCase.args...)
			if err == nil {
				t.Fatalf("render accepted %s:\n%s", testCase.name, rendered)
			}
			if !strings.Contains(strings.ToLower(rendered), strings.ToLower(testCase.want)) {
				t.Errorf("render refused %s for an unexpected reason (want %q): %v\n%s", testCase.name, testCase.want, err, rendered)
			}
			if strings.Contains(rendered, oauthSonasCodeSentinel) {
				t.Errorf("refusal for %s echoed the access-code sentinel:\n%s", testCase.name, rendered)
			}
		})
	}
}

// TestOAuthSonasChartRefusesTagOnlyImages pins the digest guard at render time.
func TestOAuthSonasChartRefusesTagOnlyImages(t *testing.T) {
	helm, chart := stageStackChart(t)
	values := oauthSonasValuesFile(t, oauthSonasBaseValues)
	for _, testCase := range []struct{ flag, message string }{
		{"oauthsonas.image=ghcr.io/optimiware/oauthsonas:1.5.0", "sha256"},
		{"oauthsonas.tlsProxyImage=docker.io/library/caddy:2.11.9-alpine", "sha256"},
	} {
		rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values, "--set", testCase.flag)
		if err == nil {
			t.Errorf("render accepted a tag-only image override %q:\n%s", testCase.flag, rendered)
			continue
		}
		if !strings.Contains(strings.ToLower(rendered), testCase.message) {
			t.Errorf("render refused %q for an unexpected reason: %v\n%s", testCase.flag, err, rendered)
		}
	}
}

// TestOAuthSonasChartRefusesNonHTTPSIssuer pins the https-only issuer and the
// exact host/port binding the certificate covers.
func TestOAuthSonasChartRefusesNonHTTPSIssuer(t *testing.T) {
	helm, chart := stageStackChart(t)
	cases := []struct {
		name   string
		values string
		args   []string
		want   string
	}{
		{
			name:   "http issuer",
			values: strings.Replace(oauthSonasBaseValues, "issuer: https://", "issuer: http://", 1),
			want:   "https",
		},
		{
			name:   "missing issuer",
			values: "oauthsonas:\n  enabled: true\n  config: |\n    clients: []\n",
			want:   "issuer",
		},
		{
			name:   "port mismatch",
			values: oauthSonasBaseValues,
			args:   []string{"--set", "oauthsonas.port=9443"},
			want:   "issuer",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values := oauthSonasValuesFile(t, testCase.values)
			rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values, testCase.args...)
			if err == nil {
				t.Fatalf("render accepted %s:\n%s", testCase.name, rendered)
			}
			if !strings.Contains(strings.ToLower(rendered), strings.ToLower(testCase.want)) {
				t.Errorf("render refused %s for an unexpected reason (want %q): %v\n%s", testCase.name, testCase.want, err, rendered)
			}
		})
	}
}

// TestOAuthSonasChartRefusesBadHostPortAndNullValues pins the remaining
// render guards: the provider runs as non-root, so a privileged TLS port can
// never bind; a host that is not a lower-case DNS name would inject directives
// into the Caddy site address; and a null oauthsonas value must fail with a
// clear message instead of a nil-pointer rendering error.
func TestOAuthSonasChartRefusesBadHostPortAndNullValues(t *testing.T) {
	helm, chart := stageStackChart(t)
	values := oauthSonasValuesFile(t, oauthSonasBaseValues)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"privileged port", []string{"--set", "oauthsonas.port=80"}, "1,024"},
		{"bad host", []string{"--set", "oauthsonas.host=Bad Host"}, "host"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values, testCase.args...)
			if err == nil {
				t.Fatalf("render accepted %s:\n%s", testCase.name, rendered)
			}
			if !strings.Contains(strings.ToLower(rendered), strings.ToLower(testCase.want)) {
				t.Errorf("render refused %s for an unexpected reason (want %q): %v\n%s", testCase.name, testCase.want, err, rendered)
			}
			if strings.Contains(rendered, "nil pointer") {
				t.Errorf("render failed with a nil-pointer message:\n%s", rendered)
			}
		})
	}

	// A null oauthsonas value is treated as disabled, never as a rendering
	// crash: the chart must not evaluate fields of a nil map.
	rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values, "--set-json", "oauthsonas=null")
	if err != nil {
		t.Fatalf("render failed for oauthsonas=null: %v\n%s", err, rendered)
	}
	if strings.Contains(rendered, "name: oauthsonas") || strings.Contains(rendered, "kind: Ingress") {
		t.Errorf("oauthsonas=null still rendered a provider object:\n%s", rendered)
	}
	if strings.Contains(rendered, "nil pointer") {
		t.Errorf("render failed with a nil-pointer message:\n%s", rendered)
	}
}

// TestOAuthSonasChartHasNoExposureKnobs pins the absence of an Ingress,
// NodePort, LoadBalancer or node-networking override in the values contract.
func TestOAuthSonasChartHasNoExposureKnobs(t *testing.T) {
	helm, chart := stageStackChart(t)
	values := oauthSonasValuesFile(t, oauthSonasBaseValues)
	for _, flag := range []string{
		"oauthsonas.service.type=NodePort",
		"oauthsonas.ingress.enabled=true",
		"oauthsonas.nodePort=30000",
		"oauthsonas.hostNetwork=true",
	} {
		rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values, "--set", flag)
		if err == nil {
			t.Errorf("render accepted unsupported exposure knob %q:\n%s", flag, rendered)
		}
	}
}

// TestOAuthSonasChartHostFollowsReleaseNamespace pins the default issuer host
// to the certificate SAN that bootstrap-secrets issues for the release
// namespace.
func TestOAuthSonasChartHostFollowsReleaseNamespace(t *testing.T) {
	helm, chart := stageStackChart(t)
	values := oauthSonasValuesFile(t, strings.Replace(oauthSonasBaseValues, "oauthsonas.kodama-system.svc.cluster.local", "oauthsonas.preview.svc.cluster.local", 1))
	rendered, err := oauthSonasRender(t, helm, chart, "preview", values)
	if err != nil {
		t.Fatalf("helm template in preview namespace failed: %v\n%s", err, rendered)
	}
	configMap := oauthSonasDocument(rendered, "ConfigMap", "oauthsonas-config")
	if !strings.Contains(configMap, "issuer: https://oauthsonas.preview.svc.cluster.local:8443") {
		t.Errorf("preview render does not carry the preview issuer:\n%s", configMap)
	}
	if !strings.Contains(configMap, "https://oauthsonas.preview.svc.cluster.local:8443 {") {
		t.Errorf("Caddyfile does not serve the preview issuer host:\n%s", configMap)
	}

	// The kodama-system issuer must be refused in another namespace: the
	// certificate SAN and the issuer would no longer agree.
	base := oauthSonasValuesFile(t, oauthSonasBaseValues)
	if rendered, err := oauthSonasRender(t, helm, chart, "preview", base); err == nil {
		t.Errorf("render accepted a kodama-system issuer in the preview namespace:\n%s", rendered)
	}
}

// TestOAuthSonasChartRefusesInlineClientSecret proves inline client secrets are
// refused in oauthsonas.config (which renders into a ConfigMap), both by
// values.schema.json and by the template fail guard, without echoing the
// credential value.
func TestOAuthSonasChartRefusesInlineClientSecret(t *testing.T) {
	helm, chart := stageStackChart(t)
	const secretSentinel = "OAUTHSONAS-INLINE-CLIENT-SECRET-SENTINEL"

	for _, testCase := range []struct {
		name  string
		field string
	}{
		{name: "unquoted secret", field: "secret: " + secretSentinel},
		{name: "double-quoted secret", field: `secret: "` + secretSentinel + `"`},
		{name: "single-quoted secret", field: "secret: '" + secretSentinel + "'"},
		{name: "client_secret key", field: "client_secret: " + secretSentinel},
		{name: "clientSecret key", field: "clientSecret: " + secretSentinel},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			doc := strings.Replace(oauthSonasBaseValues, "- id: kodactl\n", "- id: kodactl\n        "+testCase.field+"\n", 1)
			values := oauthSonasValuesFile(t, doc)

			rendered, err := oauthSonasRender(t, helm, chart, "kodama-system", values)
			if err == nil {
				t.Fatalf("render accepted %s:\n%s", testCase.name, rendered)
			}
			if strings.Contains(rendered, secretSentinel) {
				t.Errorf("schema refusal echoed the inline client secret sentinel:\n%s", rendered)
			}

			rendered, err = oauthSonasRender(t, helm, chart, "kodama-system", values, "--skip-schema-validation")
			if err == nil {
				t.Fatalf("render --skip-schema-validation accepted %s:\n%s", testCase.name, rendered)
			}
			lower := strings.ToLower(rendered)
			if !strings.Contains(lower, "inline client secrets are forbidden") || !strings.Contains(lower, "configmap") {
				t.Errorf("template refusal for %s lacked expected ConfigMap/inline-secret message: %v\n%s", testCase.name, err, rendered)
			}
			if strings.Contains(rendered, secretSentinel) {
				t.Errorf("template refusal echoed the inline client secret sentinel:\n%s", rendered)
			}
		})
	}
}
