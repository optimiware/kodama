package installation

// Deployment asset tests. The Helm chart is inspected as text and as a
// bounded JSON-Schema subset; no helm binary, template engine, YAML decoder,
// cluster or network access is used. The chart must deploy one control-plane
// replica, per-tenant namespaces, default-deny tenant networking, non-root
// workloads with resource limits, non-replaying Jobs and secrets by reference.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestChartMetadata(t *testing.T) {
	body := readText(t, chartPath(t, "Chart.yaml"))
	requirePattern(t, body, `(?m)^apiVersion:\s*v2\s*$`, "Chart.yaml apiVersion")
	requirePattern(t, body, `(?m)^name:\s*kodama\s*$`, "Chart.yaml name")
	requirePattern(t, body, `(?m)^version:\s*"?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?"?\s*$`, "Chart.yaml version")
	requirePattern(t, body, `(?m)^appVersion:\s*\S+`, "Chart.yaml appVersion")
}

func TestValuesDeclareSchemaKeys(t *testing.T) {
	body := readText(t, chartPath(t, "values.yaml"))
	declared := make(map[string]bool)
	for _, match := range regexp.MustCompile(`(?m)^([a-z][A-Za-z0-9]*):`).FindAllStringSubmatch(body, -1) {
		declared[match[1]] = true
	}
	for _, key := range chartRequiredKeys {
		if !declared[key] {
			t.Errorf("values.yaml does not declare required key %q", key)
		}
	}
	requirePattern(t, body, `(?m)^replicaCount:\s*1\s*$`, "values.yaml replicaCount")
	requirePattern(t, body, `(?m)^environment:\s*"?(development|test|production)"?\s*$`, "values.yaml environment")
}

func TestValuesReferenceSecretsOnly(t *testing.T) {
	values := readText(t, chartPath(t, "values.yaml"))
	if !strings.Contains(values, "existingSecret:") {
		t.Error("values.yaml must reference an existing Kubernetes Secret")
	}
	if line := firstSecretLiteral(values); line != "" {
		t.Fatalf("values.yaml carries a literal secret value: %s", line)
	}

	bodies := templateBodies(t)
	references := 0
	for name, body := range bodies {
		if strings.Contains(body, "kind: Secret") {
			t.Errorf("%s defines a Secret object; secrets are referenced, not shipped", name)
		}
		if strings.Contains(body, "stringData:") {
			t.Errorf("%s contains stringData; secrets are referenced, not shipped", name)
		}
		if strings.Contains(body, "secretKeyRef") || strings.Contains(body, "secretRef:") {
			references++
		}
	}
	if references == 0 {
		t.Error("no template references a Secret by name")
	}

	for _, name := range []string{"values.yaml", "values.schema.json", "Chart.yaml"} {
		if strings.Contains(readText(t, chartPath(t, name)), "hunter2") {
			t.Errorf("%s contains a fixture secret value", name)
		}
	}
}

// firstSecretLiteral returns the first values.yaml line that looks like a
// literal credential value.
func firstSecretLiteral(body string) string {
	keyPattern := regexp.MustCompile(`(?i)^[a-z0-9_.-]*(password|passwd|token|secretvalue|secretkey|apikey|privatekey|accesskey|credential)$`)
	skip := map[string]bool{
		"secrets":        true,
		"existingsecret": true,
		"secretref":      true,
		"secretkeyref":   true,
	}
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		before, after, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(before))
		if skip[key] || !keyPattern.MatchString(key) {
			continue
		}
		value := strings.TrimSpace(after)
		value = strings.TrimSuffix(value, ",")
		value = strings.TrimSpace(value)
		if value == "" || value == `""` || value == "''" || strings.HasPrefix(value, "${") || strings.HasPrefix(value, "{") {
			continue
		}
		return trimmed
	}
	return ""
}

func TestValuesSchemaShape(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schema["type"])
	}
	if allowed, ok := schema["additionalProperties"].(bool); !ok || allowed {
		t.Fatalf("schema additionalProperties = %v, want false", schema["additionalProperties"])
	}

	required := stringsAt(t, schema["required"], "schema.required")
	if got, want := sortedCopy(required), sortedCopy(chartRequiredKeys); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("schema.required = %v, want %v", got, want)
	}

	properties := objectAt(t, schema["properties"], "schema.properties")

	environment := objectAt(t, properties["environment"], "schema.properties.environment")
	if got, want := sortedCopy(stringsAt(t, environment["enum"], "environment.enum")), sortedCopy([]string{"development", "test", "production"}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("environment enum = %v, want %v", got, want)
	}

	replicas := objectAt(t, properties["replicaCount"], "schema.properties.replicaCount")
	if replicas["type"] != "integer" {
		t.Errorf("replicaCount type = %v", replicas["type"])
	}
	if minimum, ok := schemaNumber(replicas, "minimum"); !ok || minimum != 1 {
		t.Errorf("replicaCount minimum = %v, want 1", replicas["minimum"])
	}
	if maximum, ok := schemaNumber(replicas, "maximum"); !ok || maximum != 1 {
		t.Errorf("replicaCount maximum = %v, want 1", replicas["maximum"])
	}

	secrets := objectAt(t, properties["secrets"], "schema.properties.secrets")
	if secrets["type"] != "object" {
		t.Errorf("secrets type = %v", secrets["type"])
	}
	reference := objectAt(t, secrets["additionalProperties"], "secrets.additionalProperties")
	if !containsString(stringsAt(t, reference["required"], "secrets.reference.required"), "existingSecret") {
		t.Error("secrets entries must require existingSecret")
	}
	if allowed, ok := reference["additionalProperties"].(bool); !ok || allowed {
		t.Errorf("secrets entry additionalProperties = %v, want false", reference["additionalProperties"])
	}

	tenants := objectAt(t, properties["tenants"], "schema.properties.tenants")
	if tenants["type"] != "array" {
		t.Errorf("tenants type = %v", tenants["type"])
	}
	tenantItems := objectAt(t, tenants["items"], "tenants.items")
	if tenantItems["type"] != "string" {
		t.Errorf("tenants.items type = %v", tenantItems["type"])
	}
	if minimum, ok := schemaInt(tenantItems, "minLength"); !ok || minimum < 1 {
		t.Errorf("tenants.items minLength = %v, want at least 1", tenantItems["minLength"])
	}

	for _, name := range []string{"image", "config", "networkPolicy", "securityContext", "resources"} {
		property := objectAt(t, properties[name], "schema.properties."+name)
		if property["type"] != "object" {
			t.Errorf("%s type = %v, want object", name, property["type"])
		}
		if required, ok := property["required"].([]any); ok && len(required) > 0 {
			t.Errorf("%s must not require properties: %v", name, required)
		}
	}
}

func TestValuesSchemaFixtures(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	fixtures := filepath.Join("testdata", "chart")

	valid := loadJSON(t, filepath.Join(fixtures, "values.valid.json"))
	if err := validateAgainstSchema(schema, valid, "values"); err != nil {
		t.Fatalf("schema refused the valid fixture: %v", err)
	}

	for _, testCase := range []struct {
		file string
		want string
	}{
		{file: "values.invalid-secret.json", want: "secrets"},
		{file: "values.invalid-replicas.json", want: "replicaCount"},
		{file: "values.invalid-networkpolicy.json", want: "networkPolicy"},
	} {
		t.Run(testCase.file, func(t *testing.T) {
			document := loadJSON(t, filepath.Join(fixtures, testCase.file))
			err := validateAgainstSchema(schema, document, "values")
			if err == nil {
				t.Fatalf("schema accepted %s", testCase.file)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("schema rejected %s for the wrong reason: %v", testCase.file, err)
			}
		})
	}
}

func TestControllerSingleReplica(t *testing.T) {
	bodies := templateBodies(t)
	name, body := templateContaining(t, bodies, "kind: Deployment")
	if !regexp.MustCompile(`(?m)^\s*replicas:\s*(\{\{\s*\.Values\.replicaCount\s*\}\}|1)\s*$`).MatchString(body) {
		t.Fatalf("%s does not deploy exactly the configured single replica", name)
	}
}

func TestTenantNamespaceTemplate(t *testing.T) {
	bodies := templateBodies(t)
	name, body := templateContaining(t, bodies, "kind: Namespace")
	requirePattern(t, body, `range\s+.*\.Values\.tenants`, name+" tenant range")
	requirePattern(t, body, `kodama\.dev/tenant`, name+" tenant label")
	requirePattern(t, body, `(?m)^\s*kind:\s*Namespace\s*$`, name+" Namespace kind")
}

func TestDefaultDenyNetworkPolicy(t *testing.T) {
	bodies := templateBodies(t)
	name, body := templateContaining(t, bodies, "kind: NetworkPolicy")
	requirePattern(t, body, `(?m)^\s*podSelector:\s*\{\s*\}\s*$`, name+" default-deny pod selector")
	requirePattern(t, body, `policyTypes:`, name+" policy types")
	requirePattern(t, body, `Ingress`, name+" ingress policy type")
	requirePattern(t, body, `Egress`, name+" egress policy type")
	requirePattern(t, body, `ports:`, name+" permitted ports")

	lower := strings.ToLower(body)
	for _, reachable := range []string{"kodama", "git"} {
		if !strings.Contains(lower, reachable) {
			t.Errorf("%s does not permit reachability to %s", name, reachable)
		}
	}
}

func TestWorkloadSecurityAndResources(t *testing.T) {
	bodies := templateBodies(t)
	name, body := templateContaining(t, bodies, "kind: Deployment")
	for _, want := range []string{
		"runAsNonRoot: true",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"seccompProfile:",
		"type: RuntimeDefault",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s does not set %q", name, want)
		}
	}
	requirePattern(t, body, `(?m)^\s*limits:\s*$`, name+" resource limits")
	requirePattern(t, body, `(?m)^\s*cpu:`, name+" cpu limit")
	requirePattern(t, body, `(?m)^\s*memory:`, name+" memory limit")
}

func TestControlPlaneServiceSchemaAndClusterIPGuard(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	properties := objectAt(t, schema["properties"], "schema.properties")
	service := objectAt(t, properties["service"], "schema.properties.service")

	if service["type"] != "object" {
		t.Fatalf("service type = %v, want object", service["type"])
	}
	if allowed, ok := service["additionalProperties"].(bool); !ok || allowed {
		t.Fatalf("service additionalProperties = %v, want false", service["additionalProperties"])
	}

	serviceProps := objectAt(t, service["properties"], "service.properties")
	serviceType := objectAt(t, serviceProps["type"], "service.properties.type")
	if serviceType["type"] != "string" || serviceType["const"] != "ClusterIP" {
		t.Fatalf("service.type schema = %v, want string const ClusterIP", serviceType)
	}
	for _, portKey := range []string{"port", "targetPort"} {
		portSchema := objectAt(t, serviceProps[portKey], "service.properties."+portKey)
		if portSchema["type"] != "integer" {
			t.Errorf("service.%s type = %v, want integer", portKey, portSchema["type"])
		}
		if minimum, ok := schemaInt(portSchema, "minimum"); !ok || minimum != 1 {
			t.Errorf("service.%s minimum = %v, want 1", portKey, portSchema["minimum"])
		}
		if maximum, ok := schemaInt(portSchema, "maximum"); !ok || maximum != 65535 {
			t.Errorf("service.%s maximum = %v, want 65535", portKey, portSchema["maximum"])
		}
	}
	annotations := objectAt(t, serviceProps["annotations"], "service.properties.annotations")
	if annotations["type"] != "object" {
		t.Errorf("service.annotations type = %v, want object", annotations["type"])
	}

	valid := map[string]any{
		"type":        "ClusterIP",
		"port":        float64(8443),
		"targetPort":  float64(8443),
		"annotations": map[string]any{"kodama.dev/internal": "true"},
	}
	if err := validateAgainstSchema(service, valid, "service"); err != nil {
		t.Fatalf("schema refused valid ClusterIP service: %v", err)
	}
	for name, invalid := range map[string]map[string]any{
		"NodePort":      {"type": "NodePort"},
		"LoadBalancer":  {"type": "LoadBalancer"},
		"ExternalName":  {"type": "ExternalName"},
		"zero port":     {"port": float64(0)},
		"high port":     {"port": float64(65536)},
		"zero target":   {"targetPort": float64(0)},
		"high target":   {"targetPort": float64(65536)},
		"unknown field": {"nodePort": float64(30000)},
	} {
		if err := validateAgainstSchema(service, invalid, "service"); err == nil {
			t.Errorf("schema accepted invalid service (%s): %v", name, invalid)
		}
	}

	serviceBody := templateBodies(t)["service.yaml"]
	requirePattern(t, serviceBody, `(?m)^[ \t]*type:[ \t]*ClusterIP[ \t]*$`, "service.yaml hardcoded ClusterIP")
	if !strings.Contains(serviceBody, `hasKey .Values.service "type"`) || !strings.Contains(serviceBody, `ne (toString .Values.service.type) "ClusterIP"`) || !strings.Contains(serviceBody, "fail ") {
		t.Errorf("service.yaml lacks a render-time fail guard when service.type is not ClusterIP:\n%s", serviceBody)
	}
}

func TestOptionalCapabilitiesRemovedAndRefused(t *testing.T) {
	for _, path := range []string{
		chartPath(t, "values.yaml"),
		filepath.Join(repoRoot(t), "deploy", "charts", "kodama-stack", "values.yaml"),
	} {
		if strings.Contains(readText(t, path), "optionalCapabilities") {
			t.Errorf("%s still declares silent no-op optionalCapabilities", path)
		}
	}

	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	valid := objectAt(t, loadJSON(t, filepath.Join("testdata", "chart", "values.valid.json")), "values")
	config := objectAt(t, valid["config"], "values.config")
	config["optionalCapabilities"] = []any{}
	err := validateAgainstSchema(schema, valid, "values")
	if err == nil {
		t.Fatal("values.schema.json accepted config.optionalCapabilities")
	}
	if !strings.Contains(err.Error(), "optionalCapabilities") {
		t.Fatalf("values.schema.json rejected config.optionalCapabilities for the wrong reason: %v", err)
	}

	_, deployBody := templateContaining(t, templateBodies(t), "kind: Deployment")
	if !strings.Contains(deployBody, `hasKey .Values.config "optionalCapabilities"`) || !strings.Contains(deployBody, "config.optionalCapabilities") {
		t.Errorf("deployment.yaml lacks a render-time fail guard against config.optionalCapabilities")
	}
}

// TestNoReplayingJobTemplates keeps the non-replay rule for any Job the chart
// ships, and pins the absence of the placeholder installation-check Job (see
// chart_hook_test.go).
func TestNoReplayingJobTemplates(t *testing.T) {
	bodies := templateBodies(t)
	if _, ok := bodies["job.yaml"]; ok {
		t.Fatal("templates/job.yaml is back; the chart must not ship a placeholder pre-install Job")
	}

	for templateName, templateBody := range bodies {
		for _, expression := range []string{
			`(?m)^\s*kind:\s*(Job|CronJob)\s*$`,
			`restartPolicy:\s*(OnFailure|Always)`,
			`(?m)^\s*backoffLimit:\s*[1-9]`,
		} {
			if regexp.MustCompile(expression).MatchString(templateBody) {
				t.Errorf("%s matches forbidden Job/replay pattern %s", templateName, expression)
			}
		}
	}
}

func TestControlPlaneServiceAndRBACAndStorage(t *testing.T) {
	bodies := templateBodies(t)

	serviceBody := bodies["service.yaml"]
	if serviceBody == "" {
		t.Fatal("missing templates/service.yaml")
	}
	requirePattern(t, serviceBody, `(?m)^\s*kind:\s*Service\s*$`, "service.yaml Service kind")
	requirePattern(t, serviceBody, `(?m)^\s*name:\s*kodama\s*$`, "service.yaml service name")
	requirePattern(t, serviceBody, `(?m)^\s*port:\s*`, "service.yaml service port")

	saBody := bodies["serviceaccount.yaml"]
	if saBody == "" {
		t.Fatal("missing templates/serviceaccount.yaml")
	}
	requirePattern(t, saBody, `(?m)^\s*kind:\s*ServiceAccount\s*$`, "serviceaccount.yaml ServiceAccount kind")

	rbacBody := bodies["rbac.yaml"]
	if rbacBody == "" {
		t.Fatal("missing templates/rbac.yaml")
	}
	requirePattern(t, rbacBody, `range\s+\$tenant\s*:=\s*\.Values\.tenants`, "rbac.yaml tenant Role range")
	requirePattern(t, rbacBody, `pods/log`, "rbac.yaml pods/log permission")
	requirePattern(t, rbacBody, `persistentvolumeclaims`, "rbac.yaml PVC permission")
	requirePattern(t, rbacBody, `(?m)^\s*kind:\s*RoleBinding\s*$`, "rbac.yaml RoleBinding kind")

	pvcBody := bodies["pvc.yaml"]
	if pvcBody == "" {
		t.Fatal("missing templates/pvc.yaml")
	}
	requirePattern(t, pvcBody, `(?m)^\s*kind:\s*PersistentVolumeClaim\s*$`, "pvc.yaml PVC kind")
	requirePattern(t, pvcBody, `(?m)^\s*name:\s*kodama-retained-logs\s*$`, "pvc.yaml retained logs claim")

	deployName, deployBody := templateContaining(t, bodies, "kind: Deployment")
	requirePattern(t, deployBody, `livenessProbe:`, deployName+" liveness probe")
	requirePattern(t, deployBody, `readinessProbe:`, deployName+" readiness probe")
}

func TestMultiCloudDeployStructure(t *testing.T) {
	root := repoRoot(t)

	stackChart := readText(t, filepath.Join(root, "deploy", "charts", "kodama-stack", "Chart.yaml"))
	requirePattern(t, stackChart, `(?m)^name:\s*kodama-stack\s*$`, "kodama-stack Chart.yaml name")
	requirePattern(t, stackChart, `repository:\s*"file://\.\./kodama"`, "kodama-stack subchart dependency")

	contextVars := readText(t, filepath.Join(root, "deploy", "modules", "context", "variables.tf"))
	requirePattern(t, contextVars, `variable\s+"cluster_name"`, "context module cluster_name variable")
	requirePattern(t, contextVars, `default\s*=\s*"stage"`, "context module default cluster_name")

	for _, target := range []struct {
		cloud      string
		keyAdapter string
	}{
		{cloud: "scw", keyAdapter: "scaleway_key_manager_key"},
		{cloud: "gcp", keyAdapter: "google_kms_crypto_key"},
		{cloud: "aws", keyAdapter: "aws_kms_key"},
	} {
		targetDir := filepath.Join(root, "deploy", target.cloud)
		for _, requiredFile := range []string{
			"versions.tf",
			"backend.tf",
			"providers.tf",
			"variables.tf",
			"network.tf",
			"kubernetes.tf",
			"postgres.tf",
			"kms.tf",
			"outputs.tf",
			filepath.Join("bootstrap", "main.tf"),
			filepath.Join("clusters", "stage.tfvars.json.example"),
			filepath.Join("clusters", "preview.tfvars.json.example"),
		} {
			body := readText(t, filepath.Join(targetDir, requiredFile))
			if strings.TrimSpace(body) == "" {
				t.Errorf("deploy/%s/%s is empty", target.cloud, requiredFile)
			}
		}

		varsBody := readText(t, filepath.Join(targetDir, "variables.tf"))
		requirePattern(t, varsBody, `default\s*=\s*"stage"`, target.cloud+" default cluster_name")

		kmsBody := readText(t, filepath.Join(targetDir, "kms.tf"))
		requirePattern(t, kmsBody, target.keyAdapter, target.cloud+" KMS resource")

		outputsBody := readText(t, filepath.Join(targetDir, "outputs.tf"))
		requirePattern(t, outputsBody, `output\s+"deployment_descriptor"`, target.cloud+" deployment_descriptor output")
	}

	for _, script := range []string{"deploy", "bootstrap-state", "bootstrap-secrets"} {
		body := readText(t, filepath.Join(root, "scripts", script))
		if !strings.HasPrefix(body, "#!/usr/bin/env bash") {
			t.Errorf("scripts/%s does not start with bash shebang", script)
		}
	}
}

func TestDeployREADMELinks(t *testing.T) {
	path := filepath.Join(repoRoot(t), "deploy", "README.md")
	body := readText(t, path)
	lower := strings.ToLower(body)
	for _, want := range []string{"prerequisite", "aws", "gcp", "scaleway", "rpo", "rto"} {
		if !strings.Contains(lower, want) {
			t.Errorf("deploy/README.md does not document %q", want)
		}
	}

	links := regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`).FindAllStringSubmatch(body, -1)
	if len(links) == 0 {
		t.Fatal("deploy/README.md has no links")
	}
	base := filepath.Dir(path)
	for _, match := range links {
		target := match[1]
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
			continue
		}
		if index := strings.Index(target, "#"); index >= 0 {
			target = target[:index]
		}
		if target == "" {
			continue
		}
		resolved := filepath.Join(base, filepath.FromSlash(target))
		if _, err := os.Stat(resolved); err != nil {
			t.Errorf("deploy/README.md link %q does not resolve: %v", match[1], err)
		}
	}
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}
