//go:build component

// Rendered-chart checks: the serviceAccount/rbac create flags,
// the image digest grammar, the required database CA and KMS credentials, the
// production OIDC requirement, the secret key references the server reads and
// the pinned NATS image. Every check renders the real chart with helm in a
// test-owned temporary copy; nothing contacts a cluster or the network.
package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// productionOIDCArgs returns the four environment variables
// cmd/kodama/production.go refuses to start without. prefix is "" for the
// kodama chart and "kodama." for the stack chart.
func productionOIDCArgs(prefix string) []string {
	return []string{
		"--set", prefix + "config.extraEnv.KODAMA_OIDC_ISSUER=https://issuer.example",
		"--set", prefix + "config.extraEnv.KODAMA_OIDC_AUDIENCE=kodama",
		"--set", prefix + "config.extraEnv.KODAMA_OIDC_JWKS_URL=https://issuer.example/jwks",
		"--set", prefix + "config.extraEnv.KODAMA_OIDC_TENANT_CLAIM=org_id",
	}
}

// kodamaProductionArgs renders the kodama chart with its production inputs.
func kodamaProductionArgs(extra ...string) []string {
	return append(productionOIDCArgs(""), extra...)
}

// stackProductionArgs renders the stack chart with its production inputs.
func stackProductionArgs(extra ...string) []string {
	return append(productionOIDCArgs("kodama."), extra...)
}

// renderedObjectNamed returns one rendered object by kind and metadata.name.
func renderedObjectNamed(t *testing.T, rendered, kind, name string) map[string]any {
	t.Helper()
	for _, object := range renderedObjects(t, rendered) {
		if stringAtKey(t, object, "kind") != kind {
			continue
		}
		metadata := objectAtKey(t, object, "metadata")
		if stringAtKey(t, metadata, "name") == name {
			return object
		}
	}
	t.Fatalf("rendered output has no %s named %s", kind, name)
	return nil
}

// renderedVolume returns one pod volume by name.
func renderedVolume(t *testing.T, deployment map[string]any, name string) map[string]any {
	t.Helper()
	spec := objectAtKey(t, objectAtKey(t, objectAtKey(t, deployment, "spec"), "template"), "spec")
	volumes, ok := spec["volumes"].([]any)
	if !ok {
		t.Fatalf("rendered pod has no volumes")
	}
	for _, entry := range volumes {
		volume, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if stringAtKey(t, volume, "name") == name {
			return volume
		}
	}
	t.Fatalf("rendered pod has no volume named %s", name)
	return nil
}

// renderedEnvEntry returns one container environment entry by name.
func renderedEnvEntry(t *testing.T, deployment map[string]any, name string) map[string]any {
	t.Helper()
	for _, container := range containersOf(t, deployment) {
		env, ok := container["env"].([]any)
		if !ok {
			continue
		}
		for _, entry := range env {
			environment, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if stringAtKey(t, environment, "name") == name {
				return environment
			}
		}
	}
	t.Fatalf("rendered Deployment has no %s environment entry", name)
	return nil
}

// TestKodamaChartServiceAccountAndRBACCreateFalse proves an explicit
// create=false actually disables the object. A `| default true` treats false
// as an absent value and still renders both.
func TestKodamaChartServiceAccountAndRBACCreateFalse(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")

	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama: %v\n%s", err, rendered)
	}
	if got := renderedKindCount(rendered, "ServiceAccount"); got != 1 {
		t.Fatalf("default render has %d ServiceAccounts, want 1", got)
	}
	if got := renderedKindCount(rendered, "ClusterRoleBinding"); got != 1 {
		t.Fatalf("default render has %d ClusterRoleBindings, want 1", got)
	}

	rendered, err = renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs("--set", "serviceAccount.create=false")...)
	if err != nil {
		t.Fatalf("helm template kodama with serviceAccount.create=false: %v\n%s", err, rendered)
	}
	if got := renderedKindCount(rendered, "ServiceAccount"); got != 0 {
		t.Errorf("serviceAccount.create=false still rendered %d ServiceAccounts:\n%s", got, rendered)
	}

	rendered, err = renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs("--set", "rbac.create=false")...)
	if err != nil {
		t.Fatalf("helm template kodama with rbac.create=false: %v\n%s", err, rendered)
	}
	for _, kind := range []string{"ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"} {
		if got := renderedKindCount(rendered, kind); got != 0 {
			t.Errorf("rbac.create=false still rendered %d %s objects", got, kind)
		}
	}

	// An absent create key keeps the documented default (create).
	rendered, err = renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs("--set-json", "serviceAccount={}")...)
	if err != nil {
		t.Fatalf("helm template kodama with an empty serviceAccount object: %v\n%s", err, rendered)
	}
	if got := renderedKindCount(rendered, "ServiceAccount"); got != 1 {
		t.Errorf("an absent serviceAccount.create rendered %d ServiceAccounts, want the default 1", got)
	}
}

// TestKodamaChartImageDigestSchemaRejectsNonDigest pins the schema grammar at
// render time: only an empty value or a full lower-case sha256 digest is
// accepted.
func TestKodamaChartImageDigestSchemaRejectsNonDigest(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")

	for _, digest := range []string{
		"sha256:abc",
		"sha256:" + strings.Repeat("a", 63),
		"sha256:" + strings.Repeat("A", 64),
		"sha512:" + strings.Repeat("a", 64),
		"latest",
	} {
		rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs("--set", "image.digest="+digest)...)
		if err == nil {
			t.Errorf("helm template accepted image.digest=%q:\n%s", digest, rendered)
			continue
		}
		if !strings.Contains(rendered, "schema") {
			t.Errorf("helm template rejected image.digest=%q for a reason other than the schema: %v\n%s", digest, err, rendered)
		}
	}

	digest := "sha256:" + strings.Repeat("a", 64)
	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs("--set", "image.digest="+digest)...)
	if err != nil {
		t.Fatalf("helm template refused a valid image digest: %v\n%s", err, rendered)
	}
	if !strings.Contains(rendered, "registry.example.com/kodama@"+digest) {
		t.Errorf("rendered Deployment does not use the digest image reference:\n%s", rendered)
	}
}

// TestKodamaChartDatabaseCAVolumeIsRequired proves the database CA Secret is
// mounted without optional: true. A missing Secret must stop the pod instead
// of starting it without a certificate root.
func TestKodamaChartDatabaseCAVolumeIsRequired(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama: %v\n%s", err, rendered)
	}
	deployment := renderedObjectOfKind(t, rendered, "Deployment")
	volume := renderedVolume(t, deployment, "database-ca")
	secret := objectAtKey(t, volume, "secret")
	if optional, present := secret["optional"]; present && optional == true {
		t.Fatalf("database-ca Secret volume is optional: %v", secret)
	}
	if stringAtKey(t, secret, "secretName") != "kodama-database" {
		t.Errorf("database-ca volume does not read the kodama-database Secret: %v", secret)
	}
}

// TestKodamaChartKMSenvFromFollowsProfile proves the KMS credential Secret is
// required in production for the default scw-kms adapter (its composition
// requires SCW_SECRET_KEY) and optional only for a development or test profile.
// The aws-kms and gcp-kms adapters, which render no reference in production,
// are covered by kms_credential_mode_component_test.go.
func TestKodamaChartKMSenvFromFollowsProfile(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")

	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama: %v\n%s", err, rendered)
	}
	deployment := renderedObjectOfKind(t, rendered, "Deployment")
	containers := containersOf(t, deployment)
	if len(containers) == 0 {
		t.Fatal("rendered Deployment has no containers")
	}
	envFrom, ok := containers[0]["envFrom"].([]any)
	if !ok || len(envFrom) != 1 {
		t.Fatalf("rendered Deployment envFrom = %v, want exactly the KMS Secret", containers[0]["envFrom"])
	}
	entry := objectAtKey(t, envFrom[0].(map[string]any), "secretRef")
	if stringAtKey(t, entry, "name") != "kodama-kms-credentials" {
		t.Errorf("envFrom does not read kodama-kms-credentials: %v", entry)
	}
	if optional, present := entry["optional"]; present && optional == true {
		t.Errorf("production envFrom marks the KMS credentials optional: %v", entry)
	}

	rendered, err = renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs("--set", "environment=development")...)
	if err != nil {
		t.Fatalf("helm template kodama with environment=development: %v\n%s", err, rendered)
	}
	development := renderedObjectOfKind(t, rendered, "Deployment")
	devContainers := containersOf(t, development)
	devEnvFrom, ok := devContainers[0]["envFrom"].([]any)
	if !ok || len(devEnvFrom) != 1 {
		t.Fatalf("development render envFrom = %v", devContainers[0]["envFrom"])
	}
	devEntry := objectAtKey(t, devEnvFrom[0].(map[string]any), "secretRef")
	if optional, ok := devEntry["optional"].(bool); !ok || !optional {
		t.Errorf("development envFrom does not mark the KMS credentials optional: %v", devEntry)
	}
}

// TestKodamaChartRequiresProductionOIDC proves the render fails when the four
// production OIDC variables are missing, naming the variable, and succeeds
// once they are supplied. A development profile does not require them.
func TestKodamaChartRequiresProductionOIDC(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")

	rendered, err := renderChartArgs(t, helm, "kodama", chart)
	if err == nil {
		t.Fatalf("helm template accepted a production render without OIDC variables:\n%s", rendered)
	}
	named := false
	for _, name := range []string{
		"KODAMA_OIDC_ISSUER", "KODAMA_OIDC_AUDIENCE", "KODAMA_OIDC_JWKS_URL", "KODAMA_OIDC_TENANT_CLAIM",
	} {
		if strings.Contains(rendered, name) {
			named = true
		}
	}
	if !named {
		t.Errorf("the production OIDC refusal names none of the required variables:\n%s", rendered)
	}

	rendered, err = renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template refused a production render with OIDC variables: %v\n%s", err, rendered)
	}

	rendered, err = renderChartArgs(t, helm, "kodama", chart, "--set", "environment=development")
	if err != nil {
		t.Fatalf("helm template refused a development render without OIDC variables: %v\n%s", err, rendered)
	}

	// The template guard is a second layer: with the schema requirement removed
	// it must still refuse the production render with its own message.
	helm, charts = stagedCharts(t)
	chart = filepath.Join(charts, "kodama")
	schemaPath := filepath.Join(chart, "values.schema.json")
	schemaMap := map[string]any{}
	if err := json.Unmarshal([]byte(readFileForTest(t, schemaPath)), &schemaMap); err != nil {
		t.Fatalf("decode staged schema: %v", err)
	}
	delete(schemaMap, "allOf")
	encoded, err := json.MarshalIndent(schemaMap, "", "  ")
	if err != nil {
		t.Fatalf("encode staged schema: %v", err)
	}
	rewriteStagedFile(t, schemaPath, string(encoded))

	rendered, err = renderChartArgs(t, helm, "kodama", chart)
	if err == nil {
		t.Fatalf("template guard accepted a production render without OIDC variables:\n%s", rendered)
	}
	if !strings.Contains(rendered, "production requires") {
		t.Errorf("the template guard did not refuse with its own message: %v\n%s", err, rendered)
	}
}

// readFileForTest reads a staged chart file.
func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// TestKodamaChartSecretKeyReferencesAndTLSPaths pins the exact Secret keys the
// server environment reads, plus the fixed TLS/CA paths. A rename on either
// side would otherwise surface only when a stage pod fails to start.
func TestKodamaChartSecretKeyReferencesAndTLSPaths(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama: %v\n%s", err, rendered)
	}
	deployment := renderedObjectOfKind(t, rendered, "Deployment")

	references := []struct {
		name   string
		secret string
		key    string
	}{
		{"KODAMA_DATABASE_URL", "kodama-database", "database_url"},
		{"KODAMA_CONTROLLER_DATABASE_URL", "kodama-database", "controller_url"},
		{"KODAMA_PUBLISHER_DATABASE_URL", "kodama-database", "publisher_url"},
		{"KODAMA_AUTH_DATABASE_URL", "kodama-database", "auth_url"},
		{"KODAMA_NATS_URL", "kodama-broker", "nats_url"},
	}
	for _, reference := range references {
		entry := renderedEnvEntry(t, deployment, reference.name)
		valueFrom := objectAtKey(t, entry, "valueFrom")
		secretKeyRef := objectAtKey(t, valueFrom, "secretKeyRef")
		if got := stringAtKey(t, secretKeyRef, "name"); got != reference.secret {
			t.Errorf("%s reads Secret %q, want %q", reference.name, got, reference.secret)
		}
		if got := stringAtKey(t, secretKeyRef, "key"); got != reference.key {
			t.Errorf("%s reads key %q, want %q", reference.name, got, reference.key)
		}
		if _, present := entry["value"]; present {
			t.Errorf("%s carries a literal value as well as a secretKeyRef", reference.name)
		}
	}

	fixed := map[string]string{
		"KODAMA_NATS_CA_FILE":   "/etc/kodama/broker-tls/ca.crt",
		"KODAMA_NATS_CERT_FILE": "/etc/kodama/broker-tls/tls.crt",
		"KODAMA_NATS_KEY_FILE":  "/etc/kodama/broker-tls/tls.key",
		"KODAMA_TLS_DIR":        "/etc/kodama/tls",
		"KODAMA_KEY_BLOBS_DIR":  "/etc/kodama/keys",
	}
	for name, want := range fixed {
		entry := renderedEnvEntry(t, deployment, name)
		if got := stringAtKey(t, entry, "value"); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// assertKodamaRecreateClearsRollingUpdate proves the rendered kodama strategy
// block clears rollingUpdate. The kodama Deployment has always been Recreate on
// the stage (a controller upgrade must never overlap two pods), so the null
// keeps the submitted object free of the API-server-defaulted field. It does
// not switch an existing RollingUpdate Deployment to Recreate under server-side
// apply; deploy/README.md documents the one-time operator remedy for that.
func assertKodamaRecreateClearsRollingUpdate(t *testing.T, deployment map[string]any) {
	t.Helper()
	strategy := objectAtKey(t, deployment, "spec")
	strategy = objectAtKey(t, strategy, "strategy")
	if stringAtKey(t, strategy, "type") != "Recreate" {
		t.Errorf("kodama: strategy.type = %q, want Recreate", stringAtKey(t, strategy, "type"))
	}
	rollingUpdate, present := strategy["rollingUpdate"]
	if !present || rollingUpdate != nil {
		t.Errorf("kodama: strategy.rollingUpdate = %v (present=%v), want an explicit null", rollingUpdate, present)
	}
}

// TestRecreateDeploymentsClearRollingUpdate pins the kodama Deployment: it is
// Recreate on the stage and clears rollingUpdate, so an upgrade of that
// existing object stays valid.
func TestRecreateDeploymentsClearRollingUpdate(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama: %v\n%s", err, rendered)
	}
	assertKodamaRecreateClearsRollingUpdate(t, renderedObjectOfKind(t, rendered, "Deployment"))
}

// checkOAuthSonasSinglePodStrategy verifies the rendered OAuthSonas
// Deployment's no-overlap guarantee: one replica and a RollingUpdate with
// maxSurge 0 and maxUnavailable 1. At one replica the old pod stops before the
// new one starts, the same guarantee as Recreate, without a strategy type
// change the API server refuses under server-side apply.
func checkOAuthSonasSinglePodStrategy(deployment map[string]any) error {
	spec, ok := deployment["spec"].(map[string]any)
	if !ok {
		return fmt.Errorf("Deployment has no spec object")
	}
	replicas, ok := intAtKey(spec, "replicas")
	if !ok || replicas != 1 {
		return fmt.Errorf("spec.replicas = %v, want exactly 1", spec["replicas"])
	}
	strategy, ok := spec["strategy"].(map[string]any)
	if !ok {
		return fmt.Errorf("Deployment has no spec.strategy object")
	}
	if strategyType, _ := strategy["type"].(string); strategyType != "RollingUpdate" {
		return fmt.Errorf("spec.strategy.type = %v, want RollingUpdate", strategy["type"])
	}
	rollingUpdate, ok := strategy["rollingUpdate"].(map[string]any)
	if !ok {
		return fmt.Errorf("spec.strategy.rollingUpdate is missing")
	}
	if maxSurge, ok := intAtKey(rollingUpdate, "maxSurge"); !ok || maxSurge != 0 {
		return fmt.Errorf("spec.strategy.rollingUpdate.maxSurge = %v, want 0", rollingUpdate["maxSurge"])
	}
	if maxUnavailable, ok := intAtKey(rollingUpdate, "maxUnavailable"); !ok || maxUnavailable != 1 {
		return fmt.Errorf("spec.strategy.rollingUpdate.maxUnavailable = %v, want 1", rollingUpdate["maxUnavailable"])
	}
	return nil
}

// intAtKey decodes an integer field from a rendered YAML document.
func intAtKey(object map[string]any, key string) (int, bool) {
	switch value := object[key].(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case uint64:
		return int(value), true
	case float64:
		return int(value), true
	}
	return 0, false
}

// TestOAuthSonasDeploymentStrategyIsSinglePodRollingUpdate pins the rendered
// OAuthSonas strategy block and proves each of its four values is load-bearing.
// Two mutations (replicas, maxSurge) must fail the render through the template
// guard; the other two (type, maxUnavailable) must be rejected by the rendered
// assertion.
func TestOAuthSonasDeploymentStrategyIsSinglePodRollingUpdate(t *testing.T) {
	helm, stack := stageStackChart(t)
	values := oauthSonasValuesFile(t, oauthSonasBaseValues)
	rendered, err := oauthSonasRender(t, helm, stack, "kodama-system", values)
	if err != nil {
		t.Fatalf("helm template enabled oauthsonas: %v\n%s", err, rendered)
	}
	if err := checkOAuthSonasSinglePodStrategy(renderedObjectNamed(t, rendered, "Deployment", "oauthsonas")); err != nil {
		t.Fatalf("oauthsonas Deployment: %v", err)
	}

	mutations := []struct {
		name        string
		old         string
		new         string
		renderFails bool
	}{
		{
			name: "the strategy type reverts to Recreate",
			old:  "    type: RollingUpdate\n",
			new:  "    type: Recreate\n",
		},
		{
			name: "maxSurge allows a surge",
			old:  "      maxSurge: {{ $maxSurge }}",
			new:  "      maxSurge: 1",
		},
		{
			name: "maxUnavailable stops nothing",
			old:  "      maxUnavailable: {{ $maxUnavailable }}",
			new:  "      maxUnavailable: 0",
		},
		{
			name:        "the replicas guard allows two pods",
			old:         "{{- $replicas := 1 }}",
			new:         "{{- $replicas := 2 }}",
			renderFails: true,
		},
		{
			name:        "the maxSurge guard allows a surge",
			old:         "{{- $maxSurge := 0 }}",
			new:         "{{- $maxSurge := 1 }}",
			renderFails: true,
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			helm, stack := stageStackChart(t)
			path := filepath.Join(stack, "templates", "oauthsonas.yaml")
			body := readFileForTest(t, path)
			mutated := strings.Replace(body, mutation.old, mutation.new, 1)
			if mutated == body {
				t.Fatalf("mutation %q did not change the template", mutation.name)
			}
			rewriteStagedFile(t, path, mutated)

			values := oauthSonasValuesFile(t, oauthSonasBaseValues)
			rendered, err := oauthSonasRender(t, helm, stack, "kodama-system", values)
			if mutation.renderFails {
				if err == nil {
					t.Fatalf("render accepted the mutation %q:\n%s", mutation.name, rendered)
				}
				if !strings.Contains(rendered, "must") {
					t.Errorf("guard failure for %q is not a clear message: %v\n%s", mutation.name, err, rendered)
				}
				return
			}
			if err != nil {
				t.Fatalf("render failed for the strategy mutation %q: %v\n%s", mutation.name, err, rendered)
			}
			if err := checkOAuthSonasSinglePodStrategy(renderedObjectNamed(t, rendered, "Deployment", "oauthsonas")); err == nil {
				t.Errorf("the strategy checker accepted the mutation %q", mutation.name)
			}
		})
	}
}

// TestStackChartNATSImageIsTagAndDigestPinned proves the broker image keeps
// both the tag pin and the digest, and that neither can be removed.
func TestStackChartNATSImageIsTagAndDigestPinned(t *testing.T) {
	helm, stack := stageStackChart(t)
	rendered, err := renderChartArgs(t, helm, "kodama-stack", stack, stackProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama-stack: %v\n%s", err, rendered)
	}
	statefulSet := renderedObjectOfKind(t, rendered, "StatefulSet")
	podSpec := objectAtKey(t, objectAtKey(t, statefulSet, "spec"), "template")
	pod := objectAtKey(t, podSpec, "spec")
	containers, ok := pod["containers"].([]any)
	if !ok || len(containers) == 0 {
		t.Fatalf("rendered nats StatefulSet has no containers")
	}
	image := stringAtKey(t, containers[0].(map[string]any), "image")
	if !regexp.MustCompile(`^docker\.io/library/nats:[^\s@]+@sha256:[0-9a-f]{64}$`).MatchString(image) {
		t.Errorf("rendered NATS image %q is not repository:tag@sha256:<64 hex>", image)
	}

	for _, override := range [][]string{
		{"--set", "nats.image.digest="},
		{"--set", "nats.image.tag="},
	} {
		rendered, err := renderChartArgs(t, helm, "kodama-stack", stack, stackProductionArgs(override...)...)
		if err == nil {
			t.Errorf("helm template accepted %v:\n%s", override, rendered)
		}
	}
}
