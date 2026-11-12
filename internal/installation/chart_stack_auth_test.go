package installation

// Broker-authentication checks for the kodama-stack chart.
//
// KODAMA.md:309 and docs/specs/08-operations-events.md:65 require broker
// authentication and authorization on the same credential: a pod that can
// reach nats.<namespace>:4222 must not be able to read every tenant's events
// and logs. The stack chart is therefore fail-closed: it refuses to render
// unless mTLS client verification (the default) or explicit credentials are
// configured, and it never renders verify: false, no_auth_user or anonymous
// access.
//
// The chart is inspected as text: no helm binary, template engine, YAML
// decoder, cluster or network access is used.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// stackChartPath resolves one path inside deploy/charts/kodama-stack.
func stackChartPath(t *testing.T, parts ...string) string {
	t.Helper()
	all := append([]string{repoRoot(t), "deploy", "charts", "kodama-stack"}, parts...)
	return filepath.Join(all...)
}

// stackTemplates returns every stack chart template body keyed by file name.
func stackTemplates(t *testing.T) map[string]string {
	t.Helper()
	dir := stackChartPath(t, "templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read stack chart templates: %v", err)
	}
	bodies := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch filepath.Ext(name) {
		case ".yaml", ".yml", ".tpl":
			bodies[name] = readText(t, filepath.Join(dir, name))
		}
	}
	if len(bodies) == 0 {
		t.Fatalf("no stack chart templates in %s", dir)
	}
	return bodies
}

// stackTemplateContaining returns the sorted first template containing marker.
func stackTemplateContaining(t *testing.T, bodies map[string]string, marker string) (string, string) {
	t.Helper()
	names := make([]string, 0, len(bodies))
	for name := range bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.Contains(bodies[name], marker) {
			return name, bodies[name]
		}
	}
	t.Fatalf("no stack chart template contains %q", marker)
	return "", ""
}

func requireStackPattern(t *testing.T, body, expression, description string) {
	t.Helper()
	if !regexp.MustCompile(expression).MatchString(body) {
		t.Errorf("%s: no match for %s", description, expression)
	}
}

// TestStackNATSAuthDefaultsToCallout pins the default: per-credential
// authorization callout is on, so external clients never share the control
// plane's credential. The callout Secret is referenced by name only.
func TestStackNATSAuthDefaultsToCallout(t *testing.T) {
	values := readText(t, stackChartPath(t, "values.yaml"))

	requireStackPattern(t, values, `(?ms)^[ \t]*auth:[ \t]*\r?\n(?:[ \t]+.*\r?\n)*?[ \t]*mode:[ \t]*"?callout"?[ \t]*$`, "values.yaml nats.auth.mode default")
	requireStackPattern(t, values, `(?ms)^[ \t]*callout:[ \t]*\r?\n(?:[ \t]+.*\r?\n)*?[ \t]*existingSecret:[ \t]*"?kodama-nats-auth"?[ \t]*$`, "values.yaml callout Secret")
	requireStackPattern(t, values, `(?m)^[ \t]*existingSecret:[ \t]*"?kodama-broker-tls"?[ \t]*$`, "values.yaml mTLS client CA secret")
	if !strings.Contains(values, "nats.auth.mtls.existingSecret") {
		// The comment above the default documents the fail-closed contract.
		t.Log("values.yaml does not name the mTLS secret key in a comment (not required)")
	}
}

// TestStackNATSAuthGuardFailsClosed pins the render-time guards. Helm's
// required/fail makes `helm template`/`helm upgrade` refuse rather than fall
// back to an unauthenticated broker. Both rendered workloads must include the
// guard, so deleting an include cannot silently open the broker.
func TestStackNATSAuthGuardFailsClosed(t *testing.T) {
	bodies := stackTemplates(t)
	name, body := stackTemplateContaining(t, bodies, "anonymous NATS access is forbidden")
	configName, config := stackTemplateContaining(t, bodies, "nats.conf")
	statefulName, statefulSet := stackTemplateContaining(t, bodies, "kind: StatefulSet")
	for _, workload := range []struct {
		name string
		body string
	}{
		{name: configName, body: config},
		{name: statefulName, body: statefulSet},
	} {
		if !strings.Contains(workload.body, `include "kodama-stack.natsAuthGuard"`) {
			t.Errorf("%s does not include the fail-closed NATS auth guard; an unauthenticated render would succeed", workload.name)
		}
	}
	for _, want := range []string{
		"nats.auth.mode must be",
		"nats.auth.mtls.existingSecret is required",
		"nats.auth.credentials.existingSecret is required",
		"nats.auth.callout.existingSecret is required",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s does not refuse an unauthenticated render: missing %q", name, want)
		}
	}
	if !regexp.MustCompile(`(?m)^[ \t]*\{\{-?[ \t]*fail[ \t]`).MatchString(body) &&
		!strings.Contains(body, "required ") {
		t.Errorf("%s has no Helm fail/required guard, so an unauthenticated render would succeed", name)
	}
	if !regexp.MustCompile(`"callout"`).MatchString(body) || !regexp.MustCompile(`"mtls"`).MatchString(body) || !strings.Contains(body, `"credentials"`) {
		t.Errorf("%s does not restrict nats.auth.mode to callout, mtls or credentials", name)
	}
}

// TestStackNATSClientCertificatesAlwaysVerified proves the rendered NATS
// configuration verifies client certificates against the mounted CA and never
// contains an opt-out.
func TestStackNATSClientCertificatesAlwaysVerified(t *testing.T) {
	bodies := stackTemplates(t)
	name, config := stackTemplateContaining(t, bodies, "nats.conf")

	requireStackPattern(t, config, `(?m)^[ \t]*verify:[ \t]*true[ \t]*$`, name+" client certificate verification")
	requireStackPattern(t, config, `(?m)^[ \t]*ca_file:[ \t]*"/etc/nats/client-ca/ca\.[a-z]+"[ \t]*$`, name+" client CA file")

	if regexp.MustCompile(`(?m)^[ \t]*verify:[ \t]*false[ \t]*$`).MatchString(config) {
		t.Errorf("%s renders verify: false, which disables client certificate verification", name)
	}
	for templateName, body := range bodies {
		for _, forbidden := range []string{"no_auth_user", "verifyClient"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s references %q; client verification is always on and verifyClient is ignored", templateName, forbidden)
			}
		}
		if regexp.MustCompile(`(?m)^[ \t]*verify:[ \t]*false[ \t]*$`).MatchString(body) {
			t.Errorf("%s renders verify: false, which disables client certificate verification", templateName)
		}
	}
}

// TestStackNATSMountsClientCAReadOnly pins the StatefulSet side of mTLS: the
// client CA comes from a Secret reference and is mounted read-only.
func TestStackNATSMountsClientCAReadOnly(t *testing.T) {
	bodies := stackTemplates(t)
	name, statefulSet := stackTemplateContaining(t, bodies, "kind: StatefulSet")

	mount := regexp.MustCompile(`(?ms)^[ \t]*-[ \t]*name:[ \t]*client-ca[ \t]*\r?\n(?:[ \t]+[^\r\n]*\r?\n){0,4}?[ \t]*readOnly:[ \t]*true[ \t]*$`)
	if !mount.MatchString(statefulSet) {
		t.Errorf("%s does not mount the client CA read-only (volumeMount client-ca with readOnly: true)", name)
	}
	volume := regexp.MustCompile(`(?ms)^[ \t]*-[ \t]*name:[ \t]*client-ca[ \t]*\r?\n[ \t]*secret:[ \t]*\r?\n(?:[ \t]+[^\r\n]*\r?\n){0,5}?[ \t]*secretName:[ \t]*\{\{.*nats\.auth\.mtls\.existingSecret.*\}\}`)
	if !volume.MatchString(statefulSet) {
		t.Errorf("%s does not source the client CA volume from nats.auth.mtls.existingSecret", name)
	}
	requireStackPattern(t, statefulSet, `(?m)^[ \t]*path:[ \t]*ca\.[a-z]+[ \t]*$`, name+" client CA secret item path")
	if !strings.Contains(statefulSet, "nats.auth.mtls.caKey") {
		t.Errorf("%s does not read the CA key name from nats.auth.mtls.caKey", name)
	}
}

// TestStackNATSCredentialsModeUsesSecretReferences proves the credentials
// mode renders a NATS authorization block sourced from an existing Secret and
// never from a literal value in values or templates.
func TestStackNATSCredentialsModeUsesSecretReferences(t *testing.T) {
	bodies := stackTemplates(t)
	configName, config := stackTemplateContaining(t, bodies, "nats.conf")
	statefulName, statefulSet := stackTemplateContaining(t, bodies, "kind: StatefulSet")

	requireStackPattern(t, config, `(?m)^[ \t]*authorization[ \t]*\{[ \t]*$`, configName+" authorization block")
	requireStackPattern(t, config, `eq[ \t]+[^\r\n]*auth\.mode[^\r\n]*"credentials"`, configName+" credentials gate")
	requireStackPattern(t, config, `(?m)^[ \t]*user:[ \t]*\$NATS_AUTH_USER[ \t]*$`, configName+" authorization user from environment")
	requireStackPattern(t, config, `(?m)^[ \t]*password:[ \t]*\$NATS_AUTH_PASSWORD[ \t]*$`, configName+" authorization password from environment")

	for _, key := range []string{"NATS_AUTH_USER", "NATS_AUTH_PASSWORD"} {
		if !strings.Contains(statefulSet, "name: "+key) {
			t.Errorf("%s does not declare the %s environment variable", statefulName, key)
		}
	}
	if got := strings.Count(statefulSet, "nats.auth.credentials.existingSecret"); got < 2 {
		t.Errorf("%s sources %d credential environment values from nats.auth.credentials.existingSecret, want 2", statefulName, got)
	}
	if !strings.Contains(statefulSet, "nats.auth.credentials.userKey") || !strings.Contains(statefulSet, "nats.auth.credentials.passwordKey") {
		t.Errorf("%s does not read the credential key names from nats.auth.credentials.*Key", statefulName)
	}

	values := readText(t, stackChartPath(t, "values.yaml"))
	literal := regexp.MustCompile(`(?m)^[ \t]*(user|password):[ \t]*["'][^"'$]+["'][ \t]*$`)
	for _, source := range []struct {
		name string
		body string
	}{
		{name: "values.yaml", body: values},
		{name: configName, body: config},
		{name: statefulName, body: statefulSet},
	} {
		for _, match := range literal.FindAllString(source.body, -1) {
			t.Errorf("%s contains a literal broker credential: %s", source.name, strings.TrimSpace(match))
		}
	}
	requireStackPattern(t, values, `(?ms)^[ \t]*credentials:[ \t]*\r?\n(?:[ \t]+.*\r?\n)*?[ \t]*existingSecret:[ \t]*""[ \t]*$`, "values.yaml credentials secret starts empty (fail-closed)")
}

// TestStackNATSCalloutModeUsesSecretReferences proves the per-credential mode
// binds both the control plane and the callout service to bypass users and the
// issuer key from an existing Secret, never from values or templates.
func TestStackNATSCalloutModeUsesSecretReferences(t *testing.T) {
	bodies := stackTemplates(t)
	configName, config := stackTemplateContaining(t, bodies, "auth_callout")
	statefulName, statefulSet := stackTemplateContaining(t, bodies, "kind: StatefulSet")
	requireStackPattern(t, config, `eq[ \t]+[^\r\n]*auth\.mode[^\r\n]*"callout"`, configName+" callout gate")
	requireStackPattern(t, config, `(?m)^[ \t]*issuer:[ \t]*\$NATS_CALLOUT_ISSUER[ \t]*$`, configName+" issuer from environment")
	requireStackPattern(t, config, `(?m)^[ \t]*auth_users:[ \t]*\[ \$NATS_SHELL_USER, \$NATS_CALLOUT_USER \][ \t]*$`, configName+" bypass users")
	for _, key := range []string{"NATS_SHELL_USER", "NATS_SHELL_PASSWORD", "NATS_CALLOUT_USER", "NATS_CALLOUT_PASSWORD", "NATS_CALLOUT_ISSUER"} {
		if !strings.Contains(statefulSet, "name: "+key) {
			t.Errorf("%s does not declare the %s environment variable", statefulName, key)
		}
	}
	if got := strings.Count(statefulSet, "nats.auth.callout.existingSecret"); got < 5 {
		t.Errorf("%s sources %d callout values from nats.auth.callout.existingSecret, want 5", statefulName, got)
	}
	// The control plane must also receive its bypass user and the callout
	// seed so it can connect and answer.
	deployment := readText(t, filepath.Join(repoRoot(t), "deploy", "charts", "kodama", "templates", "deployment.yaml"))
	for _, key := range []string{"KODAMA_NATS_USER", "KODAMA_NATS_PASSWORD", "KODAMA_NATS_CALLOUT_USER", "KODAMA_NATS_CALLOUT_PASSWORD", "KODAMA_NATS_CALLOUT_ISSUER_SEED"} {
		if !strings.Contains(deployment, "name: "+key) {
			t.Errorf("kodama deployment does not declare %s", key)
		}
	}
}

// TestStackNATSCalloutAccountIsWiredFromOneValue pins the callout account:
// the broker's auth_callout.account and the control plane's
// KODAMA_NATS_CALLOUT_ACCOUNT come from the same YAML anchor, so the account
// the broker admits clients into and the account the callout signs for cannot
// drift apart.
func TestStackNATSCalloutAccountIsWiredFromOneValue(t *testing.T) {
	values := readText(t, stackChartPath(t, "values.yaml"))
	if got := strings.Count(values, "&natsCalloutAccount"); got != 1 {
		t.Fatalf("values.yaml defines the callout account anchor %d times; want exactly 1", got)
	}
	uses := regexp.MustCompile(`(?m)^[ \t]*account:[ \t]*\*natsCalloutAccount[ \t]*$`).FindAllString(values, -1)
	if len(uses) != 2 {
		t.Fatalf("values.yaml references the callout account anchor %d times; want 2 (nats.auth.callout and kodama.secrets.natsAuth)", len(uses))
	}
	requireStackPattern(t, values, `(?ms)^[ \t]*callout:[ \t]*\r?\n(?:[ \t]+.*\r?\n)*?[ \t]*account:[ \t]*\*natsCalloutAccount[ \t]*$`, "values.yaml nats.auth.callout.account from the anchor")
	requireStackPattern(t, values, `(?ms)^[ \t]*natsAuth:[ \t]*\r?\n(?:[ \t]+.*\r?\n)*?[ \t]*account:[ \t]*\*natsCalloutAccount[ \t]*$`, "values.yaml kodama.secrets.natsAuth.account from the anchor")

	deployment := readText(t, filepath.Join(repoRoot(t), "deploy", "charts", "kodama", "templates", "deployment.yaml"))
	if !strings.Contains(deployment, "KODAMA_NATS_CALLOUT_ACCOUNT") || !strings.Contains(deployment, ".Values.secrets.natsAuth.account") {
		t.Fatalf("kodama deployment does not render KODAMA_NATS_CALLOUT_ACCOUNT from secrets.natsAuth.account")
	}
	if regexp.MustCompile(`(?m)^[ \t]*value:[ \t]*"?KODAMA"?[ \t]*$`).MatchString(deployment) {
		t.Fatalf("kodama deployment hardcodes the callout account instead of reading secrets.natsAuth.account")
	}

	guard := readText(t, stackChartPath(t, "templates", "_nats-auth.tpl"))
	if !strings.Contains(guard, "must equal") || !strings.Contains(guard, "natsAuth.account") {
		t.Fatalf("stack auth guard does not refuse a callout account that differs from kodama.secrets.natsAuth.account")
	}
}

// TestStackNATSNoAnonymousAccess scans the whole chart for an anonymous escape
// hatch; the README states the default and the deferred per-principal work.
func TestStackNATSNoAnonymousAccess(t *testing.T) {
	anonymous := regexp.MustCompile(`(?m)^[ \t]*anonymous:[ \t]*\S`)
	for name, body := range stackTemplates(t) {
		if strings.Contains(body, "no_auth_user") {
			t.Errorf("%s contains no_auth_user; anonymous broker access is forbidden", name)
		}
		for _, match := range anonymous.FindAllString(body, -1) {
			t.Errorf("%s contains an anonymous-access setting: %s", name, strings.TrimSpace(match))
		}
	}
}

// TestDeployREADMEDocumentsPerCredentialBrokerAuthorization keeps the
// security contract visible: every external broker client presents a current
// Kodama credential to the authorization callout, and the control plane never
// hands out its bypass user.
func TestDeployREADMEDocumentsPerCredentialBrokerAuthorization(t *testing.T) {
	body := readText(t, filepath.Join(repoRoot(t), "deploy", "README.md"))
	lower := strings.ToLower(body)
	for _, want := range []string{
		"nats",
		"mtls",
		"fail-closed",
		"callout",
		"per-credential",
		"revocation",
		"bypass user",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("deploy/README.md does not document %q", want)
		}
	}
	if !strings.Contains(body, "32-byte") {
		t.Errorf("deploy/README.md does not require a first-stage 32-byte KEK encrypt/decrypt round trip")
	}
}

// TestStackNATSAuthGuardAllowsOnlyMTLSAndCredentials pins the guard's allowed
// list as data: adding any other mode (none, open, off, empty, mixed-case
// anonymous) would let an unauthenticated broker render. The rendered-chart
// test in test/component/deploy asserts `helm template` fails for each; this
// static check keeps the same regression visible when helm is absent.
func TestStackNATSAuthGuardAllowsOnlyMTLSAndCredentials(t *testing.T) {
	bodies := stackTemplates(t)
	name, body := stackTemplateContaining(t, bodies, "nats.auth.mode must be")
	match := regexp.MustCompile(`has\s+\$mode\s+\(list([^)]*)\)`).FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("%s does not restrict nats.auth.mode with a has/list guard", name)
	}
	var got []string
	for _, value := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(match[1], -1) {
		got = append(got, value[1])
	}
	want := []string{"callout", "mtls", "credentials"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("%s allows nats.auth.mode values %v, want exactly %v", name, got, want)
	}
}

// checkStackNATSAuthTemplate is exercised by the mutation tests below so a
// regression in the checker cannot silently pass.
func checkStackNATSAuthTemplate(config, statefulSet string) error {
	if !regexp.MustCompile(`(?m)^[ \t]*verify:[ \t]*true[ \t]*$`).MatchString(config) {
		return fmt.Errorf("NATS config does not verify client certificates")
	}
	if regexp.MustCompile(`(?m)^[ \t]*verify:[ \t]*false[ \t]*$`).MatchString(config) {
		return fmt.Errorf("NATS config disables client certificate verification")
	}
	if !strings.Contains(config, `ca_file: "/etc/nats/client-ca/ca.crt"`) {
		return fmt.Errorf("NATS config does not verify against the mounted client CA")
	}
	if !regexp.MustCompile(`(?ms)^[ \t]*-[ \t]*name:[ \t]*client-ca[ \t]*\r?\n(?:[ \t]+[^\r\n]*\r?\n){0,4}?[ \t]*readOnly:[ \t]*true[ \t]*$`).MatchString(statefulSet) {
		return fmt.Errorf("StatefulSet does not mount the client CA read-only")
	}
	return nil
}

func TestStackNATSAuthTemplateMutationSensitivity(t *testing.T) {
	bodies := stackTemplates(t)
	_, config := stackTemplateContaining(t, bodies, "nats.conf")
	_, statefulSet := stackTemplateContaining(t, bodies, "kind: StatefulSet")

	if err := checkStackNATSAuthTemplate(config, statefulSet); err != nil {
		t.Fatalf("checker rejected the real templates: %v", err)
	}

	disabled := strings.Replace(config, "verify: true", "verify: false", 1)
	if disabled == config {
		t.Fatal("mutation did not change verify")
	}
	if err := checkStackNATSAuthTemplate(disabled, statefulSet); err == nil {
		t.Fatal("checker accepted verify: false")
	}

	unmounted := regexp.MustCompile(`(?ms)^[ \t]*-[ \t]*name:[ \t]*client-ca[ \t]*\r?\n(?:[ \t]+[^\r\n]*\r?\n){0,4}?[ \t]*readOnly:[ \t]*true[ \t]*$`).ReplaceAllString(statefulSet, "")
	if unmounted == statefulSet {
		t.Fatal("mutation did not remove the client CA mount")
	}
	if err := checkStackNATSAuthTemplate(config, unmounted); err == nil {
		t.Fatal("checker accepted a StatefulSet without the client CA mount")
	}
}
