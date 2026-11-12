//go:build component

// Rendered-chart checks for the optional per-tenant `git serve` StatefulSet and
// the optional services Deployment.
//
// Both workloads are OFF by default. When a flag is off the chart must render
// exactly the base object set; when it is on, the tests decode the real helm
// output and assert the workload shape, hardening, secret references, probes,
// storage placement and NetworkPolicy peers. The values schema must refuse a
// partial or malformed block before any object renders.
//
// helm runs locally against a test-owned copy of the chart; nothing contacts a
// cluster or the network.
package deploy

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// workloadValues is one decoded YAML values document passed to helm.
type workloadValues map[string]any

// renderWorkloadValues writes one values document and renders the kodama chart
// with the production OIDC inputs.
func renderWorkloadValues(t *testing.T, helm, chart string, values workloadValues) (string, error) {
	t.Helper()
	return renderWorkloadValuesWithArgs(t, helm, chart, values)
}

// renderWorkloadValuesWithArgs adds extra helm arguments after the values file.
func renderWorkloadValuesWithArgs(t *testing.T, helm, chart string, values workloadValues, extra ...string) (string, error) {
	t.Helper()
	body, err := yaml.Marshal(values)
	if err != nil {
		t.Fatalf("encode values: %v", err)
	}
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write values: %v", err)
	}
	args := append(kodamaProductionArgs(), "--values", path)
	args = append(args, extra...)
	return renderChartArgs(t, helm, "kodama", chart, args...)
}

// workloadMap normalises a decoded mapping to map[string]any.
func workloadMap(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case workloadValues:
		return map[string]any(typed)
	default:
		panic("workload values member is not an object")
	}
}

// workloadList normalises a decoded list to []any.
func workloadList(value any) []any {
	list, ok := value.([]any)
	if !ok {
		panic("workload values member is not a list")
	}
	return list
}

// cloneWorkloadValues returns a deep-enough copy for one test case.
func cloneWorkloadValues(source workloadValues) workloadValues {
	body, err := yaml.Marshal(source)
	if err != nil {
		panic(err)
	}
	var cloned workloadValues
	if err := yaml.Unmarshal(body, &cloned); err != nil {
		panic(err)
	}
	return cloned
}

// baseWorkloadValues is the smallest complete enabled configuration: two
// tenants with one Git service each and three service adapters.
func baseWorkloadValues() workloadValues {
	return workloadValues{
		"tenants": []any{"tenant-a", "tenant-b"},
		"config": map[string]any{
			"installationID": "kodama-stage",
			"publicURL":      "https://kodama.example.test:8443",
			"keyAdapter":     "scw-kms",
			"keyReference":   "scw-kms:fr-par/00000000-0000-0000-0000-000000000000",
			"mcpPath":        "/mcp",
		},
		"gitService": map[string]any{
			"enabled": true,
			"tenants": []any{
				map[string]any{
					"tenant": "tenant-a",
					"repositories": []any{
						map[string]any{"name": "shop/web", "resource": "res-111"},
					},
					"credential": map[string]any{
						"existingSecret": "kodama-git-tenant-a",
						"key":            "token",
					},
					"tls": map[string]any{"existingSecret": "kodama-tls"},
					"storage": map[string]any{
						"storageClass": "sbs-default",
						"size":         "10Gi",
						"accessMode":   "ReadWriteOnce",
					},
					"clients": []any{
						map[string]any{
							"namespace": "kodama-tenant-tenant-a",
							"podLabels": map[string]any{"kodama.dev/tenant": "tenant-a"},
						},
					},
				},
				map[string]any{
					"tenant": "tenant-b",
					"repositories": []any{
						map[string]any{"name": "shop/api", "resource": "res-222"},
					},
					"credential": map[string]any{
						"existingSecret": "kodama-git-tenant-b",
						"key":            "token",
					},
					"tls": map[string]any{"existingSecret": "kodama-tls"},
					"storage": map[string]any{
						"size":       "5Gi",
						"accessMode": "ReadWriteOnce",
					},
					"clients": []any{
						map[string]any{"namespace": "kodama-tenant-tenant-b"},
					},
				},
			},
		},
		"services": map[string]any{
			"enabled": true,
			"entries": []any{
				map[string]any{
					"name":    "responder",
					"kind":    "responder",
					"service": "responder-svc",
					"tenant":  "tenant-a",
					"project": "project-a",
					"credential": map[string]any{
						"existingSecret": "kodama-service-responder",
						"key":            "token",
					},
				},
				map[string]any{
					"name":     "timer",
					"kind":     "timer",
					"service":  "timer-svc",
					"tenant":   "tenant-b",
					"project":  "project-b",
					"schedule": "30s",
					"credential": map[string]any{
						"existingSecret": "kodama-service-timer",
						"key":            "token",
					},
				},
				map[string]any{
					"name":    "email",
					"kind":    "email",
					"service": "email-svc",
					"tenant":  "tenant-a",
					"project": "project-a",
					"credential": map[string]any{
						"existingSecret": "kodama-service-email",
						"key":            "token",
					},
					"mailboxCredential": map[string]any{
						"existingSecret": "kodama-email-mailbox",
						"key":            "password",
					},
				},
			},
		},
	}
}

// optionalStringAtKey returns a string member or the empty string.
func optionalStringAtKey(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

// renderedInventory returns a stable kind/name@namespace list.
func renderedInventory(t *testing.T, rendered string) []string {
	t.Helper()
	var inventory []string
	for _, object := range renderedObjects(t, rendered) {
		metadata := objectAtKey(t, object, "metadata")
		inventory = append(inventory, stringAtKey(t, object, "kind")+"/"+
			optionalStringAtKey(metadata, "name")+"@"+optionalStringAtKey(metadata, "namespace"))
	}
	sort.Strings(inventory)
	return inventory
}

// baseInventory is the exact default object set, with both workloads off.
func baseInventory() []string {
	return []string{
		"ClusterRole/kodama-namespace-reader-kodama-system@",
		"ClusterRoleBinding/kodama-namespace-reader-kodama-system@",
		"Deployment/kodama@kodama-system",
		"LimitRange/kodama-tenant-limits@kodama-tenant-tenant-a",
		"Namespace/kodama-tenant-tenant-a@",
		"NetworkPolicy/kodama-tenant-default-deny@kodama-tenant-tenant-a",
		"PersistentVolumeClaim/kodama-retained-logs@kodama-system",
		"Role/kodama-runtime@kodama-tenant-tenant-a",
		"RoleBinding/kodama-runtime@kodama-tenant-tenant-a",
		"Service/kodama@kodama-system",
		"ServiceAccount/kodama@kodama-system",
	}
}

// TestKodamaChartWorkloadsDefaultRenderUnchanged pins the OFF default: no new
// object may render, and the object set stays exactly the base set.
func TestKodamaChartWorkloadsDefaultRenderUnchanged(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")

	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama: %v\n%s", err, rendered)
	}
	got := renderedInventory(t, rendered)
	want := baseInventory()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("default render object set changed:\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, object := range renderedObjects(t, rendered) {
		metadata := objectAtKey(t, object, "metadata")
		name := stringAtKey(t, metadata, "name")
		if strings.HasPrefix(name, "kodama-git-") || strings.HasPrefix(name, "kodama-service-") {
			t.Errorf("default render contains %s/%s although both workload flags are off", object["kind"], name)
		}
	}

	// Explicit false must stay off as well: `--set ...=false` is the form an
	// operator uses to pin the default in their own values file.
	rendered, err = renderChartArgs(t, helm, "kodama", chart,
		append(kodamaProductionArgs(), "--set", "gitService.enabled=false", "--set", "services.enabled=false")...)
	if err != nil {
		t.Fatalf("helm template with explicit disabled flags: %v\n%s", err, rendered)
	}
	if strings.Join(renderedInventory(t, rendered), "\n") != strings.Join(want, "\n") {
		t.Fatalf("explicitly disabled workloads changed the default object set")
	}
}

// assertWorkloadAPICertificate checks the mounted private CA that verifies the
// API's TLS certificate.
func assertWorkloadAPICertificate(t *testing.T, object map[string]any, containerName string) {
	t.Helper()
	container := workloadContainer(t, object, containerName)
	entry := renderedEnvEntry(t, object, "SSL_CERT_FILE")
	if got := stringAtKey(t, entry, "value"); got != "/etc/kodama/api-ca/ca.crt" {
		t.Errorf("%v container %v SSL_CERT_FILE = %q, want /etc/kodama/api-ca/ca.crt", object["kind"], containerName, got)
	}
	volume := renderedVolume(t, object, "api-ca")
	secret := objectAtKey(t, volume, "secret")
	if got := stringAtKey(t, secret, "secretName"); got != "kodama-tls" {
		t.Errorf("%v api-ca secretName = %q, want kodama-tls", object["kind"], got)
	}
	mount := findVolumeMount(t, container, "api-ca")
	if readOnly, ok := mount["readOnly"].(bool); !ok || !readOnly {
		t.Errorf("%v api-ca mount is not read-only", object["kind"])
	}
	if got := stringAtKey(t, mount, "mountPath"); got != "/etc/kodama/api-ca" {
		t.Errorf("%v api-ca mountPath = %q, want /etc/kodama/api-ca", object["kind"], got)
	}
}

// workloadContainer returns one named container of a workload object.
func workloadContainer(t *testing.T, object map[string]any, name string) map[string]any {
	t.Helper()
	for _, container := range containersOf(t, object) {
		if stringAtKey(t, container, "name") == name {
			return container
		}
	}
	t.Fatalf("%v has no container named %s", object["kind"], name)
	return nil
}

// containerArgs returns the argv of one container.
func containerArgs(t *testing.T, container map[string]any) []string {
	t.Helper()
	values, ok := container["args"].([]any)
	if !ok {
		t.Fatalf("container %v has no args", container["name"])
	}
	args := make([]string, 0, len(values))
	for _, value := range values {
		args = append(args, stringAtKey(t, map[string]any{"v": value}, "v"))
	}
	return args
}

// containsArg reports one exact argv element.
func containsArg(args []string, want string) bool {
	return slices.Contains(args, want)
}

// assertContainerHardened checks the pod/container security contract.
func assertContainerHardened(t *testing.T, object map[string]any, container map[string]any) {
	t.Helper()
	security := objectAtKey(t, container, "securityContext")
	if allowed, ok := security["allowPrivilegeEscalation"].(bool); !ok || allowed {
		t.Errorf("%v container %v allows privilege escalation", object["kind"], container["name"])
	}
	if readOnly, ok := security["readOnlyRootFilesystem"].(bool); !ok || !readOnly {
		t.Errorf("%v container %v has a writable root filesystem", object["kind"], container["name"])
	}
	capabilities := objectAtKey(t, security, "capabilities")
	drops, ok := capabilities["drop"].([]any)
	if !ok || len(drops) != 1 || drops[0] != "ALL" {
		t.Errorf("%v container %v does not drop ALL capabilities: %v", object["kind"], container["name"], capabilities)
	}
}

// assertPodHardened checks non-root pod settings.
func assertPodHardened(t *testing.T, object map[string]any) {
	t.Helper()
	spec := objectAtKey(t, objectAtKey(t, objectAtKey(t, object, "spec"), "template"), "spec")
	security := objectAtKey(t, spec, "securityContext")
	if nonRoot, ok := security["runAsNonRoot"].(bool); !ok || !nonRoot {
		t.Errorf("%v does not run as non-root", object["kind"])
	}
	if uid, ok := probeInt(security["runAsUser"]); !ok || uid == 0 {
		t.Errorf("%v runAsUser = %v, want a non-zero UID", object["kind"], security["runAsUser"])
	}
	seccomp := objectAtKey(t, security, "seccompProfile")
	if got := stringAtKey(t, seccomp, "type"); got != "RuntimeDefault" {
		t.Errorf("%v pod seccompProfile.type = %q, want RuntimeDefault", object["kind"], got)
	}
}

// assertProbesPresent checks that both probes exist.
func assertProbesPresent(t *testing.T, container map[string]any) {
	t.Helper()
	for _, name := range []string{"livenessProbe", "readinessProbe"} {
		if _, ok := container[name].(map[string]any); !ok {
			t.Errorf("container %v has no %s", container["name"], name)
		}
	}
}

// assertTCPProbePair checks that both probes observe a listening socket.
func assertTCPProbePair(t *testing.T, container map[string]any) {
	t.Helper()
	assertProbesPresent(t, container)
	for _, name := range []string{"livenessProbe", "readinessProbe"} {
		probe, ok := container[name].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := probe["tcpSocket"]; !ok {
			t.Errorf("container %v %s is not a tcpSocket probe: %v", container["name"], name, probe)
		}
	}
}

func policyRulePortNumbers(t *testing.T, rule map[string]any) []int {
	t.Helper()
	rawPorts := sliceAtKey(t, rule, "ports")
	ports := make([]int, 0, len(rawPorts))
	for _, raw := range rawPorts {
		entry := raw.(map[string]any)
		port, ok := probeInt(entry["port"])
		if !ok {
			t.Fatalf("NetworkPolicy port entry is not an integer: %v", entry["port"])
		}
		ports = append(ports, port)
	}
	return ports
}

// TestKodamaChartWorkloadAPIEgressUsesTargetPort proves the NetworkPolicy
// matches the control-plane pod port, not the Service port: a NetworkPolicy
// applies after Service DNAT. It also verifies kodama-tenant-default-deny uses
// service.targetPort, drops unserved port 8080, and avoids duplicate ports when
// gitService.port equals service.targetPort.
func TestKodamaChartWorkloadAPIEgressUsesTargetPort(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")

	defaultRendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama default: %v\n%s", err, defaultRendered)
	}
	defaultTenantPolicy := workloadObjectNamed(t, defaultRendered, "NetworkPolicy", "kodama-tenant-default-deny", "kodama-tenant-tenant-a")
	defaultSpec := objectAtKey(t, defaultTenantPolicy, "spec")
	defaultIngressPorts := policyRulePortNumbers(t, sliceAtKey(t, defaultSpec, "ingress")[0].(map[string]any))
	if !slices.Equal(defaultIngressPorts, []int{8443}) {
		t.Errorf("default tenant NetworkPolicy ingress ports = %v, want [8443] (no unserved 8080)", defaultIngressPorts)
	}
	defaultKodamaEgressPorts := policyRulePortNumbers(t, sliceAtKey(t, defaultSpec, "egress")[1].(map[string]any))
	if !slices.Equal(defaultKodamaEgressPorts, []int{8443}) {
		t.Errorf("default tenant NetworkPolicy Kodama egress ports = %v, want [8443] (no unserved 8080)", defaultKodamaEgressPorts)
	}

	rendered, err := renderWorkloadValuesWithArgs(t, helm, chart, baseWorkloadValues(),
		"--set", "service.port=443", "--set", "service.targetPort=9443")
	if err != nil {
		t.Fatalf("helm template kodama with a non-default service port: %v\n%s", err, rendered)
	}

	for _, name := range []string{
		"kodama-git-tenant-a", "kodama-git-tenant-b",
		"kodama-service-responder", "kodama-service-timer", "kodama-service-email",
	} {
		policy := renderedObjectNamed(t, rendered, "NetworkPolicy", name)
		egress := sliceAtKey(t, objectAtKey(t, policy, "spec"), "egress")
		apiRule := egress[len(egress)-1].(map[string]any)
		port := sliceAtKey(t, apiRule, "ports")[0].(map[string]any)
		value, ok := probeInt(port["port"])
		if !ok {
			t.Errorf("%s API egress port is not an integer: %v", name, port["port"])
			continue
		}
		if value != 9443 {
			t.Errorf("%s API egress port = %d, want the control-plane targetPort 9443 (not the Service port 443)", name, value)
		}
	}

	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		tenantPolicy := workloadObjectNamed(t, rendered, "NetworkPolicy", "kodama-tenant-default-deny", "kodama-tenant-"+tenant)
		spec := objectAtKey(t, tenantPolicy, "spec")
		ingressPorts := policyRulePortNumbers(t, sliceAtKey(t, spec, "ingress")[0].(map[string]any))
		if !slices.Equal(ingressPorts, []int{9443}) {
			t.Errorf("tenant %s ingress ports = %v, want [9443] (service.targetPort)", tenant, ingressPorts)
		}
		kodamaEgressPorts := policyRulePortNumbers(t, sliceAtKey(t, spec, "egress")[1].(map[string]any))
		if !slices.Equal(kodamaEgressPorts, []int{9443, 8443}) {
			t.Errorf("tenant %s Kodama egress ports = %v, want [9443 8443] (API targetPort plus distinct gitService.port)", tenant, kodamaEgressPorts)
		}
	}

	samePortRendered, err := renderWorkloadValuesWithArgs(t, helm, chart, baseWorkloadValues(),
		"--set", "service.port=443", "--set", "service.targetPort=9443", "--set", "gitService.port=9443")
	if err != nil {
		t.Fatalf("helm template kodama with matching targetPort and gitService.port: %v\n%s", err, samePortRendered)
	}
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		tenantPolicy := workloadObjectNamed(t, samePortRendered, "NetworkPolicy", "kodama-tenant-default-deny", "kodama-tenant-"+tenant)
		spec := objectAtKey(t, tenantPolicy, "spec")
		kodamaEgressPorts := policyRulePortNumbers(t, sliceAtKey(t, spec, "egress")[1].(map[string]any))
		if !slices.Equal(kodamaEgressPorts, []int{9443}) {
			t.Errorf("tenant %s Kodama egress ports with gitService.port==targetPort = %v, want single [9443] without duplicates", tenant, kodamaEgressPorts)
		}
	}
}

// TestKodamaChartGitPVCSurvivesReleaseRemoval proves the repository/journal
// claim is retained when the Helm release or the tenant entry goes away.
func TestKodamaChartGitPVCSurvivesReleaseRemoval(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	rendered, err := renderWorkloadValues(t, helm, chart, baseWorkloadValues())
	if err != nil {
		t.Fatalf("helm template kodama with workloads: %v\n%s", err, rendered)
	}
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		pvc := workloadObjectNamed(t, rendered, "PersistentVolumeClaim", "kodama-git-"+tenant+"-data", "kodama-system")
		metadata := objectAtKey(t, pvc, "metadata")
		annotations, ok := metadata["annotations"].(map[string]any)
		if !ok {
			t.Fatalf("git PVC %s has no annotations", tenant)
		}
		if got := stringAtKey(t, annotations, "helm.sh/resource-policy"); got != "keep" {
			t.Errorf("git PVC %s annotation helm.sh/resource-policy = %q, want keep", tenant, got)
		}
	}
}

// TestKodamaChartGitServiceWorkloads renders the enabled Git StatefulSets.
func TestKodamaChartGitServiceWorkloads(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	rendered, err := renderWorkloadValues(t, helm, chart, baseWorkloadValues())
	if err != nil {
		t.Fatalf("helm template kodama with workloads: %v\n%s", err, rendered)
	}

	// One StatefulSet, PVC, Service and NetworkPolicy per configured Git tenant;
	// the TLS proxy ConfigMap is shared by every entry in the release namespace.
	if got := renderedKindCount(rendered, "StatefulSet"); got != 2 {
		t.Fatalf("rendered %d StatefulSets, want one per Git tenant (2)", got)
	}
	if got := len(rbacObjectsOfKind(t, rendered, "NetworkPolicy")); got != 7 {
		t.Fatalf("rendered %d NetworkPolicies, want two base tenant policies, one per Git tenant and one per service (7)", got)
	}

	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		statefulSet := renderedObjectNamed(t, rendered, "StatefulSet", "kodama-git-"+tenant)
		metadata := objectAtKey(t, statefulSet, "metadata")
		if got := stringAtKey(t, metadata, "namespace"); got != "kodama-system" {
			t.Errorf("git StatefulSet namespace = %q, want the release namespace", got)
		}
		assertPodHardened(t, statefulSet)

		git := workloadContainer(t, statefulSet, "git")
		tlsProxy := workloadContainer(t, statefulSet, "tls")
		assertContainerHardened(t, statefulSet, git)
		assertContainerHardened(t, statefulSet, tlsProxy)
		assertWorkloadAPICertificate(t, statefulSet, "git")
		assertTCPProbePair(t, git)
		assertTCPProbePair(t, tlsProxy)

		args := containerArgs(t, git)
		for _, want := range []string{
			"git",
			"serve",
			"--tenant=" + tenant,
			"--listen=0.0.0.0:8081",
			"--storage-root=/var/lib/kodama/git/repos",
			"--journal-dir=/var/lib/kodama/git/journal",
			"--credential-file=/etc/kodama/git/credential",
		} {
			if !containsArg(args, want) {
				t.Errorf("git StatefulSet %s args %v do not contain %q", tenant, args, want)
			}
		}
		if !containsArg(args, "--base-url=https://kodama.example.test:8443") {
			t.Errorf("git StatefulSet %s does not use config.publicURL as --base-url: %v", tenant, args)
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--resource=") {
				continue
			}
			if strings.Contains(arg, "/repos/") && strings.Contains(arg, "journal") {
				t.Errorf("journal directory is inside a served repository path: %q", arg)
			}
		}

		// Every repository mapping is passed as one --resource argument.
		wantResource := "--resource=shop/web=res-111"
		if tenant == "tenant-b" {
			wantResource = "--resource=shop/api=res-222"
		}
		if !containsArg(args, wantResource) {
			t.Errorf("git StatefulSet %s does not pass %q: %v", tenant, wantResource, args)
		}

		// The service credential and TLS material come only from Secrets.
		for name, wantSecret := range map[string]string{
			"credential": "kodama-git-" + tenant,
			"tls":        "kodama-tls",
		} {
			volume := renderedVolume(t, statefulSet, name)
			secret := objectAtKey(t, volume, "secret")
			if got := stringAtKey(t, secret, "secretName"); got != wantSecret {
				t.Errorf("git StatefulSet %s volume %s secretName = %q, want %q", tenant, name, got, wantSecret)
			}
		}
		credentialMount := findVolumeMount(t, git, "credential")
		if readOnly, ok := credentialMount["readOnly"].(bool); !ok || !readOnly {
			t.Errorf("git service credential mount is not read-only")
		}
		if got := stringAtKey(t, credentialMount, "mountPath"); got != "/etc/kodama/git" {
			t.Errorf("git credential mountPath = %q, want /etc/kodama/git", got)
		}
		tlsMount := findVolumeMount(t, tlsProxy, "tls")
		if readOnly, ok := tlsMount["readOnly"].(bool); !ok || !readOnly {
			t.Errorf("Git TLS mount is not read-only")
		}

		// Persistent repositories lie on the tenant's PVC; journal and repos
		// share it but remain siblings.
		dataMount := findVolumeMount(t, git, "data")
		if got := stringAtKey(t, dataMount, "mountPath"); got != "/var/lib/kodama/git" {
			t.Errorf("git data mountPath = %q, want /var/lib/kodama/git", got)
		}
		pvc := workloadObjectNamed(t, rendered, "PersistentVolumeClaim", "kodama-git-"+tenant+"-data", "kodama-system")
		spec := objectAtKey(t, pvc, "spec")
		accessModes := sliceAtKey(t, spec, "accessModes")
		if len(accessModes) != 1 || accessModes[0] != "ReadWriteOnce" {
			t.Errorf("git PVC accessModes = %v, want [ReadWriteOnce]", accessModes)
		}
		requests := objectAtKey(t, objectAtKey(t, spec, "resources"), "requests")
		wantSize := "10Gi"
		if tenant == "tenant-b" {
			wantSize = "5Gi"
		}
		if got := stringAtKey(t, requests, "storage"); got != wantSize {
			t.Errorf("git PVC size = %q, want %q", got, wantSize)
		}

		service := renderedObjectNamed(t, rendered, "Service", "kodama-git-"+tenant)
		serviceSpec := objectAtKey(t, service, "spec")
		if got := stringAtKey(t, serviceSpec, "type"); got != "ClusterIP" {
			t.Errorf("git Service type = %q, want ClusterIP", got)
		}
		ports := sliceAtKey(t, serviceSpec, "ports")
		if len(ports) != 1 {
			t.Fatalf("git Service has %d ports, want one TLS port", len(ports))
		}
		port := ports[0].(map[string]any)
		if got := stringAtKey(t, port, "name"); got != "https" {
			t.Errorf("git Service port name = %q, want https", got)
		}
		if value, ok := probeInt(port["port"]); !ok || value != 8443 {
			t.Errorf("git Service port = %v, want 8443", port["port"])
		}
	}

	assertGitServiceNetworkPolicy(t, rendered, "tenant-a", "kodama-system", []string{"kodama-tenant-tenant-a"})
	assertGitServiceNetworkPolicy(t, rendered, "tenant-b", "kodama-system", []string{"kodama-tenant-tenant-b"})

	// No rendered value may be a credential literal.
	for _, sentinel := range []string{"kodama-git-tenant-a", "kodama-git-tenant-b"} {
		// The names are refs, not values: they may appear only next to a
		// secretName/name key, never as a token value.
		if strings.Contains(rendered, sentinel+": token") {
			t.Errorf("rendered output carries a literal credential for %s", sentinel)
		}
	}
}

// TestKodamaChartGitServicePortIsReserved refuses the plaintext smart-HTTP
// backend port as the externally served TLS port. Both containers share the
// Pod network namespace, so a public port of 8081 makes the git process and
// Caddy claim the same socket: the sidecar crash-loops and the Pod never turns
// Ready.
func TestKodamaChartGitServicePortIsReserved(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	values := cloneWorkloadValues(baseWorkloadValues())
	workloadMap(values["gitService"])["port"] = 8081

	rendered, err := renderWorkloadValues(t, helm, chart, values)
	if err == nil {
		t.Fatalf("helm accepted gitService.port=8081, which collides with the plaintext git listener:\n%s", rendered)
	}
	if !strings.Contains(rendered, "gitService/port") {
		t.Errorf("values-schema rejection does not name gitService.port:\n%s", rendered)
	}

	// A values path that skips schema validation must hit the template guard,
	// whose message names the setting, the reserved port and its listener.
	rendered, err = renderWorkloadValuesWithArgs(t, helm, chart, values, "--skip-schema-validation")
	if err == nil {
		t.Fatalf("helm --skip-schema-validation accepted gitService.port=8081:\n%s", rendered)
	}
	for _, want := range []string{"gitService.port", "8081"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("template rejection does not name %q:\n%s", want, rendered)
		}
	}
}

// TestKodamaChartGitServicePortsNeverCollide renders a spread of valid ports
// and proves no two containers of one Git Pod declare the same containerPort:
// the TLS sidecar serves the configured port and the git backend keeps its own
// private port.
func TestKodamaChartGitServicePortsNeverCollide(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	for _, port := range []int{1024, 8080, 8443, 9443, 65535} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			values := cloneWorkloadValues(baseWorkloadValues())
			workloadMap(values["gitService"])["port"] = port
			rendered, err := renderWorkloadValues(t, helm, chart, values)
			if err != nil {
				t.Fatalf("helm template with gitService.port=%d: %v\n%s", port, err, rendered)
			}
			for _, tenant := range []string{"tenant-a", "tenant-b"} {
				statefulSet := renderedObjectNamed(t, rendered, "StatefulSet", "kodama-git-"+tenant)
				owners := map[int]string{}
				for _, container := range containersOf(t, statefulSet) {
					declared, _ := container["ports"].([]any)
					for _, entry := range declared {
						entry, ok := entry.(map[string]any)
						if !ok {
							continue
						}
						number, ok := probeInt(entry["containerPort"])
						if !ok {
							t.Errorf("git Pod %s container %v declares a non-integer containerPort: %v", tenant, container["name"], entry["containerPort"])
							continue
						}
						owner := stringAtKey(t, container, "name")
						if previous, taken := owners[number]; taken {
							t.Errorf("git Pod %s declares containerPort %d in both %s and %s", tenant, number, previous, owner)
							continue
						}
						owners[number] = owner
					}
				}
				if owner, ok := owners[port]; !ok {
					t.Errorf("git Pod %s declares no containerPort %d", tenant, port)
				} else if owner != "tls" {
					t.Errorf("git Pod %s containerPort %d is declared by %q, want tls", tenant, port, owner)
				}
				if owner := owners[8081]; owner != "git" {
					t.Errorf("git Pod %s containerPort 8081 is declared by %q, want git", tenant, owner)
				}
			}
			service := renderedObjectNamed(t, rendered, "Service", "kodama-git-tenant-a")
			servicePort := sliceAtKey(t, objectAtKey(t, service, "spec"), "ports")[0].(map[string]any)
			if value, ok := probeInt(servicePort["port"]); !ok || value != port {
				t.Errorf("git Service port = %v, want %d", servicePort["port"], port)
			}
		})
	}
}

// workloadObjectNamed returns one object by kind, name and namespace.
func workloadObjectNamed(t *testing.T, rendered, kind, name, namespace string) map[string]any {
	t.Helper()
	for _, object := range renderedObjects(t, rendered) {
		if stringAtKey(t, object, "kind") != kind {
			continue
		}
		metadata := objectAtKey(t, object, "metadata")
		if optionalStringAtKey(metadata, "name") == name && optionalStringAtKey(metadata, "namespace") == namespace {
			return object
		}
	}
	t.Fatalf("rendered output has no %s named %s in namespace %s", kind, name, namespace)
	return nil
}

// findVolumeMount returns one named volumeMount of a container.
func findVolumeMount(t *testing.T, container map[string]any, name string) map[string]any {
	t.Helper()
	mounts, ok := container["volumeMounts"].([]any)
	if !ok {
		t.Fatalf("container %v has no volumeMounts", container["name"])
	}
	for _, entry := range mounts {
		mount, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if stringAtKey(t, mount, "name") == name {
			return mount
		}
	}
	t.Fatalf("container %v has no volume mount %s", container["name"], name)
	return nil
}

// assertGitServiceNetworkPolicy checks one Git NetworkPolicy: ingress only
// from the declared client namespaces on the TLS port, egress only DNS and the
// Kodama API.
func assertGitServiceNetworkPolicy(t *testing.T, rendered, tenant, namespace string, clients []string) {
	t.Helper()
	policy := renderedObjectNamed(t, rendered, "NetworkPolicy", "kodama-git-"+tenant)
	metadata := objectAtKey(t, policy, "metadata")
	if got := stringAtKey(t, metadata, "namespace"); got != namespace {
		t.Errorf("git NetworkPolicy namespace = %q, want %q", got, namespace)
	}
	spec := objectAtKey(t, policy, "spec")
	policyTypes := rbacStringList(t, spec, "policyTypes")
	sort.Strings(policyTypes)
	if strings.Join(policyTypes, ",") != "Egress,Ingress" {
		t.Errorf("git NetworkPolicy policyTypes = %v, want Ingress and Egress", policyTypes)
	}
	podSelector := objectAtKey(t, spec, "podSelector")
	matchLabels := objectAtKey(t, podSelector, "matchLabels")
	if got := stringAtKey(t, matchLabels, "app.kubernetes.io/component"); got != "git-service" {
		t.Errorf("git NetworkPolicy selects component %q, want git-service", got)
	}

	ingress, ok := spec["ingress"].([]any)
	if !ok || len(ingress) != len(clients) {
		t.Fatalf("git NetworkPolicy has %d ingress rules, want one per client source (%d)", len(ingress), len(clients))
	}
	seen := map[string]bool{}
	for _, entry := range ingress {
		rule := entry.(map[string]any)
		from := rule["from"].([]any)
		if len(from) != 1 {
			t.Fatalf("git NetworkPolicy ingress has %d peers, want one declared source", len(from))
		}
		peer := from[0].(map[string]any)
		selector := objectAtKey(t, peer, "namespaceSelector")
		labels := objectAtKey(t, selector, "matchLabels")
		seen[stringAtKey(t, labels, "kubernetes.io/metadata.name")] = true
		ports := rule["ports"].([]any)
		if len(ports) != 1 {
			t.Fatalf("git NetworkPolicy ingress rule has %d ports, want only the TLS port", len(ports))
		}
		port := ports[0].(map[string]any)
		if value, ok := probeInt(port["port"]); !ok || value != 8443 {
			t.Errorf("git NetworkPolicy ingress port = %v, want 8443", port["port"])
		}
	}
	for _, client := range clients {
		if !seen[client] {
			t.Errorf("git NetworkPolicy does not allow the declared client namespace %q", client)
		}
	}

	egress, ok := spec["egress"].([]any)
	if !ok || len(egress) != 2 {
		t.Fatalf("git NetworkPolicy has %d egress rules, want exactly DNS and the Kodama API", len(egress))
	}
	assertDNSEgressRule(t, egress[0].(map[string]any))
	apiPeer := egress[1].(map[string]any)
	to := apiPeer["to"].([]any)
	if len(to) != 1 {
		t.Fatalf("git NetworkPolicy API egress has %d peers, want one", len(to))
	}
	apiSelector := objectAtKey(t, to[0].(map[string]any), "namespaceSelector")
	apiLabels := objectAtKey(t, apiSelector, "matchLabels")
	if got := stringAtKey(t, apiLabels, "kubernetes.io/metadata.name"); got != "kodama-system" {
		t.Errorf("git NetworkPolicy API egress namespace = %q, want kodama-system", got)
	}
	apiPorts := apiPeer["ports"].([]any)
	if len(apiPorts) != 1 {
		t.Fatalf("git NetworkPolicy API egress has %d ports, want one", len(apiPorts))
	}
	if value, ok := probeInt(apiPorts[0].(map[string]any)["port"]); !ok || value != 8443 {
		t.Errorf("git NetworkPolicy API egress port = %v, want 8443", apiPorts[0])
	}
}

// assertDNSEgressRule checks the exact cluster-DNS rule.
func assertDNSEgressRule(t *testing.T, rule map[string]any) {
	t.Helper()
	to, ok := rule["to"].([]any)
	if !ok || len(to) != 1 {
		t.Fatalf("DNS egress rule has %d peers, want one", len(to))
	}
	peer := to[0].(map[string]any)
	podSelector := objectAtKey(t, peer, "podSelector")
	labels := objectAtKey(t, podSelector, "matchLabels")
	if got := stringAtKey(t, labels, "k8s-app"); got != "kube-dns" {
		t.Errorf("DNS egress pod selector = %q, want kube-dns", got)
	}
	namespaceSelector := objectAtKey(t, peer, "namespaceSelector")
	namespaceLabels := objectAtKey(t, namespaceSelector, "matchLabels")
	if got := stringAtKey(t, namespaceLabels, "kubernetes.io/metadata.name"); got != "kube-system" {
		t.Errorf("DNS egress namespace = %q, want kube-system", got)
	}
	ports := rule["ports"].([]any)
	if len(ports) != 2 {
		t.Fatalf("DNS egress rule has %d ports, want UDP and TCP 53", len(ports))
	}
}

// TestKodamaChartServicesDeployment renders the enabled service adapters.
func TestKodamaChartServicesDeployment(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	rendered, err := renderWorkloadValues(t, helm, chart, baseWorkloadValues())
	if err != nil {
		t.Fatalf("helm template kodama with workloads: %v\n%s", err, rendered)
	}

	if got := renderedKindCount(rendered, "Deployment"); got != 4 {
		t.Fatalf("rendered %d Deployments, want the control plane plus three services (4)", got)
	}
	// No merge_coordinator workload renders.
	for _, object := range renderedObjects(t, rendered) {
		if object["kind"] != "Deployment" {
			continue
		}
		metadata := objectAtKey(t, object, "metadata")
		if strings.HasPrefix(stringAtKey(t, metadata, "name"), "kodama-service-merge") {
			t.Error("rendered a merge_coordinator service")
		}
	}

	for _, testCase := range []struct {
		name    string
		kind    string
		service string
		tenant  string
		project string
	}{
		{name: "responder", kind: "responder", service: "responder-svc", tenant: "tenant-a", project: "project-a"},
		{name: "timer", kind: "timer", service: "timer-svc", tenant: "tenant-b", project: "project-b"},
		{name: "email", kind: "email", service: "email-svc", tenant: "tenant-a", project: "project-a"},
	} {
		deployment := renderedObjectNamed(t, rendered, "Deployment", "kodama-service-"+testCase.name)
		metadata := objectAtKey(t, deployment, "metadata")
		if got := stringAtKey(t, metadata, "namespace"); got != "kodama-system" {
			t.Errorf("service %s namespace = %q, want the release namespace", testCase.name, got)
		}
		assertPodHardened(t, deployment)
		container := workloadContainer(t, deployment, "service")
		assertContainerHardened(t, deployment, container)
		assertWorkloadAPICertificate(t, deployment, "service")
		assertProbesPresent(t, container)
		// The adapter opens no listener and both probes are exec-based, so the
		// services NetworkPolicy can deny all ingress without cutting traffic.
		if ports, present := container["ports"]; present {
			t.Errorf("service %s container declares ports %v; the adapter has no listener", testCase.name, ports)
		}
		for _, probeName := range []string{"livenessProbe", "readinessProbe"} {
			probe := objectAtKey(t, container, probeName)
			if _, ok := probe["exec"]; !ok {
				t.Errorf("service %s %s is not exec-based: %v", testCase.name, probeName, probe)
			}
		}

		strategy := objectAtKey(t, objectAtKey(t, deployment, "spec"), "strategy")
		if got := stringAtKey(t, strategy, "type"); got != "Recreate" {
			t.Errorf("service %s strategy = %q, want Recreate so old and new adapters never overlap", testCase.name, got)
		}

		args := containerArgs(t, container)
		for _, want := range []string{
			"services",
			"--kind=" + testCase.kind,
			"--service=" + testCase.service,
			"--tenant=" + testCase.tenant,
			"--project=" + testCase.project,
			"--base-url=https://kodama.example.test:8443",
			"--credential-file=/etc/kodama/service/credential",
		} {
			if !containsArg(args, want) {
				t.Errorf("service %s args %v do not contain %q", testCase.name, args, want)
			}
		}
		if testCase.kind == "timer" && !containsArg(args, "--schedule=30s") {
			t.Errorf("timer service does not pass its schedule: %v", args)
		}
		if testCase.kind == "timer" {
			continue
		}
		if containsArg(args, "--schedule=30s") {
			t.Errorf("non-timer service %s passes a timer schedule", testCase.name)
		}

		// The service credential comes from an existing Secret only.
		volume := renderedVolume(t, deployment, "credential")
		secret := objectAtKey(t, volume, "secret")
		if got := stringAtKey(t, secret, "secretName"); got != "kodama-service-"+testCase.name {
			t.Errorf("service %s credential secretName = %q", testCase.name, got)
		}
		mount := findVolumeMount(t, container, "credential")
		if readOnly, ok := mount["readOnly"].(bool); !ok || !readOnly {
			t.Errorf("service %s credential mount is not read-only", testCase.name)
		}
	}

	// The email adapter receives a mailbox credential by reference only.
	email := renderedObjectNamed(t, rendered, "Deployment", "kodama-service-email")
	emailContainer := workloadContainer(t, email, "service")
	mailbox := renderedVolume(t, email, "mailbox")
	mailboxSecret := objectAtKey(t, mailbox, "secret")
	if got := stringAtKey(t, mailboxSecret, "secretName"); got != "kodama-email-mailbox" {
		t.Errorf("email mailbox secretName = %q", got)
	}
	if mount := findVolumeMount(t, emailContainer, "mailbox"); stringAtKey(t, mount, "mountPath") != "/etc/kodama/service/mailbox" {
		t.Errorf("email mailbox mountPath = %q", mount["mountPath"])
	}
	if entry := renderedEnvEntry(t, email, "KODAMA_MAILBOX_CREDENTIAL_FILE"); stringAtKey(t, entry, "value") != "/etc/kodama/service/mailbox/password" {
		t.Errorf("email mailbox credential env = %v", entry)
	}

	// Egress is exactly DNS plus the Kodama API, and ingress is isolated: the
	// adapter has no listener, so the empty ingress list denies everything
	// without affecting the exec probes or the egress calls.
	for _, testCase := range []string{"responder", "timer", "email"} {
		policy := renderedObjectNamed(t, rendered, "NetworkPolicy", "kodama-service-"+testCase)
		spec := objectAtKey(t, policy, "spec")
		policyTypes := rbacStringList(t, spec, "policyTypes")
		sort.Strings(policyTypes)
		if strings.Join(policyTypes, ",") != "Egress,Ingress" {
			t.Errorf("service %s NetworkPolicy policyTypes = %v, want Ingress and Egress so ingress is isolated", testCase, policyTypes)
		}
		if ingress, present := spec["ingress"]; present {
			if rules, ok := ingress.([]any); !ok || len(rules) != 0 {
				t.Errorf("service %s NetworkPolicy has ingress rules %v, want none", testCase, ingress)
			}
		}
		egress := sliceAtKey(t, spec, "egress")
		if len(egress) != 2 {
			t.Fatalf("service %s NetworkPolicy has %d egress rules, want DNS and the Kodama API", testCase, len(egress))
		}
		assertDNSEgressRule(t, egress[0].(map[string]any))
		apiRule := egress[1].(map[string]any)
		to := apiRule["to"].([]any)
		peer := to[0].(map[string]any)
		namespaceSelector := objectAtKey(t, peer, "namespaceSelector")
		labels := objectAtKey(t, namespaceSelector, "matchLabels")
		if got := stringAtKey(t, labels, "kubernetes.io/metadata.name"); got != "kodama-system" {
			t.Errorf("service %s API egress namespace = %q", testCase, got)
		}
	}
}

// TestKodamaChartWorkloadSchemaRefusals proves a partial or malformed block is
// refused before any object renders.
func TestKodamaChartWorkloadSchemaRefusals(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")

	cases := []struct {
		name   string
		mutate func(workloadValues)
	}{
		{
			name: "git enabled without tenants",
			mutate: func(values workloadValues) {
				workloadMap(values["gitService"])["tenants"] = []any{}
			},
		},
		{
			name: "git entry without tenant",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["gitService"])["tenants"])[0])
				delete(entry, "tenant")
			},
		},
		{
			name: "git tenant name too long for the StatefulSet label",
			mutate: func(values workloadValues) {
				long := "tenant-" + strings.Repeat("a", 35)
				values["tenants"] = []any{long, "tenant-b"}
				entry := workloadMap(workloadList(workloadMap(values["gitService"])["tenants"])[0])
				entry["tenant"] = long
			},
		},
		{
			name: "git entry without repositories",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["gitService"])["tenants"])[0])
				delete(entry, "repositories")
			},
		},
		{
			name: "git entry with empty repository list",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["gitService"])["tenants"])[0])
				entry["repositories"] = map[string]any{}
			},
		},
		{
			name: "git entry without credential secret",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["gitService"])["tenants"])[0])
				delete(entry, "credential")
			},
		},
		{
			name: "git entry with empty credential secret name",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["gitService"])["tenants"])[0])
				entry["credential"] = map[string]any{"existingSecret": "", "key": "token"}
			},
		},
		{
			name: "git entry without tls secret",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["gitService"])["tenants"])[0])
				delete(entry, "tls")
			},
		},
		{
			name: "git entry without storage",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["gitService"])["tenants"])[0])
				delete(entry, "storage")
			},
		},
		{
			name: "git entry without clients",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["gitService"])["tenants"])[0])
				delete(entry, "clients")
			},
		},
		{
			name: "git entry with empty client list",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["gitService"])["tenants"])[0])
				entry["clients"] = []any{}
			},
		},
		{
			name: "git tenant not declared",
			mutate: func(values workloadValues) {
				values["tenants"] = []any{"tenant-a"}
			},
		},
		{
			name: "duplicate git tenant",
			mutate: func(values workloadValues) {
				entries := workloadList(workloadMap(values["gitService"])["tenants"])
				workloadMap(values["gitService"])["tenants"] = append(entries, entries[0])
			},
		},
		{
			name: "services enabled without entries",
			mutate: func(values workloadValues) {
				workloadMap(values["services"])["entries"] = []any{}
			},
		},
		{
			name: "unknown service kind",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[0])
				entry["kind"] = "artifact_publisher"
			},
		},
		{
			name: "merge coordinator kind",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[0])
				entry["kind"] = "merge_coordinator"
			},
		},
		{
			name: "service without name",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[0])
				delete(entry, "name")
			},
		},
		{
			name: "service without service identity",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[0])
				delete(entry, "service")
			},
		},
		{
			name: "service without tenant",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[0])
				delete(entry, "tenant")
			},
		},
		{
			name: "service without project",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[0])
				delete(entry, "project")
			},
		},
		{
			name: "service without credential",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[0])
				delete(entry, "credential")
			},
		},
		{
			name: "timer without schedule",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[1])
				delete(entry, "schedule")
			},
		},
		{
			name: "non-timer with schedule",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[0])
				entry["schedule"] = "30s"
			},
		},
		{
			name: "service tenant not declared",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[0])
				entry["tenant"] = "tenant-z"
			},
		},
		{
			name: "mailbox credential without secret",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[2])
				entry["mailboxCredential"] = map[string]any{"key": "password"}
			},
		},
		{
			name: "mailbox credential without key",
			mutate: func(values workloadValues) {
				entry := workloadMap(workloadList(workloadMap(values["services"])["entries"])[2])
				entry["mailboxCredential"] = map[string]any{"existingSecret": "kodama-email-mailbox"}
			},
		},
		{
			name: "duplicate service name",
			mutate: func(values workloadValues) {
				entries := workloadList(workloadMap(values["services"])["entries"])
				workloadMap(values["services"])["entries"] = append(entries, entries[0])
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values := cloneWorkloadValues(baseWorkloadValues())
			testCase.mutate(values)
			rendered, err := renderWorkloadValues(t, helm, chart, values)
			if err == nil {
				t.Fatalf("helm accepted %s:\n%s", testCase.name, rendered)
			}
		})
	}
}
