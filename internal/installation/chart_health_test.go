package installation

// Readiness chart checks: the default and descriptor-generated probes
// are HTTPS httpGet probes against the served operational routes, with an
// explicit timeout below which the server's whole readiness budget must fit.
//
// The chart is inspected as text and through the bounded JSON-Schema subset,
// like the other chart tests: no helm binary, template engine or YAML
// dependency. The rendered form of the same invariants is checked in
// test/component/deploy with a real `helm template`.

import (
	"path/filepath"
	"strings"
	"testing"
)

// healthYAMLBlock returns the indented body of the first line equal to header.
func healthYAMLBlock(t *testing.T, body, header string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	start := -1
	indent := 0
	for index, line := range lines {
		if strings.TrimSpace(line) == header {
			start = index
			indent = len(line) - len(strings.TrimLeft(line, " \t"))
			break
		}
	}
	if start < 0 {
		t.Fatalf("no %q block in the chart source", header)
	}
	var block []string
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			block = append(block, line)
			continue
		}
		current := len(line) - len(strings.TrimLeft(line, " \t"))
		if current <= indent {
			break
		}
		block = append(block, line)
	}
	return strings.Join(block, "\n")
}

// TestProbeDefaultsAreHTTPSHttpGet pins the shipped default values: the stage
// and every fresh install must render HTTPS httpGet probes, not a TCP connect
// that would report ready while every dependency is down.
func TestProbeDefaultsAreHTTPSHttpGet(t *testing.T) {
	values := readText(t, chartPath(t, "values.yaml"))
	block := healthYAMLBlock(t, values, "probes:")
	for _, want := range []string{
		"mode: httpGet",
		"livenessPath: /healthz",
		"readinessPath: /readyz",
		"timeoutSeconds: 10",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("values.yaml probes block does not contain %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "tcpSocket") {
		t.Fatalf("values.yaml still defaults to tcpSocket probes:\n%s", block)
	}
}

// TestDeploymentProbesAreHTTPSHttpGet pins the Deployment template: both
// probes are httpGet on the named https port with scheme HTTPS, the exact
// operational paths, an explicit timeoutSeconds and no TCP fallback branch.
func TestDeploymentProbesAreHTTPSHttpGet(t *testing.T) {
	bodies := templateBodies(t)
	name, body := templateContaining(t, bodies, "livenessProbe:")
	if strings.Contains(body, "tcpSocket") {
		t.Fatalf("%s still renders a tcpSocket probe:\n%s", name, body)
	}
	if !strings.Contains(body, "probes.mode must be httpGet") {
		t.Fatalf("%s has no template-level guard refusing a non-httpGet probe mode", name)
	}

	for _, probe := range []struct {
		header string
		path   string
	}{
		{header: "livenessProbe:", path: `livenessPath | default "/healthz"`},
		{header: "readinessProbe:", path: `readinessPath | default "/readyz"`},
	} {
		block := healthYAMLBlock(t, body, probe.header)
		for _, want := range []string{
			"httpGet:",
			"port: https",
			"scheme: HTTPS",
			"timeoutSeconds:",
			probe.path,
		} {
			if !strings.Contains(block, want) {
				t.Fatalf("%s %s block does not contain %q:\n%s", name, probe.header, want, block)
			}
		}
	}
}

// TestProbeSchemaIsClosed pins the schema shape: probe values are not an open
// object, the mode is httpGet only, the paths are the served routes
// and the timeout is an explicit bounded integer.
func TestProbeSchemaIsClosed(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	properties := objectAt(t, schema["properties"], "schema.properties")
	probes := objectAt(t, properties["probes"], "schema.properties.probes")

	if allowed, ok := probes["additionalProperties"].(bool); !ok || allowed {
		t.Fatalf("schema.properties.probes additionalProperties = %v; want false", probes["additionalProperties"])
	}
	required := stringsAt(t, probes["required"], "schema.properties.probes.required")
	for _, name := range []string{"mode", "livenessPath", "readinessPath", "timeoutSeconds"} {
		if !containsString(required, name) {
			t.Fatalf("schema.properties.probes.required = %v; want %q", required, name)
		}
	}
	probeProperties := objectAt(t, probes["properties"], "schema.properties.probes.properties")
	mode := objectAt(t, probeProperties["mode"], "probes.properties.mode")
	if got := stringsAt(t, mode["enum"], "probes.properties.mode.enum"); len(got) != 1 || got[0] != "httpGet" {
		t.Fatalf("probes.mode enum = %v; want exactly httpGet", got)
	}
	for name, want := range map[string]string{
		"livenessPath":  "/healthz",
		"readinessPath": "/readyz",
	} {
		path := objectAt(t, probeProperties[name], "probes.properties."+name)
		if path["const"] != want {
			t.Fatalf("probes.%s const = %v; want %q", name, path["const"], want)
		}
	}
	timeout := objectAt(t, probeProperties["timeoutSeconds"], "probes.properties.timeoutSeconds")
	if timeout["type"] != "integer" {
		t.Fatalf("probes.timeoutSeconds type = %v; want integer", timeout["type"])
	}
	minimum, ok := schemaInt(timeout, "minimum")
	if !ok || minimum < 5 {
		t.Fatalf("probes.timeoutSeconds minimum = %v; want at least 5", timeout["minimum"])
	}
}

// probeFixture loads the valid values fixture and adds a complete probes block.
func probeFixture(t *testing.T) map[string]any {
	t.Helper()
	values := objectAt(t, loadJSON(t, filepath.Join("testdata", "chart", "values.valid.json")), "values")
	values["probes"] = map[string]any{
		"mode":           "httpGet",
		"livenessPath":   "/healthz",
		"readinessPath":  "/readyz",
		"timeoutSeconds": float64(10),
	}
	return values
}

// TestProbeSchemaRefusesInvalidInputs proves the closed schema rejects a TCP
// mode, a path the server does not serve, a timeout that cannot exceed the
// application's whole readiness budget and unknown probe keys.
func TestProbeSchemaRefusesInvalidInputs(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	if err := validateAgainstSchema(schema, probeFixture(t), "values"); err != nil {
		t.Fatalf("schema refused the valid probe fixture: %v", err)
	}

	// Omitting probes entirely keeps the chart defaults, which are valid.
	withoutProbes := objectAt(t, loadJSON(t, filepath.Join("testdata", "chart", "values.valid.json")), "values")
	if err := validateAgainstSchema(schema, withoutProbes, "values"); err != nil {
		t.Fatalf("schema refused values without a probes block (chart defaults): %v", err)
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "tcpSocket mode", mutate: func(values map[string]any) { values["probes"].(map[string]any)["mode"] = "tcpSocket" }},
		{name: "wrong liveness path", mutate: func(values map[string]any) { values["probes"].(map[string]any)["livenessPath"] = "/live" }},
		{name: "wrong readiness path", mutate: func(values map[string]any) { values["probes"].(map[string]any)["readinessPath"] = "/ready" }},
		{name: "timeout below the readiness budget", mutate: func(values map[string]any) { values["probes"].(map[string]any)["timeoutSeconds"] = float64(4) }},
		{name: "missing timeout", mutate: func(values map[string]any) { delete(values["probes"].(map[string]any), "timeoutSeconds") }},
		{name: "unknown probe key", mutate: func(values map[string]any) { values["probes"].(map[string]any)["periodSeconds"] = float64(10) }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values := probeFixture(t)
			testCase.mutate(values)
			err := validateAgainstSchema(schema, values, "values")
			if err == nil {
				t.Fatalf("schema accepted %s", testCase.name)
			}
			if !strings.Contains(err.Error(), "probes") {
				t.Fatalf("schema rejected %s for the wrong reason: %v", testCase.name, err)
			}
		})
	}
}

// TestDeployGeneratorEmitsHTTPSHttpGetProbes pins the descriptor producer in
// scripts/lib/deploy.sh. The generator writes an explicit probe mode, so a
// tcpSocket mode there would keep TCP probes regardless of the chart defaults.
func TestDeployGeneratorEmitsHTTPSHttpGetProbes(t *testing.T) {
	script := readText(t, filepath.Join(repoRoot(t), "scripts", "lib", "deploy.sh"))
	block := healthYAMLBlock(t, script, "probes: {")
	for _, want := range []string{
		`mode: "httpGet"`,
		`livenessPath: "/healthz"`,
		`readinessPath: "/readyz"`,
		`timeoutSeconds: 10`,
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("scripts/lib/deploy.sh probes block does not contain %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "tcpSocket") {
		t.Fatalf("scripts/lib/deploy.sh still generates tcpSocket probes:\n%s", block)
	}
}

// TestDeployReadmeDocumentsProbes pins the operator contract: the probes are
// HTTPS httpGet against the served routes, the kubelet does not verify the
// server certificate for httpGet, and dependency TLS remains verified.
func TestDeployReadmeDocumentsProbes(t *testing.T) {
	readme := readText(t, filepath.Join(repoRoot(t), "deploy", "README.md"))
	for _, want := range []string{
		"/healthz",
		"/readyz",
		"httpGet",
		"timeoutSeconds",
		"HTTPS",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("deploy/README.md does not document %q", want)
		}
	}
	lower := strings.ToLower(readme)
	if !strings.Contains(lower, "kubelet") || !strings.Contains(lower, "certificate") {
		t.Fatal("deploy/README.md does not state that the kubelet's HTTPS httpGet probe does not verify the server certificate")
	}
	if !strings.Contains(lower, "dependency tls") && !strings.Contains(lower, "database, broker") {
		t.Fatal("deploy/README.md does not state that dependency TLS stays verified")
	}
}
