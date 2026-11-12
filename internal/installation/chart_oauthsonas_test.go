package installation

// The optional OAuthSonas development identity provider in the kodama-stack
// chart.
//
// OAuthSonas exists so stage and preview installations can sign in with a
// persona picker and scripts/oauthsonas-token can mint an access token without
// a browser. It is a development identity provider, never a production one,
// and it is dangerous in three specific ways this file pins as text:
//
//  1. It must be opt-in. `oauthsonas.enabled` defaults to false and the whole
//     template body is gated, so the default render contains no oauthsonas
//     object at all. Deploying the default chart must not delete a stage
//     identity provider either: a disabled template renders nothing rather
//     than an empty/removed workload.
//  2. Its access code gates the persona picker. The code belongs only in the
//     `oauthsonas-access` Secret (`access_code` key) that
//     scripts/bootstrap-secrets creates; a value, a command argument or a
//     literal environment value would be visible in values files, Helm
//     history, `ps` and /proc, so the chart refuses them.
//  3. Kodama accepts only https issuers. The provider listens on loopback and
//     a Caddy sidecar terminates TLS with the certificate bootstrap-secrets
//     issues from the installation CA into the `oauthsonas-tls` Secret
//     (tls.crt/tls.key). The issuer therefore has to be exactly
//     https://<host>:<port>.
//
// The rendered-chart twin of these checks is
// test/component/deploy/oauthsonas_component_test.go, which runs helm. This
// file stays text-only so `make check` needs neither helm nor processes.

import (
	"fmt"
	"maps"
	"regexp"
	"strings"
	"testing"
)

// oauthsonasTemplateBody reads the provider template as text.
func oauthsonasTemplateBody(t *testing.T) string {
	t.Helper()
	return readText(t, stackChartPath(t, "templates", "oauthsonas.yaml"))
}

// oauthsonasValuesBody reads the stack chart values.
func oauthsonasValuesBody(t *testing.T) string {
	t.Helper()
	return readText(t, stackChartPath(t, "values.yaml"))
}

// oauthsonasNOTESBody reads the install-time notes.
func oauthsonasNOTESBody(t *testing.T) string {
	t.Helper()
	return readText(t, stackChartPath(t, "templates", "NOTES.txt"))
}

// imageDigestPattern is the only accepted image reference form: a repository
// (optionally tagged) pinned by a full sha256 digest.
var imageDigestPattern = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// oauthsonasImageValues returns the image and TLS-proxy image literals from
// values.yaml. It is a tiny line scanner on purpose: the chart is text-checked
// without a YAML dependency, like the rest of internal/installation.
func oauthsonasImageValues(t *testing.T, values string) map[string]string {
	t.Helper()
	images := make(map[string]string)
	inOAuth := false
	for line := range strings.SplitSeq(values, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "oauthsonas:" {
			inOAuth = true
			continue
		}
		if inOAuth && trimmed != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inOAuth = false
		}
		if !inOAuth {
			continue
		}
		for _, key := range []string{"image", "tlsProxyImage"} {
			if after, ok := strings.CutPrefix(trimmed, key+":"); ok {
				images[key] = strings.TrimSpace(after)
			}
		}
	}
	return images
}

// TestStackOAuthSonasDisabledByDefault pins the opt-in gate: the shipped
// default is false and the template body only starts after the gate. A default
// `helm upgrade` must not create, replace or delete any identity provider.
func TestStackOAuthSonasDisabledByDefault(t *testing.T) {
	values := oauthsonasValuesBody(t)
	requireStackPattern(t, values, `(?ms)^oauthsonas:[ \t]*\r?\n(?:[ \t]+.*\r?\n)*?[ \t]*enabled:[ \t]*false[ \t]*$`, "values.yaml oauthsonas.enabled default")

	template := oauthsonasTemplateBody(t)
	before, _, ok := strings.Cut(template, "if .Values.oauthsonas.enabled")
	if !ok {
		t.Fatalf("templates/oauthsonas.yaml has no `if .Values.oauthsonas.enabled` gate")
	}
	if count := strings.Count(template, "if .Values.oauthsonas.enabled"); count != 1 {
		t.Errorf("templates/oauthsonas.yaml gates the body %d times, want exactly 1", count)
	}
	if strings.Contains(before, "kind:") {
		t.Errorf("templates/oauthsonas.yaml renders a Kubernetes object before the enabled gate; a disabled provider would still be created")
	}
	if !strings.Contains(template, "{{- end }}") && !strings.Contains(template, "{{- end -}}") {
		t.Errorf("templates/oauthsonas.yaml does not close the enabled gate")
	}

	notes := oauthsonasNOTESBody(t)
	if !strings.Contains(notes, "if .Values.oauthsonas.enabled") {
		t.Errorf("templates/NOTES.txt is not gated on oauthsonas.enabled")
	}
}

// TestStackOAuthSonasClusterIPOnly pins the service surface: the provider is
// reached by the Kodama server and the token script inside the cluster. An
// Ingress, NodePort or LoadBalancer would publish a development identity
// provider that accepts an access code; the template must have no such knob.
func TestStackOAuthSonasClusterIPOnly(t *testing.T) {
	template := oauthsonasTemplateBody(t)
	for _, forbidden := range []string{
		"kind: Ingress",
		"NodePort",
		"LoadBalancer",
		"hostPort",
		"hostNetwork",
		"hostPID",
		"hostIPC",
		"externalIPs",
		"externalTrafficPolicy",
	} {
		if strings.Contains(template, forbidden) {
			t.Errorf("templates/oauthsonas.yaml contains %q; OAuthSonas is ClusterIP-only", forbidden)
		}
	}
	requireStackPattern(t, template, `(?m)^[ \t]*type:[ \t]*ClusterIP[ \t]*$`, "oauthsonas Service type")
}

// TestStackOAuthSonasImagesPinnedByDigest pins both containers to immutable
// image digests. A tag can be repointed by the registry, which would change
// what a stage deployment runs without any chart change.
func TestStackOAuthSonasImagesPinnedByDigest(t *testing.T) {
	images := oauthsonasImageValues(t, oauthsonasValuesBody(t))
	for _, key := range []string{"image", "tlsProxyImage"} {
		reference, ok := images[key]
		if !ok || reference == "" {
			t.Fatalf("values.yaml has no oauthsonas.%s", key)
		}
		if !imageDigestPattern.MatchString(reference) {
			t.Errorf("oauthsonas.%s %q is not pinned by digest (want repository[:tag]@sha256:<64 hex>)", key, reference)
		}
	}

	template := oauthsonasTemplateBody(t)
	if !strings.Contains(template, "@sha256:") {
		t.Errorf("templates/oauthsonas.yaml does not require an image digest")
	}
	if !strings.Contains(template, "regexMatch") {
		t.Errorf("templates/oauthsonas.yaml has no render-time digest guard; a tag-only override would render")
	}
	for line := range strings.SplitSeq(template, "\n") {
		if !strings.Contains(line, "fail") {
			continue
		}
		if !strings.Contains(strings.ToLower(line), "digest") && !strings.Contains(strings.ToLower(line), "sha256") {
			// Other failures are fine; the digest guard is asserted above.
			continue
		}
	}
}

// TestStackOAuthSonasPodHardening pins the pod security context. Both images
// run with a read-only root filesystem (the provider writes no state, Caddy
// writes only to the mounted /tmp emptyDir), so there is no exception to
// document here.
func TestStackOAuthSonasPodHardening(t *testing.T) {
	template := oauthsonasTemplateBody(t)

	for _, want := range []string{
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"seccompProfile:",
		"type: RuntimeDefault",
	} {
		if !strings.Contains(template, want) {
			t.Errorf("templates/oauthsonas.yaml does not contain %q", want)
		}
	}
	if got := strings.Count(template, "allowPrivilegeEscalation: false"); got < 2 {
		t.Errorf("templates/oauthsonas.yaml sets allowPrivilegeEscalation: false %d times, want both containers", got)
	}
	if got := strings.Count(template, "readOnlyRootFilesystem: true"); got < 2 {
		t.Errorf("templates/oauthsonas.yaml sets readOnlyRootFilesystem: true %d times, want both containers", got)
	}
	if got := regexp.MustCompile(`drop:[ \t]*\[[ \t]*"ALL"[ \t]*\]`).FindAllString(template, -1); len(got) < 2 {
		t.Errorf("templates/oauthsonas.yaml drops ALL capabilities %d times, want both containers", len(got))
	}
	if got := strings.Count(template, "automountServiceAccountToken: false"); got < 2 {
		t.Errorf("templates/oauthsonas.yaml disables service account token automount %d times, want the ServiceAccount and the pod", got)
	}
	if err := checkOAuthSonasStrategy(template); err != nil {
		t.Errorf("templates/oauthsonas.yaml: %v", err)
	}
	if !strings.Contains(template, "--healthcheck") {
		t.Errorf("templates/oauthsonas.yaml has no oauthsonas --healthcheck probe")
	}
	if strings.Contains(template, "OAUTHSONAS_ALLOW_NON_LOOPBACK") {
		t.Errorf("templates/oauthsonas.yaml acknowledges non-loopback exposure although --listen pins 127.0.0.1")
	}
	for _, forbidden := range []string{
		"privileged: true",
		"hostPath:",
		"readOnlyRootFilesystem: false",
		"allowPrivilegeEscalation: true",
		"runAsNonRoot: false",
		"runAsUser: 0",
	} {
		if strings.Contains(template, forbidden) {
			t.Errorf("templates/oauthsonas.yaml contains %q", forbidden)
		}
	}
}

// oauthsonasAccessCodeEnv returns the environment stanza of the OAUTHSONAS_
// ACCESS_CODE variable, or "" when the variable is absent.
func oauthsonasAccessCodeEnv(template string) string {
	lines := strings.Split(template, "\n")
	start := -1
	for index, line := range lines {
		if strings.Contains(line, "name: OAUTHSONAS_ACCESS_CODE") {
			start = index
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for index := start + 1; index < len(lines); index++ {
		if strings.Contains(lines[index], "- name:") || strings.Contains(lines[index], "volumeMounts:") || strings.Contains(lines[index], "resources:") {
			end = index
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// TestStackOAuthSonasAccessCodeOnlyFromSecret pins credential custody: the
// picker access code is read only from the oauthsonas-access Secret. Values,
// command arguments and literal environment values are all refused, and the
// chart will not render a provider without a Secret, which would leave the
// picker ungated.
func TestStackOAuthSonasAccessCodeOnlyFromSecret(t *testing.T) {
	values := oauthsonasValuesBody(t)
	if regexp.MustCompile(`(?m)^[ \t]*accessCode:[ \t]*\S`).MatchString(values) {
		t.Errorf("values.yaml carries a literal oauthsonas access code; it belongs only in the oauthsonas-access Secret")
	}
	for _, want := range []string{
		"accessCodeSecret: oauthsonas-access",
		"accessCodeKey: access_code",
	} {
		if !strings.Contains(values, want) {
			t.Errorf("values.yaml does not contain %q", want)
		}
	}

	template := oauthsonasTemplateBody(t)
	env := oauthsonasAccessCodeEnv(template)
	if env == "" {
		t.Fatalf("templates/oauthsonas.yaml does not declare the OAUTHSONAS_ACCESS_CODE environment variable")
	}
	for _, want := range []string{"secretKeyRef:", "accessCodeSecret", "accessCodeKey"} {
		if !strings.Contains(env, want) {
			t.Errorf("OAUTHSONAS_ACCESS_CODE does not read from the Secret (%q missing):\n%s", want, env)
		}
	}
	if regexp.MustCompile(`(?m)^[ \t]*value:`).MatchString(env) {
		t.Errorf("OAUTHSONAS_ACCESS_CODE has a literal value instead of a secretKeyRef:\n%s", env)
	}
	if strings.Contains(env, "valueFrom") && !strings.Contains(env, "secretKeyRef") {
		t.Errorf("OAUTHSONAS_ACCESS_CODE has a valueFrom that is not a secretKeyRef:\n%s", env)
	}

	for _, want := range []string{
		`hasKey $oauth "accessCode"`,
		"accessCodeSecret",
		"access_code",
	} {
		if !strings.Contains(template, want) {
			t.Errorf("templates/oauthsonas.yaml has no guard against %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^[ \t]*\{\{-?[ \t]*if not \$oauth\.accessCodeSecret`).MatchString(template) {
		t.Errorf("templates/oauthsonas.yaml does not fail the render when oauthsonas.accessCodeSecret is empty; the picker could render ungated")
	}
	if !strings.Contains(template, "access code") {
		t.Errorf("templates/oauthsonas.yaml has no explanatory message for the access-code guard")
	}
	if strings.Contains(template, "OAUTHSONAS_ACCESS_CODE=") {
		t.Errorf("templates/oauthsonas.yaml puts the access code in a command argument or literal value")
	}
	for line := range strings.SplitSeq(template, "\n") {
		if !strings.Contains(line, "args:") {
			continue
		}
		lower := strings.ToLower(line)
		for _, forbidden := range []string{"accesscode", "access-code", "access_code"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("templates/oauthsonas.yaml mentions the access code in command arguments: %s", strings.TrimSpace(line))
			}
		}
	}
}

// TestStackOAuthSonasConfigRequiresNonEmptyOIDCValues pins the parsed-config
// guards: a blank issuer, a client without a non-empty id, or a client whose
// secret key is explicitly empty must abort the render by name instead of
// configuring an unusable identity provider.
func TestStackOAuthSonasConfigRequiresNonEmptyOIDCValues(t *testing.T) {
	template := oauthsonasTemplateBody(t)
	for _, want := range []string{
		"fromYaml $oauth.config",
		`get $document "issuer"`,
		"non-empty issuer",
		`get $client "id"`,
		"client id must not be empty",
		`hasKey $client "secret"`,
		"client secret must not be empty",
		"inline client secrets are forbidden",
		"renders into a ConfigMap",
	} {
		if !strings.Contains(template, want) {
			t.Errorf("templates/oauthsonas.yaml does not guard %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^[ \t]*\{\{-?[ \t]*range[ \t]+\$client[ \t]*:=[ \t]*\$clients`).MatchString(template) {
		t.Error("templates/oauthsonas.yaml does not range over the configured clients")
	}
}

// TestStackOAuthSonasIssuerHTTPSThroughTLSProxy pins the issuer path: Caddy
// terminates TLS with the installation-CA certificate in oauthsonas-tls and
// proxies to the loopback listener. The rendered issuer must be exactly
// https://<host>:<port>; the render refuses an http:// issuer.
func TestStackOAuthSonasIssuerHTTPSThroughTLSProxy(t *testing.T) {
	values := oauthsonasValuesBody(t)
	for _, want := range []string{
		"tlsSecret: oauthsonas-tls",
		"accessCodeSecret: oauthsonas-access",
		"accessCodeKey: access_code",
	} {
		if !strings.Contains(values, want) {
			t.Errorf("values.yaml does not contain %q (scripts/bootstrap-secrets creates these names/keys)", want)
		}
	}

	template := oauthsonasTemplateBody(t)
	for _, want := range []string{
		"issuer: %s",
		"https://",
		"contains \"issuer: http://\"",
		"tls /etc/oauthsonas-tls/tls.crt /etc/oauthsonas-tls/tls.key",
		"reverse_proxy 127.0.0.1:8181",
		"--listen",
		"127.0.0.1:8181",
		"tlsSecret",
		"tls.crt",
		"tls.key",
	} {
		if !strings.Contains(template, want) {
			t.Errorf("templates/oauthsonas.yaml does not contain %q", want)
		}
	}
	if strings.Contains(template, ":8181 {") {
		t.Errorf("templates/oauthsonas.yaml may expose the plain-HTTP listener as a site address")
	}
	if !strings.Contains(template, "readOnly: true") {
		t.Errorf("templates/oauthsonas.yaml does not mount its config/certificate read-only")
	}
}

// TestStackOAuthSonasStagePreviewOnlyDocumentation keeps the warning with the
// asset: the stage/preview statement lives in the template comment, values
// comment and NOTES so it lands with the chart and shows on install.
func TestStackOAuthSonasStagePreviewOnlyDocumentation(t *testing.T) {
	for name, body := range map[string]string{
		"templates/oauthsonas.yaml": oauthsonasTemplateBody(t),
		"values.yaml":               oauthsonasValuesBody(t),
		"templates/NOTES.txt":       oauthsonasNOTESBody(t),
	} {
		if !strings.Contains(strings.ToLower(body), "stage and preview") {
			t.Errorf("%s does not say OAuthSonas is for stage and preview only", name)
		}
	}
	values := oauthsonasValuesBody(t)
	for _, want := range []string{"org_id", "tenant", "project", "claim"} {
		if !strings.Contains(strings.ToLower(values), want) {
			t.Errorf("values.yaml does not document the single-claim mapping (%q missing)", want)
		}
	}
	if !strings.Contains(values, "A-Za-z0-9_-") {
		t.Errorf("values.yaml does not document the persona subject grammar [A-Za-z0-9_-]")
	}
}

// TestStackOAuthSonasValuesSchemaRejectsLiteralAccessCode pins the stack
// chart's values schema. Helm validates the schema before rendering, so an
// unknown oauthsonas key such as accessCode is refused even if a template
// guard were removed later.
func TestStackOAuthSonasValuesSchemaRejectsLiteralAccessCode(t *testing.T) {
	schema := objectAt(t, loadJSON(t, stackChartPath(t, "values.schema.json")), "values.schema.json")
	properties := objectAt(t, schema["properties"], "values.schema.json properties")
	oauth := objectAt(t, properties["oauthsonas"], "values.schema.json oauthsonas")

	if allowed, ok := oauth["additionalProperties"].(bool); !ok || allowed {
		t.Fatalf("values.schema.json oauthsonas must set additionalProperties: false so a literal access code is refused")
	}
	oauthProperties := objectAt(t, oauth["properties"], "values.schema.json oauthsonas.properties")
	for _, forbidden := range []string{"accessCode", "access_code", "accessCodeValue"} {
		if _, present := oauthProperties[forbidden]; present {
			t.Errorf("values.schema.json declares oauthsonas.%s; the access code may only come from the Secret", forbidden)
		}
	}

	valid := map[string]any{
		"enabled":          false,
		"image":            "ghcr.io/optimiware/oauthsonas:1.5.0@sha256:39f371048640a95aef951d4ffbc5efd58168f58910e9bb4b972694fa7e82d178",
		"tlsProxyImage":    "docker.io/library/caddy:2.11.9-alpine@sha256:0bbb055bda6793c999bde78b1b5ce3e89f829fb031af2cf50056ea1246559fee",
		"host":             "",
		"port":             float64(8443),
		"tlsSecret":        "oauthsonas-tls",
		"accessCodeSecret": "oauthsonas-access",
		"accessCodeKey":    "access_code",
		"config":           "",
	}
	if err := validateAgainstSchema(oauth, valid, "oauthsonas"); err != nil {
		t.Fatalf("values.schema.json rejects the shipped defaults: %v", err)
	}
	for _, forbidden := range []string{"accessCode", "access_code"} {
		values := make(map[string]any, len(valid)+1)
		maps.Copy(values, valid)
		values[forbidden] = "OAUTHSONAS-LITERAL-CODE-SENTINEL"
		if err := validateAgainstSchema(oauth, values, "oauthsonas"); err == nil {
			t.Errorf("values.schema.json accepted oauthsonas.%s", forbidden)
		}
	}
	tagOnly := make(map[string]any, len(valid))
	maps.Copy(tagOnly, valid)
	tagOnly["image"] = "ghcr.io/optimiware/oauthsonas:1.5.0"
	if err := validateAgainstSchema(oauth, tagOnly, "oauthsonas"); err == nil {
		t.Errorf("values.schema.json accepted a tag-only oauthsonas.image")
	}

	publicClient := make(map[string]any, len(valid))
	maps.Copy(publicClient, valid)
	publicClient["config"] = "issuer: https://oauthsonas.kodama-system.svc.cluster.local:8443\nclients:\n  - id: kodactl\n"
	if err := validateAgainstSchema(oauth, publicClient, "oauthsonas"); err != nil {
		t.Errorf("values.schema.json rejected a public OAuthSonas client config: %v", err)
	}

	for name, inlineSecretConfig := range map[string]string{
		"unquoted secret":      "issuer: https://oauthsonas.kodama-system.svc.cluster.local:8443\nclients:\n  - id: kodactl\n    secret: INLINE-SECRET\n",
		"double-quoted secret": "issuer: https://oauthsonas.kodama-system.svc.cluster.local:8443\nclients:\n  - id: kodactl\n    secret: \"INLINE-SECRET\"\n",
		"single-quoted secret": "issuer: https://oauthsonas.kodama-system.svc.cluster.local:8443\nclients:\n  - id: kodactl\n    secret: 'INLINE-SECRET'\n",
		"client_secret key":    "issuer: https://oauthsonas.kodama-system.svc.cluster.local:8443\nclients:\n  - id: kodactl\n    client_secret: INLINE-SECRET\n",
		"clientSecret key":     "issuer: https://oauthsonas.kodama-system.svc.cluster.local:8443\nclients:\n  - id: kodactl\n    clientSecret: INLINE-SECRET\n",
	} {
		inline := make(map[string]any, len(valid))
		maps.Copy(inline, valid)
		inline["config"] = inlineSecretConfig
		if err := validateAgainstSchema(oauth, inline, "oauthsonas"); err == nil {
			t.Errorf("values.schema.json accepted %s in oauthsonas.config", name)
		}
	}
}

// checkOAuthSonasTemplate is the invariant checker exercised by the mutation
// test below. It mirrors the assertions above in one place so a regression in
// the checker itself is visible.
func checkOAuthSonasTemplate(template string) error {
	if !strings.Contains(template, "if .Values.oauthsonas.enabled") {
		return fmt.Errorf("OAuthSonas body is not gated on oauthsonas.enabled")
	}
	if strings.Contains(template, "kind: Ingress") || strings.Contains(template, "NodePort") || strings.Contains(template, "LoadBalancer") {
		return fmt.Errorf("OAuthSonas service is exposed beyond ClusterIP")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*type:[ \t]*ClusterIP[ \t]*$`).MatchString(template) {
		return fmt.Errorf("OAuthSonas Service is not ClusterIP")
	}
	if !strings.Contains(template, "automountServiceAccountToken: false") {
		return fmt.Errorf("service account token automount is not disabled")
	}
	if !strings.Contains(template, "runAsNonRoot: true") {
		return fmt.Errorf("pod is not runAsNonRoot")
	}
	if err := checkOAuthSonasStrategy(template); err != nil {
		return err
	}
	if !strings.Contains(template, "secretKeyRef") || !strings.Contains(template, "accessCodeSecret") {
		return fmt.Errorf("access code is not sourced from the oauthsonas-access Secret")
	}
	env := oauthsonasAccessCodeEnv(template)
	if env == "" || !strings.Contains(env, "secretKeyRef") {
		return fmt.Errorf("OAUTHSONAS_ACCESS_CODE is not a secretKeyRef")
	}
	if regexp.MustCompile(`(?m)^[ \t]*value:`).MatchString(env) {
		return fmt.Errorf("OAUTHSONAS_ACCESS_CODE has a literal value")
	}
	if strings.Contains(template, "OAUTHSONAS_ACCESS_CODE=") {
		return fmt.Errorf("access code appears in a literal env value or argument")
	}
	for line := range strings.SplitSeq(template, "\n") {
		if !strings.Contains(line, "args:") {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "accesscode") || strings.Contains(lower, "access-code") || strings.Contains(lower, "access_code") {
			return fmt.Errorf("access code appears in command arguments")
		}
	}
	if !strings.Contains(template, `contains "issuer: http://"`) {
		return fmt.Errorf("no guard refuses an http issuer")
	}
	if !strings.Contains(template, "regexMatch") {
		return fmt.Errorf("no render-time image digest guard")
	}
	if !regexp.MustCompile(`(?ms)^spec:\s*\r?\n\s*replicas:[ \t]*\{\{[ \t]*\$replicas[ \t]*\}\}\s*$`).MatchString(template) {
		return fmt.Errorf("OAuthSonas Deployment does not pin exactly one replica")
	}
	if !strings.Contains(template, `args: ["--config", "/etc/oauthsonas/config.yaml", "--listen", "127.0.0.1:8181"]`) {
		return fmt.Errorf("the provider argument line does not pin --config and the loopback --listen address")
	}
	if !regexp.MustCompile(`(?s)\{\{-\s*else\s*\}\}\s*\{\{-\s*fail\s+"oauthsonas\.config inline client secrets are forbidden`).MatchString(template) {
		return fmt.Errorf("no else branch refuses an inline client secret")
	}
	if !strings.Contains(template, "lt (int $port) 1024") {
		return fmt.Errorf("no guard refuses a privileged TLS port (a non-root container cannot bind below 1024)")
	}
	for _, guard := range []string{
		`if not (regexMatch "^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$" (toString $host))`,
		`if or (lt (int $port) 1024) (gt (int $port) 65535)`,
		`if not $oauth.accessCodeKey`,
		`if not $oauth.tlsSecret`,
		`hasKey $client "secret"`,
		`client secret must not be empty`,
		`inline client secrets are forbidden`,
	} {
		if !strings.Contains(template, guard) {
			return fmt.Errorf("missing template guard %q", guard)
		}
	}
	return nil
}

// TestStackOAuthSonasMutationSensitivity proves the checker above fails for
// each security regression, so a deleted assertion is not
// silently equivalent to a passing chart.
func TestStackOAuthSonasMutationSensitivity(t *testing.T) {
	template := oauthsonasTemplateBody(t)
	if err := checkOAuthSonasTemplate(template); err != nil {
		t.Fatalf("checker rejected the real template: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "service becomes NodePort",
			mutate: func(body string) string {
				return strings.Replace(body, "type: ClusterIP", "type: NodePort", 1)
			},
		},
		{
			name:   "an Ingress is rendered",
			mutate: func(body string) string { return body + "\nkind: Ingress\n" },
		},
		{
			name: "the strategy is Recreate",
			mutate: func(body string) string {
				return strings.Replace(body, "    type: RollingUpdate\n", "    type: Recreate\n", 1)
			},
		},
		{
			name: "service account token is mounted",
			mutate: func(body string) string {
				return strings.ReplaceAll(body, "automountServiceAccountToken: false", "automountServiceAccountToken: true")
			},
		},
		{
			name: "pod may run as root",
			mutate: func(body string) string {
				return strings.Replace(body, "runAsNonRoot: true", "runAsNonRoot: false", 1)
			},
		},
		{
			name: "access code becomes a literal env value",
			mutate: func(body string) string {
				return strings.Replace(body, "valueFrom:", `value: "LEAK"`, 1)
			},
		},
		{
			name: "access code appears in args",
			mutate: func(body string) string {
				return strings.Replace(body, `"--listen", "127.0.0.1:8181"`, `"--listen", "127.0.0.1:8181", "OAUTHSONAS_ACCESS_CODE=LEAK"`, 1)
			},
		},
		{
			name: "http issuer is not refused",
			mutate: func(body string) string {
				return strings.Replace(body, `contains "issuer: http://"`, `contains "issuer: ftp://"`, 1)
			},
		},
		{
			name: "image digest guard is removed",
			mutate: func(body string) string {
				return strings.ReplaceAll(body, "regexMatch", "hasPrefix")
			},
		},
		{
			name: "the single replica is not pinned",
			mutate: func(body string) string {
				return strings.Replace(body, "{{- $replicas := 1 }}", "{{- $replicas := 2 }}", 1)
			},
		},
		{
			name: "a privileged TLS port is allowed",
			mutate: func(body string) string {
				return strings.Replace(body, "if or (lt (int $port) 1024) (gt (int $port) 65535)", "if or (lt (int $port) 1) (gt (int $port) 65535)", 1)
			},
		},
		{
			name: "the TLS Secret guard is removed",
			mutate: func(body string) string {
				return strings.Replace(body, `{{- if not $oauth.tlsSecret }}
{{- fail "oauthsonas.tlsSecret is required when oauthsonas.enabled is true: scripts/bootstrap-secrets creates the oauthsonas-tls Secret from the installation CA" }}
{{- end }}`, "", 1)
			},
		},
		{
			name: "inline client secret guard is removed",
			mutate: func(body string) string {
				return strings.Replace(body, `{{- else }}
{{- fail "oauthsonas.config inline client secrets are forbidden: oauthsonas.config renders into a ConfigMap, so OAuthSonas clients must be public (omit secret)" }}`, "", 1)
			},
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := mutation.mutate(template)
			if mutated == template {
				t.Fatalf("mutation %q did not change the template", mutation.name)
			}
			if err := checkOAuthSonasTemplate(mutated); err == nil {
				t.Errorf("checker accepted the mutation %q", mutation.name)
			}
		})
	}
}
