package installation

// Chart and deployment hardening.
//
// These checks pin the hardening rules that the text-level chart tests can
// express without helm: the image digest grammar, the production OIDC and KMS
// requirements, the required database CA mount, the NATS image pin, the secret
// key references the server environment reads, and the provider descriptors'
// TLS mode. The rendered behaviour of the same invariants is exercised with a
// real `helm template` in test/component/deploy.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// productionOIDCNames is the exact set cmd/kodama/production.go refuses to
// start without.
var productionOIDCNames = []string{
	"KODAMA_OIDC_ISSUER",
	"KODAMA_OIDC_AUDIENCE",
	"KODAMA_OIDC_JWKS_URL",
	"KODAMA_OIDC_TENANT_CLAIM",
}

// schemaPath resolves values.schema.json inside the kodama chart.
func schemaPath(t *testing.T) string {
	t.Helper()
	return chartPath(t, "values.schema.json")
}

// chartSchema loads the kodama chart schema.
func chartSchema(t *testing.T) map[string]any {
	t.Helper()
	return objectAt(t, loadJSON(t, schemaPath(t)), "schema")
}

// TestImageDigestSchemaAcceptsOnlyRealDigests pins the digest grammar: an
// image.digest override may be empty (the tag form) or a full lower-case
// sha256 digest, and nothing else. A short, upper-case or non-sha256 string
// would render an image reference the registry cannot resolve.
func TestImageDigestSchemaAcceptsOnlyRealDigests(t *testing.T) {
	schema := chartSchema(t)
	properties := objectAt(t, schema["properties"], "schema.properties")
	image := objectAt(t, properties["image"], "schema.properties.image")
	imageProperties := objectAt(t, image["properties"], "schema.properties.image.properties")
	digest := objectAt(t, imageProperties["digest"], "schema.properties.image.properties.digest")

	valid := []string{
		"",
		"sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("0123456789abcdef", 4),
	}
	for _, value := range valid {
		if err := validateAgainstSchema(digest, value, "image.digest"); err != nil {
			t.Errorf("schema refused a valid image digest %q: %v", value, err)
		}
	}

	invalid := []string{
		"sha256:abc",
		"sha256:" + strings.Repeat("a", 63),
		"sha256:" + strings.Repeat("a", 65),
		"sha256:" + strings.Repeat("A", 64),
		"sha256:" + strings.Repeat("g", 64),
		"sha512:" + strings.Repeat("a", 64),
		strings.Repeat("a", 64),
		" sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("a", 64) + " ",
	}
	for _, value := range invalid {
		if err := validateAgainstSchema(digest, value, "image.digest"); err == nil {
			t.Errorf("schema accepted a non-digest image.digest %q", value)
		}
	}
}

// TestProductionOIDCAndKMSRequiredBySchema pins the conditional contract: a
// production values document must carry the four OIDC variables production.go
// reads and the cloud KMS credential of its key adapter. That is the KMS
// credentials Secret for every adapter except aws-kms and gcp-kms, which have
// no static credential and instead need the workload-identity annotation on
// the ServiceAccount the chart creates. A development or test profile without
// them stays valid so the chart can render a local install.
func TestProductionOIDCAndKMSRequiredBySchema(t *testing.T) {
	schema := chartSchema(t)

	production := fqdnValues([]any{})
	if err := validateAgainstSchema(schema, production, "values"); err != nil {
		t.Fatalf("schema refused the production baseline: %v", err)
	}

	for _, name := range productionOIDCNames {
		values := fqdnValues([]any{})
		delete(extraEnvOf(values), name)
		err := validateAgainstSchema(schema, values, "values")
		if err == nil {
			t.Errorf("schema accepted production values without %s", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal for a missing %s does not name it: %v", name, err)
		}
	}

	noKMS := fqdnValues([]any{})
	delete(noKMS["secrets"].(map[string]any), "kmsCredentials")
	if err := validateAgainstSchema(schema, noKMS, "values"); err == nil {
		t.Error("schema accepted production values without secrets.kmsCredentials")
	}

	// withAdapter builds production values for one key adapter without the
	// KMS credentials Secret and with the given serviceAccount object.
	withAdapter := func(adapter string, serviceAccount map[string]any) map[string]any {
		values := fqdnValues([]any{})
		values["config"].(map[string]any)["keyAdapter"] = adapter
		delete(values["secrets"].(map[string]any), "kmsCredentials")
		if serviceAccount != nil {
			values["serviceAccount"] = serviceAccount
		}
		return values
	}
	annotated := func(name, value string) map[string]any {
		return map[string]any{"create": true, "annotations": map[string]any{name: value}}
	}
	const (
		awsAnnotation = "eks.amazonaws.com/role-arn"
		gcpAnnotation = "iam.gke.io/gcp-service-account"
		awsRole       = "arn:aws:iam::123456789012:role/kodama"
		gcpAccount    = "kodama@demo-project.iam.gserviceaccount.com"
	)
	for _, testCase := range []struct {
		name    string
		values  map[string]any
		refused string
	}{
		{name: "scw-kms without the Secret", values: withAdapter("scw-kms", nil), refused: "kmsCredentials"},
		{name: "an unknown adapter without the Secret", values: withAdapter("aws_kms", annotated(awsAnnotation, awsRole)), refused: "kmsCredentials"},
		{name: "aws-kms with the IRSA annotation", values: withAdapter("aws-kms", annotated(awsAnnotation, awsRole))},
		{name: "aws-kms without a serviceAccount object", values: withAdapter("aws-kms", nil), refused: "serviceAccount"},
		{name: "aws-kms without annotations", values: withAdapter("aws-kms", map[string]any{"create": true}), refused: "annotations"},
		{name: "aws-kms with an empty annotation", values: withAdapter("aws-kms", annotated(awsAnnotation, "")), refused: awsAnnotation},
		{name: "aws-kms with only the GKE annotation", values: withAdapter("aws-kms", annotated(gcpAnnotation, gcpAccount)), refused: awsAnnotation},
		{name: "aws-kms with an external ServiceAccount", values: withAdapter("aws-kms", map[string]any{"create": false})},
		{name: "gcp-kms with the Workload Identity annotation", values: withAdapter("gcp-kms", annotated(gcpAnnotation, gcpAccount))},
		{name: "gcp-kms without annotations", values: withAdapter("gcp-kms", map[string]any{"create": true}), refused: "annotations"},
		{name: "gcp-kms with an empty annotation", values: withAdapter("gcp-kms", annotated(gcpAnnotation, "")), refused: gcpAnnotation},
		{name: "gcp-kms with only the EKS annotation", values: withAdapter("gcp-kms", annotated(awsAnnotation, awsRole)), refused: gcpAnnotation},
		{name: "gcp-kms with an external ServiceAccount", values: withAdapter("gcp-kms", map[string]any{"create": false})},
	} {
		err := validateAgainstSchema(schema, testCase.values, "values")
		switch {
		case testCase.refused == "" && err != nil:
			t.Errorf("schema refused %s: %v", testCase.name, err)
		case testCase.refused != "" && err == nil:
			t.Errorf("schema accepted %s", testCase.name)
		case testCase.refused != "" && !strings.Contains(err.Error(), testCase.refused):
			t.Errorf("refusal for %s does not name %s: %v", testCase.name, testCase.refused, err)
		}
	}

	for _, profile := range []string{"development", "test"} {
		values := fqdnValues([]any{})
		values["environment"] = profile
		delete(extraEnvOf(values), "KODAMA_OIDC_ISSUER")
		delete(values["secrets"].(map[string]any), "kmsCredentials")
		if err := validateAgainstSchema(schema, values, "values"); err != nil {
			t.Errorf("schema refused a %s profile without production-only inputs: %v", profile, err)
		}
		values["config"].(map[string]any)["keyAdapter"] = "aws-kms"
		if err := validateAgainstSchema(schema, values, "values"); err != nil {
			t.Errorf("schema refused a %s aws-kms profile without a ServiceAccount annotation: %v", profile, err)
		}
	}
}

// TestProductionOIDCValuesMustBeNonEmpty pins the non-empty half of the
// production OIDC contract. A check that the key merely exists lets
// `KODAMA_OIDC_ISSUER: ""` render an empty environment value and fail later
// at runtime. An empty issuer, audience, JWKS URL, tenant
// claim, or discovery client id must be a named configuration refusal instead.
func TestProductionOIDCValuesMustBeNonEmpty(t *testing.T) {
	schema := chartSchema(t)
	names := append([]string{
		"KODAMA_DISCOVERY_ISSUER",
		"KODAMA_DISCOVERY_CLIENT_ID",
		"KODAMA_OIDC_SUBJECT_CLAIM",
		"KODAMA_OIDC_PROJECT_CLAIM",
	}, productionOIDCNames...)
	for _, name := range names {
		values := extraEnvValues(map[string]any{name: ""})
		err := validateAgainstSchema(schema, values, "values")
		if err == nil {
			t.Errorf("schema accepted an empty config.extraEnv.%s", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("schema refusal for an empty %s does not name it: %v", name, err)
		}
	}

	guard := extraEnvGuardBody(t)
	if !regexp.MustCompile(`eq[ \t]+\(toString[ \t]+\$value\)[ \t]+\"\"`).MatchString(guard) {
		t.Error("_extraenv.tpl has no non-empty value guard")
	}
	if !strings.Contains(guard, "must not be empty") {
		t.Error("_extraenv.tpl non-empty guard has no explanatory message")
	}
}

// TestDeploymentRequiresDatabaseCAVolume pins the database CA mount: the
// server verifies the provider certificate with /etc/kodama/pg-ca/ca.crt, so
// a missing Secret must stop the pod instead of starting it without a root.
func TestDeploymentRequiresDatabaseCAVolume(t *testing.T) {
	deployment := readText(t, chartPath(t, "templates", "deployment.yaml"))
	volume := volumeByName(deployment, "database-ca")
	if volume == "" {
		t.Fatal("deployment.yaml has no database-ca volume")
	}
	if regexp.MustCompile(`(?m)^[ \t]*optional:[ \t]*true[ \t]*$`).MatchString(volume) {
		t.Fatalf("database-ca volume is optional:\n%s", volume)
	}
	for _, want := range []string{"key: ca.crt", "path: ca.crt"} {
		if !strings.Contains(volume, want) {
			t.Errorf("database-ca volume lacks %q:\n%s", want, volume)
		}
	}
}

// volumeByName returns the YAML stanza of one named pod volume (not the
// volumeMount of the same name).
func volumeByName(body, name string) string {
	lines := strings.Split(body, "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) != "- name: "+name {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		end := len(lines)
		for scan := index + 1; scan < len(lines); scan++ {
			trimmed := strings.TrimSpace(lines[scan])
			if trimmed == "" {
				continue
			}
			current := len(lines[scan]) - len(strings.TrimLeft(lines[scan], " \t"))
			if current < indent || (current == indent && strings.HasPrefix(trimmed, "- name:")) || current == 0 {
				end = scan
				break
			}
		}
		block := strings.Join(lines[index:end], "\n")
		if strings.Contains(block, "secret:") {
			return block
		}
	}
	return ""
}

// TestKMSEnvFromIsRequiredOnlyInProduction pins the envFrom profile split:
// production reads the static cloud credential Secret (the scw-kms composition
// requires SCW_SECRET_KEY), while development and test may run without it. The
// only production exemption is the pair of workload-identity adapters, aws-kms
// and gcp-kms: the Deployment renders no envFrom for them and the guard
// requires the ServiceAccount annotation instead.
func TestKMSEnvFromIsRequiredOnlyInProduction(t *testing.T) {
	deployment := readText(t, chartPath(t, "templates", "deployment.yaml"))
	block, ok := indentedBlock(deployment, "envFrom:")
	if !ok {
		t.Fatal("deployment.yaml has no envFrom block")
	}
	requirePattern(t, block, `(?m)^[ \t]*name:[ \t]*\{\{[ \t]*\.Values\.secrets\.kmsCredentials\.existingSecret[ \t]*\}\}[ \t]*$`, "envFrom KMS credentials Secret")
	requirePattern(t, deployment, `(?ms)if ne \.Values\.environment "production".*?optional:[ \t]*true`, "envFrom optional only outside production")
	if !regexp.MustCompile(`(?ms)\{\{-[ \t]*if ne \.Values\.environment "production"[ \t]*\}\}\s*optional:[ \t]*true\s*\{\{-[ \t]*end`).MatchString(deployment) {
		t.Errorf("deployment.yaml does not gate optional: true on a non-production profile:\n%s", block)
	}

	guard := readText(t, chartPath(t, "templates", "_extraenv.tpl"))
	if !strings.Contains(guard, `if eq .Values.environment "production"`) {
		t.Error("_extraenv.tpl has no production requirement guard")
	}
	for _, name := range productionOIDCNames {
		if !strings.Contains(guard, name) {
			t.Errorf("the production guard does not require %s", name)
		}
	}
	if !strings.Contains(guard, "kmsCredentials") {
		t.Error("the production guard does not require secrets.kmsCredentials")
	}

	// The exemption names exactly the two workload-identity adapters, in the
	// Deployment and in the guard, and the guard checks their annotations.
	const exemption = `(list "aws-kms" "gcp-kms")`
	if !strings.Contains(deployment, "has .Values.config.keyAdapter "+exemption) {
		t.Error("deployment.yaml does not gate envFrom on the aws-kms/gcp-kms exemption")
	}
	if !strings.Contains(guard, "has $adapter "+exemption) {
		t.Error("the production guard does not exempt exactly aws-kms and gcp-kms from the Secret requirement")
	}
	for _, annotation := range []string{"eks.amazonaws.com/role-arn", "iam.gke.io/gcp-service-account"} {
		if !strings.Contains(guard, annotation) {
			t.Errorf("the production guard does not require the %s annotation", annotation)
		}
	}
}

// TestDeploymentSecretKeyReferencesTable pins every secretKeyRef the server
// environment depends on, plus the fixed TLS and CA paths. A key rename in the
// chart or in scripts/bootstrap-secrets would otherwise surface only when a
// stage pod fails to start.
func TestDeploymentSecretKeyReferencesTable(t *testing.T) {
	deployment := readText(t, chartPath(t, "templates", "deployment.yaml"))

	references := []struct {
		name string
		key  string
	}{
		{"KODAMA_DATABASE_URL", "database_url"},
		{"KODAMA_CONTROLLER_DATABASE_URL", "controller_url"},
		{"KODAMA_PUBLISHER_DATABASE_URL", "publisher_url"},
		{"KODAMA_AUTH_DATABASE_URL", "auth_url"},
		{"KODAMA_NATS_URL", "nats_url"},
	}
	for _, reference := range references {
		block := envEntryByName(deployment, reference.name)
		if block == "" {
			t.Errorf("deployment.yaml has no %s environment entry", reference.name)
			continue
		}
		if !regexp.MustCompile(`(?m)^[ \t]*key:[ \t]*` + regexp.QuoteMeta(reference.key) + `[ \t]*$`).MatchString(block) {
			t.Errorf("%s does not read the %q key:\n%s", reference.name, reference.key, block)
		}
		if !regexp.MustCompile(`(?m)^[ \t]*name:[ \t]*\{\{[ \t]*\.Values\.secrets\.(database|broker)\.existingSecret[ \t]*\}\}[ \t]*$`).MatchString(block) {
			t.Errorf("%s does not name its Secret through existingSecret:\n%s", reference.name, block)
		}
	}

	fixed := map[string]string{
		"KODAMA_NATS_CA_FILE":   "/etc/kodama/broker-tls/ca.crt",
		"KODAMA_NATS_CERT_FILE": "/etc/kodama/broker-tls/tls.crt",
		"KODAMA_NATS_KEY_FILE":  "/etc/kodama/broker-tls/tls.key",
		"KODAMA_TLS_DIR":        "/etc/kodama/tls",
		"KODAMA_KEY_BLOBS_DIR":  "/etc/kodama/keys",
	}
	for name, path := range fixed {
		block := envEntryByName(deployment, name)
		if block == "" {
			t.Errorf("deployment.yaml has no %s environment entry", name)
			continue
		}
		if !strings.Contains(block, path) {
			t.Errorf("%s does not carry %q:\n%s", name, path, block)
		}
	}
}

// envEntryByName returns the env entry YAML stanza for one variable.
func envEntryByName(body, name string) string {
	lines := strings.Split(body, "\n")
	start := -1
	indent := -1
	for index, line := range lines {
		if strings.TrimSpace(line) != "- name: "+name {
			continue
		}
		start = index
		indent = len(line) - len(strings.TrimLeft(line, " \t"))
		break
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for index := start + 1; index < len(lines); index++ {
		trimmed := strings.TrimSpace(lines[index])
		if trimmed == "" {
			continue
		}
		current := len(lines[index]) - len(strings.TrimLeft(lines[index], " \t"))
		if current <= indent {
			end = index
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// TestStackNATSImagePinnedByTagAndDigest pins the broker image: the stack
// chart requires both a tag and a full digest for its validation contract and
// renders repository:tag@sha256:<digest>. A tag-only override can be
// repointed by the registry.
func TestStackNATSImagePinnedByTagAndDigest(t *testing.T) {
	values := readText(t, stackChartPath(t, "values.yaml"))
	requireStackPattern(t, values, `(?m)^[ \t]*repository:[ \t]*docker\.io/library/nats[ \t]*$`, "stack values nats.image.repository")
	requireStackPattern(t, values, `(?m)^[ \t]*tag:[ \t]*"?[^"\s]+"?[ \t]*$`, "stack values nats.image.tag")
	requireStackPattern(t, values, `(?m)^[ \t]*digest:[ \t]*"?sha256:[0-9a-f]{64}"?[ \t]*$`, "stack values nats.image.digest")

	schema := objectAt(t, loadJSON(t, stackChartPath(t, "values.schema.json")), "values.schema.json")
	properties := objectAt(t, schema["properties"], "values.schema.json properties")
	nats := objectAt(t, properties["nats"], "values.schema.json nats")
	natsProperties := objectAt(t, nats["properties"], "values.schema.json nats.properties")
	image := objectAt(t, natsProperties["image"], "values.schema.json nats.properties.image")
	imageProperties := objectAt(t, image["properties"], "values.schema.json nats.properties.image.properties")

	digest := objectAt(t, imageProperties["digest"], "nats.image.digest")
	tag := objectAt(t, imageProperties["tag"], "nats.image.tag")

	validImage := map[string]any{
		"repository": "docker.io/library/nats",
		"tag":        "2.12-alpine",
		"digest":     "sha256:" + strings.Repeat("a", 64),
	}
	if err := validateAgainstSchema(image, validImage, "nats.image"); err != nil {
		t.Fatalf("schema refused a pinned NATS image: %v", err)
	}
	for _, key := range []string{"repository", "tag", "digest"} {
		if !containsString(stringsAt(t, image["required"], "nats.image.required"), key) {
			t.Errorf("nats.image does not require %q", key)
		}
	}
	for _, bad := range []string{"", "sha256:short", "latest", strings.Repeat("a", 64)} {
		if err := validateAgainstSchema(digest, bad, "nats.image.digest"); err == nil {
			t.Errorf("schema accepted nats.image.digest %q", bad)
		}
	}
	if err := validateAgainstSchema(tag, "", "nats.image.tag"); err == nil {
		t.Error("schema accepted an empty nats.image.tag")
	}

	// The OAuthSonas TLS port must be non-privileged; the container runs as a
	// non-root user.
	stackSchema := objectAt(t, loadJSON(t, stackChartPath(t, "values.schema.json")), "values.schema.json")
	stackProperties := objectAt(t, stackSchema["properties"], "values.schema.json properties")
	oauth := objectAt(t, stackProperties["oauthsonas"], "values.schema.json oauthsonas")
	oauthProperties := objectAt(t, oauth["properties"], "values.schema.json oauthsonas.properties")
	port := objectAt(t, oauthProperties["port"], "values.schema.json oauthsonas.properties.port")
	if minimum, ok := schemaNumber(port, "minimum"); !ok || minimum != 1024 {
		t.Errorf("oauthsonas.port minimum = %v, want 1024", port["minimum"])
	}

	template := readText(t, stackChartPath(t, "templates", "nats-statefulset.yaml"))
	requireStackPattern(t, template, `@\{\{[ \t]*\.Values\.nats\.image\.digest[ \t]*\}\}`, "nats-statefulset renders the digest")
	requireStackPattern(t, template, `:\{\{[ \t]*\.Values\.nats\.image\.tag[ \t]*\}\}`, "nats-statefulset renders the tag")
	if !strings.Contains(template, "must be pinned by digest") {
		t.Error("nats-statefulset has no render-time digest guard")
	}
}

// TestRecreateStrategyClearsRollingUpdate pins the kodama Deployment's
// strategy block. The kodama Deployment is Recreate (a controller upgrade
// must never overlap two pods), and `rollingUpdate: null` keeps the submitted
// object free of the field. It does not, however, switch an
// existing API-server-defaulted RollingUpdate Deployment to Recreate under Helm
// server-side apply: the defaulted spec.strategy.rollingUpdate belongs to the
// API server's field manager, so a null from Helm is a no-op and the object is
// rejected. deploy/README.md documents the one-time operator remedy (delete the
// Deployment before the upgrade) for a cluster that still has it as
// RollingUpdate.
func TestRecreateStrategyClearsRollingUpdate(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "deployment.yaml"))
	pattern := regexp.MustCompile(`(?ms)^[ \t]*strategy:[ \t]*\r?\n[ \t]*type:[ \t]*Recreate[ \t]*\r?\n[ \t]*rollingUpdate:[ \t]*null[ \t]*$`)
	if !pattern.MatchString(body) {
		t.Errorf("deployment.yaml does not render rollingUpdate: null beside strategy.type: Recreate")
	}

	// Mutation proof: removing the explicit null leaves the field defaulted on
	// an existing Recreate Deployment.
	mutated := strings.Replace(body, "    type: Recreate\n    rollingUpdate: null\n", "    type: Recreate\n", 1)
	if mutated == body {
		t.Fatal("mutation did not remove rollingUpdate: null")
	}
	if pattern.MatchString(mutated) {
		t.Fatal("the strategy checker accepted a block without rollingUpdate: null")
	}
}

// TestOAuthSonasStrategyKeepsSinglePodOverlapFree pins the OAuthSonas
// Deployment's strategy block. OAuthSonas keeps its protocol state and signing
// key in memory, so two pods must never serve at once: they would sign with
// different keys. Recreate expresses that, but a Deployment cannot be switched
// from the API server's defaulted RollingUpdate to Recreate through Helm
// server-side apply (a rendered `rollingUpdate: null` is a no-op), so the chart
// uses RollingUpdate with maxSurge 0 and maxUnavailable 1. At one replica that
// stops the old pod before the new one starts, the same no-overlap guarantee,
// and it applies over an existing RollingUpdate Deployment and on a first
// install. The template also fails the render when its replica or maxSurge
// constants drift, so the guarantee cannot rot.
func TestOAuthSonasStrategyKeepsSinglePodOverlapFree(t *testing.T) {
	body := readText(t, stackChartPath(t, "templates", "oauthsonas.yaml"))
	if err := checkOAuthSonasStrategy(body); err != nil {
		t.Fatalf("templates/oauthsonas.yaml: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "the strategy type reverts to Recreate",
			mutate: func(template string) string {
				return strings.Replace(template, "    type: RollingUpdate\n", "    type: Recreate\n", 1)
			},
		},
		{
			name: "maxSurge allows a surge",
			mutate: func(template string) string {
				return strings.Replace(template, "      maxSurge: {{ $maxSurge }}", "      maxSurge: 1", 1)
			},
		},
		{
			name: "maxUnavailable stops nothing",
			mutate: func(template string) string {
				return strings.Replace(template, "      maxUnavailable: {{ $maxUnavailable }}", "      maxUnavailable: 0", 1)
			},
		},
		{
			name: "the replicas guard allows two pods",
			mutate: func(template string) string {
				return strings.Replace(template, "if ne (int $replicas) 1", "if ne (int $replicas) 2", 1)
			},
		},
		{
			name: "the maxSurge guard allows a surge",
			mutate: func(template string) string {
				return strings.Replace(template, "if ne (int $maxSurge) 0", "if ne (int $maxSurge) 1", 1)
			},
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := mutation.mutate(body)
			if mutated == body {
				t.Fatalf("mutation %q did not change the template", mutation.name)
			}
			if err := checkOAuthSonasStrategy(mutated); err == nil {
				t.Errorf("the strategy checker accepted the mutation %q", mutation.name)
			}
		})
	}
}

// oauthSonasStrategyPattern is the exact no-overlap strategy block OAuthSonas
// must render; the constants themselves are pinned separately below.
var oauthSonasStrategyPattern = regexp.MustCompile(`(?ms)^[ \t]*strategy:[ \t]*\r?\n[ \t]*type:[ \t]*RollingUpdate[ \t]*\r?\n[ \t]*rollingUpdate:[ \t]*\r?\n[ \t]*maxSurge:[ \t]*\{\{[ \t]*\$maxSurge[ \t]*\}\}[ \t]*\r?\n[ \t]*maxUnavailable:[ \t]*\{\{[ \t]*\$maxUnavailable[ \t]*\}\}[ \t]*$`)

// checkOAuthSonasStrategy verifies the single-pod strategy and its render
// guards: one replica, RollingUpdate with maxSurge 0 and maxUnavailable 1, and
// clear failures when either constant drifts.
func checkOAuthSonasStrategy(template string) error {
	if !oauthSonasStrategyPattern.MatchString(template) {
		return fmt.Errorf("Deployment does not render strategy.type: RollingUpdate with maxSurge: 0 and maxUnavailable: 1")
	}
	if regexp.MustCompile(`(?m)^[ \t]*type:[ \t]*Recreate[ \t]*$`).MatchString(template) {
		return fmt.Errorf("Deployment strategy type is Recreate; Helm server-side apply cannot switch it from the API server's defaulted RollingUpdate")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*replicas:[ \t]*\{\{[ \t]*\$replicas[ \t]*\}\}[ \t]*$`).MatchString(template) {
		return fmt.Errorf("Deployment replicas is not the guarded $replicas value")
	}
	if !strings.Contains(template, "{{- $replicas := 1 }}") {
		return fmt.Errorf("the single replica is not pinned to 1")
	}
	if !strings.Contains(template, "{{- $maxSurge := 0 }}") {
		return fmt.Errorf("maxSurge is not pinned to 0")
	}
	if !strings.Contains(template, "{{- $maxUnavailable := 1 }}") {
		return fmt.Errorf("maxUnavailable is not pinned to 1")
	}
	if !regexp.MustCompile(`if ne \(int \$replicas\) 1`).MatchString(template) {
		return fmt.Errorf("no render guard refuses a replica count other than 1")
	}
	if !regexp.MustCompile(`if ne \(int \$maxSurge\) 0`).MatchString(template) {
		return fmt.Errorf("no render guard refuses a maxSurge other than 0")
	}
	if !strings.Contains(template, "oauthsonas replicas must be exactly 1") {
		return fmt.Errorf("the replica guard has no clear failure message")
	}
	if !strings.Contains(template, "oauthsonas rollingUpdate.maxSurge must be 0") {
		return fmt.Errorf("the maxSurge guard has no clear failure message")
	}
	return nil
}

// extraEnvOf returns config.extraEnv from a decoded values document.
func extraEnvOf(values map[string]any) map[string]any {
	config, _ := values["config"].(map[string]any)
	extra, _ := config["extraEnv"].(map[string]any)
	return extra
}
