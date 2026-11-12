//go:build component

// Rendered-chart readiness checks: the real Helm render must produce
// HTTPS httpGet probes against /healthz and /readyz on the named https port,
// with an explicit timeout, for the standalone chart, the stack chart and the
// values the descriptor generator writes for the stage. The chart schema must
// refuse a TCP mode, the wrong path and a timeout shorter than the server's
// whole readiness budget.
package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// probeInt accepts the integer representations yaml.v3 uses.
func probeInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	default:
		return 0, false
	}
}

// assertHTTPSProbes checks the server container's two probes.
func assertHTTPSProbes(t *testing.T, deployment map[string]any) {
	t.Helper()
	containers := containersOf(t, deployment)
	if len(containers) == 0 {
		t.Fatal("rendered Deployment has no containers")
	}
	container := containers[0]
	for _, probe := range []struct {
		name string
		path string
	}{
		{name: "livenessProbe", path: "/healthz"},
		{name: "readinessProbe", path: "/readyz"},
	} {
		block := objectAtKey(t, container, probe.name)
		if _, present := block["tcpSocket"]; present {
			t.Fatalf("%s renders a tcpSocket probe; the operational route must be an HTTPS httpGet", probe.name)
		}
		httpGet := objectAtKey(t, block, "httpGet")
		if got := stringAtKey(t, httpGet, "path"); got != probe.path {
			t.Fatalf("%s httpGet.path = %q; want %q", probe.name, got, probe.path)
		}
		if got := stringAtKey(t, httpGet, "port"); got != "https" {
			t.Fatalf("%s httpGet.port = %q; want the named port https", probe.name, got)
		}
		if got := stringAtKey(t, httpGet, "scheme"); got != "HTTPS" {
			t.Fatalf("%s httpGet.scheme = %q; want HTTPS", probe.name, got)
		}
		timeout, ok := probeInt(block["timeoutSeconds"])
		if !ok {
			t.Fatalf("%s has no explicit integer timeoutSeconds", probe.name)
		}
		if timeout < 5 {
			t.Fatalf("%s timeoutSeconds = %d; the readiness budget must fit below it", probe.name, timeout)
		}
	}
}

func TestKodamaChartDefaultProbesAreHTTPSHttpGet(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	helmLint(t, helm, chart, kodamaProductionArgs()...)

	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama: %v\n%s", err, rendered)
	}
	deployment := renderedObjectNamed(t, rendered, "Deployment", "kodama")
	assertHTTPSProbes(t, deployment)
}

func TestKodamaChartRefusesInvalidProbeInputs(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	cases := []struct {
		name string
		args []string
	}{
		{name: "tcpSocket mode", args: []string{"--set", "probes.mode=tcpSocket"}},
		{name: "wrong liveness path", args: []string{"--set", "probes.livenessPath=/live"}},
		{name: "wrong readiness path", args: []string{"--set", "probes.readinessPath=/ready"}},
		{name: "timeout below the readiness budget", args: []string{"--set", "probes.timeoutSeconds=4"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			args := append(kodamaProductionArgs(), testCase.args...)
			rendered, err := renderChartArgs(t, helm, "kodama", chart, args...)
			if err == nil {
				t.Fatalf("helm accepted %s:\n%s", testCase.name, rendered)
			}
			if !strings.Contains(rendered, "probes") && !strings.Contains(rendered, "healthz") &&
				!strings.Contains(rendered, "readyz") && !strings.Contains(rendered, "timeoutSeconds") {
				t.Fatalf("render refused %s for an unrelated reason: %s", testCase.name, rendered)
			}
		})
	}
}

func TestStackChartDefaultProbesAreHTTPSHttpGet(t *testing.T) {
	helm, stack := stageStackChart(t)
	rendered, err := renderChartArgs(t, helm, "kodama-stack", stack, stackProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama-stack: %v\n%s", err, rendered)
	}
	deployment := renderedObjectNamed(t, rendered, "Deployment", "kodama")
	assertHTTPSProbes(t, deployment)
}

// TestDescriptorValuesRenderHTTPSHttpGetProbes runs the real descriptor
// generator (scripts/lib/deploy.sh through scripts/deploy render-values) and
// renders the stack chart with the exact values file the stage deploy uses.
func TestDescriptorValuesRenderHTTPSHttpGetProbes(t *testing.T) {
	helm, stack := stageStackChart(t)

	box := newDeploySandbox(t)
	code, output := box.run(t, "render-values", "--cluster", "stage")
	if code != 0 {
		t.Fatalf("scripts/deploy render-values failed: exit %d\n%s", code, output)
	}
	valuesPath := filepath.Join(box.workDir, "values.generated.yaml")
	body, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("read rendered values: %v", err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode rendered values: %v\n%s", err, body)
	}
	kodama, ok := document["kodama"].(map[string]any)
	if !ok {
		t.Fatalf("rendered values have no kodama block:\n%s", body)
	}
	probes, ok := kodama["probes"].(map[string]any)
	if !ok {
		t.Fatalf("rendered values have no kodama.probes block:\n%s", body)
	}
	if probes["mode"] != "httpGet" {
		t.Fatalf("descriptor generator emitted probes.mode = %v; want httpGet", probes["mode"])
	}
	if probes["livenessPath"] != "/healthz" || probes["readinessPath"] != "/readyz" {
		t.Fatalf("descriptor generator emitted paths %v/%v; want /healthz and /readyz", probes["livenessPath"], probes["readinessPath"])
	}
	if timeout, ok := probeInt(probes["timeoutSeconds"]); !ok || timeout != 10 {
		t.Fatalf("descriptor generator emitted timeoutSeconds = %v; want 10", probes["timeoutSeconds"])
	}

	args := append(stackProductionArgs(), "--values", valuesPath)
	rendered, err := renderChartArgs(t, helm, "kodama-stack", stack, args...)
	if err != nil {
		t.Fatalf("helm template kodama-stack with descriptor values: %v\n%s", err, rendered)
	}
	deployment := renderedObjectNamed(t, rendered, "Deployment", "kodama")
	assertHTTPSProbes(t, deployment)
}
