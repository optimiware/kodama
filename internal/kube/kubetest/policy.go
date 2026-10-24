package kubetest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Images are fixture image references. They are digest-pinned by syntax only;
// unit tests never pull them. The cluster fixtures replace them with the locally
// built, digest-pinned images they load into the node.
const (
	WorkloadImage = "registry.t021.invalid/workload@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	InjectorImage = "registry.t021.invalid/injector@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// CA generates a throwaway CA certificate (PEM) for the runtime trust roots.
func CA(t testing.TB) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t021 test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// Tenant returns a tenant ID and its namespace/service-account target.
func Tenant(name string) (domain.TenantID, kube.TenantTarget) {
	return domain.TenantID("t021_" + name), kube.TenantTarget{Namespace: "t021-" + name, ServiceAccount: "t021-runner"}
}

// PolicyConfig is a complete, valid test policy for the given tenant names.
func PolicyConfig(t testing.TB, tenants ...string) kube.PolicyConfig {
	t.Helper()
	cfg := kube.PolicyConfig{
		Tenants:         map[domain.TenantID]kube.TenantTarget{},
		InjectorImage:   InjectorImage,
		InjectorCommand: []string{"cp", "/usr/local/bin/kodama", "/kodama/bin/kodama"},
		RuntimeURL:      "https://192.0.2.10:8443",
		RuntimeCA:       CA(t),
		RuntimeEgress:   []kube.EgressRule{{CIDR: "192.0.2.10/32", Ports: []int32{8443}}},
		Workload: kube.ResourceBounds{
			CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi", EphemeralRequest: "128Mi", EphemeralLimit: "1Gi",
		},
		Injector: kube.ResourceBounds{
			CPURequest: "10m", CPULimit: "100m", MemoryRequest: "16Mi", MemoryLimit: "64Mi", EphemeralRequest: "16Mi", EphemeralLimit: "128Mi",
		},
		Volumes: kube.VolumeSizes{
			Workspace: "256Mi", Outputs: "64Mi", Scratch: "128Mi", Results: "1Mi", Tools: "64Mi", Secrets: "4Mi", Identity: "1Mi",
		},
		RunAsUser: 65532, RunAsGroup: 65532, TerminationGrace: 30 * time.Second,
	}
	for _, n := range tenants {
		id, tgt := Tenant(n)
		cfg.Tenants[id] = tgt
	}
	return cfg
}

// Policy builds the validated policy of PolicyConfig.
func Policy(t testing.TB, tenants ...string) kube.ExecutionPolicy {
	t.Helper()
	p, err := kube.NewExecutionPolicy(PolicyConfig(t, tenants...))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Baseline provisions the fake cluster's namespace for a tenant exactly as the
// execution baseline requires, so a preflight passes; tests then break one
// property at a time.
func Baseline(f *Fake, p kube.ExecutionPolicy, tenant domain.TenantID) {
	tgt, err := p.Tenant(tenant)
	if err != nil {
		panic(err)
	}
	no := false
	f.Namespaces[tgt.Namespace] = kubernetes.Namespace{Metadata: kubernetes.ObjectMeta{
		Name:        tgt.Namespace,
		Labels:      map[string]string{"pod-security.kubernetes.io/enforce": "restricted"},
		Annotations: map[string]string{kube.AnnNamespaceTenant: string(tenant)},
	}}
	f.Namespaces[tgt.Namespace] = withPhase(f.Namespaces[tgt.Namespace], "Active")
	f.ServiceAccounts[tgt.Namespace+"/"+tgt.ServiceAccount] = kubernetes.ServiceAccount{
		Metadata: kubernetes.ObjectMeta{Name: tgt.ServiceAccount, Namespace: tgt.Namespace}, AutomountServiceAccountToken: &no,
	}
	var q kubernetes.ResourceQuota
	q.Metadata.Name = "compute"
	q.Spec.Hard = map[string]string{"pods": "20", "limits.cpu": "16", "limits.memory": "32Gi", "limits.ephemeral-storage": "100Gi"}
	f.Quotas[tgt.Namespace] = []kubernetes.ResourceQuota{q}
	var deny kubernetes.NetworkPolicy
	deny.Metadata.Name = "default-deny"
	deny.Spec.PolicyTypes = []string{"Ingress", "Egress"}
	var allow kubernetes.NetworkPolicy
	allow.Metadata.Name = "runtime-egress"
	allow.Spec.PodSelector.MatchLabels = map[string]string{kube.LabelManagedBy: kube.ValueManagedBy}
	allow.Spec.PolicyTypes = []string{"Egress"}
	for _, r := range p.RuntimeEgress() {
		var ports []kubernetes.NetworkPort
		for _, pt := range r.Ports {
			ports = append(ports, kubernetes.NetworkPort{Protocol: r.Protocol, Port: float64(pt)})
		}
		allow.Spec.Egress = append(allow.Spec.Egress, kubernetes.NetworkEgress{To: []kubernetes.NetworkPeer{{IPBlock: &kubernetes.IPBlock{CIDR: r.CIDR}}}, Ports: ports})
	}
	f.NetPols[tgt.Namespace] = []kubernetes.NetworkPolicy{deny, allow}
}

func withPhase(n kubernetes.Namespace, phase string) kubernetes.Namespace {
	n.Status.Phase = phase
	return n
}
