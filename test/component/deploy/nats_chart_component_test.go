//go:build component

// Rendered-chart checks for the kodama-stack broker authentication. The
// installation unit tests inspect the chart as text, so a template edit that
// wraps `verify: true` in a flag or makes the fail-closed guard conditional
// would keep them green while `helm template` renders an unauthenticated
// broker. These tests render the real chart with helm.
//
// The chart is copied into a test-owned temporary directory and its file
// dependency is built there, so no Chart.lock or charts/*.tgz is written into
// the repository. helm runs locally; nothing contacts a cluster or network.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stackChartFile resolves one path inside deploy/charts/kodama-stack.
func stackChartFile(t *testing.T, parts ...string) string {
	t.Helper()
	all := append([]string{repoRoot(t), "deploy", "charts", "kodama-stack"}, parts...)
	return filepath.Join(all...)
}

// readStackChartFile reads one required stack chart file.
func readStackChartFile(t *testing.T, parts ...string) string {
	t.Helper()
	body, err := os.ReadFile(stackChartFile(t, parts...))
	if err != nil {
		t.Fatalf("read stack chart file %v: %v", parts, err)
	}
	return string(body)
}

// helmBinary locates helm. The rendered-check contract is fail-closed for
// infrastructure runs: if KODAMA_REQUIRE_INFRA=1 is set, a missing helm is a
// test failure rather than a skip.
func helmBinary(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("helm"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", "helm")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	if os.Getenv("KODAMA_REQUIRE_INFRA") == "1" {
		t.Fatalf("helm is required to render the kodama-stack chart but was not found on PATH or in ~/.local/bin (KODAMA_REQUIRE_INFRA=1)")
	}
	t.Skip("helm not installed; skipping rendered-chart checks (set KODAMA_REQUIRE_INFRA=1 to fail instead)")
	return ""
}

// copyChartTree copies deploy/charts into a test-owned directory.
func copyChartTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		t.Fatalf("copy chart tree: %v", err)
	}
}

// stageStackChart returns helm and a temporary copy of the stack chart with
// its file dependency built.
func stageStackChart(t *testing.T) (string, string) {
	t.Helper()
	helm := helmBinary(t)
	repositoryCharts := filepath.Join(repoRoot(t), "deploy", "charts")
	destination := filepath.Join(t.TempDir(), "charts")
	copyChartTree(t, repositoryCharts, destination)
	stack := filepath.Join(destination, "kodama-stack")

	command := exec.Command(helm, "dependency", "build", "--skip-refresh", stack)
	command.Dir = stack
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helm dependency build: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(repositoryCharts, "kodama-stack", "charts")); err == nil {
		t.Fatalf("helm dependency build wrote into the repository working tree")
	}
	return helm, stack
}

// anonymousGuardMessage is the phrase the chart's fail-closed auth guard emits
// when a render would expose the broker without client authentication.
const anonymousGuardMessage = "anonymous NATS access is forbidden"

// renderStack renders the chart with optional `--set key=value` overrides.
func renderStack(t *testing.T, helm, chart string, sets ...string) (string, error) {
	t.Helper()
	args := []string{"template", "kodama-stack", chart, "--namespace", "kodama-system"}
	args = append(args, productionOIDCArgs("kodama.")...)
	for _, set := range sets {
		args = append(args, "--set", set)
	}
	command := exec.Command(helm, args...)
	command.Dir = chart
	output, err := command.CombinedOutput()
	return string(output), err
}

// natsConfFromRender extracts the nats.conf block from the rendered
// nats-config ConfigMap.
func natsConfFromRender(t *testing.T, rendered string) string {
	t.Helper()
	for document := range strings.SplitSeq(rendered, "\n---") {
		if !strings.Contains(document, "kind: ConfigMap") || !strings.Contains(document, "name: nats-config") {
			continue
		}
		var block []string
		inBlock := false
		for line := range strings.SplitSeq(document, "\n") {
			if !inBlock {
				if strings.HasSuffix(strings.TrimSpace(line), "nats.conf: |") {
					inBlock = true
				}
				continue
			}
			if strings.TrimSpace(line) == "" {
				block = append(block, "")
				continue
			}
			if !strings.HasPrefix(line, "    ") {
				break
			}
			block = append(block, strings.TrimPrefix(line, "    "))
		}
		if len(block) > 0 {
			return strings.Join(block, "\n")
		}
	}
	t.Fatalf("rendered output has no nats-config ConfigMap with a nats.conf block:\n%s", rendered)
	return ""
}

// tlsBlockFromConf returns the `tls { ... }` body of nats.conf, working both
// for the chart template (indented inside the ConfigMap literal) and for the
// rendered nats.conf (unindented).
func tlsBlockFromConf(conf string) string {
	lines := strings.Split(conf, "\n")
	start := -1
	for index, line := range lines {
		if strings.HasSuffix(strings.TrimSpace(line), "tls {") {
			start = index
			break
		}
	}
	if start < 0 {
		return ""
	}
	for index := start; index < len(lines); index++ {
		if index > start && strings.TrimSpace(lines[index]) == "}" {
			return strings.Join(lines[start:index+1], "\n")
		}
	}
	return ""
}

// statefulSetFromRender returns the rendered StatefulSet document.
func statefulSetFromRender(rendered string) string {
	for document := range strings.SplitSeq(rendered, "\n---") {
		if strings.Contains(document, "kind: StatefulSet") {
			return document
		}
	}
	return ""
}

// assertRenderedMTLS proves the rendered broker verifies client certificates
// against the mounted CA, has no anonymous escape hatch, and mounts the client
// CA read-only from kodama-broker-tls.
func assertRenderedMTLS(t *testing.T, label, rendered string) {
	t.Helper()
	conf := natsConfFromRender(t, rendered)
	tls := tlsBlockFromConf(conf)
	if tls == "" {
		t.Fatalf("%s: rendered nats.conf has no tls {} block:\n%s", label, conf)
	}
	if !regexp.MustCompile(`(?m)^\s*verify:\s*true\s*$`).MatchString(tls) {
		t.Errorf("%s: rendered tls block does not verify client certificates:\n%s", label, tls)
	}
	if !strings.Contains(tls, "ca_file:") {
		t.Errorf("%s: rendered tls block has no ca_file:\n%s", label, tls)
	}
	for _, forbidden := range []string{"no_auth_user", "anonymous", "verify: false"} {
		if strings.Contains(conf, forbidden) {
			t.Errorf("%s: rendered nats.conf contains %q", label, forbidden)
		}
	}
	if strings.Contains(conf, "{{") {
		t.Errorf("%s: rendered nats.conf still contains template directives:\n%s", label, conf)
	}

	statefulSet := statefulSetFromRender(rendered)
	if statefulSet == "" {
		t.Fatalf("%s: rendered output has no StatefulSet", label)
	}
	mount := regexp.MustCompile(`(?ms)name:\s*client-ca\s*\r?\n\s+mountPath:\s*/etc/nats/client-ca\s*\r?\n\s+readOnly:\s*true`)
	if !mount.MatchString(statefulSet) {
		t.Errorf("%s: rendered StatefulSet does not mount the client CA read-only", label)
	}
	volume := regexp.MustCompile(`(?ms)name:\s*client-ca\s*\r?\n\s+secret:\s*\r?\n\s+secretName:\s*kodama-broker-tls\s*$`)
	if !volume.MatchString(statefulSet) {
		t.Errorf("%s: rendered StatefulSet does not source the client CA from kodama-broker-tls", label)
	}
}

// TestStackChartRenderedDefaultVerifiesClients pins the shipped default.
func TestStackChartRenderedDefaultVerifiesClients(t *testing.T) {
	helm, chart := stageStackChart(t)
	rendered, err := renderStack(t, helm, chart)
	if err != nil {
		t.Fatalf("helm template default values failed: %v\n%s", err, rendered)
	}
	assertRenderedMTLS(t, "default", rendered)
}

// TestStackChartRenderedCredentialsModeVerifiesClients pins the credentials
// mode: authorization comes from a Secret and client verification stays on.
func TestStackChartRenderedCredentialsModeVerifiesClients(t *testing.T) {
	helm, chart := stageStackChart(t)
	rendered, err := renderStack(t, helm, chart,
		"nats.auth.mode=credentials",
		"nats.auth.credentials.existingSecret=kodama-broker-credentials")
	if err != nil {
		t.Fatalf("helm template credentials mode failed: %v\n%s", err, rendered)
	}
	assertRenderedMTLS(t, "credentials", rendered)
	conf := natsConfFromRender(t, rendered)
	if !strings.Contains(conf, "authorization {") {
		t.Errorf("credentials mode did not render an authorization block:\n%s", conf)
	}
	for _, want := range []string{"user: $NATS_AUTH_USER", "password: $NATS_AUTH_PASSWORD"} {
		if !strings.Contains(conf, want) {
			t.Errorf("credentials mode did not render %q", want)
		}
	}
	statefulSet := statefulSetFromRender(rendered)
	if count := strings.Count(statefulSet, "name: kodama-broker-credentials"); count < 2 {
		t.Errorf("credentials mode sources %d values from kodama-broker-credentials, want 2", count)
	}
}

// TestStackChartRenderedCalloutAccountMatchesControlPlane pins the account
// wiring in the real template: the rendered broker auth_callout account and
// the control plane's KODAMA_NATS_CALLOUT_ACCOUNT are the same value. A broker
// bound to one account and a callout signing grants for another would refuse
// every admitted client.
func TestStackChartRenderedCalloutAccountMatchesControlPlane(t *testing.T) {
	helm, chart := stageStackChart(t)
	rendered, err := renderStack(t, helm, chart)
	if err != nil {
		t.Fatalf("helm template default values failed: %v\n%s", err, rendered)
	}
	conf := natsConfFromRender(t, rendered)
	brokerAccount := regexp.MustCompile(`(?m)^[ \t]*account:[ \t]*"?([^"\s]+)"?[ \t]*$`).FindStringSubmatch(conf)
	if brokerAccount == nil || brokerAccount[1] == "" {
		t.Fatalf("rendered nats.conf has no auth_callout account:\n%s", conf)
	}
	var controlPlaneAccount string
	for document := range strings.SplitSeq(rendered, "\n---") {
		if !strings.Contains(document, "kind: Deployment") || !strings.Contains(document, "KODAMA_NATS_CALLOUT_ACCOUNT") {
			continue
		}
		match := regexp.MustCompile(`(?ms)name:[ \t]*KODAMA_NATS_CALLOUT_ACCOUNT[ \t]*\r?\n[ \t]*value:[ \t]*"?([^"\s]+)"?`).FindStringSubmatch(document)
		if match != nil {
			controlPlaneAccount = match[1]
		}
	}
	if controlPlaneAccount == "" {
		t.Fatalf("rendered output has no KODAMA_NATS_CALLOUT_ACCOUNT value:\n%s", rendered)
	}
	if controlPlaneAccount != brokerAccount[1] {
		t.Fatalf("KODAMA_NATS_CALLOUT_ACCOUNT = %q; broker auth_callout account = %q; want one value", controlPlaneAccount, brokerAccount[1])
	}

	// An override of only one side must fail the render, never produce a
	// broker bound to one account and a callout signing grants for another.
	diverged, err := renderStack(t, helm, chart, "nats.auth.callout.account=OTHER")
	if err == nil {
		t.Fatalf("render accepted a callout account that differs from the control plane:\n%s", diverged)
	}
	if !strings.Contains(diverged, "must equal") {
		t.Fatalf("render rejected the divergent callout account for a reason other than the account guard: %v\n%s", err, diverged)
	}
}

// TestStackChartRenderedCannotDisableClientVerification smuggles every
// plausible opt-out flag into the render. Each render must either fail closed
// or keep `verify: true` and the client CA. A flag that disables the guard
// must not let anonymous mode through.
func TestStackChartRenderedCannotDisableClientVerification(t *testing.T) {
	helm, chart := stageStackChart(t)

	smuggled := []string{
		"nats.legacyOpen=true",
		"nats.tls.verifyClient=false",
		"nats.verify=false",
		"nats.auth.mode=anonymous",
		"nats.auth.mtls.existingSecret=",
	}
	for _, flag := range smuggled {
		name := strings.NewReplacer(".", "_", "=", "_", "-", "_").Replace(flag)
		t.Run(name, func(t *testing.T) {
			rendered, err := renderStack(t, helm, chart, flag)
			if err != nil {
				// A failed render is only acceptable when the fail-closed
				// auth guard rejected it; a render that dies for any other
				// reason would hide a broken chart behind "fail-closed".
				if !strings.Contains(rendered, anonymousGuardMessage) {
					t.Errorf("%s: render failed for a reason other than the NATS auth guard: %v\n%s", flag, err, rendered)
				}
				return
			}
			assertRenderedMTLS(t, flag, rendered)
		})
	}

	// A guard skipped by one of these flags would let the forbidden anonymous
	// mode render; that is never acceptable.
	for _, flag := range []string{
		"nats.legacyOpen=true",
		"nats.tls.verifyClient=false",
		"nats.verify=false",
	} {
		name := strings.NewReplacer(".", "_", "=", "_", "-", "_").Replace(flag)
		t.Run(name+"_anonymous", func(t *testing.T) {
			rendered, err := renderStack(t, helm, chart, flag, "nats.auth.mode=anonymous")
			if err == nil {
				t.Fatalf("render accepted nats.auth.mode=anonymous together with %s:\n%s", flag, rendered)
			}
			// The render must fail because the auth guard rejected anonymous
			// mode, not because of an unrelated template error.
			if !strings.Contains(rendered, anonymousGuardMessage) {
				t.Errorf("render rejected %s for a reason other than anonymous access: %v\n%s", flag, err, rendered)
			}
		})
	}
}

// TestStackChartRejectsUnknownAuthModes pins the fail-closed auth guard
// against every plausible spelling of "no authentication". An allowed-list
// regression that admits any of these must fail the render with the guard
// message, never render a broker without client authentication.
func TestStackChartRejectsUnknownAuthModes(t *testing.T) {
	helm, chart := stageStackChart(t)
	for _, mode := range []string{"none", "open", "off", "", "Anonymous"} {
		name := mode
		if name == "" {
			name = "empty"
		}
		name = strings.NewReplacer(".", "_", "=", "_", "-", "_").Replace(name)
		t.Run(name, func(t *testing.T) {
			rendered, err := renderStack(t, helm, chart, "nats.auth.mode="+mode)
			if err == nil {
				t.Fatalf("render accepted nats.auth.mode=%q:\n%s", mode, rendered)
			}
			if !strings.Contains(rendered, anonymousGuardMessage) {
				t.Errorf("render rejected nats.auth.mode=%q for a reason other than the NATS auth guard: %v\n%s", mode, err, rendered)
			}
		})
	}
}

// TestStackChartStructureFailsClosed pins the structural properties the
// rendered checks rely on, so a regression names the cause. The tls block in
// nats-configmap.yaml must be unconditional, the fail-closed guard include
// must be the first render directive after the nats.enabled gate in both
// workloads, and the only conditionals in nats-configmap.yaml are the enabled
// gate and the credentials and callout branches.
func TestStackChartStructureFailsClosed(t *testing.T) {
	config := readStackChartFile(t, "templates", "nats-configmap.yaml")
	statefulSet := readStackChartFile(t, "templates", "nats-statefulset.yaml")

	for _, workload := range []struct {
		name string
		body string
	}{
		{name: "nats-configmap.yaml", body: config},
		{name: "nats-statefulset.yaml", body: statefulSet},
	} {
		assertGuardIncludeFirst(t, workload.name, workload.body)
	}

	tls := tlsBlockFromConf(config)
	if tls == "" {
		t.Fatalf("nats-configmap.yaml has no tls {} block")
	}
	if strings.Contains(tls, "{{") {
		t.Errorf("nats-configmap.yaml tls {} block contains a template conditional; client verification must be unconditional:\n%s", tls)
	}
	if !regexp.MustCompile(`(?m)^\s*verify:\s*true\s*$`).MatchString(tls) {
		t.Errorf("nats-configmap.yaml tls {} block does not contain an unconditional verify: true")
	}

	conditional := regexp.MustCompile(`\{\{-?\s*(if|with|range|else)\b([^}]*)\}\}`)
	allowed := 0
	for _, match := range conditional.FindAllStringSubmatch(config, -1) {
		expression := strings.TrimSpace(match[2])
		switch {
		case strings.Contains(expression, ".Values.nats.enabled"):
			allowed++
		case strings.Contains(expression, `eq $auth.mode "credentials"`):
			allowed++
		case strings.Contains(expression, `eq $auth.mode "callout"`):
			allowed++
		default:
			t.Errorf("nats-configmap.yaml has an unexpected template conditional %q; only the enabled gate and the credentials and callout branches are allowed", strings.TrimSpace(match[0]))
		}
	}
	if allowed != 3 {
		t.Errorf("nats-configmap.yaml has %d allowed conditionals, want 3 (enabled gate, credentials branch and callout branch)", allowed)
	}
}

// assertGuardIncludeFirst requires the fail-closed guard include to be the
// first render directive after `{{- if .Values.nats.enabled }}` and to appear
// exactly once. Wrapping it in another conditional (a flag that skips the
// guard) or moving code above it fails.
func assertGuardIncludeFirst(t *testing.T, name, body string) {
	t.Helper()
	const include = `include "kodama-stack.natsAuthGuard"`
	if count := strings.Count(body, include); count != 1 {
		t.Errorf("%s includes the fail-closed NATS auth guard %d times, want exactly 1", name, count)
	}
	lines := strings.Split(body, "\n")
	gate := -1
	for index, line := range lines {
		if strings.Contains(line, "if .Values.nats.enabled") {
			gate = index
			break
		}
	}
	if gate < 0 {
		t.Errorf("%s has no `if .Values.nats.enabled` gate", name)
		return
	}
	for index := gate + 1; index < len(lines); index++ {
		trimmed := strings.TrimSpace(lines[index])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		directive := regexp.MustCompile(`^\{\{-?\s*include\s+"kodama-stack\.natsAuthGuard"`)
		if !directive.MatchString(trimmed) || strings.Count(trimmed, "{{") != 1 {
			t.Errorf("%s: first render directive after the enabled gate is %q, want the unconditional fail-closed guard include; a conditional here can skip the guard", name, trimmed)
		}
		return
	}
	t.Errorf("%s has no render directive after the enabled gate", name)
}

func TestStackChartNATSPodHardeningAndSingleReplica(t *testing.T) {
	helm, chart := stageStackChart(t)
	rendered, err := renderStack(t, helm, chart)
	if err != nil {
		t.Fatalf("helm template default values failed: %v\n%s", err, rendered)
	}
	statefulSet := renderedObjectNamed(t, rendered, "StatefulSet", "nats")
	spec := objectAtKey(t, statefulSet, "spec")
	if replicas, ok := probeInt(spec["replicas"]); !ok || replicas != 1 {
		t.Errorf("rendered nats StatefulSet replicas = %v, want 1", spec["replicas"])
	}
	podSpec := objectAtKey(t, objectAtKey(t, spec, "template"), "spec")
	if automount, ok := podSpec["automountServiceAccountToken"].(bool); !ok || automount {
		t.Errorf("rendered nats StatefulSet automountServiceAccountToken = %v, want false", podSpec["automountServiceAccountToken"])
	}
	podSecurity := objectAtKey(t, podSpec, "securityContext")
	if got := stringAtKey(t, objectAtKey(t, podSecurity, "seccompProfile"), "type"); got != "RuntimeDefault" {
		t.Errorf("rendered nats StatefulSet pod seccompProfile.type = %q, want RuntimeDefault", got)
	}

	for _, badReplicas := range []string{"0", "2", "3"} {
		t.Run("replicas_"+badReplicas, func(t *testing.T) {
			out, err := renderStack(t, helm, chart, "nats.replicaCount="+badReplicas)
			if err == nil {
				t.Fatalf("render accepted nats.replicaCount=%s:\n%s", badReplicas, out)
			}

			outSkip, errSkip := renderChartArgs(t, helm, "kodama-stack", chart, stackProductionArgs(
				"--skip-schema-validation",
				"--set", "nats.replicaCount="+badReplicas,
			)...)
			if errSkip == nil {
				t.Fatalf("render --skip-schema-validation accepted nats.replicaCount=%s:\n%s", badReplicas, outSkip)
			}
			if !strings.Contains(outSkip, "nats.replicaCount must be 1") {
				t.Errorf("template guard failed for an unexpected reason: %v\n%s", errSkip, outSkip)
			}
		})
	}
}
