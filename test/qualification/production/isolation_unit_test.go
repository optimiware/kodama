//go:build qualification

package production

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// The fixtures below mirror the chart's rendered tenant namespace, LimitRange,
// NetworkPolicy and the runtime adapter's workload Pod. Each test mutates
// exactly one fact so the detector is proven red for a widened boundary and
// green for the deployed shape.

const isolationNamespaceFixture = `{
  "metadata": {
    "name": "kodama-tenant-tenant-a",
    "uid": "0d9e6b2a-5d15-4d35-9a65-62e2f2b2b3a1",
    "labels": {
      "kodama.dev/tenant": "tenant-a",
      "pod-security.kubernetes.io/enforce": "baseline",
      "pod-security.kubernetes.io/warn": "restricted",
      "pod-security.kubernetes.io/audit": "restricted"
    }
  }
}`

const isolationLimitRangeFixture = `{
  "metadata": {
    "name": "kodama-tenant-limits",
    "namespace": "kodama-tenant-tenant-a",
    "labels": {"kodama.dev/tenant": "tenant-a"}
  },
  "spec": {"limits": [{
    "type": "Container",
    "default": {"cpu": "1", "memory": "1Gi"},
    "defaultRequest": {"cpu": "250m", "memory": "256Mi"}
  }]}
}`

const isolationNetworkPolicyFixture = `{
  "metadata": {
    "name": "kodama-tenant-default-deny",
    "namespace": "kodama-tenant-tenant-a",
    "labels": {"kodama.dev/tenant": "tenant-a"}
  },
  "spec": {
    "podSelector": {},
    "policyTypes": ["Ingress", "Egress"],
    "ingress": [{
      "from": [{
        "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kodama-system"}},
        "podSelector": {"matchLabels": {"app.kubernetes.io/name": "kodama"}}
      }],
      "ports": [{"protocol": "TCP", "port": 8443}, {"protocol": "TCP", "port": 8080}]
    }],
    "egress": [
      {
        "to": [{
          "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
          "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}
        }],
        "ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}]
      },
      {
        "to": [{
          "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kodama-system"}},
          "podSelector": {"matchLabels": {"app.kubernetes.io/name": "kodama"}}
        }],
        "ports": [{"protocol": "TCP", "port": 8443}, {"protocol": "TCP", "port": 8080}]
      }
    ]
  }
}`

const isolationPodFixture = `{
  "metadata": {
    "name": "kodama-job-abc-1",
    "namespace": "kodama-tenant-tenant-a",
    "labels": {"kodama.dev/tenant": "tenant-a", "kodama.dev/execution": "execution-a"}
  },
  "spec": {
    "hostNetwork": false,
    "hostPID": false,
    "hostIPC": false,
    "automountServiceAccountToken": false,
    "volumes": [{"name": "workspace"}, {"name": "activation"}, {"name": "credential"}],
    "containers": [{
      "name": "workload",
      "resources": {
        "requests": {"cpu": "250m", "memory": "256Mi"},
        "limits": {"cpu": "1", "memory": "1Gi"}
      }
    }]
  },
  "status": {"phase": "Running", "podIP": "10.0.0.17"}
}`

// TestHarnessIsolationObjectsAcceptCompliantDeployment proves the chart's
// deployed shape passes every isolation check.
func TestHarnessIsolationObjectsAcceptCompliantDeployment(t *testing.T) {
	namespace, err := parseIsolationNamespace([]byte(isolationNamespaceFixture))
	if err != nil {
		t.Fatalf("parse namespace: %v", err)
	}
	if err := validateTenantNamespace(namespace, "tenant-a", "kodama-tenant-tenant-a", "kodama-tenant-tenant-b"); err != nil {
		t.Fatalf("the compliant namespace was refused: %v", err)
	}
	limits, err := parseIsolationLimitRange([]byte(isolationLimitRangeFixture), "tenant-a", "kodama-tenant-tenant-a")
	if err != nil {
		t.Fatalf("parse limit range: %v", err)
	}
	policy, err := parseIsolationNetworkPolicy([]byte(isolationNetworkPolicyFixture))
	if err != nil {
		t.Fatalf("parse network policy: %v", err)
	}
	if err := validateTenantNetworkPolicy(policy, "tenant-a", "kodama-tenant-tenant-a", "kodama-tenant-tenant-b", ReleaseNamespace); err != nil {
		t.Fatalf("the compliant network policy was refused: %v", err)
	}
	pods, err := parseIsolationPods([]byte(`{"items":[` + isolationPodFixture + `]}`))
	if err != nil || len(pods) != 1 {
		t.Fatalf("parse pod list: %v, %d pods", err, len(pods))
	}
	if err := validateIsolationPod(pods[0], "tenant-a", "kodama-tenant-tenant-a", limits); err != nil {
		t.Fatalf("the compliant workload pod was refused: %v", err)
	}
}

// TestHarnessIsolationObjectsRejectForeignNetworkPeer is the red case for a
// policy that would let tenant A reach tenant B.
func TestHarnessIsolationObjectsRejectForeignNetworkPeer(t *testing.T) {
	document := mutateIsolationFixture(t, isolationNetworkPolicyFixture, func(policy map[string]any) {
		spec := policy["spec"].(map[string]any)
		ingress := spec["ingress"].([]any)
		rule := ingress[0].(map[string]any)
		from := rule["from"].([]any)
		rule["from"] = append(from, map[string]any{
			"namespaceSelector": map[string]any{"matchLabels": map[string]any{isolationNamespaceNameLabel: "kodama-tenant-tenant-b"}},
		})
	})
	policy, err := parseIsolationNetworkPolicy(document)
	if err != nil {
		t.Fatalf("parse network policy: %v", err)
	}
	if err := validateTenantNetworkPolicy(policy, "tenant-a", "kodama-tenant-tenant-a", "kodama-tenant-tenant-b", ReleaseNamespace); err == nil {
		t.Fatal("a policy admitting the other tenant namespace was accepted")
	} else if !strings.Contains(err.Error(), "other tenant") {
		t.Fatalf("the foreign-peer refusal does not name the other tenant: %v", err)
	}
}

// TestHarnessIsolationObjectsRejectAllNamespacePeer is the red case for an
// empty namespace selector, an empty rule, an ipBlock, a same-namespace peer,
// an ingress peer outside the release namespace, or a rule that names no ports
// or the wrong port.
func TestHarnessIsolationObjectsRejectAllNamespacePeer(t *testing.T) {
	cases := map[string]func(map[string]any){
		"empty namespace selector": func(policy map[string]any) {
			spec := policy["spec"].(map[string]any)
			spec["ingress"] = []any{map[string]any{
				"from": []any{map[string]any{"namespaceSelector": map[string]any{}}},
			}}
		},
		"empty peer": func(policy map[string]any) {
			spec := policy["spec"].(map[string]any)
			spec["egress"] = []any{map[string]any{"to": []any{map[string]any{}}}}
		},
		"no egress rule": func(policy map[string]any) {
			policy["spec"].(map[string]any)["egress"] = []any{}
		},
		"ipblock peer": func(policy map[string]any) {
			spec := policy["spec"].(map[string]any)
			rule := spec["ingress"].([]any)[0].(map[string]any)
			rule["from"] = append(rule["from"].([]any), map[string]any{"ipBlock": map[string]any{"cidr": "10.0.0.0/8"}})
		},
		"ipblock every address": func(policy map[string]any) {
			spec := policy["spec"].(map[string]any)
			rule := spec["ingress"].([]any)[0].(map[string]any)
			rule["from"] = append(rule["from"].([]any), map[string]any{"ipBlock": map[string]any{"cidr": "0.0.0.0/0"}})
		},
		"same namespace peer": func(policy map[string]any) {
			spec := policy["spec"].(map[string]any)
			spec["ingress"] = []any{map[string]any{
				"from":  []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app": "peer"}}}},
				"ports": []any{map[string]any{"protocol": "TCP", "port": 8443}},
			}}
		},
		"ingress from kube-system": func(policy map[string]any) {
			spec := policy["spec"].(map[string]any)
			spec["ingress"] = []any{map[string]any{
				"from":  []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{isolationNamespaceNameLabel: isolationReleaseDNSNamespace}}}},
				"ports": []any{map[string]any{"protocol": "TCP", "port": 53}},
			}}
		},
		"ingress from git": func(policy map[string]any) {
			spec := policy["spec"].(map[string]any)
			spec["ingress"] = []any{map[string]any{
				"from":  []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{isolationNamespaceNameLabel: isolationGitNamespace}}}},
				"ports": []any{map[string]any{"protocol": "TCP", "port": 443}},
			}}
		},
		"ingress on a non kodama port": func(policy map[string]any) {
			spec := policy["spec"].(map[string]any)
			rule := spec["ingress"].([]any)[0].(map[string]any)
			rule["ports"] = []any{map[string]any{"protocol": "TCP", "port": 443}}
		},
		"dns egress on the kodama port": func(policy map[string]any) {
			spec := policy["spec"].(map[string]any)
			rule := spec["egress"].([]any)[0].(map[string]any)
			rule["ports"] = []any{map[string]any{"protocol": "TCP", "port": 8443}}
		},
		"egress rule with no ports": func(policy map[string]any) {
			spec := policy["spec"].(map[string]any)
			rule := spec["egress"].([]any)[0].(map[string]any)
			delete(rule, "ports")
		},
		"missing egress type": func(policy map[string]any) {
			policy["spec"].(map[string]any)["policyTypes"] = []any{"Ingress"}
		},
		"missing ingress type": func(policy map[string]any) {
			policy["spec"].(map[string]any)["policyTypes"] = []any{"Egress"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			document := mutateIsolationFixture(t, isolationNetworkPolicyFixture, mutate)
			policy, err := parseIsolationNetworkPolicy(document)
			if err != nil {
				t.Fatalf("parse network policy: %v", err)
			}
			if err := validateTenantNetworkPolicy(policy, "tenant-a", "kodama-tenant-tenant-a", "kodama-tenant-tenant-b", ReleaseNamespace); err == nil {
				t.Fatal("a policy with an all-namespace, ipBlock or wrong-port boundary was accepted")
			}
		})
	}
}

// TestHarnessIsolationObjectsRejectUnboundedWorkload is the red case for a Pod
// without limits, with a host namespace or with a host path.
func TestHarnessIsolationObjectsRejectUnboundedWorkload(t *testing.T) {
	limits := isolationResources{CPURequest: "250m", CPULimit: "1", MemoryRequest: "256Mi", MemoryLimit: "1Gi"}
	cases := map[string]func(map[string]any){
		"missing memory limit": func(pod map[string]any) {
			container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			delete(container["resources"].(map[string]any)["limits"].(map[string]any), "memory")
		},
		"limit below request": func(pod map[string]any) {
			container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			container["resources"].(map[string]any)["limits"].(map[string]any)["cpu"] = "100m"
		},
		"host network": func(pod map[string]any) {
			pod["spec"].(map[string]any)["hostNetwork"] = true
		},
		"host pid": func(pod map[string]any) {
			pod["spec"].(map[string]any)["hostPID"] = true
		},
		"host ipc": func(pod map[string]any) {
			pod["spec"].(map[string]any)["hostIPC"] = true
		},
		"automount service account token": func(pod map[string]any) {
			pod["spec"].(map[string]any)["automountServiceAccountToken"] = true
		},
		"implicit automount with kube api access volume": func(pod map[string]any) {
			spec := pod["spec"].(map[string]any)
			delete(spec, "automountServiceAccountToken")
			spec["volumes"] = append(spec["volumes"].([]any), map[string]any{"name": "kube-api-access-abcde"})
		},
		"host path": func(pod map[string]any) {
			spec := pod["spec"].(map[string]any)
			spec["volumes"] = append(spec["volumes"].([]any), map[string]any{"name": "host", "hostPath": map[string]any{"path": "/etc"}})
		},
		"privileged container": func(pod map[string]any) {
			container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			container["securityContext"] = map[string]any{"privileged": true}
		},
		"missing cpu request": func(pod map[string]any) {
			container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			delete(container["resources"].(map[string]any)["requests"].(map[string]any), "cpu")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			document := mutateIsolationFixture(t, isolationPodFixture, mutate)
			pods, err := parseIsolationPods([]byte(`{"items":[` + string(document) + `]}`))
			if err != nil || len(pods) != 1 {
				t.Fatalf("parse pod: %v", err)
			}
			if err := validateIsolationPod(pods[0], "tenant-a", "kodama-tenant-tenant-a", limits); err == nil {
				t.Fatal("an unbounded workload Pod was accepted")
			}
		})
	}
}

// TestHarnessIsolationObjectsRejectMissingTenantLabels proves a namespace or
// limit range without the tenant label fails closed.
func TestHarnessIsolationObjectsRejectMissingTenantLabels(t *testing.T) {
	namespaceDocument := mutateIsolationFixture(t, isolationNamespaceFixture, func(namespace map[string]any) {
		delete(namespace["metadata"].(map[string]any)["labels"].(map[string]any), isolationLabelTenant)
	})
	namespace, err := parseIsolationNamespace(namespaceDocument)
	if err != nil {
		t.Fatalf("parse namespace: %v", err)
	}
	if err := validateTenantNamespace(namespace, "tenant-a", "kodama-tenant-tenant-a", ""); err == nil {
		t.Fatal("a namespace without the tenant label was accepted")
	}
	limitDocument := mutateIsolationFixture(t, isolationLimitRangeFixture, func(limit map[string]any) {
		delete(limit["metadata"].(map[string]any)["labels"].(map[string]any), isolationLabelTenant)
	})
	if _, err := parseIsolationLimitRange(limitDocument, "tenant-a", "kodama-tenant-tenant-a"); err == nil {
		t.Fatal("a LimitRange without the tenant label was accepted")
	}
}

// TestHarnessIsolationObjectsRejectPrivilegedPodSecurity is the red case for a
// tenant namespace that weakens the chart's baseline/restricted profile.
func TestHarnessIsolationObjectsRejectPrivilegedPodSecurity(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"privileged enforce": func(namespace map[string]any) {
			namespace["metadata"].(map[string]any)["labels"].(map[string]any)[isolationPodSecurityEnforce] = "privileged"
		},
		"missing restricted warn": func(namespace map[string]any) {
			delete(namespace["metadata"].(map[string]any)["labels"].(map[string]any), isolationPodSecurityWarn)
		},
		"missing restricted audit": func(namespace map[string]any) {
			delete(namespace["metadata"].(map[string]any)["labels"].(map[string]any), isolationPodSecurityAudit)
		},
	} {
		t.Run(name, func(t *testing.T) {
			document := mutateIsolationFixture(t, isolationNamespaceFixture, mutate)
			namespace, err := parseIsolationNamespace(document)
			if err != nil {
				t.Fatalf("parse namespace: %v", err)
			}
			if err := validateTenantNamespace(namespace, "tenant-a", "kodama-tenant-tenant-a", "kodama-tenant-tenant-b"); err == nil {
				t.Fatal("a namespace with a weakened pod-security profile was accepted")
			}
		})
	}
}

// TestHarnessIsolationQuantityAndPortParsing pins the bounded quantity subset.
func TestHarnessIsolationQuantityAndPortParsing(t *testing.T) {
	if value, err := isolationCPU("250m"); err != nil || value != 250 {
		t.Fatalf("cpu 250m = %d, %v; want 250", value, err)
	}
	if value, err := isolationCPU("1"); err != nil || value != 1000 {
		t.Fatalf("cpu 1 = %d, %v; want 1000", value, err)
	}
	if value, err := isolationMemory("256Mi"); err != nil || value != 256<<20 {
		t.Fatalf("memory 256Mi = %d, %v", value, err)
	}
	if _, err := isolationCPU("0"); err == nil {
		t.Fatal("a zero CPU quantity was accepted")
	}
	if _, err := isolationMemory(""); err == nil {
		t.Fatal("an empty memory quantity was accepted")
	}
	if value, ok := isolationPortValue(json.RawMessage(`8443`)); !ok || value != 8443 {
		t.Fatalf("numeric port = %d, %v", value, ok)
	}
	if value, ok := isolationPortValue(json.RawMessage(`"https"`)); ok {
		t.Fatalf("a named port was accepted as numeric: %d", value)
	}
}

// TestHarnessIsolationForeignRefusalIsUniformNotFound pins the cross-tenant
// refusal classifier: only 404 not_found passes, and a success or another
// status is red.
func TestHarnessIsolationForeignRefusalIsUniformNotFound(t *testing.T) {
	if err := isolationNotFoundError(&APIError{Status: http.StatusNotFound, Code: "not_found"}); err != nil {
		t.Fatalf("a 404 not_found refusal was rejected: %v", err)
	}
	for name, err := range map[string]error{
		"success":         nil,
		"forbidden":       &APIError{Status: http.StatusForbidden, Code: "forbidden"},
		"unauthenticated": &APIError{Status: http.StatusUnauthorized, Code: "unauthenticated"},
		"transport":       errors.New("the public endpoint is unavailable"),
	} {
		if refusal := isolationNotFoundError(err); refusal == nil {
			t.Fatalf("%s was accepted as the uniform not_found refusal", name)
		}
	}
}

// TestHarnessIsolationGitRefusal pins the Git refusal classifier: a served 200
// advertisement is red, a 401/403/404 is the only accepted refusal.
func TestHarnessIsolationGitRefusal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		if err := isolationGitRefusedError(status, nil); err != nil {
			t.Fatalf("status %d was rejected: %v", status, err)
		}
	}
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError, 0} {
		if err := isolationGitRefusedError(status, nil); err == nil {
			t.Fatalf("status %d was accepted as a Git refusal", status)
		}
	}
}

// TestHarnessIsolationGitAdvertisementScopesCredentials drives the real Git
// advertisement helper against a service that serves one repository to one
// credential. The own-credential read is green, the foreign read is red, and a
// service that serves the foreign credential is caught as a widened boundary.
func TestHarnessIsolationGitAdvertisementScopesCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if ok && username == "kodama" && password == "token-a" && request.URL.Path == "/repo-a/info/refs" {
			writer.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			writer.WriteHeader(http.StatusOK)
			return
		}
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	status, header, _, err := isolationGitAdvertisement(context.Background(), server.Client(), server.URL, "repo-a", "token-a")
	isolationExpectGitAllowed(t, "own Git credential", status, header, err)
	status, _, _, err = isolationGitAdvertisement(context.Background(), server.Client(), server.URL, "repo-a", "token-b")
	if refusal := isolationGitRefusedError(status, err); refusal != nil {
		t.Fatalf("the foreign Git credential was not refused: %v", refusal)
	}

	permissive := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		writer.WriteHeader(http.StatusOK)
	}))
	defer permissive.Close()
	status, _, _, err = isolationGitAdvertisement(context.Background(), permissive.Client(), permissive.URL, "repo-a", "token-b")
	if refusal := isolationGitRefusedError(status, err); refusal == nil {
		t.Fatal("a Git service that served a foreign credential was accepted as isolated")
	}
}

// TestHarnessIsolationNetworkCommands pins the live cross-tenant commands: the
// client must target the other tenant's address, reach Kodama and refuse the
// Kubernetes API; the server must fail when nc is absent.
func TestHarnessIsolationNetworkCommands(t *testing.T) {
	client := string(isolationNetworkClientPipeline("image@sha256:"+strings.Repeat("a", 64), "10.0.0.17", "kodama", "isolated-marker"))
	for _, want := range []string{"http://10.0.0.17:18080/", "https://kodama.kodama-system.svc.cluster.local:8443/healthz", "https://kubernetes.default.svc/api", "isolated-marker"} {
		if !strings.Contains(client, want) {
			t.Fatalf("the network client command does not contain %q:\n%s", want, client)
		}
	}
	server := string(isolationNetworkServerPipeline("image@sha256:"+strings.Repeat("a", 64), "ready-marker", "done-marker"))
	for _, want := range []string{"command -v nc", "command -v python3", "HTTP/1.1 200 OK", "timeout 300", "while :", "nc -l -p 18080", "q02-nc-missing", "ready-marker", "done-marker"} {
		if !strings.Contains(server, want) {
			t.Fatalf("the network server command does not contain %q:\n%s", want, server)
		}
	}
}

// TestHarnessIsolationClaimsRequireOwnWorkspace is the red case for a tenant
// namespace with no workspace claim, or with a claim that names the other
// tenant. An empty claim list must fail: otherwise the cross-namespace
// disjointness check passes vacuously.
func TestHarnessIsolationClaimsRequireOwnWorkspace(t *testing.T) {
	claim := isolationPVC{}
	claim.Metadata.Name = "workspace-abc"
	claim.Metadata.Namespace = "kodama-tenant-tenant-a"
	claim.Metadata.Labels = map[string]string{isolationLabelTenant: "tenant-a"}
	names, err := isolationValidateClaims([]isolationPVC{claim}, "tenant-a", "kodama-tenant-tenant-a", "tenant-b")
	if err != nil {
		t.Fatalf("a compliant claim was refused: %v", err)
	}
	if !names["workspace-abc"] {
		t.Fatal("a compliant claim was not returned by name")
	}
	if _, err := isolationValidateClaims(nil, "tenant-a", "kodama-tenant-tenant-a", "tenant-b"); err == nil {
		t.Fatal("a namespace with no workspace claim was accepted")
	}
	foreign := claim
	foreign.Metadata.Labels = map[string]string{isolationLabelTenant: "tenant-b"}
	if _, err := isolationValidateClaims([]isolationPVC{foreign}, "tenant-a", "kodama-tenant-tenant-a", "tenant-b"); err == nil {
		t.Fatal("a claim owned by the other tenant was accepted")
	}
}

// mutateIsolationFixture decodes one JSON fixture, applies a mutation and
// re-encodes it.
func mutateIsolationFixture(t *testing.T, fixture string, mutate func(map[string]any)) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(fixture), &document); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	mutate(document)
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode mutated fixture: %v", err)
	}
	return encoded
}

// TestHarnessIsolationDocumentsAreAdmitted proves every pipeline document the
// cross-tenant and network probes author passes the same authoring limits the
// server applies, so a fixture-only mistake (a below-minimum action timeout, an
// unbounded name) fails here instead of surfacing as a stage invalid_input.
func TestHarnessIsolationDocumentsAreAdmitted(t *testing.T) {
	image := "rg.example.test/kodama@sha256:" + strings.Repeat("a", 64)
	documents := map[string][]byte{
		"cross-tenant": isolationPipelineDocument(image, "q02-record"),
		"workspace":    isolationWorkspacePipeline(image, "own", "tenant-a", "other", "log", "isolated"),
		"listener":     isolationNetworkServerPipeline(image, "ready", "done"),
		"client":       isolationNetworkClientPipeline(image, "10.0.0.1", "kodama.kodama-system.svc.cluster.local", "isolated"),
	}
	for name, document := range documents {
		if _, err := pipeline.Decode(document, pipeline.DefaultLimits); err != nil {
			t.Fatalf("%s pipeline document is not admitted: %v", name, err)
		}
	}
}

// TestHarnessIsolationGitSecretTokenTrimsTrailingNewline proves Git service
// credential Secrets written with a trailing newline decode cleanly while
// embedded whitespace and wrong prefixes stay refused.
func TestHarnessIsolationGitSecretTokenTrimsTrailingNewline(t *testing.T) {
	const raw = "kodsvc.git.abcDEF123_-"
	for _, encoded := range []string{
		base64.StdEncoding.EncodeToString([]byte(raw)),
		base64.StdEncoding.EncodeToString([]byte(raw + "\n")),
		base64.StdEncoding.EncodeToString([]byte(raw + "\r\n")),
	} {
		token, err := parseIsolationGitSecretToken("kodama-git-tenant-a", encoded)
		if err != nil || token.Reveal() != raw {
			t.Fatalf("parseIsolationGitSecretToken = %q, err = %v; want %q", token.Reveal(), err, raw)
		}
	}
	for _, bad := range []string{
		"",
		"not-base64!",
		base64.StdEncoding.EncodeToString([]byte("kodsvc.git.abc def\n")),
		base64.StdEncoding.EncodeToString([]byte("kodex.exec-1.abcDEF\n")),
	} {
		if _, err := parseIsolationGitSecretToken("kodama-git-tenant-a", bad); err == nil {
			t.Fatalf("parseIsolationGitSecretToken accepted %q", bad)
		}
	}
}
